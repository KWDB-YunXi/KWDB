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

package storage

import (
	"context"
	"math"
	"strings"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage/enginepb"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/kr/pretty"
)

func TestMVCCOpLogWriter(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	for _, engineImpl := range mvccEngineImpls {
		t.Run(engineImpl.name, func(t *testing.T) {
			engine := engineImpl.create()
			defer engine.Close()

			batch := engine.NewBatch()
			ol := NewOpLoggerBatch(batch)
			defer ol.Close()

			// Write a value and an intent.
			if err := MVCCPut(ctx, ol, nil, testKey1, hlc.Timestamp{Logical: 1}, value1, nil); err != nil {
				t.Fatal(err)
			}
			txn1ts := makeTxn(*txn1, hlc.Timestamp{Logical: 2})
			if err := MVCCPut(ctx, ol, nil, testKey1, txn1ts.ReadTimestamp, value2, txn1ts); err != nil {
				t.Fatal(err)
			}

			// Write a value and an intent on local keys.
			localKey := keys.MakeRangeIDPrefix(1)
			if err := MVCCPut(ctx, ol, nil, localKey, hlc.Timestamp{Logical: 1}, value1, nil); err != nil {
				t.Fatal(err)
			}
			if err := MVCCPut(ctx, ol, nil, localKey, txn1ts.ReadTimestamp, value2, txn1ts); err != nil {
				t.Fatal(err)
			}

			// Update the intents and write another. Use a distinct batch.
			olDist := ol.Distinct()
			txn1ts.Sequence++
			txn1ts.WriteTimestamp = hlc.Timestamp{Logical: 3}
			if err := MVCCPut(ctx, olDist, nil, testKey1, txn1ts.ReadTimestamp, value2, txn1ts); err != nil {
				t.Fatal(err)
			}
			if err := MVCCPut(ctx, olDist, nil, localKey, txn1ts.ReadTimestamp, value2, txn1ts); err != nil {
				t.Fatal(err)
			}
			// Set the txn timestamp to a larger value than the intent.
			txn1LargerTS := makeTxn(*txn1, hlc.Timestamp{Logical: 4})
			txn1LargerTS.WriteTimestamp = hlc.Timestamp{Logical: 4}
			if err := MVCCPut(ctx, olDist, nil, testKey2, txn1LargerTS.ReadTimestamp, value3, txn1LargerTS); err != nil {
				t.Fatal(err)
			}
			olDist.Close()

			// Resolve all three intent.
			txn1CommitTS := *txn1Commit
			txn1CommitTS.WriteTimestamp = hlc.Timestamp{Logical: 4}
			if _, _, err := MVCCResolveWriteIntentRange(ctx, ol, nil,
				roachpb.MakeLockUpdate(
					&txn1CommitTS,
					roachpb.Span{Key: testKey1, EndKey: testKey2.Next()}),
				math.MaxInt64); err != nil {
				t.Fatal(err)
			}
			if _, _, err := MVCCResolveWriteIntentRange(ctx, ol, nil,
				roachpb.MakeLockUpdate(
					&txn1CommitTS,
					roachpb.Span{Key: localKey, EndKey: localKey.Next()}),
				math.MaxInt64); err != nil {
				t.Fatal(err)
			}

			// Write another intent, push it, then abort it.
			txn2ts := makeTxn(*txn2, hlc.Timestamp{Logical: 5})
			if err := MVCCPut(ctx, ol, nil, testKey3, txn2ts.ReadTimestamp, value4, txn2ts); err != nil {
				t.Fatal(err)
			}
			txn2Pushed := *txn2
			txn2Pushed.WriteTimestamp = hlc.Timestamp{Logical: 6}
			if _, err := MVCCResolveWriteIntent(ctx, ol, nil,
				roachpb.MakeLockUpdate(&txn2Pushed, roachpb.Span{Key: testKey3}),
			); err != nil {
				t.Fatal(err)
			}
			txn2Abort := txn2Pushed
			txn2Abort.Status = roachpb.ABORTED
			if _, err := MVCCResolveWriteIntent(ctx, ol, nil,
				roachpb.MakeLockUpdate(&txn2Abort, roachpb.Span{Key: testKey3}),
			); err != nil {
				t.Fatal(err)
			}

			// Verify that the recorded logical ops match expectations.
			makeOp := func(val interface{}) enginepb.MVCCLogicalOp {
				var op enginepb.MVCCLogicalOp
				op.MustSetValue(val)
				return op
			}
			exp := []enginepb.MVCCLogicalOp{
				makeOp(&enginepb.MVCCWriteValueOp{
					Key:       testKey1,
					Timestamp: hlc.Timestamp{Logical: 1},
				}),
				makeOp(&enginepb.MVCCWriteIntentOp{
					TxnID:           txn1.ID,
					TxnKey:          txn1.Key,
					TxnMinTimestamp: txn1.MinTimestamp,
					Timestamp:       hlc.Timestamp{Logical: 2},
					Key:             testKey1,
					Value:           value2.RawBytes,
					Publish:         true,
				}),
				makeOp(&enginepb.MVCCUpdateIntentOp{
					TxnID:     txn1.ID,
					Timestamp: hlc.Timestamp{Logical: 3},
					Key:       testKey1,
					Value:     value2.RawBytes,
					Publish:   true,
				}),
				makeOp(&enginepb.MVCCWriteIntentOp{
					TxnID:           txn1.ID,
					TxnKey:          txn1.Key,
					TxnMinTimestamp: txn1.MinTimestamp,
					Timestamp:       hlc.Timestamp{Logical: 4},
					Key:             testKey2,
					Value:           value3.RawBytes,
					Publish:         true,
				}),
				makeOp(&enginepb.MVCCCommitIntentOp{
					TxnID:     txn1.ID,
					Key:       testKey1,
					Timestamp: hlc.Timestamp{Logical: 4},
				}),
				makeOp(&enginepb.MVCCCommitIntentOp{
					TxnID:     txn1.ID,
					Key:       testKey2,
					Timestamp: hlc.Timestamp{Logical: 4},
				}),
				makeOp(&enginepb.MVCCWriteIntentOp{
					TxnID:           txn2.ID,
					TxnKey:          txn2.Key,
					TxnMinTimestamp: txn2.MinTimestamp,
					Timestamp:       hlc.Timestamp{Logical: 5},
					Key:             testKey3,
					Value:           value4.RawBytes,
					Publish:         true,
				}),
				makeOp(&enginepb.MVCCUpdateIntentOp{
					TxnID:     txn2.ID,
					Timestamp: hlc.Timestamp{Logical: 6},
					Key:       testKey3,
				}),
				makeOp(&enginepb.MVCCAbortIntentOp{
					TxnID: txn2.ID,
					Key:   testKey3,
				}),
			}
			if diff := pretty.Diff(exp, ol.LogicalOps()); diff != nil {
				t.Errorf("unexpected logical op differences:\n%s", strings.Join(diff, "\n"))
			}
		})
	}
}
