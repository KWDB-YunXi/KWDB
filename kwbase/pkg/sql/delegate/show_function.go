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

package delegate

import (
	"context"
	"fmt"

	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
)

// delegateShowFunction rewrites ShowFunction statement to select statement which returns
// function_name, argument_types... from kwdb_internal.kwdb_functions
func (d *delegator) delegateShowFunction(stmt *tree.ShowFunction) (tree.Statement, error) {
	var query string
	if stmt.ShowAllFunc {
		query = `SELECT function_name from "".kwdb_internal.kwdb_functions `
	} else {
		if stmt.FuncName != "" {
			exist, err := checkFunctionExists(d.ctx, d.evalCtx.InternalExecutor, string(stmt.FuncName))
			if err != nil {
				return nil, err
			}
			if exist {
				const getFunction = `SELECT function_name, argument_types, return_type, function_type, language from "".kwdb_internal.kwdb_functions WHERE function_name = %[1]s`
				query = fmt.Sprintf(
					getFunction,
					fmt.Sprintf("'%s'", stmt.FuncName),
				)
			}
		} else {
			return nil, pgerror.New(pgcode.Syntax, "empty function name is not supported")
		}
	}
	return parse(query)
}

// checkFunctionExists checks if a certain function exists
// input: ctx, InternalExecutor, funcName
// output: exist, error
// the check logic is to query against kwdb_internal.kwdb_functions to find if the given funcName exists
func checkFunctionExists(
	ctx context.Context, ie tree.InternalExecutor, funcName string,
) (bool, error) {
	// checkout if function exist
	const queryFunc = `SELECT function_name from "".kwdb_internal.kwdb_functions WHERE function_name = $1`
	row, err := ie.QueryRow(
		ctx,
		"query functions",
		nil,
		queryFunc,
		funcName,
	)
	if err != nil {
		return false, err
	}
	// if the result is empty, it means there's no such function
	if len(row) == 0 {
		return false, pgerror.Newf(
			pgcode.UndefinedObject, "function %s does not exists "+
				"or current user does not have privilege on this function", funcName)
	}
	return true, nil
}
