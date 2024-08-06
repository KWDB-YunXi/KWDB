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

package gc

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
	"github.com/stretchr/testify/require"
)

// TestGCIterator exercises the GC iterator by writing data to the underlying
// engine and then validating the state of the iterator as it iterates that
// data.
func TestGCIterator(t *testing.T) {
	// dataItem represents a version in the storage engine and optionally a
	// corresponding transaction which will make the MVCCKeyValue an intent.
	type dataItem struct {
		storage.MVCCKeyValue
		txn *roachpb.Transaction
	}
	// makeDataItem is a shorthand to construct dataItems.
	makeDataItem := func(k roachpb.Key, val []byte, ts int64, txn *roachpb.Transaction) dataItem {
		return dataItem{
			MVCCKeyValue: storage.MVCCKeyValue{
				Key: storage.MVCCKey{
					Key:       k,
					Timestamp: hlc.Timestamp{WallTime: ts * time.Nanosecond.Nanoseconds()},
				},
				Value: val,
			},
			txn: txn,
		}
	}
	// makeLiteralDistribution adapts dataItems for use with the data distribution
	// infrastructure.
	makeLiteralDataDistribution := func(items ...dataItem) dataDistribution {
		return func() (storage.MVCCKeyValue, *roachpb.Transaction, bool) {
			if len(items) == 0 {
				return storage.MVCCKeyValue{}, nil, false
			}
			item := items[0]
			defer func() { items = items[1:] }()
			return item.MVCCKeyValue, item.txn, true
		}
	}
	// stateExpectations are expectations about the state of the iterator.
	type stateExpectations struct {
		cur, next, afterNext int
		isNewest             bool
		isIntent             bool
		isNotValue           bool
	}
	// notation to mark that an iterator state element as either nil or metadata.
	const (
		isNil = -1
		isMD  = -2
	)
	// exp is a shorthand to construct state expectations.
	exp := func(cur, next, afterNext int, isNewest, isIntent, isNotValue bool) stateExpectations {
		return stateExpectations{
			cur: cur, next: next, afterNext: afterNext,
			isNewest:   isNewest,
			isIntent:   isIntent,
			isNotValue: isNotValue,
		}
	}
	vals := uniformValueDistribution(3, 5, 0, rand.New(rand.NewSource(1)))
	tablePrefix := keys.MakeTablePrefix(42)
	desc := roachpb.RangeDescriptor{StartKey: tablePrefix, EndKey: roachpb.RKey(roachpb.Key(tablePrefix).PrefixEnd())}
	keyA := append(tablePrefix[0:len(tablePrefix):len(tablePrefix)], 'a')
	keyB := append(tablePrefix[0:len(tablePrefix):len(tablePrefix)], 'b')
	keyC := append(tablePrefix[0:len(tablePrefix):len(tablePrefix)], 'c')
	makeTxn := func() *roachpb.Transaction {
		txn := roachpb.Transaction{}
		txn.Key = keyA
		txn.ID = uuid.MakeV4()
		txn.Status = roachpb.PENDING
		return &txn
	}

	type testCase struct {
		name         string
		data         []dataItem
		expectations []stateExpectations
	}
	// checkExpectations tests whether the state of the iterator matches the
	// expectation.
	checkExpectations := func(
		t *testing.T, data []dataItem, ex stateExpectations, s gcIteratorState,
	) {
		check := func(ex int, kv *storage.MVCCKeyValue) {
			switch {
			case ex >= 0:
				require.EqualValues(t, &data[ex].MVCCKeyValue, kv)
			case ex == isNil:
				require.Nil(t, kv)
			case ex == isMD:
				require.False(t, kv.Key.IsValue())
			}
		}
		check(ex.cur, s.cur)
		check(ex.next, s.next)
		check(ex.afterNext, s.afterNext)
		require.Equal(t, ex.isNewest, s.curIsNewest())
		require.Equal(t, ex.isIntent, s.curIsIntent())
	}
	makeTest := func(tc testCase) func(t *testing.T) {
		return func(t *testing.T) {
			eng := storage.NewDefaultInMem()
			defer eng.Close()
			ds := makeLiteralDataDistribution(tc.data...)
			ds.setupTest(t, eng, desc)
			snap := eng.NewSnapshot()
			defer snap.Close()
			it := makeGCIterator(&desc, snap)
			defer it.close()
			expectations := tc.expectations
			for i, ex := range expectations {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					s, ok := it.state()
					require.True(t, ok)
					checkExpectations(t, tc.data, ex, s)
				})
				it.step()
			}
		}
	}
	di := makeDataItem // shorthand for convenient notation
	for _, tc := range []testCase{
		{
			name: "basic",
			data: []dataItem{
				di(keyA, vals(), 2, nil),
				di(keyA, vals(), 11, nil),
				di(keyA, vals(), 14, nil),
				di(keyB, vals(), 3, nil),
				di(keyC, vals(), 7, makeTxn()),
			},
			expectations: []stateExpectations{
				exp(4, isMD, isNil, false, true, false),
				exp(isMD, isNil, isNil, false, false, true),
				exp(3, isNil, isNil, true, false, false),
				exp(0, 1, 2, false, false, false),
				exp(1, 2, isNil, false, false, false),
				exp(2, isNil, isNil, true, false, false),
			},
		},
	} {
		t.Run(tc.name, makeTest(tc))
	}
}
