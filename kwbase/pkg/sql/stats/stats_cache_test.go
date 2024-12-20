// Copyright 2017 The Cockroach Authors.
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

package stats

import (
	"context"
	"math/rand"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/gossip"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlutil"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/encoding"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/protoutil"
	"github.com/pkg/errors"
)

func insertTableStat(
	ctx context.Context, db *kv.DB, ex sqlutil.InternalExecutor, stat *TableStatisticProto,
) error {
	insertStatStmt := `
INSERT INTO system.table_statistics ("tableID", "statisticID", name, "columnIDs", "createdAt",
	"rowCount", "distinctCount", "nullCount", histogram)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`
	columnIDs := tree.NewDArray(types.Int)
	for _, id := range stat.ColumnIDs {
		if err := columnIDs.Append(tree.NewDInt(tree.DInt(int(id)))); err != nil {
			return err
		}
	}

	args := []interface{}{
		stat.TableID,
		stat.StatisticID,
		nil, // name
		columnIDs,
		stat.CreatedAt,
		stat.RowCount,
		stat.DistinctCount,
		stat.NullCount,
		nil, // histogram
	}
	if len(stat.Name) != 0 {
		args[2] = stat.Name
	}
	if stat.HistogramData != nil {
		histogramBytes, err := protoutil.Marshal(stat.HistogramData)
		if err != nil {
			return err
		}
		args[8] = histogramBytes
	}

	var rows int
	rows, err := ex.Exec(ctx, "insert-stat", nil /* txn */, insertStatStmt, args...)
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.Errorf("%d rows affected by stats insertion; expected exactly one row affected.", rows)
	}
	return nil

}

func lookupTableStats(
	ctx context.Context, sc *TableStatisticsCache, tableID sqlbase.ID,
) ([]*TableStatistic, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if e, ok := sc.mu.cache.Get(tableID); ok {
		return e.(*cacheEntry).stats, true
	}
	return nil, false
}

func checkStatsForTable(
	ctx context.Context,
	sc *TableStatisticsCache,
	expected []*TableStatisticProto,
	tableID sqlbase.ID,
) error {
	// Initially the stats won't be in the cache.
	if statsList, ok := lookupTableStats(ctx, sc, tableID); ok {
		return errors.Errorf("lookup of missing key %d returned: %s", tableID, statsList)
	}

	// Perform the lookup and refresh, and confirm the
	// returned stats match the expected values.
	statsList, err := sc.GetTableStats(ctx, tableID)
	if err != nil {
		return errors.Errorf(err.Error())
	}
	if !checkStats(statsList, expected) {
		return errors.Errorf("for lookup of key %d, expected stats %s, got %s", tableID, expected, statsList)
	}

	// Now the stats should be in the cache.
	if _, ok := lookupTableStats(ctx, sc, tableID); !ok {
		return errors.Errorf("for lookup of key %d, expected stats %s", tableID, expected)
	}
	return nil
}

func checkStats(actual []*TableStatistic, expected []*TableStatisticProto) bool {
	if len(actual) == 0 && len(expected) == 0 {
		// DeepEqual differentiates between nil and empty slices, we don't.
		return true
	}
	var protoList []*TableStatisticProto
	for i := range actual {
		protoList = append(protoList, &actual[i].TableStatisticProto)
	}
	return reflect.DeepEqual(protoList, expected)
}

func initTestData(
	ctx context.Context, db *kv.DB, ex sqlutil.InternalExecutor,
) (map[sqlbase.ID][]*TableStatisticProto, error) {
	// The expected stats must be ordered by TableID+, CreatedAt- so they can
	// later be compared with the returned stats using reflect.DeepEqual.
	expStatsList := []TableStatisticProto{
		{
			TableID:       sqlbase.ID(100),
			StatisticID:   0,
			Name:          "table0",
			ColumnIDs:     []sqlbase.ColumnID{1},
			CreatedAt:     time.Date(2010, 11, 20, 11, 35, 24, 0, time.UTC),
			RowCount:      32,
			DistinctCount: 30,
			NullCount:     0,
			HistogramData: &HistogramData{ColumnType: *types.Int, Buckets: []HistogramData_Bucket{
				{NumEq: 3, NumRange: 30, UpperBound: encoding.EncodeVarintAscending(nil, 3000)}},
			},
		},
		{
			TableID:       sqlbase.ID(100),
			StatisticID:   1,
			ColumnIDs:     []sqlbase.ColumnID{2, 3},
			CreatedAt:     time.Date(2010, 11, 20, 11, 35, 23, 0, time.UTC),
			RowCount:      32,
			DistinctCount: 5,
			NullCount:     5,
		},
		{
			TableID:       sqlbase.ID(101),
			StatisticID:   0,
			ColumnIDs:     []sqlbase.ColumnID{0},
			CreatedAt:     time.Date(2017, 11, 20, 11, 35, 23, 0, time.UTC),
			RowCount:      320000,
			DistinctCount: 300000,
			NullCount:     100,
		},
		{
			TableID:       sqlbase.ID(102),
			StatisticID:   34,
			Name:          "table2",
			ColumnIDs:     []sqlbase.ColumnID{1, 2, 3},
			CreatedAt:     time.Date(2001, 1, 10, 5, 25, 14, 0, time.UTC),
			RowCount:      0,
			DistinctCount: 0,
			NullCount:     0,
		},
	}

	// Insert the stats into system.table_statistics
	// and store them in maps for fast retrieval.
	expectedStats := make(map[sqlbase.ID][]*TableStatisticProto)
	for i := range expStatsList {
		stat := &expStatsList[i]

		if err := insertTableStat(ctx, db, ex, stat); err != nil {
			return nil, err
		}

		expectedStats[stat.TableID] = append(expectedStats[stat.TableID], stat)
	}

	// Add another TableID for which we don't have stats.
	expectedStats[sqlbase.ID(103)] = nil

	return expectedStats, nil
}

func TestCacheBasic(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	s, _, db := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)
	ex := s.InternalExecutor().(sqlutil.InternalExecutor)

	expectedStats, err := initTestData(ctx, db, ex)
	if err != nil {
		t.Fatal(err)
	}

	// Collect the tableIDs and sort them so we can iterate over them in a
	// consistent order (Go randomizes the order of iteration over maps).
	var tableIDs sqlbase.IDs
	for tableID := range expectedStats {
		tableIDs = append(tableIDs, tableID)
	}
	sort.Sort(tableIDs)

	// Create a cache and iteratively query the cache for each tableID. This
	// will result in the cache getting populated. When the stats cache size is
	// exceeded, entries should be evicted according to the LRU policy.
	sc := NewTableStatisticsCache(2 /* cacheSize */, s.GossipI().(*gossip.Gossip), db, ex)
	for _, tableID := range tableIDs {
		if err := checkStatsForTable(ctx, sc, expectedStats[tableID], tableID); err != nil {
			t.Fatal(err)
		}
	}

	// Table IDs 0 and 1 should have been evicted since the cache size is 2.
	tableIDs = []sqlbase.ID{sqlbase.ID(100), sqlbase.ID(101)}
	for _, tableID := range tableIDs {
		if statsList, ok := lookupTableStats(ctx, sc, tableID); ok {
			t.Fatalf("lookup of evicted key %d returned: %s", tableID, statsList)
		}
	}

	// Table IDs 2 and 3 should still be in the cache.
	tableIDs = []sqlbase.ID{sqlbase.ID(102), sqlbase.ID(103)}
	for _, tableID := range tableIDs {
		if _, ok := lookupTableStats(ctx, sc, tableID); !ok {
			t.Fatalf("for lookup of key %d, expected stats %s", tableID, expectedStats[tableID])
		}
	}

	// Insert a new stat for Table ID 2.
	tableID := sqlbase.ID(102)
	stat := TableStatisticProto{
		TableID:       tableID,
		StatisticID:   35,
		Name:          "table2",
		ColumnIDs:     []sqlbase.ColumnID{1, 2, 3},
		CreatedAt:     time.Date(2001, 1, 10, 5, 26, 34, 0, time.UTC),
		RowCount:      10,
		DistinctCount: 10,
		NullCount:     0,
	}
	if err := insertTableStat(ctx, db, ex, &stat); err != nil {
		t.Fatal(err)
	}

	// After refreshing, Table ID 2 should be available immediately in the cache
	// for querying, and eventually should contain the updated stat.
	sc.RefreshTableStats(ctx, tableID)
	if _, ok := lookupTableStats(ctx, sc, tableID); !ok {
		t.Fatalf("expected lookup of refreshed key %d to succeed", tableID)
	}
	expected := append([]*TableStatisticProto{&stat}, expectedStats[tableID]...)
	testutils.SucceedsSoon(t, func() error {
		statsList, ok := lookupTableStats(ctx, sc, tableID)
		if !ok {
			return errors.Errorf("expected lookup of refreshed key %d to succeed", tableID)
		}
		if !checkStats(statsList, expected) {
			return errors.Errorf(
				"for lookup of key %d, expected stats %s but found %s", tableID, expected, statsList,
			)
		}
		return nil
	})

	// After invalidation Table ID 2 should be gone.
	sc.InvalidateTableStats(ctx, tableID)
	if statsList, ok := lookupTableStats(ctx, sc, tableID); ok {
		t.Fatalf("lookup of invalidated key %d returned: %s", tableID, statsList)
	}
}

// TestCacheWait verifies that when a table gets invalidated, we only retrieve
// the stats one time, even if there are multiple callers asking for them.
func TestCacheWait(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	s, _, db := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)
	ex := s.InternalExecutor().(sqlutil.InternalExecutor)

	expectedStats, err := initTestData(ctx, db, ex)
	if err != nil {
		t.Fatal(err)
	}

	// Collect the tableIDs and sort them so we can iterate over them in a
	// consistent order (Go randomizes the order of iteration over maps).
	var tableIDs sqlbase.IDs
	for tableID := range expectedStats {
		tableIDs = append(tableIDs, tableID)
	}
	sort.Sort(tableIDs)

	sc := NewTableStatisticsCache(len(tableIDs), s.GossipI().(*gossip.Gossip), db, ex)
	for _, tableID := range tableIDs {
		if err := checkStatsForTable(ctx, sc, expectedStats[tableID], tableID); err != nil {
			t.Fatal(err)
		}
	}

	for run := 0; run < 10; run++ {
		before := sc.mu.numInternalQueries

		id := tableIDs[rand.Intn(len(tableIDs))]
		sc.InvalidateTableStats(ctx, id)
		// Run GetTableStats multiple times in parallel.
		var wg sync.WaitGroup
		for n := 0; n < 10; n++ {
			wg.Add(1)
			go func() {
				stats, err := sc.GetTableStats(ctx, id)
				if err != nil {
					t.Error(err)
				} else if !checkStats(stats, expectedStats[id]) {
					t.Errorf("for table %d, expected stats %s, got %s", id, expectedStats[id], stats)
				}
				wg.Done()
			}()
		}
		wg.Wait()

		if t.Failed() {
			return
		}

		// Verify that we only issued one read from the statistics table.
		if num := sc.mu.numInternalQueries - before; num != 1 {
			t.Fatalf("expected 1 query, got %d", num)
		}
	}
}
