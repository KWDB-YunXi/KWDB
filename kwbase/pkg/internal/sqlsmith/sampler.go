// This file was automatically generated by genny.
// Any changes will be lost if this file is regenerated.
// see https://github.com/cheekybits/genny

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

package sqlsmith

import (
	"math/rand"

	"gitee.com/kwbasedb/kwbase/pkg/util/syncutil"
)

// statementWeight is the generic weight type.
type statementWeight struct {
	weight int
	elem   statement
}

// newWeightedStatementSampler creates a statementSampler that produces
// statements. They are returned at the relative frequency of the values of
// weights. All weights must be >= 1.
func newWeightedStatementSampler(weights []statementWeight, seed int64) *statementSampler {
	sum := 0
	for _, w := range weights {
		if w.weight < 1 {
			panic("expected weight >= 1")
		}
		sum += w.weight
	}
	if sum == 0 {
		panic("expected weights")
	}
	samples := make([]statement, sum)
	pos := 0
	for _, w := range weights {
		for count := 0; count < w.weight; count++ {
			samples[pos] = w.elem
			pos++
		}
	}
	return &statementSampler{
		rnd:     rand.New(rand.NewSource(seed)),
		samples: samples,
	}
}

type statementSampler struct {
	mu      syncutil.Mutex
	rnd     *rand.Rand
	samples []statement
}

func (w *statementSampler) Next() statement {
	w.mu.Lock()
	v := w.samples[w.rnd.Intn(len(w.samples))]
	w.mu.Unlock()
	return v
}

// tableExprWeight is the generic weight type.
type tableExprWeight struct {
	weight int
	elem   tableExpr
}

// newWeightedTableExprSampler creates a tableExprSampler that produces
// tableExprs. They are returned at the relative frequency of the values of
// weights. All weights must be >= 1.
func newWeightedTableExprSampler(weights []tableExprWeight, seed int64) *tableExprSampler {
	sum := 0
	for _, w := range weights {
		if w.weight < 1 {
			panic("expected weight >= 1")
		}
		sum += w.weight
	}
	if sum == 0 {
		panic("expected weights")
	}
	samples := make([]tableExpr, sum)
	pos := 0
	for _, w := range weights {
		for count := 0; count < w.weight; count++ {
			samples[pos] = w.elem
			pos++
		}
	}
	return &tableExprSampler{
		rnd:     rand.New(rand.NewSource(seed)),
		samples: samples,
	}
}

type tableExprSampler struct {
	mu      syncutil.Mutex
	rnd     *rand.Rand
	samples []tableExpr
}

func (w *tableExprSampler) Next() tableExpr {
	w.mu.Lock()
	v := w.samples[w.rnd.Intn(len(w.samples))]
	w.mu.Unlock()
	return v
}

// selectStatementWeight is the generic weight type.
type selectStatementWeight struct {
	weight int
	elem   selectStatement
}

// newWeightedSelectStatementSampler creates a selectStatementSampler that produces
// selectStatements. They are returned at the relative frequency of the values of
// weights. All weights must be >= 1.
func newWeightedSelectStatementSampler(
	weights []selectStatementWeight, seed int64,
) *selectStatementSampler {
	sum := 0
	for _, w := range weights {
		if w.weight < 1 {
			panic("expected weight >= 1")
		}
		sum += w.weight
	}
	if sum == 0 {
		panic("expected weights")
	}
	samples := make([]selectStatement, sum)
	pos := 0
	for _, w := range weights {
		for count := 0; count < w.weight; count++ {
			samples[pos] = w.elem
			pos++
		}
	}
	return &selectStatementSampler{
		rnd:     rand.New(rand.NewSource(seed)),
		samples: samples,
	}
}

type selectStatementSampler struct {
	mu      syncutil.Mutex
	rnd     *rand.Rand
	samples []selectStatement
}

func (w *selectStatementSampler) Next() selectStatement {
	w.mu.Lock()
	v := w.samples[w.rnd.Intn(len(w.samples))]
	w.mu.Unlock()
	return v
}

// scalarExprWeight is the generic weight type.
type scalarExprWeight struct {
	weight int
	elem   scalarExpr
}

// newWeightedScalarExprSampler creates a scalarExprSampler that produces
// scalarExprs. They are returned at the relative frequency of the values of
// weights. All weights must be >= 1.
func newWeightedScalarExprSampler(weights []scalarExprWeight, seed int64) *scalarExprSampler {
	sum := 0
	for _, w := range weights {
		if w.weight < 1 {
			panic("expected weight >= 1")
		}
		sum += w.weight
	}
	if sum == 0 {
		panic("expected weights")
	}
	samples := make([]scalarExpr, sum)
	pos := 0
	for _, w := range weights {
		for count := 0; count < w.weight; count++ {
			samples[pos] = w.elem
			pos++
		}
	}
	return &scalarExprSampler{
		rnd:     rand.New(rand.NewSource(seed)),
		samples: samples,
	}
}

type scalarExprSampler struct {
	mu      syncutil.Mutex
	rnd     *rand.Rand
	samples []scalarExpr
}

func (w *scalarExprSampler) Next() scalarExpr {
	w.mu.Lock()
	v := w.samples[w.rnd.Intn(len(w.samples))]
	w.mu.Unlock()
	return v
}
