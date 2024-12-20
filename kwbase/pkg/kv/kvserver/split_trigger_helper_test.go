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

package kvserver

import (
	"context"
	"fmt"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/protoutil"
	"github.com/stretchr/testify/assert"
	"go.etcd.io/etcd/raft/raftpb"
)

type testMsgAppDropper struct {
	initialized bool
	ticks       int
	lhs         bool

	startKey string // set by ShouldDrop
}

func (td *testMsgAppDropper) Args() (initialized bool, ticks int) {
	return td.initialized, td.ticks
}

func (td *testMsgAppDropper) ShouldDrop(startKey roachpb.RKey) (fmt.Stringer, bool) {
	if len(startKey) == 0 {
		panic("empty startKey")
	}
	td.startKey = string(startKey)
	return &Replica{}, td.lhs
}

func TestMaybeDropMsgApp(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testCases := map[testMsgAppDropper]bool{
		// Already load'ed.
		{initialized: true}: false,
		// Left hand side not found.
		{initialized: false}: false,
		// Drop message to wait for trigger.
		{initialized: false, lhs: true}: true,
		// Drop message to wait for trigger.
		{initialized: false, lhs: true, ticks: maxDelaySplitTriggerTicks}: true,
		// Escape hatch fires.
		{initialized: false, lhs: true, ticks: maxDelaySplitTriggerTicks + 1}: false,
	}

	msgHeartbeat := &raftpb.Message{
		Type: raftpb.MsgHeartbeat,
	}
	msgApp := &raftpb.Message{
		Type: raftpb.MsgApp,
	}
	ctx := context.Background()
	for dropper, exp := range testCases {
		t.Run(fmt.Sprintf("%v", dropper), func(t *testing.T) {
			assert.Equal(t, false, maybeDropMsgApp(ctx, &dropper, msgHeartbeat, nil, false))
			assert.Equal(t, false, maybeDropMsgApp(ctx, &dropper, msgApp, nil, false))
			assert.Equal(t, "", dropper.startKey)
			startKey := roachpb.RKey("foo")
			assert.Equal(t, exp, maybeDropMsgApp(ctx, &dropper, msgApp, startKey, false))
			if exp {
				assert.Equal(t, string(startKey), dropper.startKey)
			}
		})
	}
}

// TestProtoZeroNilSlice verifies that the proto encoding round-trips empty and
// nil byte slices correctly.
func TestProtoZeroNilSlice(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testutils.RunTrueAndFalse(t, "isNil", func(t *testing.T, isNil bool) {
		msg := &RaftMessageRequest{}
		if !isNil {
			msg.RangeStartKey = roachpb.RKey("foo")
		}
		b, err := protoutil.Marshal(msg)
		assert.NoError(t, err)
		out := &RaftMessageRequest{}
		assert.NoError(t, protoutil.Unmarshal(b, out))
		assert.Equal(t, isNil, out.RangeStartKey == nil)
	})
}
