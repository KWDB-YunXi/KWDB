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

package xform

import (
	"math"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
)

// TestDistinctOnLimitHint verifies that distinctOnLimitHint never returns a
// negative result.
func TestDistinctOnLimitHint(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// Generate a list of test values. We want to try the 0 value, very small
	// values, very large values, and cases where the two values are ~1 off from
	// each other, or very close to each other.
	var values []float64
	for _, v := range []float64{0, 1e-10, 1e-5, 0.1, 1, 2, 3, 10, 1e5, 1e10} {
		values = append(values, v)
		for _, noise := range []float64{1e-10, 1e-7, 0.1, 1} {
			values = append(values, v+noise)
			if v-noise >= 0 {
				values = append(values, v-noise)
			}
		}
	}

	for _, distinctCount := range values {
		for _, neededRows := range values {
			hint := distinctOnLimitHint(distinctCount, neededRows)
			if hint < 0 || math.IsNaN(hint) || math.IsInf(hint, +1) {
				t.Fatalf("distinctOnLimitHint(%g,%g)=%g", distinctCount, neededRows, hint)
			}
		}
	}
}
