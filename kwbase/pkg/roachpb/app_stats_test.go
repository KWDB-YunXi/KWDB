// Copyright 2017 The Cockroach Authors.
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

package roachpb

import (
	"math"
	"testing"
)

func TestAddNumericStats(t *testing.T) {
	var a, b, ab NumericStat
	var countA, countB, countAB int64
	var sumA, sumB, sumAB float64

	aData := []float64{1.1, 3.3, 2.2}
	bData := []float64{2.0, 3.0, 5.5, 1.2}

	// Feed some data to A.
	for _, v := range aData {
		countA++
		sumA += v
		a.Record(countA, v)
	}

	// Feed some data to B.
	for _, v := range bData {
		countB++
		sumB += v
		b.Record(countB, v)
	}

	// Feed the A and B data to AB.
	for _, v := range append(bData, aData...) {
		countAB++
		sumAB += v
		ab.Record(countAB, v)
	}

	const epsilon = 0.0000001

	// Sanity check that we have non-trivial stats to combine.
	if mean := 2.2; math.Abs(mean-a.Mean) > epsilon {
		t.Fatalf("Expected Mean %f got %f", mean, a.Mean)
	}
	if mean := sumA / float64(countA); math.Abs(mean-a.Mean) > epsilon {
		t.Fatalf("Expected Mean %f got %f", mean, a.Mean)
	}
	if mean := sumB / float64(countB); math.Abs(mean-b.Mean) > epsilon {
		t.Fatalf("Expected Mean %f got %f", mean, b.Mean)
	}
	if mean := sumAB / float64(countAB); math.Abs(mean-ab.Mean) > epsilon {
		t.Fatalf("Expected Mean %f got %f", mean, ab.Mean)
	}

	// Verify that A+B = AB -- that the stat we get from combining the two is the
	// same as the one that saw the union of values the two saw.
	combined := AddNumericStats(a, b, countA, countB)
	if e := math.Abs(combined.Mean - ab.Mean); e > epsilon {
		t.Fatalf("Mean of combined %f does not match ab %f (%f)", combined.Mean, ab.Mean, e)
	}
	if e := combined.SquaredDiffs - ab.SquaredDiffs; e > epsilon {
		t.Fatalf("SquaredDiffs of combined %f does not match ab %f (%f)", combined.SquaredDiffs, ab.SquaredDiffs, e)
	}

	reversed := AddNumericStats(b, a, countB, countA)
	if combined != reversed {
		t.Fatalf("a+b != b+a: %v vs %v", combined, reversed)
	}

	// Check the in-place side-effect version matches the standalone helper.
	a.Add(b, countA, countB)
	if a != combined {
		t.Fatalf("a.Add(b) should match add(a, b): %+v vs %+v", a, combined)
	}
}
