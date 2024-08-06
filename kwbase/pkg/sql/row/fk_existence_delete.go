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
	"github.com/cockroachdb/errors"
)

// fkExistenceCheckForDelete is an auxiliary object that facilitates the
// existence checks on the referencing table when deleting rows in a
// referenced table.
type fkExistenceCheckForDelete struct {
	// fks maps mutated index id to slice of fkExistenceCheckBaseHelper, which
	// performs FK existence checks in referencing tables.
	fks map[sqlbase.IndexID][]fkExistenceCheckBaseHelper

	// checker is the object that actually carries out the lookups in
	// KV.
	checker *fkExistenceBatchChecker
}

// makeFkExistenceCheckHelperForDelete instantiates a delete helper.
func makeFkExistenceCheckHelperForDelete(
	ctx context.Context,
	txn *kv.Txn,
	table *sqlbase.ImmutableTableDescriptor,
	otherTables FkTableMetadata,
	colMap map[sqlbase.ColumnID]int,
	alloc *sqlbase.DatumAlloc,
) (fkExistenceCheckForDelete, error) {
	h := fkExistenceCheckForDelete{
		checker: &fkExistenceBatchChecker{
			txn: txn,
		},
	}

	// We need an existence check helper for every referencing
	// table.
	for i := range table.InboundFKs {
		ref := &table.InboundFKs[i]
		originTable := otherTables[ref.OriginTableID]
		if originTable.IsAdding {
			// We can assume that a table being added but not yet public is empty,
			// and thus does not need to be checked for FK violations.
			continue
		}
		// TODO(jordan,radu): this is busted, rip out when HP is removed.
		// Fake a forward foreign key constraint. The HP requires an index on the
		// reverse table, which won't be required by the CBO. So in HP, fail if we
		// don't have this precondition.
		// This will never be on an actual table descriptor, so we don't need to
		// populate all the legacy index fields.
		fakeRef := &sqlbase.ForeignKeyConstraint{
			ReferencedTableID:   ref.OriginTableID,
			ReferencedColumnIDs: ref.OriginColumnIDs,
			OriginTableID:       ref.ReferencedTableID,
			OriginColumnIDs:     ref.ReferencedColumnIDs,
			// N.B.: Back-references always must have SIMPLE match method, because ... TODO(jordan): !!!
			Match:    sqlbase.ForeignKeyReference_SIMPLE,
			OnDelete: ref.OnDelete,
			OnUpdate: ref.OnUpdate,
		}
		searchIdx, err := sqlbase.FindFKOriginIndex(originTable.Desc.TableDesc(), ref.OriginColumnIDs)
		if err != nil {
			return fkExistenceCheckForDelete{}, errors.NewAssertionErrorWithWrappedErrf(
				err, "failed to find a suitable index on table %d for deletion", ref.OriginTableID)
		}
		mutatedIdx, err := sqlbase.FindFKReferencedIndex(table.TableDesc(), ref.ReferencedColumnIDs)
		if err != nil {
			return fkExistenceCheckForDelete{}, errors.NewAssertionErrorWithWrappedErrf(
				err, "failed to find a suitable index on table %d for deletion", ref.ReferencedTableID)
		}
		fk, err := makeFkExistenceCheckBaseHelper(txn, otherTables, fakeRef, searchIdx, mutatedIdx, colMap, alloc,
			CheckDeletes)
		if err == errSkipUnusedFK {
			continue
		}
		if err != nil {
			return fkExistenceCheckForDelete{}, err
		}
		if h.fks == nil {
			h.fks = make(map[sqlbase.IndexID][]fkExistenceCheckBaseHelper)
		}
		h.fks[mutatedIdx.ID] = append(h.fks[mutatedIdx.ID], fk)
	}

	if len(h.fks) > 0 {
		// TODO(knz,radu): FK existence checks need to see the writes
		// performed by the mutation.
		//
		// In order to make this true, we need to split the existence
		// checks into a separate sequencing step, and have the first
		// check happen no early than the end of all the "main" part of
		// the statement. Unfortunately, the organization of the code does
		// not allow this today.
		//
		// See: https://gitee.com/kwbasedb/kwbase/issues/33475
		//
		// In order to "make do" and preserve a modicum of FK semantics we
		// thus need to disable step-wise execution here. The result is that
		// it will also enable any interleaved read part to observe the
		// mutation, and thus introduce the risk of a Halloween problem for
		// any mutation that uses FK relationships.
		_ = txn.ConfigureStepping(ctx, kv.SteppingDisabled)
	}

	return h, nil
}

// addAllIdxChecks queues a FK existence check for every referencing table.
func (h fkExistenceCheckForDelete) addAllIdxChecks(
	ctx context.Context, row tree.Datums, traceKV bool,
) error {
	for idx := range h.fks {
		if err := queueFkExistenceChecksForRow(ctx, h.checker, h.fks[idx], row, traceKV); err != nil {
			return err
		}
	}
	return nil
}
