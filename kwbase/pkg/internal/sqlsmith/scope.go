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

package sqlsmith

import (
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
)

// colRef refers to a named result column. If it is from a table, def is
// populated.
type colRef struct {
	typ  *types.T
	item *tree.ColumnItem
}

func (c *colRef) typedExpr() tree.TypedExpr {
	return makeTypedExpr(c.item, c.typ)
}

type colRefs []*colRef

func (t colRefs) extend(refs ...*colRef) colRefs {
	ret := append(make(colRefs, 0, len(t)+len(refs)), t...)
	ret = append(ret, refs...)
	return ret
}

func (t colRefs) stripTableName() {
	for _, c := range t {
		c.item.TableName = nil
	}
}

// canRecurse returns whether the current function should possibly invoke
// a function that creates new nodes.
func (s *Smither) canRecurse() bool {
	return s.complexity > s.rnd.Float64()
}

// Context holds information about what kinds of expressions are legal at
// a particular place in a query.
type Context struct {
	fnClass  tree.FunctionClass
	noWindow bool
}

var (
	emptyCtx   = Context{}
	groupByCtx = Context{fnClass: tree.AggregateClass}
	havingCtx  = Context{
		fnClass:  tree.AggregateClass,
		noWindow: true,
	}
	windowCtx = Context{fnClass: tree.WindowClass}
)
