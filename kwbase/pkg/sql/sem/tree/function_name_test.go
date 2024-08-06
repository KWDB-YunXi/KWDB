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

package tree_test

import (
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/sql/parser"
	_ "gitee.com/kwbasedb/kwbase/pkg/sql/sem/builtins"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sessiondata"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
)

func TestResolveFunction(t *testing.T) {
	defer leaktest.AfterTest(t)()
	testCases := []struct {
		in, out string
		err     string
	}{
		{`count`, `count`, ``},
		{`pg_catalog.pg_typeof`, `pg_typeof`, ``},

		{`foo`, ``, `unknown function: foo`},
		{`""`, ``, `invalid function name: ""`},
	}

	searchPath := sessiondata.MakeSearchPath([]string{"pg_catalog"})
	for _, tc := range testCases {
		stmt, err := parser.ParseOne("SELECT " + tc.in + "(1)")
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		f, ok := stmt.AST.(*tree.Select).Select.(*tree.SelectClause).Exprs[0].Expr.(*tree.FuncExpr)
		if !ok {
			t.Fatalf("%s does not parse to a tree.FuncExpr", tc.in)
		}
		q := f.Func
		_, err = q.Resolve(searchPath)
		if tc.err != "" {
			if !testutils.IsError(err, tc.err) {
				t.Fatalf("%s: expected %s, but found %v", tc.in, tc.err, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: expected success, but found %v", tc.in, err)
		}
		if out := q.String(); tc.out != out {
			t.Errorf("%s: expected %s, but found %s", tc.in, tc.out, out)
		}
	}
}
