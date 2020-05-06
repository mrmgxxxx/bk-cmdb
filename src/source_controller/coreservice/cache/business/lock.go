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

package business

import "sync"

type refreshingLock struct {
	// bool, true: is refreshing, false: not refreshing.
	refreshing map[string]bool
	lock sync.Mutex
}

// canRefresh check if you can refresh the key.
func (r *refreshingLock) canRefresh(key string) bool {
	r.lock.Lock()
	refreshing, exist := r.refreshing[key]
	r.lock.Unlock()
	if !exist {
		r.refreshing[key]=false
		return true
	}
	return !refreshing
}

// setRefreshing set the key is refreshing
func (r *refreshingLock) setRefreshing(key string) {
	r.lock.Lock()
	r.refreshing[key]=true
	r.lock.Unlock()
}

// setUnRefreshing set the key is refreshing
func (r *refreshingLock) setUnRefreshing(key string) {
	r.lock.Lock()
	r.refreshing[key]=false
	r.lock.Unlock()
}