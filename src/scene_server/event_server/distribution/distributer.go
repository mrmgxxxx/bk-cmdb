/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package distribution

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"configcenter/src/apimachinery"
	"configcenter/src/apimachinery/discovery"
	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/metadata"
	"configcenter/src/common/util"
	"configcenter/src/common/watch"
	"configcenter/src/scene_server/event_server/identifier"
	"configcenter/src/scene_server/event_server/types"
	"configcenter/src/source_controller/coreservice/event"
	"configcenter/src/storage/dal"
	"configcenter/src/storage/reflector"
	"configcenter/src/storage/stream/types"

	"gopkg.in/redis.v5"
)

const (
	// defaultListWatchPageSize is default page size of list watcher.
	defaultListWatchPageSize = 500

	// defaultWatchEventStepSize is default step size of watch event.
	defaultWatchEventStepSize = 200

	// defaultWatchEventLoopInternal is default watch event loop interval.
	defaultWatchEventLoopInternal = 250 * time.Millisecond
)

// Distributer is event subscription distributer.
type Distributer struct {
	ctx context.Context

	// db is cc main database.
	db dal.RDB

	// cache is cc redis client.
	cache *redis.Client

	// subWatcher is subscription watcher.
	subWatcher reflector.Interface

	// subscriptions is local subscriptions records, update by listwatcher, subscriptionid -> subscription.
	subscriptions map[int64]interface{}

	// subscriptionsMu is subscriptions mutex.
	subscriptionsMu sync.RWMutex

	// subscribers is event subscribers map, key(ownerid:event_type) -> subscriberIDs.
	// could find subids by ownerid and event type in records.
	subscribers map[string][]int64

	// subscribersMu is subscribers mutex.
	subscribersMu sync.RWMutex

	// waitForHandleResourceCursors is channel to wait for handling resource cursors.
	waitForHandleResourceCursors <-chan struct{}

	// resourceCursors is cursors for resource, CursorType -> oldest cursor.
	resourceCursors map[watch.CursorType]*watch.Cursor

	// resourceCursorsMu is resourceCursors mutex.
	resourceCursorsMu sync.RWMutex

	// eventHandler is event handler that handles all event senders.
	eventHandler *EventHandler
}

// NewDistributer creates a new Distributer instance.
func NewDistributer(ctx context.Context, db dal.RDB, cache *redis.Client,
	subWatcher reflector.Interface, eventHandler *EventHandler) *Distributer {
	return &Distributer{
		ctx:                          ctx,
		db:                           db,
		cache:                        cache,
		subWatcher:                   subWatcher,
		eventHandler:                 eventHandler,
		subscriptions:                make(map[int64]interface{}),
		subscribers:                  make(map[string][]int64),
		resourceCursors:              make(map[CursorType]*watch.Cursor),
		waitForHandleResourceCursors: make(<-chan struct{}),
	}
}

// LoadSubscriptions loads all subscriptions in cc.
func (d *Distributer) LoadSubscriptions() error {
	// list and watch subscriptions.
	opts := types.Options{
		EventStruct: make(map[string]interface{}),
		Collection:  common.BKTableNameSubscription,
	}

	// set event handler callback funcs.
	listWatchCap := &reflector.Capable{
		OnChange: reflector.OnChangeEvent{
			OnLister:     d.onUpsertSubscriptions,
			OnListerDone: d.onListSubscriptionsDone,
			OnAdd:        d.onUpsertSubscriptions,
			OnUpdate:     d.onUpsertSubscriptions,
			OnDelete:     d.onDeleteSubscriptions,
		},
	}

	// set list watch options.
	listOpts := &types.ListWatchOptions{
		Options:  opts,
		PageSize: &defaultListWatchPageSize,
	}

	// run to list and keep watching subscriptions.
	return d.subWatcher.ListWatcher(context.Background(), listOpts, listWatchCap)
}

// onUpsertSubscriptions handles event that target subscription inserted or updated.
// It add or update subscription metadata and subscriber in local chains.
func (d *Distributer) onUpsertSubscriptions(e *types.Event) {
	d.subscriptionsMu.Lock()
	defer d.subscriptionsMu.Unlock()

	subscription := e.Document.(*metadata.Subscription)

	oldSubscription, isExist := d.subscriptions[subscription.SubscriptionID]
	if !isExist {
		// insert.
		d.subscriptions[subscription.SubscriptionID] = subscription
		eventTypes := strings.Split(subscription.SubscriptionForm, ",")

		// adds.
		for _, eventType := range eventTypes {
			d.addSubscriber(subscription.OwnerID, eventType, subscription.SubscriptionID)
		}
	} else {
		// update.
		if subscription.GetCacheKey() != oldSubscription.GetCacheKey() {
			d.subscriptions[subscription.SubscriptionID] = subscription
		}

		// update event types.
		oldEventTypes := strings.Split(oldSubscription.SubscriptionForm, ",")
		eventTypes := strings.Split(subscription.SubscriptionForm, ",")
		subs, plugs := util.CalSliceDiff(oldEventTypes, eventTypes)

		// removes.
		for _, eventType := range subs {
			d.remSubscriber(subscription.OwnerID, eventType, subscription.SubscriptionID)
		}

		// adds.
		for _, eventType := range plugs {
			d.addSubscriber(subscription.OwnerID, eventType, subscription.SubscriptionID)
		}
	}
}

// onDeleteSubscriptions handles event that target subscription deleted.
// It delete local subscription metadata and removes subscriber in local chains.
func (d *Distributer) onDeleteSubscriptions(e *types.Event) {
	d.subscriptionsMu.Lock()
	defer d.subscriptionsMu.Unlock()

	subscription := e.Document.(*metadata.Subscription)

	if _, isExist := d.subscriptions[subscription.SubscriptionID]; isExist {
		delete(d.subscriptions, subscription.SubscriptionID)
	}

	// removes.
	eventTypes := strings.Split(subscription.SubscriptionForm, ",")

	for _, eventType := range eventTypes {
		d.remSubscriber(subscription.OwnerID, eventType, subscription.SubscriptionID)
	}
}

// onListSubscriptionsDone handles event that after LIST-DONE at distributer first setup.
func (d *Distributer) onListSubscriptionsDone() {
	d.subscriptionsMu.RLock()
	defer d.subscriptionsMu.RUnlock()

	blog.Info("distributer listwatch subscriptions at first setup done, count[%d]", len(d.subscriptions))

	d.waitForHandleResourceCursors <- struct{}{}
}

func (d *Distributer) getResourceCursor(cursorType CursorType) (*watch.Cursor, error) {
	d.subscribersMu.RLock()
	defer d.subscribersMu.RUnlock()

	for k, v := range d.subscribers {
		// parse resource type.
		owneridEventType := strings.Split(k)
		if len(owneridEventType) != 2 {
			return nil, fmt.Errorf("parse resource cursors failed, the local subscribers key is invalid, %s", k)
		}

		if watch.ParseCursorTypeFromEventType(owneridEventType[1]) != cursorType {
			continue
		}

		cursorKey := fmt.Sprintf("%s%s", EventCacheSubscriberCursorPrefix, k)

		// range subscribers on target resource event.
		for _, subid := range v {
			cursorKey := fmt.Sprintf("%s:%s", cursorKey, subid)

			// get target subscriber cursor.
			var subCursor *watch.Cursor

			val, err := d.cache.Get(cursorKey).Result()
			if err != nil {
				return nil, fmt.Errorf("parse resource cursors failed, quert subscriber cursor[%s], %+v", cursorKey, err)
			}

			if len(val) != 0 {
				cursor := &watch.Cursor{}
				if err := cursor.Decode(val); err != nil {
					return nil, fmt.Errorf("parse resource cursors failed, invalid cursor[%s], %+v", cursorKey, err)
				}
				subCursor = cursor
			}

			// update local resource oldest cursors.
			if subCursor == nil {
				continue
			}

			d.resourceCursorsMu.Lock()
			if oldestCursor, isExist := d.resourceCursors[cursorType]; !isExist {
				d.resourceCursors[cursorType] = subCursor
			} else {
				// compare to get oldest cursor.
				if subCursor.ClusterTime.Sec < oldestCursor.ClusterTime.Sec {
					d.resourceCursors[cursorType] = subCursor
				}
			}
			d.resourceCursorsMu.Unlock()
		}
	}

	return d.resourceCursors[cursorType], nil
}

func (d *Distributer) watchAndDistribute(cursorType CursorType) error {
	// get inner resource key.
	resourcekey, err := event.GetResourceKeyWithCursorType(cursorType)
	if err != nil {
		return err
	}

	go func() {
		for {
			// try get newest cursor every time.
			cursor, err := d.getResourceCursor(cursorType)
			if err != nil {
				blog.Info("watch and distribute for resource[%+v] failed, can't get subscriber "+
					"cursor, using head key now, %+v", cursorType, err)
			}

			opts := &watch.WatchEventOptions{Resource: cursorType, Cursor: watch.NoEventCursor}

			if cursor != nil {
				cursorStr, err := cursor.Encode()
				if err != nil {
					blog.Info("watch and distribute for resource[%+v] failed, can't encode "+
						"subscriber cursor, %+v", cursorType, err)
				} else {
					opts.Cursor = cursorStr
				}
			}

			// watch resource with cursor.
			if err := d.watchAndDistributeWithCursor(cursorType, resourcekey, opts); err != nil {
				blog.Info("watch and distribute for resource[%+v] failed, retry now, %+v", err)
				time.Sleep(defaultWatchEventLoopInternal)
			}
		}
	}()
}

func (d *Distributer) watchAndDistributeWithCursor(cursorType CursorType, key event.Key, opts *watch.WatchEventOptions) error {
	// build a resource watcher.
	watcher := NewWatcher(s.ctx, s.cache)

	startCursor := opts.Cursor
	if startCursor == nil {
		startCursor = key.HeadKey()
	}

	for {
		nodes, err := watcher.GetNodesFromCursor(defaultWatchEventStepSize, startCursor, key)
		if err != nil {
			if err == watcher.HeadNodeNotExistError {
				time.Sleep(defaultWatchEventLoopInternal)
				continue
			}
			return err
		}

		if len(nodes) == 0 {
			time.Sleep(defaultWatchEventLoopInternal)
			continue
		}
		lastNode := nodes[len(nodes)-1]

		hitNodes := watcher.GetHitNodeWithEventType(nodes, opts.EventTypes)
		if len(hitNodes) == 0 {
			startCursor = lastNode.Cursor
			time.Sleep(defaultWatchEventLoopInternal)
			continue
		}

		// get hit events.
		events := watcher.GetEventsWithCursorNodes(opts, hitNodes, key, "INNER-WATCHER")

		// distribute to subscriber senders.
		if err := d.eventHandler.Distribute(events); err != nil {
			blog.Info("distribute resource[%+v] events[%d] to event handler failed, %+v", cursorType, len(events), err)
			time.Sleep(defaultWatchEventLoopInternal)
			continue
		}

		// distribute success and try to watch next round.
		startCursor = lastNode.Cursor
	}

	return nil
}

// subscriberKey returns the key for subscriber in local records.
func (d *Distributer) subscriberKey(ownerid, eventType string) string {
	return fmt.Sprintf("%s:%s", ownerid, eventType)
}

// addSubscriber adds new subscriber with target event type.
func (d *Distributer) addSubscriber(ownerid, eventType string, subid int64) {
	d.subscribersMu.Lock()
	defer d.subscribersMu.Unlock()

	subKey := d.subscriberKey(ownerid, eventType)
	subscribers := d.subscribers[subKey]

	for _, id := range subscribers {
		if subid == id {
			// already exist.
			return
		}
	}
	d.subscribers[subKey] = append(d.subscribers[subKey], subid)
}

// remSubscriber removes subscriber with target event type.
func (d *Distributer) remSubscriber(ownerid, eventType string, subid int64) {
	d.subscribersMu.Lock()
	defer d.subscribersMu.Unlock()

	subKey := d.subscriberKey(ownerid, eventType)
	subscribers := d.subscribers[subKey]

	updated := []int64{}
	for _, id := range subscribers {
		if subid != id {
			updated = append(updated, id)
		}
	}
	d.subscribers[subKey] = updated
}

// findSubscribers returns all subscribers on event type of target ownerid.
func (d *Distributer) findSubscribers(ownerid, eventType string) []int64 {
	d.subscribersMu.RLock()
	defer d.subscribersMu.RUnlock()

	return d.subscribers[fmt.Sprintf("%s:%s", ownerid, eventType)]
}

// Start starts the Distributer, it would load all subscriptions in listwatch mode, and handle runtime
// subscription update messages, push event to subscribers when tatget event happend.
func (d *Distributer) Start() error {
	blog.Info("distributer starting now!")

	// list and keep watching subscriptions.
	if err := d.LoadSubscriptions(); err != nil {
		return fmt.Errorf("load subscriptions at first setups failed, %+v", err)
	}

	// handle resource cursors.
	if err := d.handleResourceCursors(); err != nil {
		return fmt.Errorf("handle resource cursors failed, %+v", err)
	}

	// wait for LIST-DONE to handle resource cursors.
	<-d.waitForHandleResourceCursors

	// range all resource cursors and watch to distribute.
	for cursorType := range watch.ListCursorTypes() {
		if err := d.watchAndDistribute(cursorType); err != nil {
			return fmt.Errorf("watch and distribute resource events failed, %+v", err)
		}
	}

	blog.Info("distributer start success!")
}
