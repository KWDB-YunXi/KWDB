// Copyright 2019 The Cockroach Authors.
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

// {{/*
// +build execgen_template
//
// This file is the execgen template for min_max_agg.eg.go. It's formatted in a
// special way, so it's both valid Go and a valid text/template input. This
// permits editing this file with editor support.
//
// */}}

package colexec

import (
	"bytes"
	"math"
	"time"
	"unsafe"

	"gitee.com/kwbasedb/kwbase/pkg/col/coldata"
	"gitee.com/kwbasedb/kwbase/pkg/col/coltypes"
	// {{/*
	"gitee.com/kwbasedb/kwbase/pkg/sql/colexec/execerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/colexec/execgen"
	// */}}
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/util/duration"
	"github.com/cockroachdb/apd"
	"github.com/pkg/errors"
)

// {{/*
// Declarations to make the template compile properly.

// Dummy import to pull in "bytes" package.
var _ bytes.Buffer

// Dummy import to pull in "apd" package.
var _ apd.Decimal

// Dummy import to pull in "time" package.
var _ time.Time

// Dummy import to pull in "duration" package.
var _ duration.Duration

// Dummy import to pull in "tree" package.
var _ tree.Datum

// Dummy import to pull in "math" package.
var _ = math.MaxInt64

// _GOTYPESLICE is the template Go type slice variable for this operator. It
// will be replaced by the Go slice representation for each type in coltypes.T, for
// example []int64 for coltypes.Int64.
type _GOTYPESLICE interface{}

// _ASSIGN_CMP is the template function for assigning true to the first input
// if the second input compares successfully to the third input. The comparison
// operator is tree.LT for MIN and is tree.GT for MAX.
func _ASSIGN_CMP(_, _, _ string) bool {
	execerror.VectorizedInternalPanic("")
}

// */}}

// {{range .}} {{/* for each aggregation (min and max) */}}

// {{/* Capture the aggregation name so we can use it in the inner loop. */}}
// {{$agg := .AggNameLower}}

func new_AGG_TITLEAgg(allocator *Allocator, t coltypes.T) (aggregateFunc, error) {
	switch t {
	// {{range .Overloads}}
	case _TYPES_T:
		allocator.AdjustMemoryUsage(int64(sizeOf_AGG_TYPEAgg))
		return &_AGG_TYPEAgg{allocator: allocator}, nil
	// {{end}}
	default:
		return nil, errors.Errorf("unsupported min/max agg type %s", t)
	}
}

// {{range .Overloads}}

type _AGG_TYPEAgg struct {
	allocator *Allocator
	done      bool
	groups    []bool
	curIdx    int
	// curAgg holds the running min/max, so we can index into the slice once per
	// group, instead of on each iteration.
	// NOTE: if foundNonNullForCurrentGroup is false, curAgg is undefined.
	curAgg _GOTYPE
	// col points to the output vector we are updating.
	col _GOTYPESLICE
	// vec is the same as col before conversion from coldata.Vec.
	vec coldata.Vec
	// nulls points to the output null vector that we are updating.
	nulls *coldata.Nulls
	// foundNonNullForCurrentGroup tracks if we have seen any non-null values
	// for the group that is currently being aggregated.
	foundNonNullForCurrentGroup bool
}

var _ aggregateFunc = &_AGG_TYPEAgg{}

const sizeOf_AGG_TYPEAgg = unsafe.Sizeof(_AGG_TYPEAgg{})

func (a *_AGG_TYPEAgg) Init(groups []bool, v coldata.Vec) {
	a.groups = groups
	a.vec = v
	a.col = v._TYPE()
	a.nulls = v.Nulls()
	a.Reset()
}

func (a *_AGG_TYPEAgg) Reset() {
	a.curIdx = -1
	a.foundNonNullForCurrentGroup = false
	a.nulls.UnsetNulls()
	a.done = false
}

func (a *_AGG_TYPEAgg) CurrentOutputIndex() int {
	return a.curIdx
}

func (a *_AGG_TYPEAgg) SetOutputIndex(idx int) {
	if a.curIdx != -1 {
		a.curIdx = idx
		a.nulls.UnsetNullsAfter(idx + 1)
	}
}

func (a *_AGG_TYPEAgg) Compute(b coldata.Batch, inputIdxs []uint32) {
	if a.done {
		return
	}
	inputLen := b.Length()
	if inputLen == 0 {
		// The aggregation is finished. Flush the last value. If we haven't found
		// any non-nulls for this group so far, the output for this group should
		// be null.
		if !a.foundNonNullForCurrentGroup {
			a.nulls.SetNull(a.curIdx)
		} else {
			a.allocator.PerformOperation(
				[]coldata.Vec{a.vec},
				func() {
					execgen.SET(a.col, a.curIdx, a.curAgg)
				},
			)
		}
		a.curIdx++
		a.done = true
		return
	}
	vec, sel := b.ColVec(int(inputIdxs[0])), b.Selection()
	col, nulls := vec._TYPE(), vec.Nulls()
	// {{if eq .LTyp.String "Bytes"}}
	oldCurAggSize := len(a.curAgg)
	// {{end}}
	a.allocator.PerformOperation(
		[]coldata.Vec{a.vec},
		func() {
			if nulls.MaybeHasNulls() {
				if sel != nil {
					sel = sel[:inputLen]
					for _, i := range sel {
						_ACCUMULATE_MINMAX(a, nulls, i, true)
					}
				} else {
					col = execgen.SLICE(col, 0, inputLen)
					for execgen.RANGE(i, col, 0, inputLen) {
						_ACCUMULATE_MINMAX(a, nulls, i, true)
					}
				}
			} else {
				if sel != nil {
					sel = sel[:inputLen]
					for _, i := range sel {
						_ACCUMULATE_MINMAX(a, nulls, i, false)
					}
				} else {
					col = execgen.SLICE(col, 0, inputLen)
					for execgen.RANGE(i, col, 0, inputLen) {
						_ACCUMULATE_MINMAX(a, nulls, i, false)
					}
				}
			}
		},
	)
	// {{if eq .LTyp.String "Bytes"}}
	a.allocator.AdjustMemoryUsage(int64(len(a.curAgg) - oldCurAggSize))
	// {{end}}
}

func (a *_AGG_TYPEAgg) HandleEmptyInputScalar() {
	a.nulls.SetNull(0)
}

// {{end}}
// {{end}}

// {{/*
// _ACCUMULATE_MINMAX sets the output for the current group to be the value of
// the ith row if it is smaller/larger than the current result. If this is the
// first row of a new group, and no non-nulls have been found for the current
// group, then the output for the current group is set to null.
func _ACCUMULATE_MINMAX(a *_AGG_TYPEAgg, nulls *coldata.Nulls, i int, _HAS_NULLS bool) { // */}}

	// {{define "accumulateMinMax"}}
	if a.groups[i] {
		// If we encounter a new group, and we haven't found any non-nulls for the
		// current group, the output for this group should be null. If a.curIdx is
		// negative, it means that this is the first group.
		if a.curIdx >= 0 {
			if !a.foundNonNullForCurrentGroup {
				a.nulls.SetNull(a.curIdx)
			} else {
				execgen.SET(a.col, a.curIdx, a.curAgg)
			}
		}
		a.curIdx++
		a.foundNonNullForCurrentGroup = false
	}
	var isNull bool
	// {{ if .HasNulls }}
	isNull = nulls.NullAt(i)
	// {{ else }}
	isNull = false
	// {{ end }}
	if !isNull {
		if !a.foundNonNullForCurrentGroup {
			val := execgen.UNSAFEGET(col, i)
			execgen.COPYVAL(a.curAgg, val)
			a.foundNonNullForCurrentGroup = true
		} else {
			var cmp bool
			candidate := execgen.UNSAFEGET(col, i)
			_ASSIGN_CMP(cmp, candidate, a.curAgg)
			if cmp {
				execgen.COPYVAL(a.curAgg, candidate)
			}
		}
	}
	// {{end}}

	// {{/*
} // */}}
