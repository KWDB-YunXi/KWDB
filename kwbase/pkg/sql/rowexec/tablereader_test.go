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
	"fmt"
	"regexp"
	"sort"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/distsqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/encoding"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/tracing"
	"github.com/gogo/protobuf/types"
	"github.com/opentracing/opentracing-go"
)

func TestTableReader(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	// Create a table where each row is:
	//
	//  |     a    |     b    |         sum         |         s           |
	//  |-----------------------------------------------------------------|
	//  | rowId/10 | rowId%10 | rowId/10 + rowId%10 | IntToEnglish(rowId) |

	aFn := func(row int) tree.Datum {
		return tree.NewDInt(tree.DInt(row / 10))
	}
	bFn := func(row int) tree.Datum {
		return tree.NewDInt(tree.DInt(row % 10))
	}
	sumFn := func(row int) tree.Datum {
		return tree.NewDInt(tree.DInt(row/10 + row%10))
	}

	sqlutils.CreateTable(t, sqlDB, "t",
		"a INT, b INT, sum INT, s STRING, PRIMARY KEY (a,b), INDEX bs (b,s)",
		99,
		sqlutils.ToRowFn(aFn, bFn, sumFn, sqlutils.RowEnglishFn))

	td := sqlbase.GetTableDescriptor(kvDB, "test", "t")

	makeIndexSpan := func(start, end int) execinfrapb.TableReaderSpan {
		var span roachpb.Span
		prefix := roachpb.Key(sqlbase.MakeIndexKeyPrefix(td, td.Indexes[0].ID))
		span.Key = append(prefix, encoding.EncodeVarintAscending(nil, int64(start))...)
		span.EndKey = append(span.EndKey, prefix...)
		span.EndKey = append(span.EndKey, encoding.EncodeVarintAscending(nil, int64(end))...)
		return execinfrapb.TableReaderSpan{Span: span}
	}

	testCases := []struct {
		spec     execinfrapb.TableReaderSpec
		post     execinfrapb.PostProcessSpec
		expected string
	}{
		{
			spec: execinfrapb.TableReaderSpec{
				Spans: []execinfrapb.TableReaderSpan{{Span: td.PrimaryIndexSpan()}},
			},
			post: execinfrapb.PostProcessSpec{
				Filter:        execinfrapb.Expression{Expr: "@3 < 5 AND @2 != 3"}, // sum < 5 && b != 3
				Projection:    true,
				OutputColumns: []uint32{0, 1},
			},
			expected: "[[0 1] [0 2] [0 4] [1 0] [1 1] [1 2] [2 0] [2 1] [2 2] [3 0] [3 1] [4 0]]",
		},
		{
			spec: execinfrapb.TableReaderSpec{
				Spans: []execinfrapb.TableReaderSpan{{Span: td.PrimaryIndexSpan()}},
			},
			post: execinfrapb.PostProcessSpec{
				Filter:        execinfrapb.Expression{Expr: "@3 < 5 AND @2 != 3"},
				Projection:    true,
				OutputColumns: []uint32{3}, // s
				Limit:         4,
			},
			expected: "[['one'] ['two'] ['four'] ['one-zero']]",
		},
		{
			spec: execinfrapb.TableReaderSpec{
				IndexIdx:  1,
				Reverse:   true,
				Spans:     []execinfrapb.TableReaderSpan{makeIndexSpan(4, 6)},
				LimitHint: 1,
			},
			post: execinfrapb.PostProcessSpec{
				Filter:        execinfrapb.Expression{Expr: "@1 < 3"}, // sum < 8
				Projection:    true,
				OutputColumns: []uint32{0, 1},
			},
			expected: "[[2 5] [1 5] [0 5] [2 4] [1 4] [0 4]]",
		},
	}

	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			testutils.RunTrueAndFalse(t, "row-source", func(t *testing.T, rowSource bool) {
				ts := c.spec
				ts.Table = *td

				evalCtx := tree.MakeTestingEvalContext(s.ClusterSettings())
				defer evalCtx.Stop(ctx)
				flowCtx := execinfra.FlowCtx{
					EvalCtx: &evalCtx,
					Cfg:     &execinfra.ServerConfig{Settings: s.ClusterSettings()},
					Txn:     kv.NewTxn(ctx, s.DB(), s.NodeID()),
					NodeID:  s.NodeID(),
				}

				var out execinfra.RowReceiver
				var buf *distsqlutils.RowBuffer
				if !rowSource {
					buf = &distsqlutils.RowBuffer{}
					out = buf
				}
				tr, err := newTableReader(&flowCtx, 0 /* processorID */, &ts, &c.post, out)
				if err != nil {
					t.Fatal(err)
				}

				var results execinfra.RowSource
				if rowSource {
					tr.Start(ctx)
					results = tr
				} else {
					tr.Run(ctx)
					if !buf.ProducerClosed() {
						t.Fatalf("output RowReceiver not closed")
					}
					buf.Start(ctx)
					results = buf
				}

				var res sqlbase.EncDatumRows
				for {
					row, meta := results.Next()
					if meta != nil && meta.LeafTxnFinalState == nil && meta.Metrics == nil {
						t.Fatalf("unexpected metadata: %+v", meta)
					}
					if row == nil {
						break
					}
					res = append(res, row.Copy())
				}
				if result := res.String(tr.OutputTypes()); result != c.expected {
					t.Errorf("invalid results: %s, expected %s'", result, c.expected)
				}
			})
		})
	}
}

// Test that a TableReader outputs metadata about non-local ranges that it read.
func TestMisplannedRangesMetadata(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	tc := serverutils.StartTestCluster(t, 3, /* numNodes */
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				UseDatabase: "test",
			},
		})
	defer tc.Stopper().Stop(ctx)

	db := tc.ServerConn(0)
	sqlutils.CreateTable(t, db, "t",
		"num INT PRIMARY KEY",
		3, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))

	_, err := db.Exec(`
ALTER TABLE t SPLIT AT VALUES (1), (2), (3);
ALTER TABLE t EXPERIMENTAL_RELOCATE VALUES (ARRAY[2], 1), (ARRAY[1], 2), (ARRAY[3], 3);
`)
	if err != nil {
		t.Fatal(err)
	}

	kvDB := tc.Server(0).DB()
	td := sqlbase.GetTableDescriptor(kvDB, "test", "t")

	st := tc.Server(0).ClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	nodeID := tc.Server(0).NodeID()
	flowCtx := execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg:     &execinfra.ServerConfig{Settings: st},
		Txn:     kv.NewTxn(ctx, tc.Server(0).DB(), nodeID),
		NodeID:  nodeID,
	}
	spec := execinfrapb.TableReaderSpec{
		Spans: []execinfrapb.TableReaderSpan{{Span: td.PrimaryIndexSpan()}},
		Table: *td,
	}
	post := execinfrapb.PostProcessSpec{
		Projection:    true,
		OutputColumns: []uint32{0},
	}

	testutils.RunTrueAndFalse(t, "row-source", func(t *testing.T, rowSource bool) {
		var out execinfra.RowReceiver
		var buf *distsqlutils.RowBuffer
		if !rowSource {
			buf = &distsqlutils.RowBuffer{}
			out = buf
		}
		tr, err := newTableReader(&flowCtx, 0 /* processorID */, &spec, &post, out)
		if err != nil {
			t.Fatal(err)
		}

		var results execinfra.RowSource
		if rowSource {
			tr.Start(ctx)
			results = tr
		} else {
			tr.Run(ctx)
			if !buf.ProducerClosed() {
				t.Fatalf("output RowReceiver not closed")
			}
			buf.Start(ctx)
			results = buf
		}

		var res sqlbase.EncDatumRows
		var metas []*execinfrapb.ProducerMetadata
		for {
			row, meta := results.Next()
			if meta != nil {
				metas = append(metas, meta)
				continue
			}
			if row == nil {
				break
			}
			res = append(res, row)
		}

		if len(res) != 3 {
			t.Fatalf("expected 3 rows, got: %d", len(res))
		}
		var misplannedRanges []roachpb.RangeInfo
		for _, m := range metas {
			if len(m.Ranges) > 0 {
				misplannedRanges = m.Ranges
			} else if m.LeafTxnFinalState == nil && m.Metrics == nil {
				t.Fatalf("expected only txn coord meta, metrics, or misplanned ranges, got: %+v", metas)
			}
		}
		if len(misplannedRanges) != 2 {
			t.Fatalf("expected 2 misplanned ranges, got: %+v", misplannedRanges)
		}
		// The metadata about misplanned ranges can come in any order (it depends on
		// the order in which parallel sub-batches complete after having been split by
		// DistSender).
		sort.Slice(misplannedRanges, func(i, j int) bool {
			return misplannedRanges[i].Lease.Replica.NodeID < misplannedRanges[j].Lease.Replica.NodeID
		})
		if misplannedRanges[0].Lease.Replica.NodeID != 2 ||
			misplannedRanges[1].Lease.Replica.NodeID != 3 {
			t.Fatalf("expected misplanned ranges from nodes 2 and 3, got: %+v", metas[0])
		}
	})
}

func TestTableReaderDrain(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.Background())

	sqlutils.CreateTable(t, sqlDB, "t",
		"num INT PRIMARY KEY",
		3, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))

	td := sqlbase.GetTableDescriptor(kvDB, "test", "t")

	// Run the flow in a snowball trace so that we can test for tracing info.
	tracer := tracing.NewTracer()
	ctx, sp := tracing.StartSnowballTrace(context.Background(), tracer, "test flow ctx")
	defer sp.Finish()
	st := s.ClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)

	rootTxn := kv.NewTxn(ctx, s.DB(), s.NodeID())
	leafInputState := rootTxn.GetLeafTxnInputState(ctx)
	leafTxn := kv.NewLeafTxn(ctx, s.DB(), s.NodeID(), &leafInputState)
	flowCtx := execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg: &execinfra.ServerConfig{
			Settings: st,
		},
		Txn:    leafTxn,
		Local:  true,
		NodeID: s.NodeID(),
	}
	spec := execinfrapb.TableReaderSpec{
		Spans: []execinfrapb.TableReaderSpan{{Span: td.PrimaryIndexSpan()}},
		Table: *td,
	}
	post := execinfrapb.PostProcessSpec{
		Projection:    true,
		OutputColumns: []uint32{0},
	}

	testReaderProcessorDrain(ctx, t, func(out execinfra.RowReceiver) (execinfra.Processor, error) {
		return newTableReader(&flowCtx, 0 /* processorID */, &spec, &post, out)
	})
}

// Test that a scan with a limit doesn't touch more ranges than necessary (i.e.
// we properly set the limit on the underlying Fetcher/KVFetcher).
func TestLimitScans(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{
		UseDatabase: "test",
	})
	defer s.Stopper().Stop(ctx)

	sqlutils.CreateTable(t, sqlDB, "t",
		"num INT PRIMARY KEY",
		100, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))

	if _, err := sqlDB.Exec("ALTER TABLE t SPLIT AT VALUES (5)"); err != nil {
		t.Fatal(err)
	}

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "test", "t")

	evalCtx := tree.MakeTestingEvalContext(s.ClusterSettings())
	defer evalCtx.Stop(ctx)
	flowCtx := execinfra.FlowCtx{
		EvalCtx: &evalCtx,
		Cfg:     &execinfra.ServerConfig{Settings: s.ClusterSettings()},
		Txn:     kv.NewTxn(ctx, kvDB, s.NodeID()),
		NodeID:  s.NodeID(),
	}
	spec := execinfrapb.TableReaderSpec{
		Table: *tableDesc,
		Spans: []execinfrapb.TableReaderSpan{{Span: tableDesc.PrimaryIndexSpan()}},
	}
	// We're going to ask for 3 rows, all contained in the first range.
	const limit = 3
	post := execinfrapb.PostProcessSpec{Limit: limit}

	// Now we're going to run the tableReader and trace it.
	tracer := tracing.NewTracer()
	sp := tracer.StartSpan("root", tracing.Recordable)
	tracing.StartRecording(sp, tracing.SnowballRecording)
	ctx = opentracing.ContextWithSpan(ctx, sp)
	flowCtx.EvalCtx.Context = ctx

	tr, err := newTableReader(&flowCtx, 0 /* processorID */, &spec, &post, nil /* output */)
	if err != nil {
		t.Fatal(err)
	}

	tr.Start(ctx)
	rows := 0
	for {
		row, meta := tr.Next()
		if row != nil {
			rows++
		}

		// Simulate what the DistSQLReceiver does and ingest the trace.
		if meta != nil && len(meta.TraceData) > 0 {
			if err := tracing.ImportRemoteSpans(sp, meta.TraceData); err != nil {
				t.Fatal(err)
			}
		}

		if row == nil && meta == nil {
			break
		}
	}
	if rows != limit {
		t.Fatalf("expected %d rows, got: %d", limit, rows)
	}

	// We're now going to count how many distinct scans we've done. This regex is
	// specific so that we don't count range resolving requests, and we dedupe
	// scans from the same key as the DistSender retries scans when it detects
	// splits.
	re := regexp.MustCompile(fmt.Sprintf(`querying next range at /Table/%d/1(\S.*)?`, tableDesc.ID))
	spans := tracing.GetRecording(sp)
	ranges := make(map[string]struct{})
	for _, span := range spans {
		if span.Operation == tableReaderProcName {
			// Verify that stat collection lines up with results.
			trs := TableReaderStats{}
			if err := types.UnmarshalAny(span.Stats, &trs); err != nil {
				t.Fatal(err)
			}
			if trs.InputStats.NumRows != limit {
				t.Fatalf("read %d rows, but stats only counted: %d", limit, trs.InputStats.NumRows)
			}
		}
		for _, l := range span.Logs {
			for _, f := range l.Fields {
				match := re.FindStringSubmatch(f.Value)
				if match == nil {
					continue
				}
				ranges[match[1]] = struct{}{}
			}
		}
	}
	if len(ranges) != 1 {
		t.Fatalf("expected one ranges scanned, got: %d (%+v)", len(ranges), ranges)
	}
}

func BenchmarkTableReader(b *testing.B) {
	defer leaktest.AfterTest(b)()
	logScope := log.Scope(b)
	defer logScope.Close(b)
	ctx := context.Background()

	s, sqlDB, kvDB := serverutils.StartServer(b, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	evalCtx := tree.MakeTestingEvalContext(s.ClusterSettings())
	defer evalCtx.Stop(ctx)

	const numCols = 2
	for _, numRows := range []int{1 << 4, 1 << 8, 1 << 12, 1 << 16} {
		tableName := fmt.Sprintf("t%d", numRows)
		sqlutils.CreateTable(
			b, sqlDB, tableName,
			"k INT PRIMARY KEY, v INT",
			numRows,
			sqlutils.ToRowFn(sqlutils.RowIdxFn, sqlutils.RowModuloFn(42)),
		)
		tableDesc := sqlbase.GetTableDescriptor(kvDB, "test", tableName)
		flowCtx := execinfra.FlowCtx{
			EvalCtx: &evalCtx,
			Cfg:     &execinfra.ServerConfig{Settings: s.ClusterSettings()},
			Txn:     kv.NewTxn(ctx, s.DB(), s.NodeID()),
			NodeID:  s.NodeID(),
		}

		b.Run(fmt.Sprintf("rows=%d", numRows), func(b *testing.B) {
			spec := execinfrapb.TableReaderSpec{
				Table: *tableDesc,
				Spans: []execinfrapb.TableReaderSpan{{Span: tableDesc.PrimaryIndexSpan()}},
			}
			post := execinfrapb.PostProcessSpec{}

			b.SetBytes(int64(numRows * numCols * 8))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr, err := newTableReader(&flowCtx, 0 /* processorID */, &spec, &post, nil /* output */)
				if err != nil {
					b.Fatal(err)
				}
				tr.Start(ctx)
				count := 0
				for {
					row, meta := tr.Next()
					if meta != nil && meta.LeafTxnFinalState == nil && meta.Metrics == nil {
						b.Fatalf("unexpected metadata: %+v", meta)
					}
					if row != nil {
						count++
					} else if meta == nil {
						break
					}
				}
				if count != numRows {
					b.Fatalf("found %d rows, expected %d", count, numRows)
				}
			}
		})
	}
}
