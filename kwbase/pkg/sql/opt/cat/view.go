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

package cat

import (
	"bytes"

	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/util/treeprinter"
)

// View is an interface to a database view, exposing only the information needed
// by the query optimizer.
type View interface {
	DataSource

	// Query returns the SQL text that specifies the SELECT query that constitutes
	// this view.
	Query() string

	// ColumnNameCount returns the number of column names specified in the view.
	// If zero, then the columns are not aliased. Otherwise, it will match the
	// number of columns in the view.
	ColumnNameCount() int

	// ColumnNames returns the name of the column at the ith ordinal position
	// within the view, where i < ColumnNameCount.
	ColumnName(i int) tree.Name

	// IsSystemView returns true if this view is a system view (like
	// kwdb_internal.ranges).
	IsSystemView() bool
}

// FormatView nicely formats a catalog view using a treeprinter for debugging
// and testing.
func FormatView(view View, tp treeprinter.Node) {
	var buf bytes.Buffer
	if view.ColumnNameCount() > 0 {
		buf.WriteString(" (")
		for i := 0; i < view.ColumnNameCount(); i++ {
			if i != 0 {
				buf.WriteString(", ")
			}
			buf.WriteString(string(view.ColumnName(i)))
		}
		buf.WriteString(")")
	}

	child := tp.Childf("VIEW %s%s", view.Name(), buf.String())

	child.Child(view.Query())
}
