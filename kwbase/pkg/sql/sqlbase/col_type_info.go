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

import "gitee.com/kwbasedb/kwbase/pkg/sql/types"

// ColTypeInfo is a type that allows multiple representations of column type
// information (to avoid conversions and allocations).
type ColTypeInfo struct {
	// Only one of these fields can be set.
	resCols  ResultColumns
	colTypes []types.T
}

// ColTypeInfoFromResCols creates a ColTypeInfo from ResultColumns.
func ColTypeInfoFromResCols(resCols ResultColumns) ColTypeInfo {
	return ColTypeInfo{resCols: resCols}
}

// ColTypeInfoFromColTypes creates a ColTypeInfo from []ColumnType.
func ColTypeInfoFromColTypes(colTypes []types.T) ColTypeInfo {
	return ColTypeInfo{colTypes: colTypes}
}

// ColTypeInfoFromColDescs creates a ColTypeInfo from []ColumnDescriptor.
func ColTypeInfoFromColDescs(colDescs []ColumnDescriptor) ColTypeInfo {
	colTypes := make([]types.T, len(colDescs))
	for i, colDesc := range colDescs {
		colTypes[i] = colDesc.Type
	}
	return ColTypeInfoFromColTypes(colTypes)
}

// NumColumns returns the number of columns in the type.
func (ti ColTypeInfo) NumColumns() int {
	if ti.resCols != nil {
		return len(ti.resCols)
	}
	return len(ti.colTypes)
}

// Type returns the datum type of the i-th column.
func (ti ColTypeInfo) Type(idx int) *types.T {
	if ti.resCols != nil {
		return ti.resCols[idx].Typ
	}
	return &ti.colTypes[idx]
}

// MakeColTypeInfo returns a ColTypeInfo initialized from the given
// TableDescriptor and map from column ID to row index.
func MakeColTypeInfo(
	tableDesc *ImmutableTableDescriptor, colIDToRowIndex map[ColumnID]int,
) (ColTypeInfo, error) {
	colTypeInfo := ColTypeInfo{
		colTypes: make([]types.T, len(colIDToRowIndex)),
	}
	for colID, rowIndex := range colIDToRowIndex {
		col, err := tableDesc.FindColumnByID(colID)
		if err != nil {
			return ColTypeInfo{}, err
		}
		colTypeInfo.colTypes[rowIndex] = col.Type
	}
	return colTypeInfo, nil
}
