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

package distsql

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/colcontainer"
	"gitee.com/kwbasedb/kwbase/pkg/sql/colexec"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/rowexec"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"github.com/pkg/errors"
)

type verifyColOperatorArgs struct {
	// anyOrder determines whether the results should be matched in order (when
	// anyOrder is false) or as sets (when anyOrder is true).
	anyOrder    bool
	inputTypes  [][]types.T
	inputs      []sqlbase.EncDatumRows
	outputTypes []types.T
	pspec       *execinfrapb.ProcessorSpec
	// forceDiskSpill, if set, will force the operator to spill to disk.
	forceDiskSpill bool
	// numForcedRepartitions specifies a number of "repartitions" that a
	// disk-backed operator should be forced to perform. "Repartition" can mean
	// different things depending on the operator (for example, for hash joiner
	// it is dividing original partition into multiple new partitions; for sorter
	// it is merging already created partitions into new one before proceeding
	// to the next partition from the input).
	numForcedRepartitions int
}

// verifyColOperator passes inputs through both the processor defined by pspec
// and the corresponding columnar operator and verifies that the results match.
func verifyColOperator(args verifyColOperatorArgs) error {
	const floatPrecision = 0.0000001

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	tempEngine, tempFS, err := storage.NewTempEngine(ctx, storage.DefaultStorageEngine, base.DefaultTestTempStorageConfig(st), base.DefaultTestStoreSpec)
	if err != nil {
		return err
	}
	defer tempEngine.Close()

	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	diskMonitor := execinfra.NewTestDiskMonitor(ctx, st)
	defer diskMonitor.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg: &execinfra.ServerConfig{
			Settings:    st,
			TempStorage: tempEngine,
			DiskMonitor: diskMonitor,
		},
	}
	flowCtx.Cfg.TestingKnobs.ForceDiskSpill = args.forceDiskSpill

	inputsProc := make([]execinfra.RowSource, len(args.inputs))
	inputsColOp := make([]execinfra.RowSource, len(args.inputs))
	for i, input := range args.inputs {
		inputsProc[i] = execinfra.NewRepeatableRowSource(args.inputTypes[i], input)
		inputsColOp[i] = execinfra.NewRepeatableRowSource(args.inputTypes[i], input)
	}

	proc, err := rowexec.NewProcessor(
		ctx, flowCtx, 0, &args.pspec.Core, &args.pspec.Post,
		inputsProc, []execinfra.RowReceiver{nil}, nil,
	)
	if err != nil {
		return err
	}
	outProc, ok := proc.(execinfra.RowSource)
	if !ok {
		return errors.New("processor is unexpectedly not a RowSource")
	}

	acc := evalCtx.Mon.MakeBoundAccount()
	defer acc.Close(ctx)
	testAllocator := colexec.NewAllocator(ctx, &acc)
	columnarizers := make([]colexec.Operator, len(args.inputs))
	for i, input := range inputsColOp {
		c, err := colexec.NewColumnarizer(ctx, testAllocator, flowCtx, int32(i)+1, input)
		if err != nil {
			return err
		}
		columnarizers[i] = c
	}

	constructorArgs := colexec.NewColOperatorArgs{
		Spec:                 args.pspec,
		Inputs:               columnarizers,
		StreamingMemAccount:  &acc,
		ProcessorConstructor: rowexec.NewProcessor,
		DiskQueueCfg:         colcontainer.DiskQueueCfg{FS: tempFS},
		FDSemaphore:          colexec.NewTestingSemaphore(256),
	}
	var spilled bool
	if args.forceDiskSpill {
		constructorArgs.TestingKnobs.SpillingCallbackFn = func() { spilled = true }
	}
	constructorArgs.TestingKnobs.NumForcedRepartitions = args.numForcedRepartitions
	result, err := colexec.NewColOperator(ctx, flowCtx, constructorArgs)
	if err != nil {
		return err
	}
	defer func() {
		for _, memAccount := range result.OpAccounts {
			memAccount.Close(ctx)
		}
		for _, memMonitor := range result.OpMonitors {
			memMonitor.Stop(ctx)
		}
	}()

	outColOp, err := colexec.NewMaterializer(
		flowCtx,
		int32(len(args.inputs))+2,
		result.Op,
		args.outputTypes,
		&execinfrapb.PostProcessSpec{},
		nil, /* output */
		result.MetadataSources,
		nil, /* toClose */
		nil, /* outputStatsToTrace */
		nil, /* cancelFlow */
	)
	if err != nil {
		return err
	}

	outProc.Start(ctx)
	outColOp.Start(ctx)
	defer outProc.ConsumerClosed()
	defer outColOp.ConsumerClosed()

	printRowForChecking := func(r sqlbase.EncDatumRow) []string {
		res := make([]string, len(args.outputTypes))
		for i, col := range r {
			res[i] = col.String(&args.outputTypes[i])
		}
		return res
	}
	var procRows, colOpRows [][]string
	var procMetas, colOpMetas []execinfrapb.ProducerMetadata
	for {
		rowProc, metaProc := outProc.Next()
		if rowProc != nil {
			procRows = append(procRows, printRowForChecking(rowProc))
		}
		if metaProc != nil {
			if metaProc.Err == nil {
				return errors.Errorf("unexpectedly processor returned non-error "+
					"meta\n%+v", metaProc)
			}
			procMetas = append(procMetas, *metaProc)
		}
		rowColOp, metaColOp := outColOp.Next()
		if rowColOp != nil {
			colOpRows = append(colOpRows, printRowForChecking(rowColOp))
		}
		if metaColOp != nil {
			if metaColOp.Err == nil {
				return errors.Errorf("unexpectedly columnar operator returned "+
					"non-error meta\n%+v", metaColOp)
			}
			colOpMetas = append(colOpMetas, *metaColOp)
		}

		if rowProc == nil && metaProc == nil &&
			rowColOp == nil && metaColOp == nil {
			break
		}
	}

	if len(procMetas) != len(colOpMetas) {
		return errors.Errorf("different number of metas returned:\n"+
			"processor returned\n%+v\n\ncolumnar operator returned\n%+v",
			procMetas, colOpMetas)
	}
	// It is possible that a query will hit an error (for example, integer out of
	// range). We then expect that both the processor and the operator returned
	// such error.
	if len(procMetas) > 1 {
		return errors.Errorf("unexpectedly multiple metas returned:\n"+
			"processor returned\n%+v\n\ncolumnar operator returned\n%+v",
			procMetas, colOpMetas)
	} else if len(procMetas) == 1 {
		procErr := procMetas[0].Err.Error()
		colOpErr := colOpMetas[0].Err.Error()
		if procErr != colOpErr {
			return errors.Errorf("different errors returned:\n"+
				"processor return\n%+v\ncolumnar operator returned\n%+v",
				procMetas[0].Err, colOpMetas[0].Err)
		}
		// The errors are the same, so the rows that were returned do not matter.
		return nil
	}

	if len(procRows) != len(colOpRows) {
		return errors.Errorf("different number of rows returned:\n"+
			"processor returned\n%+v\n\ncolumnar operator returned\n%+v\n"+
			"processor metas\n%+v\ncolumnar operator metas\n%+v\n",
			procRows, colOpRows, procMetas, colOpMetas)
	}

	printRowsOutput := func(rows [][]string) string {
		res := ""
		for i, row := range rows {
			res = fmt.Sprintf("%s\n%d: %v", res, i, row)
		}
		return res
	}

	datumsMatch := func(expected, actual string, typ *types.T) (bool, error) {
		switch typ.Family() {
		case types.FloatFamily:
			// Some operations on floats (for example, aggregation) can produce
			// slightly different results in the row-by-row and vectorized engines.
			// That's why we handle them separately.

			// We first try direct string matching. If that succeeds, then great!
			if expected == actual {
				return true, nil
			}
			// If only one of the values is NULL, then the datums do not match.
			if expected == `NULL` || actual == `NULL` {
				return false, nil
			}
			// Now we will try parsing both strings as floats and check whether they
			// are within allowed precision from each other.
			expFloat, err := strconv.ParseFloat(expected, 64)
			if err != nil {
				return false, err
			}
			actualFloat, err := strconv.ParseFloat(actual, 64)
			if err != nil {
				return false, err
			}
			return math.Abs(expFloat-actualFloat) < floatPrecision, nil
		default:
			return expected == actual, nil
		}
	}

	if args.anyOrder {
		used := make([]bool, len(colOpRows))
		for i, expStrRow := range procRows {
			rowMatched := false
			for j, retStrRow := range colOpRows {
				if used[j] {
					continue
				}
				foundDifference := false
				for k, typ := range args.outputTypes {
					match, err := datumsMatch(expStrRow[k], retStrRow[k], &typ)
					if err != nil {
						return errors.Errorf("error while parsing datum in rows\n%v\n%v\n%s",
							expStrRow, retStrRow, err.Error())
					}
					if !match {
						foundDifference = true
						break
					}
				}
				if !foundDifference {
					rowMatched = true
					used[j] = true
					break
				}
			}
			if !rowMatched {
				return errors.Errorf("different results: no match found for row %d of processor output\n"+
					"processor output:%s\n\ncolumnar operator output:%s",
					i, printRowsOutput(procRows), printRowsOutput(colOpRows))
			}
		}
	} else {
		for i, expStrRow := range procRows {
			retStrRow := colOpRows[i]
			// anyOrder is false, so the result rows must match in the same order.
			for k, typ := range args.outputTypes {
				match, err := datumsMatch(expStrRow[k], retStrRow[k], &typ)
				if err != nil {
					return errors.Errorf("error while parsing datum in rows\n%v\n%v\n%s",
						expStrRow, retStrRow, err.Error())
				}
				if !match {
					return errors.Errorf(
						"different results on row %d;\nexpected:\n%s\ngot:\n%s",
						i, expStrRow, retStrRow,
					)
				}
			}
		}
	}

	if args.forceDiskSpill {
		// Check that the spilling did occur.
		if !spilled {
			return errors.Errorf("expected spilling to disk but it did *not* occur")
		}
	}
	return nil
}
