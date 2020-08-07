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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/http/httpclient"
	"configcenter/src/common/metadata"
	"configcenter/src/common/watch"
	"configcenter/src/scene_server/event_server/types"

	"gopkg.in/redis.v5"
)

var httpCli = httpclient.NewHttpClient()

const (
	// defaultTransTimeout is default timeout for trans event data from base queue to identifier
	// duplicate queue.
	defaultTransTimeout = 60 * time.Second

	// defaultHandleTimeout is default timeout for event handle.
	defaultHandleTimeout = 2 * time.Second

	// defaultSendTimeout is default timeout for send action.
	defaultSendTimeout = 10 * time.Second

	// defaultFusingEventExpireSec is default expire second num for event.
	defaultFusingEventExpireSec = 5 * 60
)

// EventSender sends target events to subscribers in callback mode.
type EventSender struct {
	ctx context.Context

	// subid is subscription id.
	subid int64

	// cache is cc redis client.
	cache *redis.Client

	// distributer handles all events distribution.
	distributer *Distributer

	// hash collections hash object, that updates target nodes in dynamic mode,
	// and calculates node base on hash key of data.
	hash *Hash
}

// NewEventSender creates a new EventSender object.
func NewEventSender(ctx context.Context, subid int64, cache *redis.Client, distributer *Distributer, hash *Hash) *EventSender {
	return &EventSender{
		ctx:         ctx,
		subid:       subid,
		cache:       cache,
		distributer: distributer,
		hash:        hash,
	}
}

// Handle add event dist inst to subscriber chan, and sender would send message to
// subscriber base on callback.
func (s *EventSender) Handle(dist *metadata.DistInst) error {
	if dist == nil {
		return errors.New("invalid event dist metadata")
	}

	distData, err := json.Marshal(dist)
	if err != nil {
		return err
	}

	if _, err := s.cache.LPush(types.EventCacheSubscriberEventQueueKeyPrefix+fmt.Sprint(dist.SubscriptionID), distData).Result(); err != nil {
		return err
	}

	return nil
}

func (s *EventSender) increaseTotal(subid int64) error {
	return s.statIncrease(subid, "total")
}

func (s *EventSender) increaseFailure(subid int64) error {
	return s.statIncrease(subid, "failue")
}

func (s *EventSender) statIncrease(subid int64, key string) error {
	return s.cache.HIncrBy(types.EventCacheDistCallBackCountPrefix+strconv.FormatInt(subid, 10), key, 1).Err()
}

func (s *EventSender) send(dist *metadata.DistInst) error {
	subscription := s.distributer.FindSubscription(s.subid)
	if subscription == nil {
		return fmt.Errorf("subscription not found, %+v", s.subid)
	}
	s.increaseTotal(subscription.SubscriptionID)

	var errFinal error

	defer func() {
		if errFinal != nil {
			s.increaseFailure(subscription.SubscriptionID)
		}
	}()

	// marshal message data.
	distData, err := json.Marshal(dist)
	if err != nil {
		errFinal = err
		return err
	}

	// build http request.
	body := bytes.NewBuffer(distData)
	req, err := http.NewRequest("POST", subscription.CallbackURL, body)
	if err != nil {
		errFinal = err
		return err
	}

	// callback timeout.
	var duration time.Duration
	if subscription.TimeOutSeconds == 0 {
		duration = defaultSendTimeout
	} else {
		duration = subscription.GetTimeout()
	}

	// send now.
	resp, err := httpCli.DoWithTimeout(duration, req)
	if err != nil {
		errFinal = err
		return err
	}
	defer resp.Body.Close()

	// read response.
	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		errFinal = err
		return err
	}

	// confirm mode.
	if subscription.ConfirmMode == metadata.ConfirmModeHTTPStatus {
		if strconv.Itoa(resp.StatusCode) != subscription.ConfirmPattern {
			errFinal = err
			return fmt.Errorf("not confirm http pattern, received %s", respData)
		}
	} else if subscription.ConfirmMode == metadata.ConfirmModeRegular {
		pattern, err := regexp.Compile(subscription.ConfirmPattern)
		if err != nil {
			errFinal = err
			return fmt.Errorf("build regexp error, %+v", err)
		}

		if !pattern.Match(respData) {
			errFinal = err
			return fmt.Errorf("not confirm regular pattern, received %s", respData)
		}
	}

	// TODO mark resource type and action cursor.

	return nil
}

func (s *EventSender) run() {
	for {
		if !s.hash.IsMatch(fmt.Sprint(s.subid)) {
			time.Sleep(defaultHandleTimeout)
			continue
		}

		// keep sending.
		distDatas := s.cache.BLPop(defaultTransTimeout, types.EventCacheSubscriberEventQueueKeyPrefix+fmt.Sprint(s.subid)).Val()
		if len(distDatas) == 0 || distDatas[1] == types.NilStr || len(distDatas[1]) == 0 {
			continue
		}
		distData := distDatas[1]

		dist := &metadata.DistInst{}
		if err := json.Unmarshal([]byte(distData), dist); err != nil {
			blog.Errorf("unmarshal dist inst for subscriber failed, %+v", err)
			continue
		}

		if time.Now().Unix()-dist.EventInst.ActionTime.Unix() > defaultFusingEventExpireSec {
			// too old event, expire it.
			continue
		}

		// send message to subscriber.
		if err := s.send(dist); err != nil {
			blog.Errorf("send to subscriber failed, err: %+v, data=[%+v]", err, dist)
			continue
		}
	}
}

// Run setups sender and keep handling event dist.
func (s *EventSender) Run() {
	// run sender.
	go s.run()
}

// EventHandler manages all event senders, and update senders in dynamic mode,
// when there are events need to be sent, the sender would check hash ring and send
// message to subscriber in callback or not.
type EventHandler struct {
	ctx context.Context

	// cache is cc redis client.
	cache *redis.Client

	// senders is local event senders, update in dynamic mode, subid -> EventSender.
	senders map[int64]*EventSender

	// sendersMu make senders ops safe.
	sendersMu sync.RWMutex

	// distributer handles all events distribution.
	distributer *Distributer

	// hash collections hash object, that updates target nodes in dynamic mode,
	// and calculates node base on hash key of data.
	hash *Hash
}

// NewEventHandler creates new EventHandler object.
func NewEventHandler(ctx context.Context, cache *redis.Client, hash *Hash) *EventHandler {

	return &EventHandler{
		ctx:     ctx,
		cache:   cache,
		hash:    hash,
		senders: make(map[int64]*EventSender),
	}
}

// SetDistributer setups distributer to event handler.
func (h *EventHandler) SetDistributer(distributer *Distributer) {
	h.distributer = distributer
}

// Handle handles events distributed by distributer, add events to real handle queue to
// handle host identifier infos. Handler would find all relate subscribers and send event message
// to target subscribers by callback.
func (h *EventHandler) Handle(events []*watch.WatchEventDetail) error {
	for _, event := range events {
		eventInst := &metadata.EventInst{
			Cursor:     event.Cursor,
			Data:       []metadata.EventData{metadata.EventData{CurData: event.Detail}},
			ActionTime: metadata.Now(),
		}

		switch event.Resource {
		case watch.Host:
			eventInst.EventType = metadata.EventTypeInstData
			eventInst.ObjType = common.BKInnerObjIDHost

			switch event.EventType {
			case watch.Create:
				eventInst.Action = metadata.EventActionCreate

			case watch.Update:
				eventInst.Action = metadata.EventActionUpdate

			case watch.Delete:
				eventInst.Action = metadata.EventActionDelete

			default:
				continue
			}

		case watch.ModuleHostRelation:
			eventInst.EventType = metadata.EventTypeRelation
			eventInst.ObjType = metadata.EventObjTypeModuleTransfer

			switch event.EventType {
			case watch.Create:
				eventInst.Action = metadata.EventActionCreate

			case watch.Update:
				eventInst.Action = metadata.EventActionUpdate

			case watch.Delete:
				eventInst.Action = metadata.EventActionDelete

			default:
				continue
			}

		case watch.Biz:
			eventInst.EventType = metadata.EventTypeInstData
			eventInst.ObjType = common.BKInnerObjIDApp

			switch event.EventType {
			case watch.Create:
				eventInst.Action = metadata.EventActionCreate

			case watch.Update:
				eventInst.Action = metadata.EventActionUpdate

			case watch.Delete:
				eventInst.Action = metadata.EventActionDelete

			default:
				continue
			}

		case watch.Set:
			eventInst.EventType = metadata.EventTypeInstData
			eventInst.ObjType = common.BKInnerObjIDSet

			switch event.EventType {
			case watch.Create:
				eventInst.Action = metadata.EventActionCreate

			case watch.Update:
				eventInst.Action = metadata.EventActionUpdate

			case watch.Delete:
				eventInst.Action = metadata.EventActionDelete

			default:
				continue
			}

		case watch.Module:
			eventInst.EventType = metadata.EventTypeInstData
			eventInst.ObjType = common.BKInnerObjIDModule

			switch event.EventType {
			case watch.Create:
				eventInst.Action = metadata.EventActionCreate

			case watch.Update:
				eventInst.Action = metadata.EventActionUpdate

			case watch.Delete:
				eventInst.Action = metadata.EventActionDelete

			default:
				continue
			}

		case watch.ObjectBase:
			eventInst.EventType = metadata.EventTypeInstData

			// TODO bk_obj_id
			eventInst.ObjType = common.BKInnerObjIDObject

			switch event.EventType {
			case watch.Create:
				eventInst.Action = metadata.EventActionCreate

			case watch.Update:
				eventInst.Action = metadata.EventActionUpdate

			case watch.Delete:
				eventInst.Action = metadata.EventActionDelete

			default:
				continue
			}

		default:
			continue
		}

		eventData, err := json.Marshal(event)
		if err != nil {
			blog.Errorf("marshal event data failed, %+v, %+v", event, err)
			continue
		}

		// push event data to main event queue.
		if _, err := h.cache.LPush(types.EventCacheEventQueueKey, eventData).Result(); err != nil {
			blog.Errorf("push event data to main event queue failed, %+v, %+v", event, err)
			continue
		}
	}

	return nil
}

// popEvent keeps poping event from main event queue and add event to duplicated queue. identifier would
// handle host events and add back to main event queue with another level EVENT.
func (h *EventHandler) popEvent() (*metadata.EventInst, error) {
	var eventStr string

	// pop from main event queue and re-add to duplicated queue for identifier.
	if err := h.cache.BRPopLPush(types.EventCacheEventQueueKey,
		types.EventCacheEventQueueDuplicateKey, defaultTransTimeout).Scan(&eventStr); err != nil {
		return nil, err
	}

	// ignore empty event.
	if eventStr == "" || eventStr == types.NilStr {
		return nil, nil
	}
	eventData := []byte(eventStr)

	// marshal to EventInst.
	event := &metadata.EventInst{}
	if err := json.Unmarshal(eventData, event); err != nil {
		return nil, err
	}
	return event, nil
}

// get event dist inst from event inst.
func (h *EventHandler) getDistInst(event *metadata.EventInst) ([]*metadata.DistInst, error) {
	distInst := metadata.DistInst{EventInst: *event}

	dists := []*metadata.DistInst{}

	// handle object event.
	if event.EventType == metadata.EventTypeInstData && event.ObjType == common.BKInnerObjIDObject {
		if len(event.Data) <= 0 {
			// ignore enpty events.
			return nil, nil
		}

		var ok bool
		var m map[string]interface{}

		// handle object data base event type. There is only prev data in delete action.
		if event.Action == metadata.EventActionDelete {
			m, ok = event.Data[0].PreData.(map[string]interface{})
		} else {
			m, ok = event.Data[0].CurData.(map[string]interface{})
		}

		if !ok {
			return nil, fmt.Errorf("can't parse event dist inst from event data, PreData[%+v], CurData[%+v]",
				event.Data[0].PreData, event.Data[0].CurData)
		}

		// mark object type in dist inst with bk_obj_id in event inst.
		if m[common.BKObjIDField] != nil {
			distInst.ObjType = m[common.BKObjIDField].(string)
		} else {
			blog.Warnf("parse event dist inst, missing field bk_obj_id, %+v", m)
		}
	}

	// return dist in slice mode.
	dists = append(dists, &distInst)

	return dists, nil
}

func (h *EventHandler) nextDistID(subid int64) (int64, error) {
	return h.cache.Incr(types.EventCacheDistIDPrefix + fmt.Sprint(subid)).Result()
}

func (h *EventHandler) pushToSender(subid int64, dist *metadata.DistInst) error {
	h.sendersMu.Lock()
	defer h.sendersMu.Unlock()

	if _, isExist := h.senders[subid]; !isExist {
		newSender := NewEventSender(h.ctx, subid, h.cache, h.distributer, h.hash)
		newSender.Run()
		h.senders[subid] = newSender
	}
	sender := h.senders[subid]

	dstbID, err := h.nextDistID(subid)
	if err != nil {
		return err
	}
	dist.DstbID = dstbID
	dist.SubscriptionID = subid

	// add to subscriber sender.
	return sender.Handle(dist)
}

// handleEvent handles target event.
func (h *EventHandler) handleEvent(event *metadata.EventInst) error {
	blog.Infof("handle event inst, %+v", event)
	defer blog.Infof("handle event inst done, %+v", event.ID)

	// trans event inst to dist inst.
	dists, err := h.getDistInst(event)
	if err != nil {
		return fmt.Errorf("trans event inst to dist inst failed, %+v", err)
	}

	for _, dist := range dists {
		subscribers := h.distributer.FindSubscribers(dist.GetType())
		if len(subscribers) <= 0 {
			blog.Infof("handle event, %v has no subscriber，ignore in this round", dist.GetType())
			continue
		}

		// distribute to subscribers.
		for _, subscriber := range subscribers {
			if !h.hash.IsMatch(fmt.Sprint(subscriber)) {
				continue
			}

			// push to subscriber sender.
			newDist := *dist
			if err := h.pushToSender(subscriber, &newDist); err != nil {
				return err
			}
		}
	}

	return nil
}

// Start starts event handler and keep processing event from distributer.
func (h *EventHandler) Start() error {
	if h.cache == nil {
		return errors.New("redis cache not inited")
	}
	if h.distributer == nil {
		return errors.New("distributer not inited")
	}
	if h.hash == nil {
		return errors.New("hash not inited")
	}

	blog.Info("event handler starting now!")

	go func() {
		// keep poping events and handle distribution.
		for {
			// pop.
			event, err := h.popEvent()
			if err != nil {
				blog.Errorf("pop event failed, %+v", err)
				time.Sleep(defaultHandleTimeout)
				continue
			}

			// ignore empty event.
			if event == nil {
				time.Sleep(defaultHandleTimeout)
				continue
			}

			// handle.
			if err := h.handleEvent(event); err != nil {
				blog.Errorf("handle event failed, %+v, %+v", event, err)
				time.Sleep(defaultHandleTimeout)
				continue
			}
		}
	}()

	return nil
}