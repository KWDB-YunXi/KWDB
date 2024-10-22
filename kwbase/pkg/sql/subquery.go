// Copyright 2015 The Cockroach Authors.
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

package sql

import (
	"gitee.com/kwbasedb/kwbase/pkg/sql/rowexec"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

// subquery represents a subquery expression in an expression tree
// after it has been converted to a query plan. It is stored in
// planTop.subqueryPlans.
type subquery struct {
	// subquery is used to show the subquery SQL when explaining plans.
	subquery     tree.NodeFormatter
	execMode     rowexec.SubqueryExecMode
	expanded     bool
	started      bool
	plan         planNode
	PlanmaybePhy planMaybePhysical
	result       tree.Datum
}

// EvalSubquery is called by `tree.Eval()` method implementations to
// retrieve the Datum result of a subquery.
func (p *planner) EvalSubquery(expr *tree.Subquery) (result tree.Datum, err error) {
	if expr.Idx == 0 {
		return nil, errors.AssertionFailedf("subquery %q was not processed", expr)
	}
	if expr.Idx < 0 || expr.Idx-1 >= len(p.curPlan.subqueryPlans) {
		return nil, errors.AssertionFailedf("invalid index %d for %q", expr.Idx, expr)
	}

	s := &p.curPlan.subqueryPlans[expr.Idx-1]
	if !s.started {
		return nil, errors.AssertionFailedf("subquery %d (%q) not started prior to evaluation", expr.Idx, expr)
	}
	return s.result, nil
}
