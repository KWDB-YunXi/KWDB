// Copyright 2020 The Cockroach Authors.
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

package kvserver_test

import (
	"context"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/storagepb"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/testcluster"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

// TestTimestampsCanBeClosedWhenRequestsAreSentToNonLeaseHolders ensures that
// the errant closed timestamp requests sent to non-leaseholder nodes do not
// prevent future closed timestamps from being created if that node later
// becomes the leaseholder. See #48553 for more details.
func TestClosedTimestampWorksWhenRequestsAreSentToNonLeaseHolders(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	// Set an incredibly long timeout so we don't need to risk node liveness
	// failures and subsequent unexpected lease transfers under extreme stress.
	serverArgs := base.TestServerArgs{
		RaftConfig: base.RaftConfig{RaftElectionTimeoutTicks: 1000},
	}
	tc := testcluster.StartTestCluster(t, 2, base.TestClusterArgs{
		ServerArgs:      serverArgs,
		ReplicationMode: base.ReplicationManual,
	})
	defer tc.Stopper().Stop(ctx)

	// We want to ensure that node 3 has a high epoch and then we want to
	// make it the leaseholder of range and then we want to tickle requesting an
	// MLAI from node 1. Then make node 1 the leaseholder and ensure that it
	// can still close timestamps.
	db1 := tc.Server(0).DB()
	sqlRunner := sqlutils.MakeSQLRunner(tc.ServerConn(0))

	// Set a very short closed timestamp target duration so that we don't need to
	// wait long for the closed timestamp machinery to propagate information.
	const closeInterval = 10 * time.Millisecond
	sqlRunner.Exec(t, "SET CLUSTER SETTING kv.closed_timestamp.target_duration = '"+
		closeInterval.String()+"'")

	// To make node3 have a large epoch, synthesize a liveness record for with
	// epoch 1000 before starting the node.
	require.NoError(t, db1.Put(ctx, keys.NodeLivenessKey(3),
		&storagepb.Liveness{
			NodeID:     3,
			Epoch:      1000,
			Expiration: hlc.LegacyTimestamp{WallTime: 1},
		}))
	tc.AddServer(t, serverArgs)

	// Create our scratch range and up-replicate it.
	k := tc.ScratchRange(t)
	_, err := tc.AddReplicas(k, tc.Target(1), tc.Target(2))
	require.NoError(t, err)
	require.NoError(t, tc.WaitForVoters(k, tc.Target(1), tc.Target(2)))

	// Wrap transferring the lease to deal with errors due to initial node
	// liveness for n3. We could probably alternatively wait for n3 to be live but
	// that felt like more work at the time and this works.
	transferLease := func(desc *roachpb.RangeDescriptor, target roachpb.ReplicationTarget) {
		testutils.SucceedsSoon(t, func() error {
			return tc.TransferRangeLease(*desc, target)
		})
	}

	// transferLeaseAndWaitForClosed will transfer the lease to the serverIdx
	// specified. It will ensure that the lease transfer happens and then will
	// call afterLease. It will then wait until at the closed timestamp moves
	// forward a few intervals.
	transferLeaseAndWaitForClosed := func(serverIdx int, afterLease func()) {
		_, repl := getFirstStoreReplica(t, tc.Server(serverIdx), k)
		target := tc.Target(serverIdx)
		transferLease(repl.Desc(), target)
		testutils.SucceedsSoon(t, func() error {
			if !repl.OwnsValidLease(db1.Clock().Now()) {
				return errors.Errorf("don't yet have the lease")
			}
			return nil
		})
		if afterLease != nil {
			afterLease()
		}
		nowClosed, ok := repl.MaxClosed(ctx)
		require.True(t, ok)
		lease, _ := repl.GetLease()
		if lease.Replica.NodeID != target.NodeID {
			t.Fatalf("lease was unexpectedly transferred away which should" +
				" not happen given the very long timeouts")
		}
		const closedMultiple = 5
		targetClosed := nowClosed.Add(closedMultiple*closeInterval.Nanoseconds(), 0)
		testutils.SucceedsSoon(t, func() error {
			curLease, _ := repl.GetLease()
			if !lease.Equivalent(curLease) {
				t.Fatalf("lease was unexpectedly transferred away which should" +
					" not happen given the very long timeouts")
			}
			closed, ok := repl.MaxClosed(ctx)
			require.True(t, ok)
			if closed.Less(targetClosed) {
				return errors.Errorf("closed timestamp %v not yet after target %v", closed, targetClosed)
			}
			return nil
		})
	}

	// Our new server should have a liveness epoch of 1000.
	s3, repl3 := getFirstStoreReplica(t, tc.Server(2), k)
	transferLeaseAndWaitForClosed(2, func() {
		s3.RequestClosedTimestamp(1, repl3.RangeID)
	})

	// At this point we expect there's a high chance that the request made its
	// way to n1. Now we're going to transfer the lease to n1 and make sure that
	// the closed timestamp advances.
	transferLeaseAndWaitForClosed(0, nil)
}
