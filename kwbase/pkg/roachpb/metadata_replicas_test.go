// Copyright 2019 The Cockroach Authors.
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

package roachpb

import (
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/raft"
)

func rd(typ *ReplicaType, id uint64) ReplicaDescriptor {
	return ReplicaDescriptor{
		Type:      typ,
		NodeID:    NodeID(100 * id),
		StoreID:   StoreID(10 * id),
		ReplicaID: ReplicaID(id),
	}
}

var vn = (*ReplicaType)(nil) // should be treated like VoterFull
var v = ReplicaTypeVoterFull()
var vi = ReplicaTypeVoterIncoming()
var vo = ReplicaTypeVoterOutgoing()
var vd = ReplicaTypeVoterDemoting()
var l = ReplicaTypeLearner()

func TestVotersLearnersAll(t *testing.T) {

	tests := [][]ReplicaDescriptor{
		{},
		{rd(v, 1)},
		{rd(vn, 1)},
		{rd(l, 1)},
		{rd(v, 1), rd(l, 2), rd(v, 3)},
		{rd(vn, 1), rd(l, 2), rd(v, 3)},
		{rd(l, 1), rd(v, 2), rd(l, 3)},
		{rd(l, 1), rd(vn, 2), rd(l, 3)},
		{rd(vi, 1)},
		{rd(vo, 1)},
		{rd(l, 1), rd(vo, 2), rd(vi, 3), rd(vi, 4)},
	}
	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			r := MakeReplicaDescriptors(test)
			seen := map[ReplicaDescriptor]struct{}{}
			for _, voter := range r.Voters() {
				typ := voter.GetType()
				switch typ {
				case VOTER_FULL, VOTER_INCOMING:
					seen[voter] = struct{}{}
				default:
					assert.FailNow(t, "unexpectedly got a %s as Voter()", typ)
				}
			}
			for _, learner := range r.Learners() {
				seen[learner] = struct{}{}
				assert.Equal(t, LEARNER, learner.GetType())
			}

			all := r.All()
			// Make sure that VOTER_OUTGOING is the only type that is skipped both
			// by Learners() and Voters()
			for _, rd := range all {
				typ := rd.GetType()
				if _, seen := seen[rd]; !seen {
					assert.Equal(t, VOTER_OUTGOING, typ)
				} else {
					assert.NotEqual(t, VOTER_OUTGOING, typ)
				}
			}
			assert.Equal(t, len(test), len(all))
		})
	}
}

func TestReplicaDescriptorsRemove(t *testing.T) {
	tests := []struct {
		replicas []ReplicaDescriptor
		remove   ReplicationTarget
		expected bool
	}{
		{
			remove:   ReplicationTarget{NodeID: 1, StoreID: 1},
			expected: false,
		},
		{
			replicas: []ReplicaDescriptor{{NodeID: 1, StoreID: 1}},
			remove:   ReplicationTarget{NodeID: 2, StoreID: 2},
			expected: false,
		},
		{
			replicas: []ReplicaDescriptor{{NodeID: 1, StoreID: 1}},
			remove:   ReplicationTarget{NodeID: 1, StoreID: 1},
			expected: true,
		},
		{
			// Make sure we sort after the swap in removal.
			replicas: []ReplicaDescriptor{
				{NodeID: 1, StoreID: 1},
				{NodeID: 2, StoreID: 2},
				{NodeID: 3, StoreID: 3},
				{NodeID: 4, StoreID: 4, Type: ReplicaTypeLearner()},
			},
			remove:   ReplicationTarget{NodeID: 2, StoreID: 2},
			expected: true,
		},
	}
	for i, test := range tests {
		r := MakeReplicaDescriptors(test.replicas)
		lenBefore := len(r.All())
		removedDesc, ok := r.RemoveReplica(test.remove.NodeID, test.remove.StoreID)
		assert.Equal(t, test.expected, ok, "testcase %d", i)
		if ok {
			assert.Equal(t, test.remove.NodeID, removedDesc.NodeID, "testcase %d", i)
			assert.Equal(t, test.remove.StoreID, removedDesc.StoreID, "testcase %d", i)
			assert.Equal(t, lenBefore-1, len(r.All()), "testcase %d", i)
		} else {
			assert.Equal(t, lenBefore, len(r.All()), "testcase %d", i)
		}
		for _, voter := range r.Voters() {
			assert.Equal(t, VOTER_FULL, voter.GetType(), "testcase %d", i)
		}
		for _, learner := range r.Learners() {
			assert.Equal(t, LEARNER, learner.GetType(), "testcase %d", i)
		}
	}
}

func TestReplicaDescriptorsConfState(t *testing.T) {
	tests := []struct {
		in  []ReplicaDescriptor
		out string
	}{
		{
			[]ReplicaDescriptor{rd(v, 1)},
			"Voters:[1] VotersOutgoing:[] Learners:[] LearnersNext:[] AutoLeave:false",
		},
		// Make sure nil is treated like VoterFull.
		{
			[]ReplicaDescriptor{rd(vn, 1)},
			"Voters:[1] VotersOutgoing:[] Learners:[] LearnersNext:[] AutoLeave:false",
		},
		{
			[]ReplicaDescriptor{rd(l, 1), rd(vn, 2)},
			"Voters:[2] VotersOutgoing:[] Learners:[1] LearnersNext:[] AutoLeave:false",
		},
		// First joint case. We're adding n3 (via atomic replication changes), so the outgoing
		// config we have to get rid of consists only of n2 (even though n2 remains a voter).
		// Note that we could simplify this config so that it's not joint, but raft expects
		// the config exactly as described by the descriptor so we don't try.
		{
			[]ReplicaDescriptor{rd(l, 1), rd(v, 2), rd(vi, 3)},
			"Voters:[2 3] VotersOutgoing:[2] Learners:[1] LearnersNext:[] AutoLeave:false",
		},
		// More complex joint change: a replica swap, switching out n4 for n3 from the initial
		// set of voters n2, n4 (plus learner n1 before and after).
		{
			[]ReplicaDescriptor{rd(l, 1), rd(v, 2), rd(vi, 3), rd(vo, 4)},
			"Voters:[2 3] VotersOutgoing:[2 4] Learners:[1] LearnersNext:[] AutoLeave:false",
		},
		// Upreplicating from n1,n2 to n1,n2,n3,n4.
		{
			[]ReplicaDescriptor{rd(v, 1), rd(v, 2), rd(vi, 3), rd(vi, 4)},
			"Voters:[1 2 3 4] VotersOutgoing:[1 2] Learners:[] LearnersNext:[] AutoLeave:false",
		},
		// Downreplicating from n1,n2,n3,n4 to n1,n2.
		{
			[]ReplicaDescriptor{rd(v, 1), rd(v, 2), rd(vo, 3), rd(vo, 4)},
			"Voters:[1 2] VotersOutgoing:[1 2 3 4] Learners:[] LearnersNext:[] AutoLeave:false",
		},
		// Completely switching to a new set of replicas: n1,n2 to n4,n5. Throw a learner in for fun.
		{
			[]ReplicaDescriptor{rd(vo, 1), rd(vo, 2), rd(vi, 3), rd(vi, 4), rd(l, 5)},
			"Voters:[3 4] VotersOutgoing:[1 2] Learners:[5] LearnersNext:[] AutoLeave:false",
		},
		// Throw in a voter demotion. The demoting voter should be treated as Outgoing and LearnersNext.
		{
			[]ReplicaDescriptor{rd(vo, 1), rd(vd, 2), rd(vi, 3), rd(vi, 4), rd(l, 5)},
			"Voters:[3 4] VotersOutgoing:[1 2] Learners:[5] LearnersNext:[2] AutoLeave:false",
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			r := MakeReplicaDescriptors(test.in)
			cs := r.ConfState()
			require.Equal(t, test.out, raft.DescribeConfState(cs))
		})
	}
}

func TestReplicaDescriptorsCanMakeProgress(t *testing.T) {
	defer leaktest.AfterTest(t)()

	type descWithLiveness struct {
		live bool
		ReplicaDescriptor
	}

	for _, test := range []struct {
		rds []descWithLiveness
		exp bool
	}{
		// One out of one voter dead.
		{[]descWithLiveness{{false, rd(v, 1)}}, false},
		// Three out of three voters dead.
		{[]descWithLiveness{
			{false, rd(v, 1)},
			{false, rd(v, 2)},
			{false, rd(v, 3)},
		}, false},
		// Two out of three voters dead.
		{[]descWithLiveness{
			{false, rd(v, 1)},
			{true, rd(v, 2)},
			{false, rd(v, 3)},
		}, false},
		// Two out of three voters alive.
		{[]descWithLiveness{
			{true, rd(v, 1)},
			{false, rd(v, 2)},
			{true, rd(v, 3)},
		}, true},
		// Two out of three voters alive, but one is an incoming voter. (This
		// still uses the fast path).
		{[]descWithLiveness{
			{true, rd(v, 1)},
			{false, rd(v, 2)},
			{true, rd(vi, 3)},
		}, true},
		// Two out of three voters dead, and they're all incoming voters. (This
		// can't happen in practice because it means there were zero voters prior
		// to the conf change, but still this result is correct, similar to others
		// below).
		{[]descWithLiveness{
			{false, rd(vi, 1)},
			{false, rd(vi, 2)},
			{true, rd(vi, 3)},
		}, false},
		// Two out of three voters dead, and two are outgoing, one incoming.
		{[]descWithLiveness{
			{false, rd(vi, 1)},
			{false, rd(vo, 2)},
			{true, rd(vo, 3)},
		}, false},
		// 1 and 3 are alive, but that's not a quorum for (1 3)&&(2 3) which is
		// the config here.
		{[]descWithLiveness{
			{true, rd(vi, 1)},
			{false, rd(vo, 2)},
			{true, rd(v, 3)},
		}, false},
		// Same as above, but all three alive.
		{[]descWithLiveness{
			{true, rd(vi, 1)},
			{true, rd(vo, 2)},
			{true, rd(v, 3)},
		}, true},
		// Same, but there are a few learners that should not matter.
		{[]descWithLiveness{
			{true, rd(vi, 1)},
			{true, rd(vo, 2)},
			{true, rd(v, 3)},
			{false, rd(l, 4)},
			{false, rd(l, 5)},
			{false, rd(l, 6)},
			{false, rd(l, 7)},
		}, true},
		// Non-joint case that should be live unless the learner is somehow taken
		// into account.
		{[]descWithLiveness{
			{true, rd(v, 1)},
			{true, rd(v, 2)},
			{false, rd(v, 4)},
			{false, rd(l, 4)},
		}, true},
	} {
		t.Run("", func(t *testing.T) {
			rds := make([]ReplicaDescriptor, 0, len(test.rds))
			for _, rDesc := range test.rds {
				rds = append(rds, rDesc.ReplicaDescriptor)
			}

			act := MakeReplicaDescriptors(rds).CanMakeProgress(func(rd ReplicaDescriptor) bool {
				for _, rdi := range test.rds {
					if rdi.ReplicaID == rd.ReplicaID {
						return rdi.live
					}
				}
				return false
			})
			require.Equal(t, test.exp, act, "input: %+v", test)
		})
	}
}
