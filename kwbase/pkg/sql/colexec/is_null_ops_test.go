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

package colexec

import (
	"context"
	"fmt"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
)

func TestIsNullProjOp(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg: &execinfra.ServerConfig{
			Settings: st,
		},
	}

	testCases := []struct {
		desc         string
		inputTuples  tuples
		outputTuples tuples
		negate       bool
	}{
		{
			desc:         "SELECT c, c IS NULL FROM t -- both",
			inputTuples:  tuples{{0}, {nil}, {1}, {2}, {nil}},
			outputTuples: tuples{{0, false}, {nil, true}, {1, false}, {2, false}, {nil, true}},
			negate:       false,
		},
		{
			desc:         "SELECT c, c IS NULL FROM t -- no NULLs",
			inputTuples:  tuples{{0}, {1}, {2}},
			outputTuples: tuples{{0, false}, {1, false}, {2, false}},
			negate:       false,
		},
		{
			desc:         "SELECT c, c IS NULL FROM t -- only NULLs",
			inputTuples:  tuples{{nil}, {nil}},
			outputTuples: tuples{{nil, true}, {nil, true}},
			negate:       false,
		},
		{
			desc:         "SELECT c, c IS NOT NULL FROM t -- both",
			inputTuples:  tuples{{0}, {nil}, {1}, {2}, {nil}},
			outputTuples: tuples{{0, true}, {nil, false}, {1, true}, {2, true}, {nil, false}},
			negate:       true,
		},
		{
			desc:         "SELECT c, c IS NOT NULL FROM t -- no NULLs",
			inputTuples:  tuples{{0}, {1}, {2}},
			outputTuples: tuples{{0, true}, {1, true}, {2, true}},
			negate:       true,
		},
		{
			desc:         "SELECT c, c IS NOT NULL FROM t -- only NULLs",
			inputTuples:  tuples{{nil}, {nil}},
			outputTuples: tuples{{nil, false}, {nil, false}},
			negate:       true,
		},
	}

	for _, c := range testCases {
		t.Run(c.desc, func(t *testing.T) {
			opConstructor := func(input []Operator) (Operator, error) {
				projExpr := "IS NULL"
				if c.negate {
					projExpr = "IS NOT NULL"
				}
				return createTestProjectingOperator(
					ctx, flowCtx, input[0], []types.T{*types.Int},
					fmt.Sprintf("@1 %s", projExpr), false, /* canFallbackToRowexec */
				)
			}
			runTests(t, []tuples{c.inputTuples}, c.outputTuples, orderedVerifier, opConstructor)
		})
	}
}

func TestIsNullSelOp(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg: &execinfra.ServerConfig{
			Settings: st,
		},
	}

	testCases := []struct {
		desc         string
		inputTuples  tuples
		outputTuples tuples
		negate       bool
	}{
		{
			desc:         "SELECT c FROM t WHERE c IS NULL -- both",
			inputTuples:  tuples{{0}, {nil}, {1}, {2}, {nil}},
			outputTuples: tuples{{nil}, {nil}},
			negate:       false,
		},
		{
			desc:         "SELECT c FROM t WHERE c IS NULL -- no NULLs",
			inputTuples:  tuples{{0}, {1}, {2}},
			outputTuples: tuples{},
			negate:       false,
		},
		{
			desc:         "SELECT c FROM t WHERE c IS NULL -- only NULLs",
			inputTuples:  tuples{{nil}, {nil}},
			outputTuples: tuples{{nil}, {nil}},
			negate:       false,
		},
		{
			desc:         "SELECT c FROM t WHERE c IS NOT NULL -- both",
			inputTuples:  tuples{{0}, {nil}, {1}, {2}, {nil}},
			outputTuples: tuples{{0}, {1}, {2}},
			negate:       true,
		},
		{
			desc:         "SELECT c FROM t WHERE c IS NOT NULL -- no NULLs",
			inputTuples:  tuples{{0}, {1}, {2}},
			outputTuples: tuples{{0}, {1}, {2}},
			negate:       true,
		},
		{
			desc:         "SELECT c FROM t WHERE c IS NOT NULL -- only NULLs",
			inputTuples:  tuples{{nil}, {nil}},
			outputTuples: tuples{},
			negate:       true,
		},
	}

	for _, c := range testCases {
		t.Run(c.desc, func(t *testing.T) {
			opConstructor := func(input []Operator) (Operator, error) {
				selExpr := "IS NULL"
				if c.negate {
					selExpr = "IS NOT NULL"
				}
				spec := &execinfrapb.ProcessorSpec{
					Input: []execinfrapb.InputSyncSpec{{ColumnTypes: []types.T{*types.Int}}},
					Core: execinfrapb.ProcessorCoreUnion{
						Noop: &execinfrapb.NoopCoreSpec{},
					},
					Post: execinfrapb.PostProcessSpec{
						Filter: execinfrapb.Expression{Expr: fmt.Sprintf("@1 %s", selExpr)},
					},
				}
				args := NewColOperatorArgs{
					Spec:                spec,
					Inputs:              input,
					StreamingMemAccount: testMemAcc,
				}
				args.TestingKnobs.UseStreamingMemAccountForBuffering = true
				result, err := NewColOperator(ctx, flowCtx, args)
				if err != nil {
					return nil, err
				}
				return result.Op, nil
			}
			runTests(t, []tuples{c.inputTuples}, c.outputTuples, orderedVerifier, opConstructor)
		})
	}
}
