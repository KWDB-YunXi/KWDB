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

package props_test

import (
	"fmt"
	"strings"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/sql/opt"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/props"
)

func TestColStatsMap(t *testing.T) {
	testcases := []struct {
		cols     []opt.ColumnID
		remove   bool
		clear    bool
		expected string
	}{
		{cols: []opt.ColumnID{1}, expected: "(1)"},
		{cols: []opt.ColumnID{1}, expected: "(1)"},
		{cols: []opt.ColumnID{2}, expected: "(1)+(2)"},
		{cols: []opt.ColumnID{1, 2}, expected: "(1)+(2)+(1,2)"},
		{cols: []opt.ColumnID{1, 2}, expected: "(1)+(2)+(1,2)"},
		{cols: []opt.ColumnID{2}, expected: "(1)+(2)+(1,2)"},
		{cols: []opt.ColumnID{1}, remove: true, expected: "(2)"},

		// Add after removing.
		{cols: []opt.ColumnID{2, 3}, expected: "(2)+(2,3)"},
		{cols: []opt.ColumnID{2, 3, 4}, expected: "(2)+(2,3)+(2-4)"},
		{cols: []opt.ColumnID{3}, expected: "(2)+(2,3)+(2-4)+(3)"},
		{cols: []opt.ColumnID{3, 4}, expected: "(2)+(2,3)+(2-4)+(3)+(3,4)"},
		{cols: []opt.ColumnID{5, 7}, expected: "(2)+(2,3)+(2-4)+(3)+(3,4)+(5,7)"},
		{cols: []opt.ColumnID{5}, expected: "(2)+(2,3)+(2-4)+(3)+(3,4)+(5,7)+(5)"},
		{cols: []opt.ColumnID{3, 4}, remove: true, expected: "(2)+(5,7)+(5)"},

		// Add after clearing.
		{cols: []opt.ColumnID{}, clear: true, expected: ""},
		{cols: []opt.ColumnID{5}, expected: "(5)"},
		{cols: []opt.ColumnID{1}, expected: "(5)+(1)"},
		{cols: []opt.ColumnID{1, 5}, expected: "(5)+(1)+(1,5)"},
		{cols: []opt.ColumnID{5, 6}, expected: "(5)+(1)+(1,5)+(5,6)"},
		{cols: []opt.ColumnID{2}, expected: "(5)+(1)+(1,5)+(5,6)+(2)"},
		{cols: []opt.ColumnID{1, 2}, expected: "(5)+(1)+(1,5)+(5,6)+(2)+(1,2)"},

		// Remove node, where remaining nodes still require prefix tree index.
		{cols: []opt.ColumnID{6}, remove: true, expected: "(5)+(1)+(1,5)+(2)+(1,2)"},
		{cols: []opt.ColumnID{3, 4}, expected: "(5)+(1)+(1,5)+(2)+(1,2)+(3,4)"},
	}

	tcStats := make([]props.ColStatsMap, len(testcases))
	// First calculate the stats for all steps, making copies every time. This
	// also tests that the stats are copied correctly and there is no aliasing.
	for tcIdx, tc := range testcases {
		stats := &tcStats[tcIdx]
		if tcIdx > 0 {
			stats.CopyFrom(&tcStats[tcIdx-1])
		}
		cols := opt.MakeColSet(tc.cols...)
		if !tc.remove {
			if tc.clear {
				stats.Clear()
			} else {
				stats.Add(cols)
			}
		} else {
			stats.RemoveIntersecting(cols)
		}
	}

	for tcIdx, tc := range testcases {
		stats := &tcStats[tcIdx]
		var b strings.Builder
		for i := 0; i < stats.Count(); i++ {
			get := stats.Get(i)
			if i != 0 {
				b.WriteRune('+')
			}
			fmt.Fprint(&b, get.Cols)

			lookup, ok := stats.Lookup(get.Cols)
			if !ok {
				t.Errorf("could not find cols in map: %s", get.Cols)
			}
			if get != lookup {
				t.Errorf("lookup did not return expected colstat: %+v vs. %+v", get, lookup)
			}
		}

		if b.String() != tc.expected {
			t.Errorf("expected: %s, actual: %s", tc.expected, b.String())
		}
	}
}
