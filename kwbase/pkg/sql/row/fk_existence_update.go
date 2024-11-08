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

package row

import (
	"context"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
)

// fkExistenceCheckForUpdate is an auxiliary object with two purposes:
//
// - its main purpose is to facilitate the existence checks on both
//   referencing and referenced tables when modifying rows in a table.
//
//   Note that users of this purpose are responsible for calling
//   addCheckForIndex() on all mutated indexes, to register a mutated
//   index for FK checking.
//
//   TODO(knz): why cannot the fkExistenceCheckForUpdate make this determination
//   itself, like the other helpers? The asymmetry is concerning.
//
// - its secondary purpose is to serve the boolean "hasFk()" for "does
//   the mutated table have any FK constraints, either forward or
//   backward?"  This boolean is used by the row writer and the
//   CASCADEing close.
//
//   TODO(knz): this responsibility should be carried by another
//   object, so that the helper can specialize to only do existence
//   checks!
//
type fkExistenceCheckForUpdate struct {
	// inbound is responsible for existence checks in referencing tables.
	inbound fkExistenceCheckForDelete
	// output is responsible for existence checks in referenced tables.
	outbound fkExistenceCheckForInsert

	// indexIDsToCheck determines the list of indexes in the mutated
	// table for which to perform FK checks.
	//
	// This may be a subset of all constraints on the mutated table.
	// The inbound and outbound checkers look at all constraints by default;
	// the update helper needs to maintain its own list of index IDs
	// to operate on only a subset, and also define its own addIndexChecks()
	// logic instead of deferring to addAllIdxChecks().
	indexIDsToCheck map[sqlbase.IndexID]struct{}

	// checker is the object that actually carries out the lookups in
	// KV.
	checker *fkExistenceBatchChecker
}

// makeFkExistenceCheckHelperForUpdate instantiates an update helper.
func makeFkExistenceCheckHelperForUpdate(
	ctx context.Context,
	txn *kv.Txn,
	table *sqlbase.ImmutableTableDescriptor,
	otherTables FkTableMetadata,
	updateCols []sqlbase.ColumnDescriptor,
	colMap map[sqlbase.ColumnID]int,
	alloc *sqlbase.DatumAlloc,
) (fkExistenceCheckForUpdate, error) {
	ret := fkExistenceCheckForUpdate{
		indexIDsToCheck: map[sqlbase.IndexID]struct{}{0: {}},
	}

	// Instantiate a helper for the referencing tables.
	var err error
	if ret.inbound, err = makeFkExistenceCheckHelperForDelete(ctx, txn, table, otherTables, colMap,
		alloc); err != nil {
		return ret, err
	}

	// Instantiate a helper for the referenced table(s).
	ret.outbound, err = makeFkExistenceCheckHelperForInsert(ctx, txn, table, otherTables, colMap, alloc)
	ret.outbound.checker = ret.inbound.checker

	// We need *some* KV batch checker to perform the checks. It doesn't
	// matter which; so we use the one instantiated by the inbound
	// checker and simply disregard/ignore the one instantiated by the
	// outbound checker.
	ret.checker = ret.inbound.checker

	return ret, err
}

// addCheckForIndex registers a mutated index to perform FK existence checks for.
func (fks fkExistenceCheckForUpdate) addCheckForIndex(
	indexID sqlbase.IndexID, descriptorType sqlbase.IndexDescriptor_Type,
) {
	if fks.checker == nil {
		return
	}
	if descriptorType == sqlbase.IndexDescriptor_FORWARD {
		// We ignore FK existence checks for inverted indexes.
		//
		// TODO(knz): verify that this is indeed correct.
		fks.indexIDsToCheck[indexID] = struct{}{}
	}
}

// hasFKs determines whether the table being mutated has any forward
// or backward FK constraints. This is the secondary purpose of the helper
// and is unrelated to the task of FK existence checks.
func (fks fkExistenceCheckForUpdate) hasFKs() bool {
	return len(fks.inbound.fks) > 0 || len(fks.outbound.fks) > 0
}

// addAllIdxChecks queues a FK existence check for the backward and forward
// constraints for the indexes
func (fks fkExistenceCheckForUpdate) addIndexChecks(
	ctx context.Context, oldValues, newValues tree.Datums, traceKV bool,
) error {
	for indexID := range fks.indexIDsToCheck {
		if err := queueFkExistenceChecksForRow(ctx, fks.checker, fks.inbound.fks[indexID], oldValues, traceKV); err != nil {
			return err
		}
		if err := queueFkExistenceChecksForRow(ctx, fks.checker, fks.outbound.fks[indexID], newValues, traceKV); err != nil {
			return err
		}
	}
	return nil
}
