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

package kvcoord_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/storagebase"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/txnwait"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
)

// This file contains contains integration tests that don't fit anywhere else.
// Generally its meant to test scenarios involving both "the client" and "the
// server".

// Test that waiters on transactions whose commit command is rejected see the
// transaction as Aborted. This test is a regression test for #30792 which was
// causing pushers in the txn wait queue to consider such a transaction
// committed. It is also a regression test for a similar bug [1] in which
// it was not the notification to the txn wait queue that was leaked, but the
// intents.
//
// The test sets up two ranges and lets a transaction (anchored on the left)
// write to both of them. It then starts readers for both keys written by the
// txn and waits for them to enter the txn wait queue. Next, it lets the txn
// attempt to commit but injects a forced error below Raft. The bugs would
// formerly notify the txn wait queue that the transaction had committed (not
// true) and that its external intent (i.e. the one on the right range) could
// be resolved (not true). Verify that neither occurs.
//
// [1]: https://gitee.com/kwbasedb/kwbase/issues/34025#issuecomment-460934278
func TestWaiterOnRejectedCommit(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	// The txn id whose commit we're going to reject. A uuid.UUID.
	var txnID atomic.Value
	// The EndTxn proposal that we want to reject. A string.
	var commitCmdID atomic.Value
	readerBlocked := make(chan struct{}, 2)
	// txnUpdate is signaled once the txn wait queue is updated for our
	// transaction. Normally it only needs a buffer length of 1, but bugs that
	// cause it to be pinged several times (e.g. #30792) might need a bigger
	// buffer to avoid the test timing out.
	txnUpdate := make(chan roachpb.TransactionStatus, 20)

	illegalLeaseIndex := true
	s, _, db := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			Store: &kvserver.StoreTestingKnobs{
				DisableMergeQueue: true,
				DisableSplitQueue: true,
				TestingProposalFilter: func(args storagebase.ProposalFilterArgs) *roachpb.Error {
					// We'll recognize the attempt to commit our transaction and store the
					// respective command id.
					ba := args.Req
					etReq, ok := ba.GetArg(roachpb.EndTxn)
					if !ok {
						return nil
					}
					if !etReq.(*roachpb.EndTxnRequest).Commit {
						return nil
					}
					v := txnID.Load()
					if v == nil {
						return nil
					}
					if !ba.Txn.ID.Equal(v.(uuid.UUID)) {
						return nil
					}
					commitCmdID.Store(args.CmdID)
					return nil
				},
				TestingApplyFilter: func(args storagebase.ApplyFilterArgs) (int, *roachpb.Error) {
					// We'll trap the processing of the commit command and return an error
					// for it.
					v := commitCmdID.Load()
					if v == nil {
						return 0, nil
					}
					cmdID := v.(storagebase.CmdIDKey)
					if args.CmdID == cmdID {
						if illegalLeaseIndex {
							illegalLeaseIndex = false
							// NB: 1 is proposalIllegalLeaseIndex.
							return 1, roachpb.NewErrorf("test injected err (illegal lease index)")
						}
						// NB: 0 is proposalNoReevaluation.
						return 0, roachpb.NewErrorf("test injected err")
					}
					return 0, nil
				},
				TxnWaitKnobs: txnwait.TestingKnobs{
					OnPusherBlocked: func(ctx context.Context, push *roachpb.PushTxnRequest) {
						// We'll trap a reader entering the wait queue for our txn.
						v := txnID.Load()
						if v == nil {
							return
						}
						if push.PusheeTxn.ID.Equal(v.(uuid.UUID)) {
							readerBlocked <- struct{}{}
						}
					},
					OnTxnUpdate: func(ctx context.Context, txn *roachpb.Transaction) {
						// We'll trap updates to our txn.
						v := txnID.Load()
						if v == nil {
							return
						}
						if txn.ID.Equal(v.(uuid.UUID)) {
							txnUpdate <- txn.Status
						}
					},
				},
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	if _, _, err := s.SplitRange(roachpb.Key("b")); err != nil {
		t.Fatal(err)
	}

	// We'll start a transaction, write an intent on both sides of the split,
	// then separately do a read on a different goroutine and wait for that read
	// to block on the intent, then we'll attempt to commit the transaction but
	// we'll intercept the processing of the commit command and reject it. Then
	// we'll assert that the txn wait queue is told that the transaction
	// aborted, and we also check that the reader got a nil value.

	txn := kv.NewTxn(ctx, db, s.NodeID())
	keyLeft, keyRight := "a", "c"
	for _, key := range []string{keyLeft, keyRight} {
		if err := txn.Put(ctx, key, "val"); err != nil {
			t.Fatal(err)
		}
	}
	txnID.Store(txn.ID())

	readerDone := make(chan error, 2)

	for _, key := range []string{keyLeft, keyRight} {
		go func(key string) {
			val, err := db.Get(ctx, key)
			if err != nil {
				readerDone <- err
			} else if val.Exists() {
				readerDone <- fmt.Errorf("%s: expected value to not exist, got: %s", key, val)
			} else {
				readerDone <- nil
			}
		}(key)
	}

	// Wait for both readers to enter the txn wait queue.
	<-readerBlocked
	<-readerBlocked

	if err := txn.CommitOrCleanup(ctx); !testutils.IsError(err, "test injected err") {
		t.Fatalf("expected injected err, got: %v", err)
	}
	// Wait for the txn wait queue to be pinged and check the status.
	if status := <-txnUpdate; status != roachpb.ABORTED {
		t.Fatalf("expected the wait queue to be updated with an Aborted txn, instead got: %s", status)
	}
	for i := 0; i < 2; i++ {
		if err := <-readerDone; err != nil {
			t.Fatal(err)
		}
	}
}
