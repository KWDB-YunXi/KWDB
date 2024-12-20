// Copyright 2015 The Cockroach Authors.
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
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/storagebase"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/pkg/errors"
)

// TestReplicaGCQueueDropReplica verifies that a removed replica is
// immediately cleaned up.
func TestReplicaGCQueueDropReplicaDirect(t *testing.T) {
	defer leaktest.AfterTest(t)()
	mtc := &multiTestContext{}
	const numStores = 3
	rangeID := roachpb.RangeID(1)

	// Use actual engines (not in memory) because the in-mem ones don't write
	// to disk. The test would still pass if we didn't do this except it
	// would probably look at an empty sideloaded directory and fail.
	tempDir, cleanup := testutils.TempDir(t)
	defer cleanup()
	cache := storage.NewRocksDBCache(1 << 20)
	defer cache.Release()
	for i := 0; i < 3; i++ {
		eng, err := storage.NewRocksDB(storage.RocksDBConfig{
			StorageConfig: base.StorageConfig{
				Dir: filepath.Join(tempDir, strconv.Itoa(i)),
			},
		}, cache)
		if err != nil {
			t.Fatal(err)
		}
		defer eng.Close()
		mtc.engines = append(mtc.engines, eng)
	}

	// In this test, the Replica on the second Node is removed, and the test
	// verifies that that Node adds this Replica to its RangeGCQueue. However,
	// the queue does a consistent lookup which will usually be read from
	// Node 1. Hence, if Node 1 hasn't processed the removal when Node 2 has,
	// no GC will take place since the consistent RangeLookup hits the first
	// Node. We use the TestingEvalFilter to make sure that the second Node
	// waits for the first.
	cfg := kvserver.TestStoreConfig(nil)
	mtc.storeConfig = &cfg
	mtc.storeConfig.TestingKnobs.EvalKnobs.TestingEvalFilter =
		func(filterArgs storagebase.FilterArgs) *roachpb.Error {
			et, ok := filterArgs.Req.(*roachpb.EndTxnRequest)
			if !ok || filterArgs.Sid != 2 {
				return nil
			}
			crt := et.InternalCommitTrigger.GetChangeReplicasTrigger()
			if crt == nil || crt.DeprecatedChangeType != roachpb.REMOVE_REPLICA {
				return nil
			}
			testutils.SucceedsSoon(t, func() error {
				r, err := mtc.stores[0].GetReplica(rangeID)
				if err != nil {
					return err
				}
				if _, ok := r.Desc().GetReplicaDescriptor(2); ok {
					return errors.New("expected second node gone from first node's known replicas")
				}
				return nil
			})
			return nil
		}

	defer mtc.Stop()
	mtc.Start(t, numStores)

	mtc.replicateRange(rangeID, 1, 2)

	{
		repl1, err := mtc.stores[1].GetReplica(rangeID)
		if err != nil {
			t.Fatal(err)
		}

		// Put some bogus sideloaded data on the replica which we're about to
		// remove. Then, at the end of the test, check that that sideloaded
		// storage is now empty (in other words, GC'ing the Replica took care of
		// cleanup).
		repl1.RaftLock()
		dir := repl1.SideloadedRaftMuLocked().Dir()
		repl1.RaftUnlock()

		if dir == "" {
			t.Fatal("no sideloaded directory")
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := ioutil.WriteFile(filepath.Join(dir, "i1000000.t100000"), []byte("foo"), 0644); err != nil {
			t.Fatal(err)
		}

		defer func() {
			if !t.Failed() {
				testutils.SucceedsSoon(t, func() error {
					// Verify that the whole directory for the replica is gone.
					repl1.RaftLock()
					dir := repl1.SideloadedRaftMuLocked().Dir()
					repl1.RaftUnlock()
					_, err := os.Stat(dir)

					if os.IsNotExist(err) {
						return nil
					}
					return errors.Errorf("replica still has sideloaded files despite GC: %v", err)
				})
			}
		}()
	}

	mtc.unreplicateRange(rangeID, 1)

	// Make sure the range is removed from the store.
	testutils.SucceedsSoon(t, func() error {
		if _, err := mtc.stores[1].GetReplica(rangeID); !testutils.IsError(err, "r[0-9]+ was not found") {
			return errors.Errorf("expected range removal: %v", err) // NB: errors.Wrapf(nil, ...) returns nil.
		}
		return nil
	})
}

// TestReplicaGCQueueDropReplicaOnScan verifies that the range GC queue
// removes a range from a store that no longer should have a replica.
func TestReplicaGCQueueDropReplicaGCOnScan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	mtc := &multiTestContext{}
	cfg := kvserver.TestStoreConfig(nil)
	cfg.TestingKnobs.DisableEagerReplicaRemoval = true
	cfg.Clock = nil // manual clock
	mtc.storeConfig = &cfg

	defer mtc.Stop()
	mtc.Start(t, 3)
	// Disable the replica gc queue to prevent direct removal of replica.
	mtc.stores[1].SetReplicaGCQueueActive(false)

	rangeID := roachpb.RangeID(1)
	mtc.replicateRange(rangeID, 1, 2)
	mtc.unreplicateRange(rangeID, 1)

	// Wait long enough for the direct replica GC to have had a chance and been
	// discarded because the queue is disabled.
	time.Sleep(10 * time.Millisecond)
	if _, err := mtc.stores[1].GetReplica(rangeID); err != nil {
		t.Error("unexpected range removal")
	}

	// Enable the queue.
	mtc.stores[1].SetReplicaGCQueueActive(true)

	// Increment the clock's timestamp to make the replica GC queue process the range.
	mtc.advanceClock(context.TODO())
	mtc.manualClock.Increment(int64(kvserver.ReplicaGCQueueInactivityThreshold + 1))

	// Make sure the range is removed from the store.
	testutils.SucceedsSoon(t, func() error {
		store := mtc.stores[1]
		store.MustForceReplicaGCScanAndProcess()
		if _, err := store.GetReplica(rangeID); !testutils.IsError(err, "r[0-9]+ was not found") {
			return errors.Errorf("expected range removal: %v", err) // NB: errors.Wrapf(nil, ...) returns nil.
		}
		return nil
	})
}
