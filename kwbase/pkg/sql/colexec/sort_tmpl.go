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

// {{/*
// +build execgen_template
//
// This file is the execgen template for sort.eg.go. It's formatted in a
// special way, so it's both valid Go and a valid text/template input. This
// permits editing this file with editor support.
//
// */}}

package colexec

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/col/coldata"
	"gitee.com/kwbasedb/kwbase/pkg/col/coltypes"
	"gitee.com/kwbasedb/kwbase/pkg/sql/colexec/execerror"
	// {{/*
	"gitee.com/kwbasedb/kwbase/pkg/sql/colexec/execgen"
	// */}}
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/util/duration"
	"github.com/cockroachdb/apd"
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

// _TYPES_T is the template type variable for coltypes.T. It will be replaced by
// coltypes.Foo for each type Foo in the coltypes.T type.
const _TYPES_T = coltypes.Unhandled

// _ISNULL is the template type variable for whether the sorter handles nulls
// or not. It will be replaced by the appropriate boolean.
const _ISNULL = false

// _ASSIGN_LT is the template equality function for assigning the first input
// to the result of the second input < the third input.
func _ASSIGN_LT(_, _, _ string) bool {
	execerror.VectorizedInternalPanic("")
}

// */}}

func isSorterSupported(t coltypes.T, dir execinfrapb.Ordering_Column_Direction) bool {
	switch t {
	// {{range $typ, $ := . }} {{/* for each type */}}
	case _TYPES_T:
		switch dir {
		// {{range (index . true).Overloads}} {{/* for each direction */}}
		case _DIR_ENUM:
			return true
		// {{end}}
		default:
			return false
		}
	// {{end}}
	default:
		return false
	}
}

func newSingleSorter(
	t coltypes.T, dir execinfrapb.Ordering_Column_Direction, hasNulls bool,
) colSorter {
	switch t {
	// {{range $typ, $ := . }} {{/* for each type */}}
	case _TYPES_T:
		switch hasNulls {
		// {{range $isNull, $ := . }} {{/* for null vs not null */}}
		case _ISNULL:
			switch dir {
			// {{range .Overloads}} {{/* for each direction */}}
			case _DIR_ENUM:
				return &sort_TYPE_DIR_HANDLES_NULLSOp{}
			// {{end}}
			default:
				execerror.VectorizedInternalPanic("nulls switch failed")
			}
			// {{end}}
		default:
			execerror.VectorizedInternalPanic("nulls switch failed")
		}
	// {{end}}
	default:
		execerror.VectorizedInternalPanic("nulls switch failed")
	}
	// This code is unreachable, but the compiler cannot infer that.
	return nil
}

// {{range $typ, $ := . }} {{/* for each type */}}
// {{range . }} {{/* for null vs not null */}}
// {{range .Overloads}} {{/* for each direction */}}

type sort_TYPE_DIR_HANDLES_NULLSOp struct {
	sortCol       _GOTYPESLICE
	nulls         *coldata.Nulls
	order         []int
	cancelChecker CancelChecker
}

func (s *sort_TYPE_DIR_HANDLES_NULLSOp) init(col coldata.Vec, order []int) {
	s.sortCol = col._TemplateType()
	s.nulls = col.Nulls()
	s.order = order
}

func (s *sort_TYPE_DIR_HANDLES_NULLSOp) sort(ctx context.Context) {
	n := execgen.LEN(s.sortCol)
	s.quickSort(ctx, 0, n, maxDepth(n))
}

func (s *sort_TYPE_DIR_HANDLES_NULLSOp) sortPartitions(ctx context.Context, partitions []int) {
	if len(partitions) < 1 {
		execerror.VectorizedInternalPanic(fmt.Sprintf("invalid partitions list %v", partitions))
	}
	order := s.order
	for i, partitionStart := range partitions {
		var partitionEnd int
		if i == len(partitions)-1 {
			partitionEnd = len(order)
		} else {
			partitionEnd = partitions[i+1]
		}
		s.order = order[partitionStart:partitionEnd]
		n := partitionEnd - partitionStart
		s.quickSort(ctx, 0, n, maxDepth(n))
	}
}

func (s *sort_TYPE_DIR_HANDLES_NULLSOp) Less(i, j int) bool {
	// {{ if eq .Nulls true }}
	n1 := s.nulls.MaybeHasNulls() && s.nulls.NullAt(s.order[i])
	n2 := s.nulls.MaybeHasNulls() && s.nulls.NullAt(s.order[j])
	// {{ if eq .DirString "Asc" }}
	// If ascending, nulls always sort first, so we encode that logic here.
	if n1 && n2 {
		return false
	} else if n1 {
		return true
	} else if n2 {
		return false
	}
	// {{ else if eq .DirString "Desc" }}
	// If descending, nulls always sort last, so we encode that logic here.
	if n1 && n2 {
		return false
	} else if n1 {
		return false
	} else if n2 {
		return true
	}
	// {{end}}
	// {{end}}
	var lt bool
	// We always indirect via the order vector.
	arg1 := execgen.UNSAFEGET(s.sortCol, s.order[i])
	arg2 := execgen.UNSAFEGET(s.sortCol, s.order[j])
	_ASSIGN_LT(lt, arg1, arg2)
	return lt
}

func (s *sort_TYPE_DIR_HANDLES_NULLSOp) Swap(i, j int) {
	// We don't physically swap the column - we merely edit the order vector.
	s.order[i], s.order[j] = s.order[j], s.order[i]
}

func (s *sort_TYPE_DIR_HANDLES_NULLSOp) Len() int {
	return len(s.order)
}

// {{end}}
// {{end}}
// {{end}}
