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

package sqlbase

import (
	"unsafe"

	"gitee.com/kwbasedb/kwbase/pkg/sql/parser"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"github.com/lib/pq/oid"
)

// PrepareMetadata encapsulates information about a statement that is gathered
// during Prepare and is later used during Describe or Execute.
type PrepareMetadata struct {
	// Note that AST may be nil if the prepared statement is empty.
	parser.Statement

	// AnonymizedStr is the anonymized statement string suitable for recording
	// in statement statistics.
	AnonymizedStr string

	// Provides TypeHints and Types fields which contain placeholder typing
	// information.
	tree.PlaceholderTypesInfo

	// Columns are the types and names of the query output columns.
	Columns ResultColumns

	// InferredTypes represents the inferred types for placeholder, using protocol
	// identifiers. Used for reporting on Describe.
	InferredTypes []oid.Oid
}

// MemoryEstimate returns an estimation (in bytes) of how much memory is used by
// the prepare metadata.
func (pm *PrepareMetadata) MemoryEstimate() int64 {
	res := int64(unsafe.Sizeof(*pm))
	res += int64(len(pm.SQL))
	// We don't have a good way of estimating the size of the AST. Just assume
	// it's a small multiple of the string length.
	res += 2 * int64(len(pm.SQL))

	res += int64(len(pm.AnonymizedStr))

	res += int64(len(pm.TypeHints)+len(pm.Types)) *
		int64(unsafe.Sizeof(tree.PlaceholderIdx(0))+unsafe.Sizeof((*types.T)(nil)))

	res += int64(len(pm.Columns)) * int64(unsafe.Sizeof(ResultColumn{}))
	res += int64(len(pm.InferredTypes)) * int64(unsafe.Sizeof(oid.Oid(0)))

	return res
}
