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

package rowcontainer

import (
	"context"
	"fmt"
	"math"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/util/encoding"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/mon"
)

func TestHashDiskBackedRowContainer(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	tempEngine, _, err := storage.NewTempEngine(ctx, storage.DefaultStorageEngine, base.DefaultTestTempStorageConfig(st), base.DefaultTestStoreSpec)
	if err != nil {
		t.Fatal(err)
	}
	defer tempEngine.Close()

	// These monitors are started and stopped by subtests.
	memoryMonitor := mon.MakeMonitor(
		"test-mem",
		mon.MemoryResource,
		nil,           /* curCount */
		nil,           /* maxHist */
		-1,            /* increment */
		math.MaxInt64, /* noteworthy */
		st,
	)
	diskMonitor := mon.MakeMonitor(
		"test-disk",
		mon.DiskResource,
		nil,           /* curCount */
		nil,           /* maxHist */
		-1,            /* increment */
		math.MaxInt64, /* noteworthy */
		st,
	)

	const numRows = 10
	const numCols = 1
	rows := sqlbase.MakeIntRows(numRows, numCols)
	storedEqColumns := columns{0}
	types := sqlbase.OneIntCol
	ordering := sqlbase.ColumnOrdering{{ColIdx: 0, Direction: encoding.Ascending}}

	getRowContainer := func() *HashDiskBackedRowContainer {
		rc := NewHashDiskBackedRowContainer(nil, &evalCtx, &memoryMonitor, &diskMonitor, tempEngine)
		err = rc.Init(
			ctx,
			false, /* shouldMark */
			types,
			storedEqColumns,
			true, /*encodeNull */
		)
		if err != nil {
			t.Fatalf("unexpected error while initializing hashDiskBackedRowContainer: %s", err.Error())
		}
		return rc
	}

	// NormalRun adds rows to a hashDiskBackedRowContainer, makes it spill to
	// disk halfway through, keeps on adding rows, and then verifies that all
	// rows were properly added to the hashDiskBackedRowContainer.
	t.Run("NormalRun", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer diskMonitor.Stop(ctx)
		rc := getRowContainer()
		defer rc.Close(ctx)
		mid := len(rows) / 2
		for i := 0; i < mid; i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		if rc.UsingDisk() {
			t.Fatal("unexpectedly using disk")
		}
		func() {
			// We haven't marked any rows, so the unmarked iterator should iterate
			// over all rows added so far.
			i := rc.NewUnmarkedIterator(ctx)
			defer i.Close()
			if err := verifyRows(ctx, i, rows[:mid], &evalCtx, ordering); err != nil {
				t.Fatalf("verifying memory rows failed with: %s", err)
			}
		}()
		if err := rc.SpillToDisk(ctx); err != nil {
			t.Fatal(err)
		}
		if !rc.UsingDisk() {
			t.Fatal("unexpectedly using memory")
		}
		for i := mid; i < len(rows); i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		func() {
			i := rc.NewUnmarkedIterator(ctx)
			defer i.Close()
			if err := verifyRows(ctx, i, rows, &evalCtx, ordering); err != nil {
				t.Fatalf("verifying disk rows failed with: %s", err)
			}
		}()
	})

	t.Run("AddRowOutOfMem", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(1))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer diskMonitor.Stop(ctx)
		rc := getRowContainer()
		defer rc.Close(ctx)

		if err := rc.AddRow(ctx, rows[0]); err != nil {
			t.Fatal(err)
		}
		if !rc.UsingDisk() {
			t.Fatal("expected to have spilled to disk")
		}
		if diskMonitor.AllocBytes() == 0 {
			t.Fatal("disk monitor reports no disk usage")
		}
		if memoryMonitor.AllocBytes() > 0 {
			t.Fatal("memory monitor reports unexpected usage")
		}
	})

	t.Run("AddRowOutOfDisk", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(1))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(1))
		rc := getRowContainer()
		defer rc.Close(ctx)

		err := rc.AddRow(ctx, rows[0])
		if code := pgerror.GetPGCode(err); code != pgcode.DiskFull {
			t.Fatalf(
				"unexpected error %v, expected disk full error %s", err, pgcode.DiskFull,
			)
		}
		if !rc.UsingDisk() {
			t.Fatal("expected to have tried to spill to disk")
		}
		if diskMonitor.AllocBytes() != 0 {
			t.Fatal("disk monitor reports unexpected usage")
		}
		if memoryMonitor.AllocBytes() != 0 {
			t.Fatal("memory monitor reports unexpected usage")
		}
	})

	// VerifyIteratorRecreation adds all rows to the container, creates a
	// recreatable unmarked iterator, iterates over half of the rows, spills the
	// container to disk, and verifies that the iterator was recreated and points
	// to the appropriate row.
	t.Run("VerifyIteratorRecreation", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer diskMonitor.Stop(ctx)
		rc := getRowContainer()
		defer rc.Close(ctx)

		for i := 0; i < len(rows); i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		if rc.UsingDisk() {
			t.Fatal("unexpectedly using disk")
		}
		i, err := rc.NewAllRowsIterator(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer i.Close()
		counter := 0
		for i.Rewind(); counter < len(rows)/2; i.Next() {
			if ok, err := i.Valid(); err != nil {
				t.Fatal(err)
			} else if !ok {
				break
			}
			row, err := i.Row()
			if err != nil {
				t.Fatal(err)
			}
			if cmp, err := compareRows(
				sqlbase.OneIntCol, row, rows[counter], &evalCtx, &sqlbase.DatumAlloc{}, ordering,
			); err != nil {
				t.Fatal(err)
			} else if cmp != 0 {
				t.Fatal(fmt.Errorf("unexpected row %v, expected %v", row, rows[counter]))
			}
			counter++
		}
		if err := rc.SpillToDisk(ctx); err != nil {
			t.Fatal(err)
		}
		if !rc.UsingDisk() {
			t.Fatal("unexpectedly using memory")
		}
		for ; ; i.Next() {
			if ok, err := i.Valid(); err != nil {
				t.Fatal(err)
			} else if !ok {
				break
			}
			row, err := i.Row()
			if err != nil {
				t.Fatal(err)
			}
			if cmp, err := compareRows(
				sqlbase.OneIntCol, row, rows[counter], &evalCtx, &sqlbase.DatumAlloc{}, ordering,
			); err != nil {
				t.Fatal(err)
			} else if cmp != 0 {
				t.Fatal(fmt.Errorf("unexpected row %v, expected %v", row, rows[counter]))
			}
			counter++
		}
		if counter != len(rows) {
			t.Fatal(fmt.Errorf("iterator returned %d rows but %d were expected", counter, len(rows)))
		}
	})

	// VerifyIteratorRecreationAfterExhaustion adds all rows to the container,
	// creates a recreatable unmarked iterator, iterates over all of the rows,
	// spills the container to disk, and verifies that the iterator was recreated
	// and is not valid.
	t.Run("VerifyIteratorRecreationAfterExhaustion", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer diskMonitor.Stop(ctx)
		rc := getRowContainer()
		defer rc.Close(ctx)

		for i := 0; i < len(rows); i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		if rc.UsingDisk() {
			t.Fatal("unexpectedly using disk")
		}
		i, err := rc.NewAllRowsIterator(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer i.Close()
		counter := 0
		for i.Rewind(); ; i.Next() {
			if ok, err := i.Valid(); err != nil {
				t.Fatal(err)
			} else if !ok {
				break
			}
			row, err := i.Row()
			if err != nil {
				t.Fatal(err)
			}
			if cmp, err := compareRows(
				sqlbase.OneIntCol, row, rows[counter], &evalCtx, &sqlbase.DatumAlloc{}, ordering,
			); err != nil {
				t.Fatal(err)
			} else if cmp != 0 {
				t.Fatal(fmt.Errorf("unexpected row %v, expected %v", row, rows[counter]))
			}
			counter++
		}
		if counter != len(rows) {
			t.Fatal(fmt.Errorf("iterator returned %d rows but %d were expected", counter, len(rows)))
		}
		if err := rc.SpillToDisk(ctx); err != nil {
			t.Fatal(err)
		}
		if !rc.UsingDisk() {
			t.Fatal("unexpectedly using memory")
		}
		if valid, err := i.Valid(); err != nil {
			t.Fatal(err)
		} else if valid {
			t.Fatal("iterator is unexpectedly valid after recreating an exhausted iterator")
		}
	})
}

func TestHashDiskBackedRowContainerPreservesMatchesAndMarks(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	tempEngine, _, err := storage.NewTempEngine(ctx, storage.DefaultStorageEngine, base.DefaultTestTempStorageConfig(st), base.DefaultTestStoreSpec)
	if err != nil {
		t.Fatal(err)
	}
	defer tempEngine.Close()

	// These monitors are started and stopped by subtests.
	memoryMonitor := mon.MakeMonitor(
		"test-mem",
		mon.MemoryResource,
		nil,           /* curCount */
		nil,           /* maxHist */
		-1,            /* increment */
		math.MaxInt64, /* noteworthy */
		st,
	)
	diskMonitor := mon.MakeMonitor(
		"test-disk",
		mon.DiskResource,
		nil,           /* curCount */
		nil,           /* maxHist */
		-1,            /* increment */
		math.MaxInt64, /* noteworthy */
		st,
	)

	const numRowsInBucket = 4
	const numRows = 12
	const numCols = 2
	rows := sqlbase.MakeRepeatedIntRows(numRowsInBucket, numRows, numCols)
	storedEqColumns := columns{0}
	types := []types.T{*types.Int, *types.Int}
	ordering := sqlbase.ColumnOrdering{{ColIdx: 0, Direction: encoding.Ascending}}

	getRowContainer := func() *HashDiskBackedRowContainer {
		rc := NewHashDiskBackedRowContainer(nil, &evalCtx, &memoryMonitor, &diskMonitor, tempEngine)
		err = rc.Init(
			ctx,
			true, /* shouldMark */
			types,
			storedEqColumns,
			true, /*encodeNull */
		)
		if err != nil {
			t.Fatalf("unexpected error while initializing hashDiskBackedRowContainer: %s", err.Error())
		}
		return rc
	}

	// PreservingMatches adds rows from three different buckets to a
	// hashDiskBackedRowContainer, makes it spill to disk, keeps on adding rows
	// from the same buckets, and then verifies that all rows were properly added
	// to the hashDiskBackedRowContainer.
	t.Run("PreservingMatches", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer diskMonitor.Stop(ctx)
		rc := getRowContainer()
		defer rc.Close(ctx)

		mid := len(rows) / 2
		for i := 0; i < mid; i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		if rc.UsingDisk() {
			t.Fatal("unexpectedly using disk")
		}
		func() {
			// We haven't marked any rows, so the unmarked iterator should iterate
			// over all rows added so far.
			i := rc.NewUnmarkedIterator(ctx)
			defer i.Close()
			if err := verifyRows(ctx, i, rows[:mid], &evalCtx, ordering); err != nil {
				t.Fatalf("verifying memory rows failed with: %s", err)
			}
		}()
		if err := rc.SpillToDisk(ctx); err != nil {
			t.Fatal(err)
		}
		if !rc.UsingDisk() {
			t.Fatal("unexpectedly using memory")
		}
		for i := mid; i < len(rows); i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		func() {
			i := rc.NewUnmarkedIterator(ctx)
			defer i.Close()
			if err := verifyRows(ctx, i, rows, &evalCtx, ordering); err != nil {
				t.Fatalf("verifying disk rows failed with: %s", err)
			}
		}()
	})

	// PreservingMarks adds rows from three buckets to a
	// hashDiskBackedRowContainer, marks all rows belonging to the first bucket,
	// spills to disk, and checks that marks are preserved correctly.
	t.Run("PreservingMarks", func(t *testing.T) {
		memoryMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer memoryMonitor.Stop(ctx)
		diskMonitor.Start(ctx, nil, mon.MakeStandaloneBudget(math.MaxInt64))
		defer diskMonitor.Stop(ctx)
		rc := getRowContainer()
		defer rc.Close(ctx)

		for i := 0; i < len(rows); i++ {
			if err := rc.AddRow(ctx, rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		if rc.UsingDisk() {
			t.Fatal("unexpectedly using disk")
		}
		func() {
			// We haven't marked any rows, so the unmarked iterator should iterate
			// over all rows added so far.
			i := rc.NewUnmarkedIterator(ctx)
			defer i.Close()
			if err := verifyRows(ctx, i, rows, &evalCtx, ordering); err != nil {
				t.Fatalf("verifying memory rows failed with: %s", err)
			}
		}()
		if err := rc.ReserveMarkMemoryMaybe(ctx); err != nil {
			t.Fatal(err)
		}
		func() {
			i, err := rc.NewBucketIterator(ctx, rows[0], storedEqColumns)
			if err != nil {
				t.Fatal(err)
			}
			defer i.Close()
			for i.Rewind(); ; i.Next() {
				if ok, err := i.Valid(); err != nil {
					t.Fatal(err)
				} else if !ok {
					break
				}
				if err := i.Mark(ctx, true); err != nil {
					t.Fatal(err)
				}
			}
		}()
		if err := rc.SpillToDisk(ctx); err != nil {
			t.Fatal(err)
		}
		if !rc.UsingDisk() {
			t.Fatal("unexpectedly using memory")
		}
		func() {
			i, err := rc.NewBucketIterator(ctx, rows[0], storedEqColumns)
			if err != nil {
				t.Fatal(err)
			}
			defer i.Close()
			for i.Rewind(); ; i.Next() {
				if ok, err := i.Valid(); err != nil {
					t.Fatal(err)
				} else if !ok {
					break
				}
				if !i.IsMarked(ctx) {
					t.Fatal("Mark is not preserved during spilling to disk")
				}
			}
		}()
	})
}
