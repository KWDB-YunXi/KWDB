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

package opttester

import (
	"fmt"

	"gitee.com/kwbasedb/kwbase/pkg/sql/opt"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/memo"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/props/physical"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/xform"
)

// forcingOptimizer is a wrapper around an Optimizer which adds low-level
// control, like restricting rule application or the expressions that can be
// part of the final expression.
type forcingOptimizer struct {
	o xform.Optimizer

	groups memoGroups

	coster forcingCoster

	// remaining is the number of "unused" steps remaining.
	remaining int

	// lastMatched records the name of the rule that was most recently matched
	// by the optimizer.
	lastMatched opt.RuleName

	// lastApplied records the name of the rule that was most recently applied by
	// the optimizer. This is not necessarily the same with lastMatched because
	// normalization rules can run in-between the match and the application of an
	// exploration rule.
	lastApplied opt.RuleName

	// lastAppliedSource is the expression matched by an exploration rule, or is
	// nil for a normalization rule.
	lastAppliedSource opt.Expr

	// lastAppliedTarget is the new expression constructed by a normalization or
	// exploration rule. For an exploration rule, it can be nil if no expressions
	// were constructed, or can have additional expressions beyond the first that
	// are accessible via NextExpr links.
	lastAppliedTarget opt.Expr
}

// newForcingOptimizer creates a forcing optimizer that stops applying any rules
// after <steps> rules are matched. If ignoreNormRules is true, normalization
// rules don't count against this limit.
func newForcingOptimizer(
	tester *OptTester, steps int, ignoreNormRules bool,
) (*forcingOptimizer, error) {
	fo := &forcingOptimizer{
		remaining:   steps,
		lastMatched: opt.InvalidRuleName,
	}
	fo.o.Init(&tester.evalCtx, tester.catalog)
	fo.coster.Init(&fo.o, &fo.groups)
	fo.o.SetCoster(&fo.coster)

	fo.o.NotifyOnMatchedRule(func(ruleName opt.RuleName) bool {
		if ignoreNormRules && ruleName.IsNormalize() {
			return true
		}
		if fo.remaining == 0 {
			return false
		}
		fo.remaining--
		fo.lastMatched = ruleName
		return true
	})

	// Hook the AppliedRule notification in order to track the portion of the
	// expression tree affected by each transformation rule.
	fo.o.NotifyOnAppliedRule(
		func(ruleName opt.RuleName, source, target opt.Expr) {
			if ignoreNormRules && ruleName.IsNormalize() {
				return
			}
			fo.lastApplied = ruleName
			fo.lastAppliedSource = source
			fo.lastAppliedTarget = target
		},
	)

	fo.o.Memo().NotifyOnNewGroup(func(expr opt.Expr) {
		fo.groups.AddGroup(expr)
	})

	if err := tester.buildExpr(fo.o.Factory()); err != nil {
		return nil, err
	}
	return fo, nil
}

func (fo *forcingOptimizer) Optimize() opt.Expr {
	expr, err := fo.o.Optimize()
	if err != nil {
		// Print the full error (it might contain a stack trace).
		fmt.Printf("%+v\n", err)
		panic(err)
	}
	return expr
}

// LookupPath returns the path of the given node.
func (fo *forcingOptimizer) LookupPath(target opt.Expr) []memoLoc {
	return fo.groups.FindPath(fo.o.Memo().RootExpr(), target)
}

// RestrictToExpr sets up the optimizer to restrict the result to only those
// expression trees which include the given expression path.
func (fo *forcingOptimizer) RestrictToExpr(path []memoLoc) {
	for _, l := range path {
		fo.coster.RestrictGroupToMember(l)
	}
}

// forcingCoster implements the xform.Coster interface so that it can suppress
// expressions in the memo that can't be part of the output tree.
type forcingCoster struct {
	o      *xform.Optimizer
	groups *memoGroups

	inner xform.Coster

	restricted map[groupID]memberOrd
}

func (fc *forcingCoster) Init(o *xform.Optimizer, groups *memoGroups) {
	fc.o = o
	fc.groups = groups
	fc.inner = o.Coster()
}

// RestrictGroupToMember forces the expression in the given location to be the
// best expression for its group.
func (fc *forcingCoster) RestrictGroupToMember(loc memoLoc) {
	if fc.restricted == nil {
		fc.restricted = make(map[groupID]memberOrd)
	}
	fc.restricted[loc.group] = loc.member
}

// ComputeCost is part of the xform.Coster interface.
func (fc *forcingCoster) ComputeCost(e memo.RelExpr, required *physical.Required) memo.Cost {
	if fc.restricted != nil {
		loc := fc.groups.MemoLoc(e)
		if mIdx, ok := fc.restricted[loc.group]; ok && loc.member != mIdx {
			return memo.MaxCost
		}
	}

	return fc.inner.ComputeCost(e, required)
}
