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

package sqlbase

import (
	"fmt"

	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
)

// ResultColumn contains the name and type of a SQL "cell".
type ResultColumn struct {
	Name string
	Typ  *types.T

	// If set, this is an implicit column; used internally.
	Hidden bool

	// TableID/PGAttributeNum identify the source of the column, if it is a simple
	// reference to a column of a base table (or view). If it is not a simple
	// reference, these fields are zeroes.
	TableID        ID       // OID of column's source table (pg_attribute.attrelid).
	PGAttributeNum ColumnID // Column's number in source table (pg_attribute.attnum).
	TypeModifier   int32    // Type-specific data size (pg_attribute.atttypmod).
}

// ResultColumns is the type used throughout the sql module to
// describe the column types of a table.
type ResultColumns []ResultColumn

// ResultColumnsFromColDescs converts ColumnDescriptors to ResultColumns.
func ResultColumnsFromColDescs(tableID ID, colDescs []ColumnDescriptor) ResultColumns {
	cols := make(ResultColumns, 0, len(colDescs))
	for i := range colDescs {
		// Convert the ColumnDescriptor to ResultColumn.
		colDesc := &colDescs[i]
		typ := &colDesc.Type
		if typ == nil {
			panic(fmt.Sprintf("unsupported column type: %s", colDesc.Type.Family()))
		}

		hidden := colDesc.Hidden
		cols = append(
			cols,
			ResultColumn{
				Name:           colDesc.Name,
				Typ:            typ,
				Hidden:         hidden,
				TableID:        tableID,
				PGAttributeNum: colDesc.ID,
				TypeModifier:   typ.TypeModifier(),
			},
		)
	}
	return cols
}

// GetTypeModifier returns the type modifier for this column. If it is not set,
// it defaults to returning -1.
func (r ResultColumn) GetTypeModifier() int32 {
	if r.TypeModifier != 0 {
		return r.TypeModifier
	}

	return -1
}

// TypesEqual returns whether the length and types of r matches other. If
// a type in other is NULL, it is considered equal.
func (r ResultColumns) TypesEqual(other ResultColumns) bool {
	if len(r) != len(other) {
		return false
	}
	for i, c := range r {
		// NULLs are considered equal because some types of queries (SELECT CASE,
		// for example) can change their output types between a type and NULL based
		// on input.
		if other[i].Typ.Family() == types.UnknownFamily {
			continue
		}
		if !c.Typ.Equivalent(other[i].Typ) {
			return false
		}
	}
	return true
}

// NodeFormatter returns a tree.NodeFormatter that, when formatted,
// represents the column at the input column index.
func (r ResultColumns) NodeFormatter(colIdx int) tree.NodeFormatter {
	return &varFormatter{ColumnName: tree.Name(r[colIdx].Name)}
}

// ExplainPlanColumns are the result columns of an EXPLAIN (PLAN) ...
// statement.
var ExplainPlanColumns = ResultColumns{
	// Tree shows the node type with the tree structure.
	{Name: "tree", Typ: types.String},
	// Field is the part of the node that a row of output pertains to.
	{Name: "field", Typ: types.String},
	// Description contains details about the field.
	{Name: "description", Typ: types.String},
}

// ExplainPlanVerboseColumns are the result columns of an
// EXPLAIN (PLAN, ...) ...
// statement when a flag like VERBOSE or TYPES is passed.
var ExplainPlanVerboseColumns = ResultColumns{
	// Tree shows the node type with the tree structure.
	{Name: "tree", Typ: types.String},
	// Level is the depth of the node in the tree. Hidden by default; can be
	// retrieved using:
	//   SELECT level FROM [ EXPLAIN (VERBOSE) ... ].
	{Name: "level", Typ: types.Int, Hidden: true},
	// Type is the node type. Hidden by default.
	{Name: "node_type", Typ: types.String, Hidden: true},
	// Field is the part of the node that a row of output pertains to.
	{Name: "field", Typ: types.String},
	// Description contains details about the field.
	{Name: "description", Typ: types.String},
	// Columns is the type signature of the data source.
	{Name: "columns", Typ: types.String},
	// Ordering indicates the known ordering of the data from this source.
	{Name: "ordering", Typ: types.String},
}

// ExplainDistSQLColumns are the result columns of an
// EXPLAIN (DISTSQL) statement.
var ExplainDistSQLColumns = ResultColumns{
	{Name: "automatic", Typ: types.Bool},
	{Name: "url", Typ: types.String, Hidden: true},
	{Name: "json", Typ: types.String},
}

// ExplainOptColumns are the result columns of an
// EXPLAIN (OPT) statement.
var ExplainOptColumns = ResultColumns{
	{Name: "text", Typ: types.String},
}

// ExplainVecColumns are the result columns of an
// EXPLAIN (VEC) statement.
var ExplainVecColumns = ResultColumns{
	{Name: "text", Typ: types.String},
}

// ExplainAnalyzeDebugColumns are the result columns of an
// EXPLAIN ANALYZE (DEBUG) statement.
var ExplainAnalyzeDebugColumns = ResultColumns{
	{Name: "text", Typ: types.String},
}

// ShowTraceColumns are the result columns of a SHOW [KV] TRACE statement.
var ShowTraceColumns = ResultColumns{
	{Name: "timestamp", Typ: types.TimestampTZ},
	{Name: "age", Typ: types.Interval}, // Note GetTraceAgeColumnIdx below.
	{Name: "message", Typ: types.String},
	{Name: "tag", Typ: types.String},
	{Name: "location", Typ: types.String},
	{Name: "operation", Typ: types.String},
	{Name: "span", Typ: types.Int},
}

// ShowCompactTraceColumns are the result columns of a
// SHOW COMPACT [KV] TRACE statement.
var ShowCompactTraceColumns = ResultColumns{
	{Name: "age", Typ: types.Interval}, // Note GetTraceAgeColumnIdx below.
	{Name: "message", Typ: types.String},
	{Name: "tag", Typ: types.String},
	{Name: "operation", Typ: types.String},
}

// GetTraceAgeColumnIdx retrieves the index of the age column
// depending on whether the compact format is used.
func GetTraceAgeColumnIdx(compact bool) int {
	if compact {
		return 0
	}
	return 1
}

// ShowReplicaTraceColumns are the result columns of a
// SHOW EXPERIMENTAL_REPLICA TRACE statement.
var ShowReplicaTraceColumns = ResultColumns{
	{Name: "timestamp", Typ: types.TimestampTZ},
	{Name: "node_id", Typ: types.Int},
	{Name: "store_id", Typ: types.Int},
	{Name: "replica_id", Typ: types.Int},
}

// ShowSyntaxColumns are the columns of a SHOW SYNTAX statement.
var ShowSyntaxColumns = ResultColumns{
	{Name: "field", Typ: types.String},
	{Name: "message", Typ: types.String},
}

// ShowFingerprintsColumns are the result columns of a
// SHOW EXPERIMENTAL_FINGERPRINTS statement.
var ShowFingerprintsColumns = ResultColumns{
	{Name: "index_name", Typ: types.String},
	{Name: "fingerprint", Typ: types.String},
}

// AlterTableSplitColumns are the result columns of an
// ALTER TABLE/INDEX .. SPLIT AT statement.
var AlterTableSplitColumns = ResultColumns{
	{Name: "key", Typ: types.Bytes},
	{Name: "pretty", Typ: types.String},
	{Name: "split_enforced_until", Typ: types.Timestamp},
}

// AlterTableUnsplitColumns are the result columns of an
// ALTER TABLE/INDEX .. UNSPLIT statement.
var AlterTableUnsplitColumns = ResultColumns{
	{Name: "key", Typ: types.Bytes},
	{Name: "pretty", Typ: types.String},
}

// AlterTableRelocateColumns are the result columns of an
// ALTER TABLE/INDEX .. EXPERIMENTAL_RELOCATE statement.
var AlterTableRelocateColumns = ResultColumns{
	{Name: "key", Typ: types.Bytes},
	{Name: "pretty", Typ: types.String},
}

// AlterTableScatterColumns are the result columns of an
// ALTER TABLE/INDEX .. SCATTER statement.
var AlterTableScatterColumns = ResultColumns{
	{Name: "key", Typ: types.Bytes},
	{Name: "pretty", Typ: types.String},
}

// ScrubColumns are the result columns of a SCRUB statement.
var ScrubColumns = ResultColumns{
	{Name: "job_uuid", Typ: types.Uuid},
	{Name: "error_type", Typ: types.String},
	{Name: "database", Typ: types.String},
	{Name: "table", Typ: types.String},
	{Name: "primary_key", Typ: types.String},
	{Name: "timestamp", Typ: types.Timestamp},
	{Name: "repaired", Typ: types.Bool},
	{Name: "details", Typ: types.Jsonb},
}

// SequenceSelectColumns are the result columns of a sequence data source.
var SequenceSelectColumns = ResultColumns{
	{Name: `last_value`, Typ: types.Int},
	{Name: `log_cnt`, Typ: types.Int},
	{Name: `is_called`, Typ: types.Bool},
}

// ExportColumns are the result columns of an EXPORT statement.
var ExportColumns = ResultColumns{
	{Name: "filename", Typ: types.String},
	{Name: "rows", Typ: types.Int},
	{Name: "node_id", Typ: types.Int},
	{Name: "file_num", Typ: types.Int},
}

// ExportTsColumns are the result columns of an EXPORT TS statement.
var ExportTsColumns = ResultColumns{
	{Name: "result", Typ: types.String},
}

// ImportPortalColumns are the result columns of an IMPORT PORTAL statement.
var ImportPortalColumns = ResultColumns{
	{Name: "all import success, SQL counts", Typ: types.Int},
}

// CreateChildTablesColumns represents the resultColumns of createMultiChildTables.
var CreateChildTablesColumns = ResultColumns{
	{Name: "created", Typ: types.Int},
	{Name: "failed", Typ: types.Int},
	{Name: "skipped", Typ: types.Int},
}
