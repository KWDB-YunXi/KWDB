// Copyright 2018 The Cockroach Authors.
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

package testcat

import "gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"

// CreateView creates a test view from a parsed DDL statement and adds it to the
// catalog.
func (tc *Catalog) CreateView(stmt *tree.CreateView) *View {
	// Update the view name to include catalog and schema if not provided.
	tc.qualifyTableName(&stmt.Name)

	fmtCtx := tree.NewFmtCtx(tree.FmtParsable)
	stmt.AsSource.Format(fmtCtx)

	view := &View{
		ViewID:      tc.nextStableID(),
		ViewName:    stmt.Name,
		QueryText:   fmtCtx.CloseAndGetString(),
		ColumnNames: stmt.ColumnNames,
	}

	// Add the new view to the catalog.
	tc.AddView(view)

	return view
}
