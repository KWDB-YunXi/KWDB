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

package norm

import (
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/memo"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/props"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/props/physical"
)

// CanSimplifyLimitOffsetOrdering returns true if the ordering required by the
// Limit or Offset operator can be made less restrictive, so that the input
// operator has more ordering choices.
func (c *CustomFuncs) CanSimplifyLimitOffsetOrdering(
	in memo.RelExpr, ordering physical.OrderingChoice,
) bool {
	return c.canSimplifyOrdering(in, ordering)
}

// SimplifyLimitOffsetOrdering makes the ordering required by the Limit or
// Offset operator less restrictive by removing optional columns, adding
// equivalent columns, and removing redundant columns.
func (c *CustomFuncs) SimplifyLimitOffsetOrdering(
	input memo.RelExpr, ordering physical.OrderingChoice,
) physical.OrderingChoice {
	return c.simplifyOrdering(input, ordering)
}

// CanSimplifyGroupingOrdering returns true if the ordering required by the
// grouping operator can be made less restrictive, so that the input operator
// has more ordering choices.
func (c *CustomFuncs) CanSimplifyGroupingOrdering(
	in memo.RelExpr, private *memo.GroupingPrivate,
) bool {
	if private.TimeBucketGapFillColId > 0 {
		return false
	}
	return c.canSimplifyOrdering(in, private.Ordering)
}

// SimplifyGroupingOrdering makes the ordering required by the grouping operator
// less restrictive by removing optional columns, adding equivalent columns, and
// removing redundant columns.
func (c *CustomFuncs) SimplifyGroupingOrdering(
	in memo.RelExpr, private *memo.GroupingPrivate,
) *memo.GroupingPrivate {
	// Copy GroupingPrivate to stack and replace Ordering field.
	copy := *private
	copy.Ordering = c.simplifyOrdering(in, private.Ordering)
	return &copy
}

// CanSimplifyOrdinalityOrdering returns true if the ordering required by the
// Ordinality operator can be made less restrictive, so that the input operator
// has more ordering choices.
func (c *CustomFuncs) CanSimplifyOrdinalityOrdering(
	in memo.RelExpr, private *memo.OrdinalityPrivate,
) bool {
	return c.canSimplifyOrdering(in, private.Ordering)
}

// SimplifyOrdinalityOrdering makes the ordering required by the Ordinality
// operator less restrictive by removing optional columns, adding equivalent
// columns, and removing redundant columns.
func (c *CustomFuncs) SimplifyOrdinalityOrdering(
	in memo.RelExpr, private *memo.OrdinalityPrivate,
) *memo.OrdinalityPrivate {
	// Copy OrdinalityPrivate to stack and replace Ordering field.
	copy := *private
	copy.Ordering = c.simplifyOrdering(in, private.Ordering)
	return &copy
}

// withinPartitionFuncDeps returns the functional dependencies that apply
// within any given partition in a window function's input. These are stronger
// than the input's FDs since within any partition the partition columns are
// held constant.
func (c *CustomFuncs) withinPartitionFuncDeps(
	in memo.RelExpr, private *memo.WindowPrivate,
) *props.FuncDepSet {
	if private.Partition.Empty() {
		return &in.Relational().FuncDeps
	}
	var fdset props.FuncDepSet
	fdset.CopyFrom(&in.Relational().FuncDeps)
	fdset.AddConstants(private.Partition)
	return &fdset
}

// CanSimplifyWindowOrdering is true if the intra-partition ordering used by
// the window function can be made less restrictive.
func (c *CustomFuncs) CanSimplifyWindowOrdering(in memo.RelExpr, private *memo.WindowPrivate) bool {
	// If any ordering is allowed, nothing to simplify.
	if private.Ordering.Any() {
		return false
	}
	deps := c.withinPartitionFuncDeps(in, private)

	return private.Ordering.CanSimplify(deps)
}

// SimplifyWindowOrdering makes the intra-partition ordering used by the window
// function less restrictive.
func (c *CustomFuncs) SimplifyWindowOrdering(
	in memo.RelExpr, private *memo.WindowPrivate,
) *memo.WindowPrivate {
	simplified := private.Ordering.Copy()
	simplified.Simplify(c.withinPartitionFuncDeps(in, private))
	cpy := *private
	cpy.Ordering = simplified
	return &cpy
}

// CanSimplifyExplainOrdering returns true if the ordering required by the
// Explain operator can be made less restrictive, so that the input operator
// has more ordering choices.
func (c *CustomFuncs) CanSimplifyExplainOrdering(
	in memo.RelExpr, private *memo.ExplainPrivate,
) bool {
	return c.canSimplifyOrdering(in, private.Props.Ordering)
}

// SimplifyExplainOrdering makes the ordering required by the Explain operator
// less restrictive by removing optional columns, adding equivalent columns, and
// removing redundant columns.
func (c *CustomFuncs) SimplifyExplainOrdering(
	in memo.RelExpr, private *memo.ExplainPrivate,
) *memo.ExplainPrivate {
	// Copy ExplainPrivate and its physical properties to stack and replace
	// Ordering field in the copied properties.
	copy := *private
	copyProps := *private.Props
	copyProps.Ordering = c.simplifyOrdering(in, private.Props.Ordering)
	copy.Props = &copyProps
	return &copy
}

func (c *CustomFuncs) canSimplifyOrdering(in memo.RelExpr, ordering physical.OrderingChoice) bool {
	// If any ordering is allowed, nothing to simplify.
	if ordering.Any() {
		return false
	}
	return ordering.CanSimplify(&in.Relational().FuncDeps)
}

func (c *CustomFuncs) simplifyOrdering(
	in memo.RelExpr, ordering physical.OrderingChoice,
) physical.OrderingChoice {
	simplified := ordering.Copy()
	simplified.Simplify(&in.Relational().FuncDeps)
	return simplified
}
