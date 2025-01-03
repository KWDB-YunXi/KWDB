// Copyright 2016 The Cockroach Authors.
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

package rowexec

import (
	"context"
	gosql "database/sql"
	"encoding/hex"
	"reflect"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/gossip"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlutil"
	"gitee.com/kwbasedb/kwbase/pkg/sql/stats"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/distsqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/protoutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/randutil"
)

func TestSampleAggregator(t *testing.T) {
	defer leaktest.AfterTest(t)()

	server, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer server.Stopper().Stop(context.TODO())

	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	runTest := func(memLimitBytes int64, expectOutOfMemory bool) {
		flowCtx := execinfra.FlowCtx{
			EvalCtx: &evalCtx,
			Cfg: &execinfra.ServerConfig{
				Settings: st,
				DB:       kvDB,
				Executor: server.InternalExecutor().(sqlutil.InternalExecutor),
				Gossip:   server.GossipI().(*gossip.Gossip),
			},
		}
		// Override the default memory limit. If memLimitBytes is small but
		// non-zero, the processor will hit this limit and disable sampling.
		flowCtx.Cfg.TestingKnobs.MemoryLimitBytes = memLimitBytes

		inputRows := [][]int{
			{-1, 1},
			{1, 1},
			{2, 2},
			{1, 3},
			{2, 4},
			{1, 5},
			{2, 6},
			{1, 7},
			{2, 8},
			{-1, 3},
			{1, -1},
		}

		// We randomly distribute the input rows between multiple Samplers and
		// aggregate the results.
		numSamplers := 3

		samplerOutTypes := []types.T{
			*types.Int,   // original column
			*types.Int,   // original column
			*types.Int,   // rank
			*types.Int,   // sketch index
			*types.Int,   // num rows
			*types.Int,   // null vals
			*types.Bytes, // sketch data
		}

		sketchSpecs := []execinfrapb.SketchSpec{
			{
				SketchType:        execinfrapb.SketchType_HLL_PLUS_PLUS_V1,
				Columns:           []uint32{0},
				GenerateHistogram: false,
				StatName:          "a",
				ColumnTypes:       []uint32{uint32(sqlbase.ColumnType_TYPE_DATA)},
			},
			{
				SketchType:          execinfrapb.SketchType_HLL_PLUS_PLUS_V1,
				Columns:             []uint32{1},
				GenerateHistogram:   true,
				HistogramMaxBuckets: 4,
				ColumnTypes:         []uint32{uint32(sqlbase.ColumnType_TYPE_DATA)},
			},
		}

		rng, _ := randutil.NewPseudoRand()
		rowPartitions := make([][][]int, numSamplers)
		for _, row := range inputRows {
			j := rng.Intn(numSamplers)
			rowPartitions[j] = append(rowPartitions[j], row)
		}

		outputs := make([]*distsqlutils.RowBuffer, numSamplers)
		for i := 0; i < numSamplers; i++ {
			rows := sqlbase.GenEncDatumRowsInt(rowPartitions[i])
			in := distsqlutils.NewRowBuffer(sqlbase.TwoIntCols, rows, distsqlutils.RowBufferArgs{})
			outputs[i] = distsqlutils.NewRowBuffer(samplerOutTypes, nil /* rows */, distsqlutils.RowBufferArgs{})

			spec := &execinfrapb.SamplerSpec{SampleSize: 100, Sketches: sketchSpecs}
			p, err := newSamplerProcessor(
				&flowCtx, 0 /* processorID */, spec, in, &execinfrapb.PostProcessSpec{}, outputs[i],
			)
			if err != nil {
				t.Fatal(err)
			}
			p.Run(context.Background())
		}
		// Randomly interleave the output rows from the samplers into a single buffer.
		samplerResults := distsqlutils.NewRowBuffer(samplerOutTypes, nil /* rows */, distsqlutils.RowBufferArgs{})
		for len(outputs) > 0 {
			i := rng.Intn(len(outputs))
			row, meta := outputs[i].Next()
			if meta != nil {
				if meta.SamplerProgress == nil {
					t.Fatalf("unexpected metadata: %v", meta)
				}
			} else if row == nil {
				outputs = append(outputs[:i], outputs[i+1:]...)
			} else {
				samplerResults.Push(row, nil /* meta */)
			}
		}

		// Now run the sample aggregator.
		finalOut := distsqlutils.NewRowBuffer([]types.T{}, nil /* rows*/, distsqlutils.RowBufferArgs{})
		spec := &execinfrapb.SampleAggregatorSpec{
			SampleSize:       100,
			Sketches:         sketchSpecs,
			SampledColumnIDs: []sqlbase.ColumnID{100, 101},
			TableID:          13,
		}

		agg, err := newSampleAggregator(
			&flowCtx, 0 /* processorID */, spec, samplerResults, &execinfrapb.PostProcessSpec{}, finalOut,
		)
		if err != nil {
			t.Fatal(err)
		}
		agg.Run(context.Background())
		// Make sure there was no error.
		finalOut.GetRowsNoMeta(t)
		r := sqlutils.MakeSQLRunner(sqlDB)

		rows := r.Query(t, `
	  SELECT "tableID",
					 "name",
					 "columnIDs",
					 "rowCount",
					 "distinctCount",
					 "nullCount",
					 histogram
	  FROM system.table_statistics
  `)
		defer rows.Close()

		type resultBucket struct {
			numEq, numRange, upper int
		}

		type result struct {
			tableID                            int
			name, colIDs                       string
			rowCount, distinctCount, nullCount int
			buckets                            []resultBucket
		}

		expected := []result{
			{
				tableID:       13,
				name:          "a",
				colIDs:        "{100}",
				rowCount:      11,
				distinctCount: 3,
				nullCount:     2,
			},
			{
				tableID:       13,
				name:          "<NULL>",
				colIDs:        "{101}",
				rowCount:      11,
				distinctCount: 9,
				nullCount:     1,
				buckets: []resultBucket{
					{numEq: 2, numRange: 0, upper: 1},
					{numEq: 2, numRange: 1, upper: 3},
					{numEq: 1, numRange: 1, upper: 5},
					{numEq: 1, numRange: 2, upper: 8},
				},
			},
		}

		for _, exp := range expected {
			if !rows.Next() {
				t.Fatal("fewer rows than expected")
			}
			if expectOutOfMemory {
				exp.buckets = nil
			}

			var histData []byte
			var name gosql.NullString
			var r result
			if err := rows.Scan(
				&r.tableID, &name, &r.colIDs, &r.rowCount, &r.distinctCount, &r.nullCount, &histData,
			); err != nil {
				t.Fatal(err)
			}
			if name.Valid {
				r.name = name.String
			} else {
				r.name = "<NULL>"
			}

			if len(histData) > 0 {
				var h stats.HistogramData
				histData, err := hex.DecodeString(string(histData[2:]))
				if err != nil {
					t.Fatal(err)
				}
				if err := protoutil.Unmarshal(histData, &h); err != nil {
					t.Fatal(err)
				}

				for _, b := range h.Buckets {
					ed, _, err := sqlbase.EncDatumFromBuffer(
						types.Int, sqlbase.DatumEncoding_ASCENDING_KEY, b.UpperBound,
					)
					if err != nil {
						t.Fatal(err)
					}
					var d sqlbase.DatumAlloc
					if err := ed.EnsureDecoded(types.Int, &d); err != nil {
						t.Fatal(err)
					}
					r.buckets = append(r.buckets, resultBucket{
						numEq:    int(b.NumEq),
						numRange: int(b.NumRange),
						upper:    int(*ed.Datum.(*tree.DInt)),
					})
				}
			} else if len(exp.buckets) > 0 {
				t.Error("no histogram")
			}

			if !reflect.DeepEqual(exp, r) {
				t.Errorf("Expected:\n  %v\ngot:\n  %v", exp, r)
			}
		}
		if rows.Next() {
			t.Fatal("more rows than expected")
		}
	}

	runTest(0 /* memLimitBytes */, false /* expectOutOfMemory */)
	runTest(1 /* memLimitBytes */, true /* expectOutOfMemory */)
	runTest(20 /* memLimitBytes */, true /* expectOutOfMemory */)
	runTest(20*1024 /* memLimitBytes */, false /* expectOutOfMemory */)
}
