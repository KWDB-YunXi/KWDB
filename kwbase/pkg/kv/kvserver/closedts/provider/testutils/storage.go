// Copyright 2018 The Cockroach Authors.
// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package testutils

import (
	"sort"

	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/closedts/ctpb"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/util/syncutil"
)

// TestStorage is a storage backed by a map[NodeID]Entry.
type TestStorage struct {
	mu syncutil.Mutex
	m  map[roachpb.NodeID][]ctpb.Entry
}

// VisitAscending implements closedts.Storage.
func (s *TestStorage) VisitAscending(nodeID roachpb.NodeID, f func(ctpb.Entry) (done bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range s.m[nodeID] {
		if f(entry) {
			break
		}
	}
}

// VisitDescending implements closedts.Storage.
func (s *TestStorage) VisitDescending(nodeID roachpb.NodeID, f func(entry ctpb.Entry) (done bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.m[nodeID]) - 1; i >= 0; i-- {
		if f(s.m[nodeID][i]) {
			break
		}
	}
}

// Add implements closedts.Storage.
func (s *TestStorage) Add(nodeID roachpb.NodeID, entry ctpb.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.m == nil {
		s.m = map[roachpb.NodeID][]ctpb.Entry{}
	}

	s.m[nodeID] = append(s.m[nodeID], entry)
	sort.Slice(s.m[nodeID], func(i, j int) bool {
		e1, e2 := s.m[nodeID][i], s.m[nodeID][j]
		if e1.ClosedTimestamp == e2.ClosedTimestamp {
			return e1.Epoch < e2.Epoch
		}
		return e1.ClosedTimestamp.Less(e2.ClosedTimestamp)
	})
}

// Clear implements closedts.Storage.
func (s *TestStorage) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = nil
}

// Snapshot returns a copy of the data contain within the TestStorage.
func (s *TestStorage) Snapshot() map[roachpb.NodeID][]ctpb.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	m := map[roachpb.NodeID][]ctpb.Entry{}
	for nodeID, entries := range s.m {
		m[nodeID] = append([]ctpb.Entry(nil), entries...)
	}
	return m
}
