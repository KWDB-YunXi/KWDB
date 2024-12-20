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

package util

import (
	"fmt"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/util/randutil"
)

func TestFastIntMap(t *testing.T) {
	cases := []struct {
		keyRange, valRange int
	}{
		{keyRange: 10, valRange: 10},
		{keyRange: numVals, valRange: maxValue + 1},
		{keyRange: numVals + 1, valRange: maxValue + 1},
		{keyRange: numVals, valRange: maxValue + 2},
		{keyRange: 100, valRange: 100},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d-%d", tc.keyRange, tc.valRange), func(t *testing.T) {
			t.Parallel() // SAFE FOR TESTING (this comment is for the linter)
			rng, _ := randutil.NewPseudoRand()
			var fm FastIntMap
			m := make(map[int]int)
			for i := 0; i < 1000; i++ {
				// Check the entire key range.
				for k := 0; k < tc.keyRange; k++ {
					v, ok := fm.Get(k)
					expV, expOk := m[k]
					if ok != expOk || (ok && v != expV) {
						t.Fatalf(
							"incorrect result for key %d: (%d, %t), expected (%d, %t)",
							k, v, ok, expV, expOk,
						)
					}
				}

				if e := fm.Empty(); e != (len(m) == 0) {
					t.Fatalf("incorrect Empty: %t expected %t (%+v %v)", e, len(m) == 0, fm, m)
				}

				if l := fm.Len(); l != len(m) {
					t.Fatalf("incorrect Len: %d expected %d (%+v %v)", l, len(m), fm, m)
				}

				// Get maximum key and value and check MaxKey and MaxValue.
				maxKey, maxVal, maxOk := 0, 0, (len(m) > 0)
				for k, v := range m {
					if maxKey < k {
						maxKey = k
					}
					if maxVal < v {
						maxVal = v
					}
				}
				if m, ok := fm.MaxKey(); ok != maxOk || m != maxKey {
					t.Fatalf("incorrect MaxKey (%d, %t), expected (%d, %t)", m, ok, maxKey, maxOk)
				}
				if m, ok := fm.MaxValue(); ok != maxOk || m != maxVal {
					t.Fatalf("incorrect MaxValue (%d, %t), expected (%d, %t)", m, ok, maxVal, maxOk)
				}

				// Check ForEach
				num := 0
				fm.ForEach(func(key, val int) {
					num++
					if m[key] != val {
						t.Fatalf("incorrect ForEach %d,%d", key, val)
					}
				})
				if num != len(m) {
					t.Fatalf("ForEach reported %d keys, expected %d", num, len(m))
				}
				k := rng.Intn(tc.keyRange)
				if rng.Intn(2) == 0 {
					v := rng.Intn(tc.valRange)
					fm.Set(k, v)
					m[k] = v
				} else {
					fm.Unset(k)
					delete(m, k)
				}
				if rng.Intn(10) == 0 {
					// Verify Copy. The next iteration will verify that the copy contains
					// the right data.
					old := fm
					fm = fm.Copy()
					old.Set(1, 1)
				}
			}
		})
	}
}

func BenchmarkFastIntMap(b *testing.B) {
	cases := []struct {
		keyRange, valRange, ops int
	}{
		{keyRange: 4, valRange: 4, ops: 4},
		{keyRange: 10, valRange: 10, ops: 4},
		{keyRange: numVals, valRange: maxValue + 1, ops: 10},
		{keyRange: 100, valRange: 100, ops: 50},
		{keyRange: 1000, valRange: 1000, ops: 500},
	}
	for _, tc := range cases {
		b.Run(fmt.Sprintf("%dx%d-%d", tc.keyRange, tc.valRange, tc.ops), func(b *testing.B) {
			rng, _ := randutil.NewPseudoRand()
			inserts := make([][2]int, tc.ops)
			for i := range inserts {
				inserts[i] = [2]int{rng.Intn(tc.keyRange), rng.Intn(tc.valRange)}
			}
			probes := make([]int, tc.ops)
			for i := range probes {
				probes[i] = rng.Intn(tc.keyRange)
			}

			b.Run("fastintmap", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					var fm FastIntMap
					for _, x := range inserts {
						fm.Set(x[0], x[1])
					}
					hash := 0
					for _, x := range probes {
						val, ok := fm.Get(x)
						if ok {
							hash ^= val
						}
					}
				}
			})
			b.Run("map", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					m := make(map[int]int)
					for _, x := range inserts {
						m[x[0]] = x[1]
					}
					hash := 0
					for _, x := range probes {
						val, ok := m[x]
						if ok {
							hash ^= val
						}
					}
				}
			})
			b.Run("map-sized", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					m := make(map[int]int, tc.keyRange)
					for _, x := range inserts {
						m[x[0]] = x[1]
					}
					hash := 0
					for _, x := range probes {
						val, ok := m[x]
						if ok {
							hash ^= val
						}
					}
				}
			})
			b.Run("slice", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					var m []int
					for _, x := range inserts {
						for len(m) <= x[0] {
							m = append(m, -1)
						}
						m[x[0]] = x[1]
					}
					hash := 0
					for _, x := range probes {
						if x < len(m) {
							val := m[x]
							if val != -1 {
								hash ^= val
							}
						}
					}
				}
			})
			b.Run("slice-sized", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					m := make([]int, tc.keyRange)
					for i := range m {
						m[i] = -1
					}
					for _, x := range inserts {
						m[x[0]] = x[1]
					}
					hash := 0
					for _, x := range probes {
						val := m[x]
						if val != -1 {
							hash ^= val
						}
					}
				}
			})

		})
	}

}
