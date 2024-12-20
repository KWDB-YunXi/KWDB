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
//
// This file implements data structures used by index constraints generation.

package constraint

import (
	"fmt"
	"math"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
)

func TestKey(t *testing.T) {
	testKey(t, EmptyKey, "")

	k := MakeKey(tree.NewDInt(1))
	testKey(t, k, "/1")

	k = MakeCompositeKey(tree.NewDInt(2))
	testKey(t, k, "/2")

	k = MakeCompositeKey(tree.NewDString("foo"), tree.NewDInt(3))
	testKey(t, k, "/'foo'/3")
}

func TestKeyCompare(t *testing.T) {
	keyCtx := testKeyContext(1, 2)

	test := func(k, l Key, kExt, lExt KeyExtension, expected int) {
		t.Helper()
		if actual := k.Compare(keyCtx, l, kExt, lExt); actual != expected {
			t.Errorf("k: %s, l %s, expected: %d, actual: %d", k, l, expected, actual)
		} else if actual := l.Compare(keyCtx, k, lExt, kExt); actual != -expected {
			t.Errorf("l: %s, k %s, expected: %d, actual: %d", l, k, -expected, actual)
		}
	}

	key0 := MakeKey(tree.NewDInt(0))
	key1 := MakeKey(tree.NewDInt(1))
	key01 := MakeCompositeKey(tree.NewDInt(0), tree.NewDInt(1))
	keyNull := MakeKey(tree.DNull)

	test(EmptyKey, keyNull, ExtendLow, ExtendLow, -1)
	test(EmptyKey, keyNull, ExtendLow, ExtendHigh, -1)
	test(EmptyKey, keyNull, ExtendHigh, ExtendLow, 1)
	test(EmptyKey, keyNull, ExtendHigh, ExtendHigh, 1)

	test(key0, key0, ExtendLow, ExtendLow, 0)
	test(key0, key0, ExtendLow, ExtendHigh, -1)
	test(key0, key0, ExtendHigh, ExtendLow, 1)
	test(key0, key0, ExtendHigh, ExtendHigh, 0)

	test(key0, key1, ExtendLow, ExtendLow, -1)
	test(key0, key1, ExtendLow, ExtendHigh, -1)
	test(key0, key1, ExtendHigh, ExtendLow, -1)
	test(key0, key1, ExtendHigh, ExtendHigh, -1)

	test(key01, key0, ExtendLow, ExtendLow, 1)
	test(key01, key0, ExtendLow, ExtendHigh, -1)
	test(key01, key0, ExtendHigh, ExtendLow, 1)
	test(key01, key0, ExtendHigh, ExtendHigh, -1)

	test(keyNull, key0, ExtendHigh, ExtendLow, -1)

	// Invert the direction of the first column.
	keyCtx = testKeyContext(-1, 2)

	test(EmptyKey, keyNull, ExtendLow, ExtendLow, -1)
	test(EmptyKey, keyNull, ExtendLow, ExtendHigh, -1)
	test(EmptyKey, keyNull, ExtendHigh, ExtendLow, 1)
	test(EmptyKey, keyNull, ExtendHigh, ExtendHigh, 1)

	test(key0, key0, ExtendLow, ExtendLow, 0)
	test(key0, key0, ExtendLow, ExtendHigh, -1)
	test(key0, key0, ExtendHigh, ExtendLow, 1)
	test(key0, key0, ExtendHigh, ExtendHigh, 0)

	test(key0, key1, ExtendLow, ExtendLow, 1)
	test(key0, key1, ExtendLow, ExtendHigh, 1)
	test(key0, key1, ExtendHigh, ExtendLow, 1)
	test(key0, key1, ExtendHigh, ExtendHigh, 1)

	test(key01, key0, ExtendLow, ExtendLow, 1)
	test(key01, key0, ExtendLow, ExtendHigh, -1)
	test(key01, key0, ExtendHigh, ExtendLow, 1)
	test(key01, key0, ExtendHigh, ExtendHigh, -1)

	test(keyNull, key0, ExtendHigh, ExtendLow, 1)
}

func TestKeyConcat(t *testing.T) {
	k := EmptyKey

	// Empty + empty.
	k = k.Concat(EmptyKey)
	testKey(t, k, "")

	// Empty + single value.
	k = k.Concat(MakeKey(tree.NewDInt(1)))
	testKey(t, k, "/1")

	// Single value + empty.
	k = k.Concat(EmptyKey)
	testKey(t, k, "/1")

	// Single value + single value.
	k = k.Concat(MakeKey(tree.NewDInt(2)))
	testKey(t, k, "/1/2")

	// Multiple values + empty.
	k = k.Concat(EmptyKey)
	testKey(t, k, "/1/2")

	// Multiple values + single value.
	k = k.Concat(MakeKey(tree.NewDInt(3)))
	testKey(t, k, "/1/2/3")

	// Multiple values + multiple values.
	k = k.Concat(MakeCompositeKey(tree.NewDString("bar"), tree.DBoolTrue))
	testKey(t, k, "/1/2/3/'bar'/true")
}

func TestKeyNextPrev(t *testing.T) {
	kcAscAsc := testKeyContext(1, 2)
	kcDesc := testKeyContext(-1)
	kcAscDesc := testKeyContext(1, -2)

	testCases := []struct {
		key     Key
		keyCtx  *KeyContext
		expNext string
		expPrev string
	}{
		{ // 0
			key:     MakeKey(tree.NewDInt(1)),
			keyCtx:  kcAscAsc,
			expNext: "/2",
			expPrev: "/0",
		},
		{ // 1
			key:     MakeKey(tree.NewDInt(math.MaxInt64)),
			keyCtx:  kcAscAsc,
			expNext: "FAIL",
			expPrev: "/9223372036854775806",
		},
		{ // 2
			key:     MakeKey(tree.NewDInt(math.MinInt64)),
			keyCtx:  kcAscAsc,
			expNext: "/-9223372036854775807",
			expPrev: "FAIL",
		},
		{ // 3
			key:     MakeCompositeKey(tree.NewDInt(1), tree.NewDInt(2)),
			keyCtx:  kcAscAsc,
			expNext: "/1/3",
			expPrev: "/1/1",
		},
		{ // 4
			key:     MakeCompositeKey(tree.NewDInt(1), tree.DBoolFalse),
			keyCtx:  kcAscAsc,
			expNext: "/1/true",
			expPrev: "FAIL",
		},
		{ // 5
			key:     MakeCompositeKey(tree.NewDInt(1), tree.DBoolTrue),
			keyCtx:  kcAscAsc,
			expNext: "FAIL",
			expPrev: "/1/false",
		},
		{ // 6
			key:     MakeCompositeKey(tree.NewDInt(1), tree.NewDString("foo")),
			keyCtx:  kcAscAsc,
			expNext: "/1/e'foo\\x00'",
			expPrev: "FAIL",
		},
		{ // 7
			key:     MakeCompositeKey(tree.NewDInt(1)),
			keyCtx:  kcDesc,
			expNext: "/0",
			expPrev: "/2",
		},
		{ // 8
			key:     MakeCompositeKey(tree.NewDInt(1), tree.NewDInt(2)),
			keyCtx:  kcAscDesc,
			expNext: "/1/1",
			expPrev: "/1/3",
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			toStr := func(k Key, ok bool) string {
				if !ok {
					return "FAIL"
				}
				return k.String()
			}

			key, ok := tc.key.Next(tc.keyCtx)
			if res := toStr(key, ok); res != tc.expNext {
				t.Errorf("Next(%s) = %s, expected %s", tc.key, res, tc.expNext)
			}
			key, ok = tc.key.Prev(tc.keyCtx)
			if res := toStr(key, ok); res != tc.expPrev {
				t.Errorf("Prev(%s) = %s, expected %s", tc.key, res, tc.expPrev)
			}
		})
	}

}

func testKey(t *testing.T, k Key, expected string) {
	t.Helper()
	if k.String() != expected {
		t.Errorf("expected: %s, actual: %s", expected, k.String())
	}
}

func testKeyContext(cols ...opt.OrderingColumn) *KeyContext {
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)

	var columns Columns
	columns.Init(cols)

	keyCtx := MakeKeyContext(&columns, &evalCtx)
	return &keyCtx
}
