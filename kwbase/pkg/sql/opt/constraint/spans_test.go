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
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/util/randutil"
)

func TestSpans(t *testing.T) {
	var s Spans
	check := func(exp string) {
		if actual := s.String(); actual != exp {
			t.Errorf("expected %s, got %s", exp, actual)
		}
	}
	add := func(x int) {
		k := MakeKey(tree.NewDInt(tree.DInt(x)))
		var span Span
		span.Init(k, IncludeBoundary, k, IncludeBoundary)
		s.Append(&span)
	}
	check("")
	add(1)
	check("[/1 - /1]")
	add(2)
	check("[/1 - /1] [/2 - /2]")
	add(3)
	check("[/1 - /1] [/2 - /2] [/3 - /3]")

	// Verify that Alloc doesn't lose spans.
	s.Alloc(10)
	check("[/1 - /1] [/2 - /2] [/3 - /3]")

	s.Truncate(2)
	check("[/1 - /1] [/2 - /2]")
	s.Truncate(1)
	check("[/1 - /1]")
	s.Truncate(0)
	check("")
}

func TestSpansSortAndMerge(t *testing.T) {
	keyCtx := testKeyContext(1)
	evalCtx := keyCtx.EvalCtx

	// To test SortAndMerge, we note that the result can also be obtained by
	// creating a constraint per span and calculating the union. We generate
	// random cases and cross-check.
	for testIdx := 0; testIdx < 100; testIdx++ {
		rng, _ := randutil.NewPseudoRand()
		n := 1 + rng.Intn(10)
		var spans Spans
		for i := 0; i < n; i++ {
			x, y := rng.Intn(20), rng.Intn(20)
			if x > y {
				x, y = y, x
			}
			xk, yk := EmptyKey, EmptyKey
			if x > 0 {
				xk = MakeKey(tree.NewDInt(tree.DInt(x)))
			}
			if y > 0 {
				yk = MakeKey(tree.NewDInt(tree.DInt(y)))
			}

			xb, yb := IncludeBoundary, IncludeBoundary
			if x != 0 && x != y && rng.Intn(2) == 0 {
				xb = ExcludeBoundary
			}
			if y != 0 && x != y && rng.Intn(2) == 0 {
				yb = ExcludeBoundary
			}
			var sp Span
			if x != 0 || y != 0 {
				sp.Init(xk, xb, yk, yb)
			}
			spans.Append(&sp)
		}
		origStr := spans.String()

		// Calculate via constraints.
		var c Constraint
		c.InitSingleSpan(keyCtx, spans.Get(0))
		for i := 1; i < spans.Count(); i++ {
			var d Constraint
			d.InitSingleSpan(keyCtx, spans.Get(i))
			c.UnionWith(evalCtx, &d)
		}
		expected := c.Spans.String()

		spans.SortAndMerge(keyCtx)
		if actual := spans.String(); actual != expected {
			t.Fatalf("%s : expected  %s  got  %s", origStr, expected, actual)
		}
	}
}
