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

package memo

import (
	"context"
	"math"
	"runtime"
	"sort"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/cat"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/props"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/props/physical"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sessiondata"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/stats"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/shirou/gopsutil/mem"
)

// Memo is a data structure for efficiently storing a forest of query plans.
// Conceptually, the memo is composed of a numbered set of equivalency classes
// called groups where each group contains a set of logically equivalent
// expressions. Two expressions are considered logically equivalent if:
//
//  1. They return the same number and data type of columns. However, order and
//     naming of columns doesn't matter.
//  2. They return the same number of rows, with the same values in each row.
//     However, order of rows doesn't matter.
//
// The different expressions in a single group are called memo expressions
// (memo-ized expressions). The children of a memo expression can themselves be
// part of memo groups. Therefore, the memo forest is composed of every possible
// combination of parent expression with its possible child expressions,
// recursively applied.
//
// Memo expressions can be relational (e.g. join) or scalar (e.g. <). Operators
// are always both logical (specify results) and physical (specify results and
// a particular implementation). This means that even a "raw" unoptimized
// expression tree can be executed (naively). Both relational and scalar
// operators are uniformly represented as nodes in memo expression trees, which
// facilitates tree pattern matching and replacement. However, because scalar
// expression memo groups never have more than one expression, scalar
// expressions can use a simpler representation.
//
// Because memo groups contain logically equivalent expressions, all the memo
// expressions in a group share the same logical properties. However, it's
// possible for two logically equivalent expression to be placed in different
// memo groups. This occurs because determining logical equivalency of two
// relational expressions is too complex to perform 100% correctly. A
// correctness failure (i.e. considering two expressions logically equivalent
// when they are not) results in invalid transformations and invalid plans.
// But placing two logically equivalent expressions in different groups has a
// much gentler failure mode: the memo and transformations are less efficient.
// Expressions within the memo may have different physical properties. For
// example, a memo group might contain both hash join and merge join
// expressions which produce the same set of output rows, but produce them in
// different orders.
//
// Expressions are inserted into the memo by the factory, which ensure that
// expressions have been fully normalized before insertion (see the comment in
// factory.go for more details). A new group is created only when unique
// normalized expressions are created by the factory during construction or
// rewrite of the tree. Uniqueness is determined by "interning" each expression,
// which means that multiple equivalent expressions are mapped to a single
// in-memory instance. This allows interned expressions to be checked for
// equivalence by simple pointer comparison. For example:
//
//	SELECT * FROM a, b WHERE a.x = b.x
//
// After insertion into the memo, the memo would contain these six groups, with
// numbers substituted for pointers to the normalized expression in each group:
//
//	G6: [inner-join [G1 G2 G5]]
//	G5: [eq [G3 G4]]
//	G4: [variable b.x]
//	G3: [variable a.x]
//	G2: [scan b]
//	G1: [scan a]
//
// Each leaf expressions is interned by hashing its operator type and any
// private field values. Expressions higher in the tree can then rely on the
// fact that all children have been interned, and include their pointer values
// in its hash value. Therefore, the memo need only hash the expression's fields
// in order to determine whether the expression already exists in the memo.
// Walking the subtree is not necessary.
//
// The normalizing factory will never add more than one expression to a memo
// group. But the explorer does add denormalized expressions to existing memo
// groups, since oftentimes one of these equivalent, but denormalized
// expressions will have a lower cost than the initial normalized expression
// added by the factory. For example, the join commutativity transformation
// expands the memo like this:
//
//	G6: [inner-join [G1 G2 G5]] [inner-join [G2 G1 G5]]
//	G5: [eq [G3 G4]]
//	G4: [variable b.x]
//	G3: [variable a.x]
//	G2: [scan b]
//	G1: [scan a]
//
// See the comments in explorer.go for more details.
type Memo struct {
	// metadata provides information about the columns and tables used in this
	// particular query.
	metadata opt.Metadata

	// interner interns all expressions in the memo, ensuring that there is at
	// most one instance of each expression in the memo.
	interner interner

	// logPropsBuilder is inlined in the memo so that it can be reused each time
	// scalar or relational properties need to be built.
	logPropsBuilder logicalPropsBuilder

	// rootExpr is the root expression of the memo expression forest. It is set
	// via a call to SetRoot. After optimization, it is set to be the root of the
	// lowest cost tree in the forest.
	rootExpr opt.Expr

	// rootProps are the physical properties required of the root memo expression.
	// It is set via a call to SetRoot.
	rootProps *physical.Required

	// memEstimate is the approximate memory usage of the memo, in bytes.
	memEstimate int64

	// The following are selected fields from SessionData which can affect
	// planning. We need to cross-check these before reusing a cached memo.
	dataConversion              sessiondata.DataConversionConfig
	reorderJoinsLimit           int
	multiModelReorderJoinsLimit int
	MultiModelEnabled           bool
	zigzagJoinEnabled           bool
	optimizerFKs                bool
	safeUpdates                 bool
	saveTablesPrefix            string
	insertFastPath              bool

	// The following are selected fields from global data which can affect
	// planning. We need to cross-check these before reusing a cached memo.
	tsCanPushAllProcessor      bool
	tsForcePushGroupToTSEngine bool
	tsOrderedScan              bool
	tsQueryOptMode             int64

	// curID is the highest currently in-use scalar expression ID.
	curID opt.ScalarID

	// curWithID is the highest currently in-use WITH ID.
	curWithID opt.WithID

	newGroupFn func(opt.Expr)

	// WARNING: if you add more members, add initialization code in Init.

	// colsUsage is used to store the access pattern for all columns accessed by a statement
	ColsUsage []opt.ColumnUsage

	// CheckHelper used to check if the expr can execute in ts engine.
	CheckHelper TSCheckHelper

	// ts engine white list map
	TSWhiteListMap *sqlbase.WhiteListMap

	// tsDop represents degree of parallelism control parallelism in time series engine
	tsDop uint32

	// QueryType represents the type of a query for multiple model processing.
	QueryType QueryTypeEnum

	// MultimodelHelper helps assist in setting configurations for multiple model processing.
	MultimodelHelper MultimodelHelper
}

// QueryTypeEnum represents the type of a query, whether it is a multi-model query
// or not.
// for multiple model processing.
type QueryTypeEnum int

const (
	// Unset represents a state where the query type is not set.
	Unset QueryTypeEnum = iota
	// MultiModel represents a query involving both time-series and relational data.
	MultiModel
	// TSOnly represents a query that involves only time-series data.
	TSOnly
	// RelOnly represents a query that involves only relational data.
	RelOnly
)

// TSCheckHelper ts check helper, helper check flags, push column, white list and so on
type TSCheckHelper struct {
	// flags record some flags used in TS query.
	flags int

	// ts white list map
	whiteList *sqlbase.WhiteListMap

	// PushHelper check expr can be pushed to ts engine
	PushHelper PushHelper

	// function to check if we run in multi node mode
	checkMultiNode CheckMultiNode

	// ctx, check multi node function param, context
	ctx context.Context

	// GroupHint is the hint that control the group by cannot be executed concurrently
	// or must be executed in the relationship engine
	// HintType: ForceNoSynchronizerGroup and ForceRelationalGroup
	GroupHint keys.GroupHintType

	// scan ordered cols
	orderedCols opt.ColSet

	// scan ordered type
	orderedScanType opt.OrderedTableType

	// only have one primary tag value
	onlyOnePTagValue bool
}

// init inits the TSCheckHelper
func (m *TSCheckHelper) init() {
	m.flags = 0
	m.PushHelper.MetaMap = make(MetaInfoMap, 0)
	m.GroupHint = keys.NoGroupHint
}

// isSingleNode check if the query is executed in single node.
func (m *TSCheckHelper) isSingleNode() bool {
	if m.checkMultiNode != nil {
		return !m.checkMultiNode(m.ctx)
	}
	return false
}

// MultimodelHelper is a helper struct designed to assist in setting
// configurations for multiple model processing.
type MultimodelHelper struct {
	TableGroup           [][][]opt.TableID
	PreGroupedAggregates AggregationStrategy
	HasBljNode           bool
	JoinCols             opt.ColList
	HashTagScan          bool
	OriginalAccessMode   execinfrapb.TSTableReadMode
	ResetReasons         map[MultiModelResetReason]struct{}
	HasLastAgg           bool
}

// init Initializes values for MultimodelHelper
func (m *MultimodelHelper) init() {
	m.PreGroupedAggregates = OutsideIn
	m.TableGroup = nil
	m.HasBljNode = false
	m.JoinCols = nil
	m.HashTagScan = false
	m.OriginalAccessMode = -1
	m.ResetReasons = make(map[MultiModelResetReason]struct{})
	m.HasLastAgg = false
}

// AggregationStrategy defines the strategy for data aggregation in queries.
// for multiple model processing.
type AggregationStrategy int

const (
	// OutsideIn represents a strategy where relational data is processed first,
	// followed by time-series data.
	OutsideIn AggregationStrategy = iota // 0

	// InsideOut represents a strategy that starts with time-series data,
	// and then relational data is processed.
	InsideOut // 1

	// Hybrid represents a combination of both OutsideIn and InsideOut strategies.
	Hybrid // 2
)

// String converts AggregationStrategy to string
// for multiple model processing.
func (a AggregationStrategy) String() string {
	switch a {
	case OutsideIn:
		return "outside-in"
	case InsideOut:
		return "inside-out"
	case Hybrid:
		return "hybrid"
	default:
		return "unknown"
	}
}

// MultiModelResetReason defines reasons for resetting the multi-model flag,
// indicating scenarios where multi-model processing is not supported.
// for multiple model processing.
type MultiModelResetReason int

const (
	// UnsupportedAggFuncOrExpr indicates the reset reason is due to the use of an aggregation function
	// or expression that is not supported in multi-model contexts.
	UnsupportedAggFuncOrExpr MultiModelResetReason = iota // 0

	// UnsupportedDataType indicates the reset reason is due to encountering a data type
	// in the query that is not supported in a multi-model context.
	UnsupportedDataType // 1

	// JoinBetweenTimeSeriesTables indicates the reset reason is a join operation
	// between two time-series tables, which is not supported in multi-model contexts.
	JoinBetweenTimeSeriesTables // 2

	// UnsupportedCrossJoin indicates the reset reason is due to a cross join operation,
	// which is not supported in multi-model contexts.
	UnsupportedCrossJoin // 3

	// LeftJoinColsPositionMismatch indicates the reset reason is due to the inability
	// to match left join columns with their positions in the relational information,
	// which is necessary for processing in multi-model contexts.
	LeftJoinColsPositionMismatch // 4

	// UnsupportedCastOnTagColumn indicates the reset reason is due to a cast operation
	// on a tag column, which is not supported in multi-model contexts.
	UnsupportedCastOnTagColumn // 5

	// JoinColsTypeOrLengthMismatch indicates the reset reason is due to a mismatch
	// in the type or length of join columns, which is necessary for processing in multi-model contexts.
	JoinColsTypeOrLengthMismatch // 6
)

// String converts MultiModelResetReason to string
// for multiple model processing.
func (r MultiModelResetReason) String() string {
	switch r {
	case UnsupportedAggFuncOrExpr:
		return "unsupported aggregation function or expression"
	case UnsupportedDataType:
		return "unsupported data type"
	case JoinBetweenTimeSeriesTables:
		return "join between time-series tables"
	case UnsupportedCrossJoin:
		return "cross join is not supported in multi-model"
	case LeftJoinColsPositionMismatch:
		return "mismatch in left join columns' positions with relationalInfo"
	case UnsupportedCastOnTagColumn:
		return "cast on tag column is not supported in multi-model"
	case JoinColsTypeOrLengthMismatch:
		return "mismatch in join columns' type or length"
	default:
		return "unknown"
	}
}

// CheckMultiNode check multi node function
type CheckMultiNode func(ctx context.Context) bool

// Init initializes a new empty memo instance, or resets existing state so it
// can be reused. It must be called before use (or reuse). The memo collects
// information about the context in which it is compiled from the evalContext
// argument. If any of that changes, then the memo must be invalidated (see the
// IsStale method for more details).
func (m *Memo) Init(evalCtx *tree.EvalContext) {
	m.metadata.Init()
	m.interner.Clear()
	m.logPropsBuilder.init(evalCtx, m)

	m.rootExpr = nil
	m.rootProps = nil
	m.memEstimate = 0

	m.dataConversion = evalCtx.SessionData.DataConversion
	m.reorderJoinsLimit = evalCtx.SessionData.ReorderJoinsLimit
	m.multiModelReorderJoinsLimit = evalCtx.SessionData.MultiModelReorderJoinsLimit
	m.MultiModelEnabled = evalCtx.SessionData.MultiModelEnabled
	m.zigzagJoinEnabled = evalCtx.SessionData.ZigzagJoinEnabled
	m.optimizerFKs = evalCtx.SessionData.OptimizerFKs
	m.safeUpdates = evalCtx.SessionData.SafeUpdates
	m.saveTablesPrefix = evalCtx.SessionData.SaveTablesPrefix
	m.insertFastPath = evalCtx.SessionData.InsertFastPath

	if evalCtx.Settings != nil {
		m.tsOrderedScan = opt.TSOrderedTable.Get(&evalCtx.Settings.SV)
		m.tsCanPushAllProcessor = opt.PushdownAll.Get(&evalCtx.Settings.SV)
		m.tsForcePushGroupToTSEngine = !stats.AutomaticTsStatisticsClusterMode.Get(&evalCtx.Settings.SV)
		m.tsQueryOptMode = opt.TSQueryOptMode.Get(&evalCtx.Settings.SV)
	} else {
		m.tsCanPushAllProcessor = true
		m.tsForcePushGroupToTSEngine = true
		m.tsQueryOptMode = opt.DefaultQueryOptMode
	}

	m.curID = 0
	m.curWithID = 0
	m.ColsUsage = nil
	m.CheckHelper.init()

	m.tsDop = 0
	m.QueryType = Unset
	m.MultimodelHelper.init()
}

// InitCheckHelper init some members of CheckHelper of memo.
// WhiteListMap is a whitelist map for check expr can exec in ts engine.
// CheckMultiNode is the function to check if we run in multi node mode.
func (m *Memo) InitCheckHelper(param interface{}) {
	switch t := param.(type) {
	case *sqlbase.WhiteListMap:
		m.CheckHelper.whiteList = t
	case CheckMultiNode:
		m.CheckHelper.checkMultiNode = t
	case context.Context:
		m.CheckHelper.ctx = t
	case keys.GroupHintType:
		m.CheckHelper.GroupHint = t
	}
}

// GetWhiteList return the white list from memo.
func (m *Memo) GetWhiteList() *sqlbase.WhiteListMap {
	return m.CheckHelper.whiteList
}

// SetWhiteList set white list
func (m *Memo) SetWhiteList(src *sqlbase.WhiteListMap) {
	m.CheckHelper.whiteList = src
}

// EnableOrderedScan check ordered scan is enable
func (m *Memo) EnableOrderedScan() bool {
	return m.tsOrderedScan
}

// TSSupportAllProcessor check ts engine support all
func (m *Memo) TSSupportAllProcessor() bool {
	return m.tsCanPushAllProcessor
}

// ForcePushGroupToTSEngine check force group to ts engine
func (m *Memo) ForcePushGroupToTSEngine() bool {
	return m.tsForcePushGroupToTSEngine
}

// NotifyOnNewGroup sets a callback function which is invoked each time we
// create a new memo group.
func (m *Memo) NotifyOnNewGroup(fn func(opt.Expr)) {
	m.newGroupFn = fn
}

// IsEmpty returns true if there are no expressions in the memo.
func (m *Memo) IsEmpty() bool {
	// Root expression can be nil before optimization and interner is empty after
	// exploration, so check both.
	return m.interner.Count() == 0 && m.rootExpr == nil
}

// MemoryEstimate returns a rough estimate of the memo's memory usage, in bytes.
// It only includes memory usage that is proportional to the size and complexity
// of the query, rather than constant overhead bytes.
func (m *Memo) MemoryEstimate() int64 {
	// Multiply by 2 to take rough account of allocation fragmentation, private
	// data, list overhead, properties, etc.
	return m.memEstimate * 2
}

// Metadata returns the metadata instance associated with the memo.
func (m *Memo) Metadata() *opt.Metadata {
	return &m.metadata
}

// RootExpr returns the root memo expression previously set via a call to
// SetRoot.
func (m *Memo) RootExpr() opt.Expr {
	return m.rootExpr
}

// RootProps returns the physical properties required of the root memo group,
// previously set via a call to SetRoot.
func (m *Memo) RootProps() *physical.Required {
	return m.rootProps
}

// SetRoot stores the root memo expression when it is a relational expression,
// and also stores the physical properties required of the root group.
func (m *Memo) SetRoot(e RelExpr, phys *physical.Required) {
	m.rootExpr = e
	if m.rootProps != phys {
		m.rootProps = m.InternPhysicalProps(phys)
	}

	// Once memo is optimized, release reference to the eval context and free up
	// the memory used by the interner.
	if m.IsOptimized() {
		m.logPropsBuilder.clear()
		m.interner.Clear()
	}
}

// SetScalarRoot stores the root memo expression when it is a scalar expression.
// Used only for testing.
func (m *Memo) SetScalarRoot(scalar opt.ScalarExpr) {
	m.rootExpr = scalar
}

// HasPlaceholders returns true if the memo contains at least one placeholder
// operator.
func (m *Memo) HasPlaceholders() bool {
	rel, ok := m.rootExpr.(RelExpr)
	if !ok {
		panic(errors.AssertionFailedf("placeholders only supported when memo root is relational"))
	}

	return rel.Relational().HasPlaceholder
}

// IsStale returns true if the memo has been invalidated by changes to any of
// its dependencies. Once a memo is known to be stale, it must be ejected from
// any query cache or prepared statement and replaced with a recompiled memo
// that takes into account the changes. IsStale checks the following
// dependencies:
//
//  1. Current database: this can change name resolution.
//  2. Current search path: this can change name resolution.
//  3. Current location: this determines time zone, and can change how time-
//     related types are constructed and compared.
//  4. Data source schema: this determines most aspects of how the query is
//     compiled.
//  5. Data source privileges: current user may no longer have access to one or
//     more data sources.
//
// This function cannot swallow errors and return only a boolean, as it may
// perform KV operations on behalf of the transaction associated with the
// provided catalog, and those errors are required to be propagated.
func (m *Memo) IsStale(
	ctx context.Context, evalCtx *tree.EvalContext, catalog cat.Catalog,
) (bool, error) {
	// Memo is stale if fields from SessionData that can affect planning have
	// changed.
	if !m.dataConversion.Equals(&evalCtx.SessionData.DataConversion) ||
		m.reorderJoinsLimit != evalCtx.SessionData.ReorderJoinsLimit ||
		m.multiModelReorderJoinsLimit != evalCtx.SessionData.MultiModelReorderJoinsLimit ||
		m.MultiModelEnabled != evalCtx.SessionData.MultiModelEnabled ||
		m.zigzagJoinEnabled != evalCtx.SessionData.ZigzagJoinEnabled ||
		m.optimizerFKs != evalCtx.SessionData.OptimizerFKs ||
		m.safeUpdates != evalCtx.SessionData.SafeUpdates ||
		m.saveTablesPrefix != evalCtx.SessionData.SaveTablesPrefix ||
		m.insertFastPath != evalCtx.SessionData.InsertFastPath ||
		m.tsOrderedScan != opt.TSOrderedTable.Get(&evalCtx.Settings.SV) ||
		m.tsCanPushAllProcessor != opt.PushdownAll.Get(&evalCtx.Settings.SV) ||
		m.tsQueryOptMode != opt.TSQueryOptMode.Get(&evalCtx.Settings.SV) ||
		m.tsForcePushGroupToTSEngine == stats.AutomaticTsStatisticsClusterMode.Get(&evalCtx.Settings.SV) {
		return true, nil
	}

	// Memo is stale if the fingerprint of any object in the memo's metadata has
	// changed, or if the current user no longer has sufficient privilege to
	// access the object.
	if depsUpToDate, err := m.Metadata().CheckDependencies(ctx, catalog); err != nil {
		return true, err
	} else if !depsUpToDate {
		return true, nil
	}

	return !m.verifyAutoLimitConsistency(evalCtx), nil
}

// verifyAutoLimitConsistency compares the new autoLimit with autolimit of cache to check if they are the same.
func (m *Memo) verifyAutoLimitConsistency(evalCtx *tree.EvalContext) bool {
	flag := true
	autoLimitQuantity := opt.AutoLimitQuantity.Get(&evalCtx.Settings.SV)
	// compare autolimit switch status in the cache with the current autolimit switch status
	if m.CheckFlag(opt.HasAutoLimit) != (autoLimitQuantity > 0) {
		flag = false
	} else {
		// if plan contain autolimit, compare quantity of autolimit in the cache with quantity of the current autolimit
		if limit, ok1 := m.RootExpr().(*LimitExpr); ok1 && m.CheckFlag(opt.HasAutoLimit) {
			if c, ok2 := limit.Limit.(*ConstExpr); ok2 && int64(tree.MustBeDInt(c.Value)) != autoLimitQuantity {
				flag = false
			}
		}
	}
	return flag
}

// InternPhysicalProps adds the given physical props to the memo if they haven't
// yet been added. If the same props was added previously, then return a pointer
// to the previously added props. This allows interned physical props to be
// compared for equality using simple pointer comparison.
func (m *Memo) InternPhysicalProps(phys *physical.Required) *physical.Required {
	// Special case physical properties that require nothing of operator.
	if !phys.Defined() {
		return physical.MinRequired
	}
	return m.interner.InternPhysicalProps(phys)
}

// SetBestProps updates the physical properties, provided ordering, and cost of
// a relational expression's memo group (see the relevant methods of RelExpr).
// It is called by the optimizer once it determines the expression in the group
// that is part of the lowest cost tree (for the overall query).
func (m *Memo) SetBestProps(
	e RelExpr, required *physical.Required, provided *physical.Provided, cost Cost,
) {
	if e.RequiredPhysical() != nil {
		if e.RequiredPhysical() != required ||
			!e.ProvidedPhysical().Equals(provided) ||
			e.Cost() != cost {
			panic(errors.AssertionFailedf(
				"cannot overwrite %s / %s (%.9g) with %s / %s (%.9g)",
				e.RequiredPhysical(),
				e.ProvidedPhysical(),
				log.Safe(e.Cost()),
				required.String(),
				provided.String(), // Call String() so provided doesn't escape.
				cost,
			))
		}
		return
	}
	bp := e.bestProps()
	bp.required = required
	bp.provided = *provided
	bp.cost = cost
}

// ResetCost updates the cost of a relational expression's memo group. It
// should *only* be called by Optimizer.RecomputeCost() for testing purposes.
func (m *Memo) ResetCost(e RelExpr, cost Cost) {
	e.bestProps().cost = cost
}

// IsOptimized returns true if the memo has been fully optimized.
func (m *Memo) IsOptimized() bool {
	// The memo is optimized once the root expression has its physical properties
	// assigned.
	rel, ok := m.rootExpr.(RelExpr)
	return ok && rel.RequiredPhysical() != nil
}

// NextID returns a new unique ScalarID to number expressions with.
func (m *Memo) NextID() opt.ScalarID {
	m.curID++
	return m.curID
}

// RequestColStat calculates and returns the column statistic calculated on the
// relational expression.
func (m *Memo) RequestColStat(
	expr RelExpr, cols opt.ColSet,
) (colStat *props.ColumnStatistic, ok bool) {
	// When SetRoot is called, the statistics builder may have been cleared.
	// If this happens, we can't serve the request anymore.
	if m.logPropsBuilder.sb.md != nil {
		return m.logPropsBuilder.sb.colStat(cols, expr), true
	}
	return nil, false
}

// SortFilters reorders the filter conditions based on the degree of filtering that is calculated.
func (m *Memo) SortFilters(selectExpr *SelectExpr, rel *props.Relational) {
	m.sortGlobalFilters(selectExpr, rel)
	m.sortLocalFilters(selectExpr, rel)
}

// sortGlobalFilters reorders the conditions that are split by 'AND'.
func (m *Memo) sortGlobalFilters(selectExpr *SelectExpr, rel *props.Relational) {
	selectivityMap := m.computeTSFiltersSelectivity(selectExpr, rel)
	if len(selectivityMap) <= 1 || len(selectivityMap) != len(selectExpr.Filters) {
		return
	}
	type selectivityPair struct {
		index       int
		selectivity float64
	}
	selectivityPairs := make([]selectivityPair, 0)
	for i := 0; i < len(selectivityMap); i++ {
		v := selectivityMap[i]
		s := selectivityPair{index: i, selectivity: v}
		selectivityPairs = append(selectivityPairs, s)
	}
	sort.SliceStable(selectivityPairs, func(i, j int) bool {
		return selectivityPairs[i].selectivity < selectivityPairs[j].selectivity
	})
	sortedFilters := make([]FiltersItem, 0)
	for _, pair := range selectivityPairs {
		sortedFilters = append(sortedFilters, selectExpr.Filters[pair.index])
	}
	selectExpr.Filters = sortedFilters
}

// computeTSFiltersSelectivity calculates filter's selectivity
func (m *Memo) computeTSFiltersSelectivity(
	sel *SelectExpr, relProps *props.Relational,
) map[int]float64 {
	if m.logPropsBuilder.sb.md != nil {
		return m.logPropsBuilder.sb.computeTSFiltersSelectivity(sel, relProps)
	}
	return nil
}

// sortLocalFilters reorders the every filter's conditions tree
func (m *Memo) sortLocalFilters(selectExpr *SelectExpr, rel *props.Relational) {
	for i := range selectExpr.Filters {
		scalar := selectExpr.Filters[i].Condition
		m.sortCondition(selectExpr, rel, &scalar)
		selectExpr.Filters[i].Condition = scalar
	}
}

// sortCondition calculates the selectivity of the two trees left and right
// of the AND operator and the OR operator and adjusts the position
func (m *Memo) sortCondition(sel *SelectExpr, rel *props.Relational, e *opt.ScalarExpr) float64 {
	switch t := (*e).(type) {
	case *OrExpr:
		l := m.sortCondition(sel, rel, &t.Left)
		r := m.sortCondition(sel, rel, &t.Right)
		if l < r {
			tmpLeft := t.Left
			t.Left = t.Right
			t.Right = tmpLeft
		}
	case *AndExpr:
		l := m.sortCondition(sel, rel, &t.Left)
		r := m.sortCondition(sel, rel, &t.Right)
		if l > r {
			tmpLeft := t.Left
			t.Left = t.Right
			t.Right = tmpLeft
		}
	default:
	}
	return m.ComputeLocalTSFiltersSelectivity(sel, rel, *e)
}

// ComputeLocalTSFiltersSelectivity calculates local ts filters selectivity
func (m *Memo) ComputeLocalTSFiltersSelectivity(
	sel *SelectExpr, relProps *props.Relational, condition opt.ScalarExpr,
) float64 {
	if m.logPropsBuilder.sb.md != nil {
		cb := constraintsBuilder{
			md:      m.logPropsBuilder.sb.md,
			evalCtx: m.logPropsBuilder.sb.evalCtx,
		}
		cs, tight := cb.buildConstraints(condition)
		return m.logPropsBuilder.sb.computeConstraintsSelectivity(sel, relProps, condition, cs, tight)
	}
	return 0
}

// RowsProcessed calculates and returns the number of rows processed by the
// relational expression. It is currently only supported for joins.
func (m *Memo) RowsProcessed(expr RelExpr) (_ float64, ok bool) {
	// When SetRoot is called, the statistics builder may have been cleared.
	// If this happens, we can't serve the request anymore.
	if m.logPropsBuilder.sb.md != nil {
		return m.logPropsBuilder.sb.rowsProcessed(expr), true
	}
	return 0, false
}

// NextWithID returns a not-yet-assigned identifier for a WITH expression.
func (m *Memo) NextWithID() opt.WithID {
	m.curWithID++
	return m.curWithID
}

// Detach is used when we detach a memo that is to be reused later (either for
// execbuilding or with AssignPlaceholders). New expressions should no longer be
// constructed in this memo.
func (m *Memo) Detach() {
	m.interner = interner{}
	// It is important to not hold on to the EvalCtx in the logicalPropsBuilder
	// (#57059).
	m.logPropsBuilder = logicalPropsBuilder{}

	// Clear all column statistics from every relational expression in the memo.
	// This is used to free up the potentially large amount of memory used by
	// histograms.
	var clearColStats func(parent opt.Expr)
	clearColStats = func(parent opt.Expr) {
		for i, n := 0, parent.ChildCount(); i < n; i++ {
			child := parent.Child(i)
			clearColStats(child)
		}

		switch t := parent.(type) {
		case RelExpr:
			t.Relational().Stats.ColStats = props.ColStatsMap{}
		}
	}
	clearColStats(m.RootExpr())
}

// AddColumn add column to map.
// col is the id of column, alias is the name of column.
// typ is type of column, pos is the position of column.
// hash is the hash value of the column name and type.
// isTimeBucket is true when the column is time_bucket.
func (m *Memo) AddColumn(
	col opt.ColumnID, alias string, typ ExprType, pos ExprPos, hash uint32, isTimeBucket bool,
) {
	m.CheckHelper.PushHelper.lock.Lock()
	m.CheckHelper.PushHelper.MetaMap[col] = ExprInfo{Alias: alias, Type: typ, Pos: pos, Hash: hash, IsTimeBucket: isTimeBucket}
	m.CheckHelper.PushHelper.lock.Unlock()
}

// GetPushHelperAddress return the push helper address
func (m *Memo) GetPushHelperAddress() *MetaInfoMap {
	return &m.CheckHelper.PushHelper.MetaMap
}

// CheckExecInTS check if the column can execute in ts engine, but
// the columns is the logical columns which may be an expr.
// col is the column ID of the logical column.
// pos is the position where the column appears, it can be ExprPosSelect,ExprPosProjList,ExprPosGroupBy
func (m *Memo) CheckExecInTS(col opt.ColumnID, pos ExprPos) bool {
	m.CheckHelper.PushHelper.lock.Lock()
	info, ok := m.CheckHelper.PushHelper.MetaMap[col]
	m.CheckHelper.PushHelper.lock.Unlock()
	if !ok {
		return false
	}

	// single column and const can always execute in ts engine.
	if info.Type == ExprTypCol || info.Type == ExprTypConst {
		return true
	}

	// check from whitelist
	return m.CheckHelper.whiteList.CheckWhiteListParam(info.Hash, uint32(pos))
}

// CheckFlag check if the flag is set.
// flag: flag that need to be checked
func (m *Memo) CheckFlag(flag int) bool {
	return m.CheckHelper.flags&flag > 0
}

// SetFlag set flag is true
func (m *Memo) SetFlag(flag int) {
	m.CheckHelper.flags |= flag
}

// SetAllFlag set all flag
func (m *Memo) SetAllFlag(flags int) {
	m.CheckHelper.flags = flags
}

// GetAllFlag return all flag
func (m *Memo) GetAllFlag() int {
	return m.CheckHelper.flags
}

// ClearFlag clear the flag.
func (m *Memo) ClearFlag(flag int) {
	m.CheckHelper.flags &= ^flag
}

// CheckWhiteListAndAddSynchronize check if the memo expr can execute in ts engine
// according to white list and set flag to add Synchronizer.
// src is the expr of memo tree.
func (m *Memo) CheckWhiteListAndAddSynchronize(src *RelExpr) error {
	if !m.CheckFlag(opt.IncludeTSTable) {
		return nil
	}
	// the main implementation of checking white list and setting flag for adding Synchronizer.
	retTop := m.CheckWhiteListAndAddSynchronizeImp(src)
	if retTop.err != nil {
		return retTop.err
	}
	addSynchronizeStruct(&retTop, *src)
	return nil
}

// DealTSScanFunc deal with ts scan expr
type DealTSScanFunc func(expr *TSScanExpr)

// addColForTSScan add ts col to TSScanExpr when only
// TSScanExpr can execute in ts engine.
// case: select 1 from tstable
func addColForTSScan(expr *TSScanExpr) {
	if expr.Cols.Empty() {
		expr.Cols.Add(expr.Table.ColumnID(0))
	}
}

// walkDealTSScan find the TsScanExpr from the memo tree.
func walkDealTSScan(expr opt.Expr, f DealTSScanFunc) {
	if expr.Op() == opt.TSScanOp {
		f(expr.(*TSScanExpr))
		return
	}

	for i := 0; i < expr.ChildCount(); i++ {
		walkDealTSScan(expr.Child(i), f)
	}
}

// DealTSScan find the memo.TSScanExpr from the memo tree.
// And add ts col to TSScanExpr when only TSScanExpr can execute in ts engine.
func (m *Memo) DealTSScan(src RelExpr) {
	if !m.CheckFlag(opt.IncludeTSTable) {
		return
	}
	walkDealTSScan(src, addColForTSScan)
}

func (m *Memo) addOrderedColumn(src *bestProps) {
	if src != nil && !m.CheckHelper.orderedCols.Empty() {
		for i := range src.provided.Ordering {
			m.CheckHelper.orderedCols.Add(src.provided.Ordering[i].ID())
		}
		m.CheckHelper.orderedCols.ForEach(func(col opt.ColumnID) {
			if m.CheckHelper.orderedScanType.Ordered() {
				src.provided.Ordering = append(src.provided.Ordering, opt.MakeOrderingColumn(col, false))
			}
		})
	}
}

// checkOptPruneFinalAgg checkout can prune final agg for single node mode
// 1. onlyOnePTagValue is a flag for primary tag value filter, only query one primary tag
// eg: select sum(a) from tsdb.t1 where ptag = 1 group by b;
// 2. group by  contain all primary tag
// eg: select sum(a) from tsdb.t1 group by ptag;
func checkOptPruneFinalAgg(gp *GroupingPrivate, meta *opt.Metadata, onlyOnePTagValue bool) {
	if onlyOnePTagValue {
		gp.OptFlags |= opt.PruneFinalAgg
	} else {
		PrimaryTagCount := 0
		tableID := opt.TableID(0)
		gp.GroupingCols.ForEach(func(colID opt.ColumnID) {
			colMeta := meta.ColumnMeta(colID)
			if colMeta.IsPrimaryTag() {
				PrimaryTagCount++
			}
			if tableID == 0 {
				tableID = colMeta.Table
			} else if colMeta.Table != 0 && tableID != colMeta.Table {
				PrimaryTagCount = 0
			}
		})

		if tableID != 0 && PrimaryTagCount != 0 {
			tableMeta := meta.TableMeta(tableID)
			if PrimaryTagCount == tableMeta.PrimaryTagCount && gp.AggIndex.Empty() {
				gp.OptFlags |= opt.PruneFinalAgg
			}
		}
	}
}

// dealWithGroupBy
// src is GroupByExpr or ScalarGroupByExpr
// child is the child expr of GroupByExpr or ScalarGroupByExpr
// ret is return param struct
func (m *Memo) dealWithGroupBy(src RelExpr, child RelExpr, ret *aggCrossEngCheckResults) {
	aggs := make([]AggregationsItem, 0)
	var gp *GroupingPrivate
	switch t := src.(type) {
	case *GroupByExpr:
		aggs = t.Aggregations
		gp = &t.GroupingPrivate
		if m.CheckHelper.GroupHint != keys.ForceNoSynchronizerGroup {
			t.engine = tree.EngineTypeRelational
		}
	case *ScalarGroupByExpr:
		aggs = t.Aggregations
		gp = &t.GroupingPrivate
		if m.CheckHelper.GroupHint != keys.ForceNoSynchronizerGroup {
			t.engine = tree.EngineTypeRelational
		}
	}

	// do nothing when group by can not execute in ts engine.
	if !ret.commonRet.execInTSEngine {
		return
	}

	// case: agg with distinct
	if ret.hasDistinct {
		// multi node, agg with distinct can not execute in ts engine.
		if !(m.CheckFlag(opt.SingleMode) || m.CheckHelper.isSingleNode()) {
			if !ret.commonRet.hasAddSynchronizer {
				m.setSynchronizerForChild(child, &ret.commonRet.hasAddSynchronizer)
			}
			ret.commonRet.execInTSEngine = false
			return
		}
	}
	src.SetEngineTS()

	// fill statistics
	m.fillStatistic(&child, aggs, gp)

	if ret.commonRet.canTimeBucketOptimize {
		checkOptPruneFinalAgg(gp, m.Metadata(), m.CheckHelper.onlyOnePTagValue)

		gp.OptFlags |= opt.PushLocalAggToScan

		// single device can prune local agg
		if m.CheckHelper.onlyOnePTagValue || gp.OptFlags.PruneFinalAggOpt() {
			gp.OptFlags |= opt.PruneLocalAgg
			// set ts scan use ordered scan table
			walkDealTSScan(src, setOrderedForce)
		}
	}

	if ret.isParallel && !ret.commonRet.hasAddSynchronizer && !gp.OptFlags.PruneFinalAggOpt() {
		if m.CheckHelper.GroupHint != keys.ForceNoSynchronizerGroup {
			src.SetAddSynchronizer()
		}
		ret.commonRet.hasAddSynchronizer = true
	}
}

func setOrderedForce(expr *TSScanExpr) {
	expr.OrderedScanType = opt.ForceOrderedScan
}

// dealWithOrderBy set engine and add flag for the child of order by
// when it's child can exec in ts engine.
// sort is memo.SortExpr of memo tree.
// ret is return param struct.
// props is the bestProps of (memo.GroupByExpr or memo.DistinctOnExpr),
// props is not nil when there is a OrderGroupBy.
func (m *Memo) dealWithOrderBy(sort *SortExpr, ret *CrossEngCheckResults, props *bestProps) {
	if ret.execInTSEngine {
		// only single node, order by can exec in ts engine.
		if m.CheckFlag(opt.SingleMode) || m.CheckHelper.isSingleNode() {
			sort.SetEngineTS()
		}

		addSynchronize(&ret.hasAddSynchronizer, sort.Input)
	}
	// OrderGroupBy case, reset bestProps of (memo.GroupByExpr or memo.DistinctOnExpr)
	if props != nil {
		sort.best.required = props.required
		props.required = &physical.Required{}
		props.provided = physical.Provided{}
	}
}

// fillStatistic fill statistics for the child of group by if group by can execute
// in ts engine and the agg can use statistics collected by storage engine.
// child is the Input of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// aggs is the Aggregations of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// gp is the GroupingPrivate of (memo.GroupByExpr / memo.ScalarGroupByExpr).
func (m *Memo) fillStatistic(child *RelExpr, aggs []AggregationsItem, gp *GroupingPrivate) {
	if !m.checkAggStatisticUsable(aggs) {
		return
	}
	switch src := (*child).(type) {
	case *TSScanExpr:
		m.tsScanFillStatistic(src, aggs, gp)
	case *SelectExpr:
		m.selectExprFillStatistic(src, aggs, gp)
	case *ProjectExpr:
		m.projectExprFillStatistic(src, aggs, gp)
	}
}

// projectExprFillStatistic fill projectExpr's statistics.
// aggs is the Aggregations of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// gp is the GroupingPrivate of (memo.GroupByExpr / memo.ScalarGroupByExpr).
func (m *Memo) projectExprFillStatistic(
	project *ProjectExpr, aggs []AggregationsItem, gp *GroupingPrivate,
) {

	for _, val := range project.Projections {
		switch val.Element.(type) {
		case *NullExpr, *ConstExpr:
		default:
			return
		}
	}
	for _, agg := range aggs {
		if agg.Agg.Op() == opt.LastOp || agg.Agg.Op() == opt.LastTimeStampOp {
			continue
		}
		for j := 0; j < agg.Agg.ChildCount(); j++ {
			switch arg := agg.Agg.Child(j).(type) {
			case *VariableExpr:
				// is not table col, null or const
				if 0 == m.metadata.ColumnMeta(arg.Col).Table {
					return
				}
			default:
				return
			}
		}
	}

	child := project.Input
	switch src := (child).(type) {
	case *TSScanExpr:
		m.tsScanFillStatistic(src, aggs, gp)
	case *SelectExpr:
		m.selectExprFillStatistic(src, aggs, gp)
	}
	return
}

// tsScanFillStatistic fill tsScan's statistics.
// tsScan is memo.TSScanExpr.
// aggs is the Aggregations of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// gp is the GroupingPrivate of (memo.GroupByExpr / memo.ScalarGroupByExpr).
func (m *Memo) tsScanFillStatistic(
	tsScan *TSScanExpr, aggs []AggregationsItem, gp *GroupingPrivate,
) {
	allColsPrimary := true
	gp.GroupingCols.ForEach(func(colID opt.ColumnID) {
		colMeta := m.Metadata().ColumnMeta(colID)
		allColsPrimary = allColsPrimary && colMeta.IsPrimaryTag()
	})
	tableMeta := m.Metadata().TableMeta(tsScan.Table)
	if !allColsPrimary || (gp.GroupingCols.Len() != 0 && gp.GroupingCols.Len() != tableMeta.PrimaryTagCount) ||
		tsScan.HintType.OnlyTag() {
		return
	}

	if tsScan.OrderedScanType != opt.NoOrdered && gp.GroupingCols.Len() > 0 {
		gp.OptFlags |= opt.PruneFinalAgg
	}

	gp.GroupingCols.ForEach(func(colID opt.ColumnID) {
		gp.AggIndex = append(gp.AggIndex, []uint32{uint32(len(tsScan.ScanAggs))})
		tsScan.ScanAggs = append(tsScan.ScanAggs, ScanAgg{Params: execinfrapb.TSStatisticReaderSpec_Params{
			Param: []execinfrapb.TSStatisticReaderSpec_ParamInfo{
				{Typ: execinfrapb.TSStatisticReaderSpec_ParamInfo_colID, Value: int64(colID)},
			},
		}, AggTyp: execinfrapb.AggregatorSpec_ANY_NOT_NULL})
	})

	if gp.OptFlags.PruneFinalAggOpt() && m.CheckFlag(opt.SingleMode) {
		for i := range aggs {
			m.addScanAggsForNoSecondAgg(aggs[i].Agg, &tsScan.ScanAggs, gp, tsScan.Table.ColumnID(0))
		}
	} else {
		for i := range aggs {
			m.addScanAggs(aggs[i].Agg, &tsScan.ScanAggs, gp, tsScan.Table.ColumnID(0))
		}
	}
}

// tsScanFillStatistic fill filter's statistics.
// selectExpr is memo.SelectExpr.
// aggs is the Aggregations of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// gp is the GroupingPrivate of (memo.GroupByExpr / memo.ScalarGroupByExpr).
func (m *Memo) selectExprFillStatistic(
	selectExpr *SelectExpr, aggs []AggregationsItem, gp *GroupingPrivate,
) {

	for _, filter := range selectExpr.Filters {
		if !m.checkFiltersStatisticUsable(filter.Condition) {
			return
		}
	}

	if tsScanExpr, ok := selectExpr.Input.(*TSScanExpr); ok {
		m.tsScanFillStatistic(tsScanExpr, aggs, gp)
	}
}

// isTsColumnOrConst checks whether the column is
// the first column in the ts table or a constant column.
// Returns:
//   - uint32: 0: It's neither a constant column nor the first column in ts table.
//     1: It's the first column in ts table.
//     2: It's a const column.
func (m *Memo) isTsColumnOrConst(src opt.ScalarExpr) uint32 {
	switch source := src.(type) {
	case *VariableExpr:
		tblID := m.Metadata().ColumnMeta(source.Col).Table
		if tblID != 0 {
			table := m.Metadata().Table(tblID)
			tsColID := table.Column(0).ColID()
			// k_timestamp column can use statistic
			if source.Col == opt.ColumnID(tsColID) {
				return 1
			}
		}
		return 0
	case *ConstExpr:
		return 2
	}
	return 0
}

// checkFiltersStatisticUsable checks whether the filtering conditions
// meet the requirements for using statistics collected by storage engine.
func (m *Memo) checkFiltersStatisticUsable(src opt.ScalarExpr) bool {
	if src.Op() == opt.LtOp || src.Op() == opt.LeOp || src.Op() == opt.EqOp || src.Op() == opt.GtOp || src.Op() == opt.GeOp {
		var sum uint32
		for i := 0; i < src.ChildCount(); i++ {
			sum |= m.isTsColumnOrConst(src.Child(i).(opt.ScalarExpr))
		}

		return sum == 3
	}
	switch source := src.(type) {
	case *VariableExpr:
		tblID := m.Metadata().ColumnMeta(source.Col).Table
		if tblID != 0 {
			table := m.Metadata().Table(tblID)
			tsColID := table.Column(0).ColID()
			// k_timestamp column can use statistic
			if source.Col == opt.ColumnID(tsColID) {
				return true
			}
		}
		return false
	case *ConstExpr:
		return true
	case *RangeExpr:
		return m.checkFiltersStatisticUsable(source.And)
	case *OrExpr:
		lCan := m.checkFiltersStatisticUsable(source.Left)
		rCan := m.checkFiltersStatisticUsable(source.Right)
		return lCan && rCan
	case *AndExpr:
		lCan := m.checkFiltersStatisticUsable(source.Left)
		rCan := m.checkFiltersStatisticUsable(source.Right)
		return lCan && rCan
	default:
		return false
	}
}

// checkAggStatisticUsable checks that statistics are available for aggregation functions
func (m *Memo) checkAggStatisticUsable(aggs []AggregationsItem) bool {
	if len(aggs) == 0 {
		return false
	}
	for i := range aggs {
		switch aggs[i].Agg.(type) {
		case *SumExpr, *MinExpr, *MaxExpr, *CountExpr, *FirstExpr, *FirstTimeStampExpr, *FirstRowExpr,
			*FirstRowTimeStampExpr, *LastExpr, *LastTimeStampExpr, *LastRowExpr, *LastRowTimeStampExpr, *CountRowsExpr:
		default:
			return false
		}
	}

	return true
}

// statisticAgg is the aggregate function which can use statistic.
type statisticAgg struct {
	LocalStage []execinfrapb.AggregatorSpec_Func
}

// StatisticAggTable is a map of the aggregate functions which can use statistic.
var StatisticAggTable = map[opt.Operator]statisticAgg{
	opt.SumOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_SUM},
	},
	opt.MinOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_MIN},
	},
	opt.MaxOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_MAX},
	},
	opt.AvgOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_SUM, execinfrapb.AggregatorSpec_COUNT},
	},
	opt.CountOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_COUNT},
	},
	opt.CountRowsOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_COUNT},
	},
	opt.FirstOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_FIRST, execinfrapb.AggregatorSpec_FIRSTTS},
	},
	opt.FirstTimeStampOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_FIRST, execinfrapb.AggregatorSpec_FIRSTTS},
	},
	opt.FirstRowOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_FIRST_ROW, execinfrapb.AggregatorSpec_FIRST_ROW_TS},
	},
	opt.FirstRowTimeStampOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_FIRST_ROW, execinfrapb.AggregatorSpec_FIRST_ROW_TS},
	},
	opt.LastOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_ANY_NOT_NULL, execinfrapb.AggregatorSpec_LASTTS, execinfrapb.AggregatorSpec_LAST},
	},
	opt.LastTimeStampOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_ANY_NOT_NULL, execinfrapb.AggregatorSpec_LASTTS, execinfrapb.AggregatorSpec_LAST},
	},
	opt.LastRowOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_LAST_ROW, execinfrapb.AggregatorSpec_LAST_ROW_TS},
	},
	opt.LastRowTimeStampOp: {
		LocalStage: []execinfrapb.AggregatorSpec_Func{execinfrapb.AggregatorSpec_LAST_ROW, execinfrapb.AggregatorSpec_LAST_ROW_TS},
	},
}

// opToSpecFuncTable  op convert to spec function table
var opToSpecFuncTable = map[opt.Operator]execinfrapb.AggregatorSpec_Func{
	opt.SumOp:               execinfrapb.AggregatorSpec_SUM,
	opt.MinOp:               execinfrapb.AggregatorSpec_MIN,
	opt.MaxOp:               execinfrapb.AggregatorSpec_MAX,
	opt.AvgOp:               execinfrapb.AggregatorSpec_AVG,
	opt.CountOp:             execinfrapb.AggregatorSpec_COUNT,
	opt.CountRowsOp:         execinfrapb.AggregatorSpec_COUNT,
	opt.FirstOp:             execinfrapb.AggregatorSpec_FIRST,
	opt.FirstTimeStampOp:    execinfrapb.AggregatorSpec_FIRSTTS,
	opt.FirstRowOp:          execinfrapb.AggregatorSpec_FIRST_ROW,
	opt.FirstRowTimeStampOp: execinfrapb.AggregatorSpec_FIRST_ROW_TS,
	opt.LastOp:              execinfrapb.AggregatorSpec_LAST,
	opt.LastTimeStampOp:     execinfrapb.AggregatorSpec_LASTTS,
	opt.LastRowOp:           execinfrapb.AggregatorSpec_LAST_ROW,
	opt.LastRowTimeStampOp:  execinfrapb.AggregatorSpec_LAST_ROW_TS,
}

// addScanAggs adds scan Aggs to tsExpr.ScanAggs.
// agg is the one of the Aggregations of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// scanAggs is ScanAggs of memo.TSScanExpr.
// gp is the GroupingPrivate of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// tsCol is the logical column ID of the timestamp colimn of the ts table.
func (m *Memo) addScanAggs(
	agg opt.ScalarExpr, scanAggs *ScanAggArray, gp *GroupingPrivate, tsCol opt.ColumnID,
) {
	if val, ok := StatisticAggTable[agg.Op()]; ok {
		colID := tsCol
		if agg.ChildCount() > 0 {
			colID = m.getArgColID(agg.Child(0).(opt.ScalarExpr))
		}

		Param := []execinfrapb.TSStatisticReaderSpec_ParamInfo{
			{Typ: execinfrapb.TSStatisticReaderSpec_ParamInfo_colID, Value: int64(colID)}}
		var ConstParam execinfrapb.TSStatisticReaderSpec_ParamInfo
		for i := 1; i < agg.ChildCount(); i++ {
			switch src := agg.Child(i).(type) {
			case *VariableExpr:
				// is not table col, null
				if 0 == m.metadata.ColumnMeta(src.Col).Table {
					var constVal int64
					constVal = math.MaxInt64
					maxBoundaryStr := tree.TsMaxTimestampString + "::TIMESTAMPTZ"
					boundaryStr := m.metadata.ColumnMeta(src.Col).Alias
					boundaryStr = strings.Replace(boundaryStr, "'", "", -1)
					if boundaryStr != maxBoundaryStr {
						constVal = parseTZ(boundaryStr)
					}
					ConstParam = execinfrapb.TSStatisticReaderSpec_ParamInfo{
						Typ: execinfrapb.TSStatisticReaderSpec_ParamInfo_const, Value: constVal}
					Param = append(Param, ConstParam)
				} else {
					Param = append(Param, execinfrapb.TSStatisticReaderSpec_ParamInfo{
						Typ: execinfrapb.TSStatisticReaderSpec_ParamInfo_colID, Value: int64(src.Col)})
				}
			}
		}

		var index []uint32
		var tmpParam []execinfrapb.TSStatisticReaderSpec_ParamInfo
		for _, v := range val.LocalStage {
			if v == execinfrapb.AggregatorSpec_ANY_NOT_NULL {
				tmpParam = []execinfrapb.TSStatisticReaderSpec_ParamInfo{ConstParam}
			} else {
				tmpParam = Param
			}
			// exists agg , use it
			if idx := m.fillScanAggs(scanAggs, tmpParam, v); idx != -1 {
				index = append(index, uint32(idx))
			} else {
				index = append(index, uint32(len(*scanAggs)-1))
			}
		}
		// We need to adjust the order of last's arguments so that
		// the first parameter is the column to query,
		// the second parameter is the k_timestamp column,
		// and the third parameter is the deadline
		if len(index) == 3 {
			newIndex := make([]uint32, 0)
			newIndex = append(newIndex, index[2], index[1], index[0])
			gp.AggIndex = append(gp.AggIndex, newIndex)
		} else {
			gp.AggIndex = append(gp.AggIndex, index)
		}
	}
	return
}

// addScanAggsForNoSecondAgg adds scan Aggs to tsExpr.ScanAggs for second agg optimize
// agg is the one of the Aggregations of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// scanAggs is ScanAggs of memo.TSScanExpr.
// gp is the GroupingPrivate of (memo.GroupByExpr / memo.ScalarGroupByExpr).
// tsCol is the logical column ID of the timestamp colimn of the ts table.
func (m *Memo) addScanAggsForNoSecondAgg(
	agg opt.ScalarExpr, scanAggs *ScanAggArray, gp *GroupingPrivate, tsCol opt.ColumnID,
) {
	if val, ok := opToSpecFuncTable[agg.Op()]; ok {
		colID := tsCol
		if agg.ChildCount() > 0 {
			colID = m.getArgColID(agg.Child(0).(opt.ScalarExpr))
		}

		var index []uint32
		Param := []execinfrapb.TSStatisticReaderSpec_ParamInfo{
			{Typ: execinfrapb.TSStatisticReaderSpec_ParamInfo_colID, Value: int64(colID)}}
		// exists agg , use it
		if idx := m.fillScanAggs(scanAggs, Param, val); idx != -1 {
			index = append(index, uint32(idx))
		} else {
			index = append(index, uint32(len(*scanAggs)-1))
		}
		gp.AggIndex = append(gp.AggIndex, index)
	}
	return
}

// parseTZ converts a string timestamp value to an integer timestamp value.
func parseTZ(boundaryStr string) int64 {
	d, err := tree.ParseDTimestampTZ(nil, boundaryStr, time.Millisecond)
	if err != nil {
		panic(pgerror.Newf(pgcode.DatatypeMismatch, "could not parse %s as TIMESTAMPTZ", boundaryStr))
	}
	nanosecond := d.Time.Nanosecond()
	second := d.Time.Unix()
	timeNew := second*1000 + int64(nanosecond/1000000)
	return timeNew
}

// getArgColID return the column ID of the parameter of the agg function.
// expr is the parameter of the agg function.
func (m *Memo) getArgColID(expr opt.ScalarExpr) opt.ColumnID {
	if arg, ok := expr.(*VariableExpr); ok {
		return arg.Col
	}
	return 1
}

// fillScanAggs adds the column id of argument of agg and
// the spec type of agg to the tsScan.ScanAggs.
// scanAggs is ScanAggs of memo.TSScanExpr.
// colID is the column ID of the parameter of the agg function.
// aggTyp is one of StatisticAggTable.
func (m *Memo) fillScanAggs(
	scanAggs *ScanAggArray,
	params []execinfrapb.TSStatisticReaderSpec_ParamInfo,
	aggTyp execinfrapb.AggregatorSpec_Func,
) int {
	var agg ScanAgg
	agg.Params.Param = params
	agg.AggTyp = aggTyp
	return addDistinctScanAgg(scanAggs, &agg)
}

// addDistinctScanAgg append distinct ScanAgg to tsScan.ScanAggs
// scanAggs is ScanAggs of memo.TSScanExpr.
func addDistinctScanAgg(scanAggs *ScanAggArray, scanAgg *ScanAgg) int {
	for i, v := range *scanAggs {
		if len(v.Params.Param) != len(scanAgg.Params.Param) {
			break
		}
		sameColID := true
		for j := range v.Params.Param {
			if v.Params.Param[j].Typ != scanAgg.Params.Param[j].Typ ||
				v.Params.Param[j].Typ != execinfrapb.TSStatisticReaderSpec_ParamInfo_colID ||
				v.Params.Param[j].Value != scanAgg.Params.Param[j].Value {
				sameColID = false
				break
			}
		}
		if sameColID && v.AggTyp == scanAgg.AggTyp {
			return i
		}
	}
	*scanAggs = append(*scanAggs, *scanAgg)
	return -1
}

// CrossEngCheckResults includes various flags and an error status related to the optimization and execution checks in ts engine.
type CrossEngCheckResults struct {
	// execInTSEngine: return true when the expr can execute in ts engine.
	execInTSEngine bool
	// hasAddSynchronizer: return true when the expr is set the addSynchronizer to true.
	hasAddSynchronizer bool
	// canTimeBucketOptimize: return true when optimizing query efficiency in time_bucket case.
	canTimeBucketOptimize bool
	// err: is the error.
	err error
}

func (r CrossEngCheckResults) init() {
	r.execInTSEngine = false
	r.hasAddSynchronizer = false
	r.canTimeBucketOptimize = false
	r.err = nil
}

// aggCrossEngCheckResults  agg cross engine check result struct
type aggCrossEngCheckResults struct {
	// isParallel return true when the (memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr) can parallel in ts engine.
	isParallel bool
	// hasDistinct return true when the agg functions with distinct.
	hasDistinct bool

	commonRet CrossEngCheckResults
}

// disableExecInTSEngine disable can execute in ts engine, return error
func (r CrossEngCheckResults) disableExecInTSEngine() CrossEngCheckResults {
	return CrossEngCheckResults{err: r.err}
}

func addSynchronize(hasAdded *bool, src RelExpr) {
	if !*hasAdded {
		src.SetAddSynchronizer()
		*hasAdded = true
	}
}

func addSynchronizeStruct(ret *CrossEngCheckResults, src RelExpr) {
	if ret.execInTSEngine && !ret.hasAddSynchronizer {
		src.SetAddSynchronizer()
		ret.hasAddSynchronizer = true
	}

	ret.execInTSEngine = false
}

// CheckWhiteListAndAddSynchronizeImp check if each expr of memo tree can execute in ts engine,
// and set the engine of expr to opt.EngineTS when it can execute in ts engine,
// and set the addSynchronizer of expr to true when it needs to be synchronized.
// src is the expr of memo tree.
// returns:
// ret: return param struct
func (m *Memo) CheckWhiteListAndAddSynchronizeImp(src *RelExpr) (ret CrossEngCheckResults) {
	ret.init()
	switch source := (*src).(type) {
	case *TSScanExpr:
		return m.CheckTSScan(source)
	case *SelectExpr:
		return m.checkSelect(source)
	case *ProjectExpr:
		return m.checkProject(source)
	case *ProjectSetExpr:
		retTmp := m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
		return retTmp.disableExecInTSEngine()
	case *GroupByExpr:
		input := source.Input
		sort, ok := (*src).Child(0).(*SortExpr)
		if ok {
			m.SetFlag(opt.OrderGroupBy)
			input = sort.Input
		}
		retAgg := m.checkGroupBy(input, &source.Aggregations, &source.GroupingPrivate)
		if retAgg.commonRet.err != nil {
			return retAgg.commonRet.disableExecInTSEngine()
		}
		m.dealWithGroupBy(source, input, &retAgg)
		if ok {
			if retAgg.commonRet.execInTSEngine {
				// swap the positions of GroupByExpr and OrderExpr, when GroupByExpr exec
				// in ts engine and there is the OrderGroupBy.
				source.Input = sort.Input
				sort.Input = source
				m.dealWithOrderBy(sort, &retAgg.commonRet, source.bestProps())
				*src = sort
			} else {
				// group by can not exec in ts engine, clear flag and not need set root.
				m.ClearFlag(opt.OrderGroupBy)
			}
		}

		if source.OptFlags.PruneFinalAggOpt() {
			m.addOrderedColumn(source.bestProps())
		}

		return retAgg.commonRet
	case *ScalarGroupByExpr:
		retAgg := m.checkGroupBy(source.Input, &source.Aggregations,
			&source.GroupingPrivate)
		if retAgg.commonRet.err != nil {
			return retAgg.commonRet.disableExecInTSEngine()
		}
		m.dealWithGroupBy(source, source.Input, &retAgg)
		return retAgg.commonRet
	case *InnerJoinExpr:
		return m.checkJoin(source)
	case *UnionAllExpr, *UnionExpr, *IntersectExpr, *IntersectAllExpr, *ExceptAllExpr, *ExceptExpr:
		return m.checkSetop((*src).Child(0).(RelExpr), (*src).Child(1).(RelExpr))
	case *AntiJoinExpr, *AntiJoinApplyExpr, *SemiJoinExpr, *SemiJoinApplyExpr, *MergeJoinExpr,
		*LeftJoinApplyExpr, *LeftJoinExpr, *RightJoinExpr, *InnerJoinApplyExpr, *FullJoinExpr:
		return m.checkOtherJoin(source)
	case *LookupJoinExpr:
		return m.checkLookupJoin(source)
	case *DistinctOnExpr:
		sort, ok := (*src).Child(0).(*SortExpr)
		if ok {
			m.SetFlag(opt.OrderGroupBy)
		}

		retAgg := m.checkGroupBy(source.Input, &source.Aggregations, &source.GroupingPrivate)
		if retAgg.commonRet.err != nil {
			return retAgg.commonRet.disableExecInTSEngine()
		}

		if retAgg.commonRet.execInTSEngine {
			if m.CheckFlag(opt.SingleMode) || m.CheckHelper.isSingleNode() {
				source.SetEngineTS()
			} else {
				retAgg.commonRet.execInTSEngine = false
			}
			if !retAgg.commonRet.hasAddSynchronizer {
				if !ok {
					source.Input.SetAddSynchronizer()
					retAgg.commonRet.hasAddSynchronizer = true
				} else {
					// swap the positions of DistinctOnExpr and OrderExpr, when DistinctOnExpr can exec
					// in ts engine and there is the OrderGroupBy.
					sort.Input.SetAddSynchronizer()
					retAgg.commonRet.hasAddSynchronizer = true
					source.Input = sort.Input
					sort.Input = source
					m.dealWithOrderBy(sort, &retAgg.commonRet, source.bestProps())
					*src = sort
				}
			}
		} else {
			// distinct can not exec in ts engine, clear flag and not need set root.
			if ok {
				m.ClearFlag(opt.OrderGroupBy)
			}
		}
		if source.OptFlags.PruneFinalAggOpt() {
			m.addOrderedColumn(source.bestProps())
		}
		return retAgg.commonRet
	case *LimitExpr:
		ret = m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
		if ret.err != nil {
			return ret.disableExecInTSEngine()
		}
		if ret.execInTSEngine {
			addSynchronize(&ret.hasAddSynchronizer, source)
			source.SetEngineTS()
		}
		return ret
	case *ScanExpr:
		return ret
	case *OffsetExpr:
		ret = m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
		ret.execInTSEngine = false
		return ret
	case *ValuesExpr:
		return ret
	case *Max1RowExpr: // local plan ,so can not add synchronizer
		return m.dealCanNotAddSynchronize(&source.Input)
	case *OrdinalityExpr: // local plan ,so can not add synchronizer
		return m.dealCanNotAddSynchronize(&source.Input)
	case *VirtualScanExpr:
		return ret
	case *ExplainExpr:
		return m.dealCanNotAddSynchronize(&source.Input)
	case *ExportExpr:
		return m.dealCanNotAddSynchronize(&source.Input)
	case *OpaqueRelExpr:
		return ret
	case *SortExpr:
		ret = m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
		if ret.err != nil {
			return ret.disableExecInTSEngine()
		}

		if !m.CheckFlag(opt.OrderGroupBy) {
			m.dealWithOrderBy(source, &ret, nil)
		}
		return ret
	case *WithExpr:
		ret1 := m.dealCanNotAddSynchronize(&source.Binding)
		if ret1.err != nil {
			return ret1.disableExecInTSEngine()
		}

		ret2 := m.dealCanNotAddSynchronize(&source.Main)
		if ret2.err != nil {
			return ret2.disableExecInTSEngine()
		}
		return ret2
	case *WindowExpr:
		return m.dealCanNotAddSynchronize(&source.Input)
	case *WithScanExpr:
		return ret
	default:
		for i := 0; i < source.ChildCount(); i++ {
			if val, ok := source.Child(i).(RelExpr); ok {
				ret1 := m.CheckWhiteListAndAddSynchronizeImp(&val)
				if ret1.err != nil {
					return ret1.disableExecInTSEngine()
				}
				addSynchronizeStruct(&ret1, val)
			}
		}

		return ret
	}
}

// dealCanNotAddSynchronize check if the child of the memo expr can execute in ts engine
// when the memo expr itself can not execute in ts engine.
// child is the child of memo expr.
// returns:
// ret: return param struct
func (m *Memo) dealCanNotAddSynchronize(child *RelExpr) CrossEngCheckResults {
	ret := m.CheckWhiteListAndAddSynchronizeImp(child)
	if ret.err != nil {
		return ret
	}
	addSynchronizeStruct(&ret, *child)
	ret.execInTSEngine = false
	ret.hasAddSynchronizer = true
	return ret
}

// CheckTSScan deal with memo.TSScanExpr of memo tree.
// Record the columns in PushHelper for future memo expr to
// determine if they can be executed in ts engine.
// returns:
// ret: return param struct
func (m *Memo) CheckTSScan(source *TSScanExpr) (ret CrossEngCheckResults) {
	hasNotTag := false
	ret.init()
	ret.execInTSEngine = true
	source.Cols.ForEach(func(colID opt.ColumnID) {
		colMeta := m.metadata.ColumnMeta(colID)
		if colMeta.IsNormalCol() {
			hasNotTag = true
		}
		m.AddColumn(colID, colMeta.Alias, ExprTypCol, ExprPosNone, 0, false)
	})
	onlyTag := source.HintType.OnlyTag()
	ret.err = nil
	ret.canTimeBucketOptimize = true
	if onlyTag && hasNotTag {
		ret.hasAddSynchronizer = onlyTag
		ret.err = pgerror.New(pgcode.FeatureNotSupported, "TAG_ONLY can only query tag columns")
	} else {
		// when the tagFilter has a subquery, it needs to Walk to check whether it can execute in ts engine.
		param := GetSubQueryExpr{m: m}
		for _, filter := range source.TagFilter {
			filter.Walk(&param)
		}
		source.SetEngineTS()
		// only tag mode should not add synchronizer, so param2 will be true.
		ret.hasAddSynchronizer = onlyTag || source.OrderedScanType == opt.SortAfterScan
	}

	if onlyTag || len(source.PrimaryTagValues) == 0 {
		source.OrderedScanType = opt.NoOrdered
	} else {
		for _, v := range source.PrimaryTagValues {
			if len(v) > 100 {
				source.OrderedScanType = opt.NoOrdered
			} else if len(v) == 1 {
				m.CheckHelper.onlyOnePTagValue = true
			}
		}
	}

	if source.OrderedScanType != opt.NoOrdered {
		m.CheckHelper.orderedCols.Add(source.Table.ColumnID(0))
	}
	m.CheckHelper.orderedScanType = source.OrderedScanType
	return ret
}

// GetSubQueryExpr save expr all info
type GetSubQueryExpr struct {
	m      *Memo
	hasSub bool
}

// IsTargetExpr checks if it's target expr to handle
func (p *GetSubQueryExpr) IsTargetExpr(self opt.Expr) bool {
	switch self.(type) {
	case *SubqueryExpr, *ExistsExpr, *ArrayFlattenExpr, *AnyExpr:
		child := self.Child(0).(RelExpr)
		_ = p.m.CheckWhiteListAndAddSynchronize(&child)
		p.hasSub = true
		return true
	}

	return false
}

// NeedToHandleChild checks if children expr need to be handled
func (p *GetSubQueryExpr) NeedToHandleChild() bool {
	return true
}

// HandleChildExpr deals with all child expr
func (p *GetSubQueryExpr) HandleChildExpr(parent opt.Expr, child opt.Expr) bool {
	return true
}

// checkSelect check if memo.SelectExpr can execute in ts engine.
// source is the memo.SelectExpr of memo tree.
// returns:
// ret: return param struct
func (m *Memo) checkSelect(source *SelectExpr) (ret CrossEngCheckResults) {
	ret = m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
	if ret.err != nil {
		return ret
	}

	// scan or group by
	param := GetSubQueryExpr{m: m}
	selfExecInTS := ret.execInTSEngine
	for i, filter := range source.Filters {
		filter.Walk(&param)
		if param.hasSub && !m.CheckFlag(opt.ScalarSubQueryPush) {
			// can not break ,  need deal with all sub query
			selfExecInTS = false
			ret.canTimeBucketOptimize = false
			continue
		}

		if ret.execInTSEngine {
			if CheckFilterExprCanExecInTSEngine(filter.Condition, ExprPosSelect, m.CheckHelper.whiteList.CheckWhiteListParam) {
				if ret.canTimeBucketOptimize {
					ret.canTimeBucketOptimize = m.checkFilterOptTimeBucket(filter.Condition)
				}
				source.Filters[i].SetEngineTS()
			} else {
				selfExecInTS = false
				ret.canTimeBucketOptimize = false
			}
		}
	}

	// has the columns that not belong to this table, so memo.SelectExpr can not execute in ts engine
	if !source.Relational().OuterCols.Empty() {
		addSynchronize(&ret.hasAddSynchronizer, source.Input)
		ret.execInTSEngine = false
		return ret
	}

	// all condition can execute in ts engine
	if ret.execInTSEngine {
		if selfExecInTS {
			source.SetEngineTS()
		} else {
			addSynchronize(&ret.hasAddSynchronizer, source.Input)
		}
	}

	ret.execInTSEngine = selfExecInTS

	return ret
}

// checkProject check if memo.ProjectExpr can execute in ts engine.
// source is the memo.ProjectExpr of memo tree.
// returns:
// ret: return param struct
func (m *Memo) checkProject(source *ProjectExpr) (ret CrossEngCheckResults) {
	ret = m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
	if ret.err != nil {
		return ret
	}

	var param GetSubQueryExpr
	param.m = m
	selfExecInTS := ret.execInTSEngine
	old := m.CheckHelper.orderedCols
	m.CheckHelper.orderedCols = opt.ColSet{}
	findTimeBucket := false
	for _, proj := range source.Projections {
		proj.Walk(&param)
		if param.hasSub && !m.CheckFlag(opt.ScalarSubQueryPush) {
			selfExecInTS = false
		}
		if ret.execInTSEngine {
			// check if element of ProjectionExpr can execute in ts engine.
			if execInTSEngine, hashcode := CheckExprCanExecInTSEngine(proj.Element.(opt.Expr), ExprPosProjList,
				m.CheckHelper.whiteList.CheckWhiteListParam, false); execInTSEngine {
				m.AddColumn(proj.Col, "", GetExprType(proj.Element), ExprPosProjList, hashcode, false)
			} else {
				selfExecInTS = false
			}
		}

		// case: check if the element is time_bucket function when where need optimize time_bucket.
		if ret.canTimeBucketOptimize {
			if proj.Element.Op() == opt.FunctionOp {
				f := proj.Element.(*FunctionExpr)
				if f.Name != tree.FuncTimeBucket {
					ret.canTimeBucketOptimize = false
				} else {
					if v, ok := m.CheckHelper.PushHelper.Find(proj.Col); ok {
						m.AddColumn(proj.Col, v.Alias, v.Type, v.Pos, v.Hash, true)
						m.CheckHelper.orderedCols.Add(proj.Col)
					}
					findTimeBucket = true
				}
			} else if proj.Element.Op() == opt.ConstOp {
				if v, ok := m.CheckHelper.PushHelper.Find(proj.Col); ok {
					m.AddColumn(proj.Col, v.Alias, v.Type, v.Pos, v.Hash, true)
					//m.CheckHelper.orderedCols.Add(proj.Col)
				}
			} else {
				ret.canTimeBucketOptimize = false
			}
		}
	}

	// has not time_bucket function
	if !findTimeBucket {
		ret.canTimeBucketOptimize = false
	}

	if selfExecInTS {
		source.SetEngineTS()
	} else {
		if ret.execInTSEngine {
			addSynchronize(&ret.hasAddSynchronizer, source.Input)
		}
	}

	if m.CheckHelper.orderedCols.Empty() {
		m.CheckHelper.orderedCols = old
	}

	ret.execInTSEngine = selfExecInTS
	return ret
}

// checkGrouping check if group cols can execute in ts engine.
// cols is the GroupingCols of (memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr).
// optTimeBucket is true when optimizing query efficiency in time_bucket case,
// optTimeBucket will set true when only group by time_bucket or primary tag column.
func (m *Memo) checkGrouping(cols opt.ColSet, optTimeBucket *bool) bool {
	execInTSEngine := true
	cols.ForEach(func(colID opt.ColumnID) {
		colMeta := m.metadata.ColumnMeta(colID)
		if !m.CheckExecInTS(colID, ExprPosGroupBy) || colMeta.Type.Family() == types.BytesFamily {
			execInTSEngine = false
		}
		if v, ok := m.CheckHelper.PushHelper.Find(colID); ok {
			if !v.IsTimeBucket && !m.metadata.ColumnMeta(colID).IsPrimaryTag() {
				*optTimeBucket = false
			}
		} else {
			*optTimeBucket = false
		}
	})
	if cols.Empty() && (*optTimeBucket) {
		*optTimeBucket = false
	}

	return execInTSEngine
}

// CheckChildExecInTS return true if the child of agg can execute in ts engine.
// srcExpr is the expr of agg
func (m *Memo) CheckChildExecInTS(srcExpr opt.ScalarExpr, hashCode uint32) bool {
	execInTSEngine := false
	if srcExpr.ChildCount() == 0 {
		execInTSEngine = m.CheckHelper.whiteList.CheckWhiteListAll(hashCode, ExprPosProjList, uint32(ExprTypConst))
	} else {
		for j := 0; j < srcExpr.ChildCount(); j++ {
			// case agg(column)
			val, ok := srcExpr.Child(j).(*VariableExpr)
			if ok {
				execInTSEngine = m.CheckExecInTS(val.Col, ExprPosProjList)
				if !execInTSEngine {
					break
				}
				continue
			}

			// case agg(distinct column)
			aggDistinct, ok1 := srcExpr.(*AggDistinctExpr)
			if ok1 {
				execInTSEngine = m.CheckChildExecInTS(aggDistinct.Input, hashCode)
			}
		}
	}
	return execInTSEngine
}

// checkParallelAgg check agg can parallel execute.
// expr is the agg function expr.
// returns:
// param1: return true when the agg can be parallel execute.
// param2: return true when the agg with distinct.
func checkParallelAgg(expr opt.Expr) (bool, bool) {
	switch t := expr.(type) {
	case *MaxExpr, *MinExpr, *SumExpr, *AvgExpr, *CountExpr, *CountRowsExpr,
		*FirstExpr, *FirstRowExpr, *FirstTimeStampExpr, *FirstRowTimeStampExpr,
		*LastExpr, *LastRowExpr, *LastTimeStampExpr, *LastRowTimeStampExpr, *ConstAggExpr:
		return true, false
	case *AggDistinctExpr:
		ok, _ := checkParallelAgg(t.Input)
		return ok, true
	}
	return false, false
}

// checkGroupBy check if memo.SelectExpr can execute in ts engine.
// input is the child of (memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr) of memo tree.
// aggs is the AggregationsExpr of (memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr).
// gp is the GroupingPrivate of (memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr).
// returns:
// ret: return param struct
func (m *Memo) checkGroupBy(
	input RelExpr, aggs *AggregationsExpr, gp *GroupingPrivate,
) (ret aggCrossEngCheckResults) {
	ret.commonRet = m.CheckWhiteListAndAddSynchronizeImp(&input)
	if ret.commonRet.err != nil || m.CheckHelper.GroupHint == keys.ForceRelationalGroup {
		// case: error or hint force group by can not execute in ts engine.
		return ret
	}

	ret.isParallel = true

	// memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr
	// should not parallel when the rows less than ten hundred.
	if input.Relational().Stats.RowCount < 1000 && !ret.commonRet.canTimeBucketOptimize &&
		!m.ForcePushGroupToTSEngine() {
		if ret.commonRet.execInTSEngine {
			addSynchronize(&ret.commonRet.hasAddSynchronizer, input)
		}
	}

	aggExecParallel := false

	m.checkOptTimeBucketFlag(input, &ret.commonRet.canTimeBucketOptimize)

	if ret.commonRet.execInTSEngine {
		// check if group cols can execute in ts engine
		ret.commonRet.execInTSEngine = m.checkGrouping(gp.GroupingCols, &ret.commonRet.canTimeBucketOptimize)
		if ret.commonRet.execInTSEngine {
			// case: child of memo.GroupByExpr or memo.ScalarGroupByExpr or memo.DistinctOnExpr and group cols can execute in ts engine
			// then check if the aggs can execute in ts engine
			for i := 0; i < len(*aggs); i++ {
				srcExpr := (*aggs)[i].Agg
				hashCode := GetExprHash(srcExpr)

				// first: check if child of agg can execute in ts engine.
				// second: check if agg itself can execute in ts engine.
				if !m.CheckChildExecInTS(srcExpr, hashCode) ||
					!m.CheckHelper.whiteList.CheckWhiteListParam(hashCode, ExprPosProjList) {
					if !ret.commonRet.hasAddSynchronizer {
						m.setSynchronizerForChild(input, &ret.commonRet.hasAddSynchronizer)
					}

					ret.commonRet = ret.commonRet.disableExecInTSEngine()
					return ret
				}
				m.AddColumn((*aggs)[i].Col, "", ExprTypeAggOp, ExprPosGroupBy, hashCode, false)
				var aggWithDistinct bool
				aggExecParallel, aggWithDistinct = checkParallelAgg((*aggs)[i].Agg)
				if aggWithDistinct {
					ret.hasDistinct = true
					ret.commonRet.canTimeBucketOptimize = false
					aggExecParallel = false
				}
				ret.isParallel = ret.isParallel && aggExecParallel
			}
			// case: group by can execute in ts engine, but agg can not Parallel.
			if !ret.isParallel && !ret.commonRet.hasAddSynchronizer {
				m.setSynchronizerForChild(input, &ret.commonRet.hasAddSynchronizer)
			}
		} else {
			// case: group cols can not execute in ts engine.
			if !ret.commonRet.hasAddSynchronizer {
				m.setSynchronizerForChild(input, &ret.commonRet.hasAddSynchronizer)
			}
		}

		return ret
	}

	ret.commonRet.execInTSEngine = false
	return ret
}

// setSynchronizerForChild add flag for the child of memo.OrderBy in OrderGroupBy case,
// otherwise , add flag for the child of (memo.GroupByExpr or memo.ScalarGroupByExpr).
// child is the child of (memo.GroupByExpr or memo.ScalarGroupByExpr).
// hasSynchronizer is true when the child have added the flag.
func (m *Memo) setSynchronizerForChild(child RelExpr, hasSynchronizer *bool) {
	if _, ok := child.(*SortExpr); ok {
		// case: OrderGroupBy, set sortExpr to ts engine when single node and set AddSynchronizer of child of sort.
		// only single node, order by can exec in ts engine.
		if m.CheckFlag(opt.SingleMode) || m.CheckHelper.isSingleNode() {
			child.SetEngineTS()
		}
		child.Child(0).(RelExpr).SetAddSynchronizer()
	} else {
		child.SetAddSynchronizer()
	}
	*hasSynchronizer = true
}

// checkJoin check if memo.InnerJoinExpr can execute in ts engine.
// join can not execute in ts engine, so just check child of InnerJoinExpr and add synchronize Expr.
// source is the memo.InnerJoinExpr of memo tree.
// returns:
// ret: return param struct
func (m *Memo) checkJoin(source *InnerJoinExpr) (ret CrossEngCheckResults) {
	return m.checkTwoParams(source.Left, source.Right)
}

// checkSetop check if (UnionAllExpr, UnionExpr, IntersectExpr,
// IntersectAllExpr, ExceptAllExpr, ExceptExpr) can execute in ts engine.
// They can not execute in ts engine, check their child
// left, right: childs of setop expr
// returns:
// ret: return param struct
func (m *Memo) checkSetop(left, right RelExpr) (ret CrossEngCheckResults) {
	return m.checkTwoParams(left, right)
}

func (m *Memo) checkTwoParams(left, right RelExpr) (ret CrossEngCheckResults) {
	lRet := m.CheckWhiteListAndAddSynchronizeImp(&left)
	if lRet.err != nil {
		return lRet
	}

	rRet := m.CheckWhiteListAndAddSynchronizeImp(&right)
	if rRet.err != nil {
		return rRet
	}
	addSynchronizeStruct(&lRet, left)
	addSynchronizeStruct(&rRet, right)
	ret.hasAddSynchronizer = lRet.hasAddSynchronizer || rRet.hasAddSynchronizer
	return ret
}

// checkOtherJoinChildExpr check if the child of (SemiJoinExpr,MergeJoinExpr,LeftJoinApplyExpr,
// RightJoinExpr,InnerJoinApplyExpr,FullJoinExpr,LeftJoinExpr) can execute in ts engine.
// left is the left child of memo.**JoinExpr, right is the right child of memo.**JoinExpr.
// returns:
// ret: return param struct
func (m *Memo) checkOtherJoinChildExpr(left, right RelExpr) (ret CrossEngCheckResults) {
	return m.checkTwoParams(left, right)
}

// checkOtherJoin check if (SemiJoinExpr,MergeJoinExpr,LeftJoinApplyExpr,RightJoinExpr,InnerJoinApplyExpr,FullJoinExpr,LeftJoinExpr)
// can execute in ts engine. they can not execute in ts engine, so just check their child node and add synchronize Expr.
// source is the memo.**JoinExpr of memo tree.
// returns:
// ret: return param struct
func (m *Memo) checkOtherJoin(source RelExpr) (ret CrossEngCheckResults) {
	switch s := source.(type) {
	case *SemiJoinExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *SemiJoinApplyExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *MergeJoinExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *LeftJoinApplyExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *RightJoinExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *InnerJoinApplyExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *FullJoinExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *LeftJoinExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *AntiJoinExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	case *AntiJoinApplyExpr:
		ret = m.checkOtherJoinChildExpr(s.Left, s.Right)
	}
	if ret.err != nil {
		return ret
	}
	return ret
}

// checkLookupJoin check if memo.LookupJoinExpr can execute in ts engine.
// join can not execute in ts engine, so just check child of LookupJoinExpr and add synchronize Expr.
// source is the memo.LookupJoinExpr of memo tree.
// returns:
// ret: return param struct
func (m *Memo) checkLookupJoin(source *LookupJoinExpr) CrossEngCheckResults {
	ret := m.CheckWhiteListAndAddSynchronizeImp(&source.Input)
	if ret.err != nil {
		return ret
	}

	addSynchronizeStruct(&ret, source.Input)

	return ret
}

// checkFilterOptTimeBucket check if only timestamp col in filter.
// Only in this way can time_bucket optimization be used.
// expr is filter expr.
func (m *Memo) checkFilterOptTimeBucket(expr opt.Expr) bool {
	switch expr.Op() {
	case opt.AndOp, opt.OrOp, opt.RangeOp:
		for i := 0; i < expr.ChildCount(); i++ {
			if !m.checkFilterOptTimeBucket(expr.Child(i)) {
				return false
			}
		}
		return true
	case opt.EqOp, opt.GeOp, opt.GtOp, opt.LeOp, opt.LtOp, opt.NeOp:
		for i := 0; i < expr.ChildCount(); i++ {
			if expr.Child(i).Op() == opt.VariableOp {
				v := expr.Child(i).(*VariableExpr)
				tableID := m.Metadata().ColumnMeta(v.Col).Table
				if v.Col == tableID.ColumnID(0) {
					return true
				}
			}
		}
	}
	return false
}

// checkOptTimeBucketFlag set optTimeBucket = false when haven't time_bucket, should not use special operator.
// input is the child expr of group by expr
func (m *Memo) checkOptTimeBucketFlag(input RelExpr, optTimeBucket *bool) {
	checkProject := func(pro *ProjectExpr) {
		for _, v := range pro.Projections {
			if tb, ok := m.CheckHelper.PushHelper.Find(v.Col); ok {
				if !tb.IsTimeBucket {
					*optTimeBucket = false
				}
			} else {
				*optTimeBucket = false
			}
		}
	}
	if project, ok := input.(*ProjectExpr); ok {
		checkProject(project)
	} else if sort, ok := input.(*SortExpr); ok {
		if project, ok := sort.Input.(*ProjectExpr); ok {
			checkProject(project)
		} else {
			*optTimeBucket = false
		}
	} else {
		*optTimeBucket = false
	}
}

// CalculateDop is used to calculate degree dynamically based on statistics
// rowCount is the RowCount of memo.TSScanExpr.
// pTagCount is the PTagCount of memo.TSScanExpr.
// allColsWidth is width of all columns.
func (m *Memo) CalculateDop(rowCount float64, pTagCount float64, allColsWidth uint32) {
	var parallelNum uint32

	// Gets the number of cores for the cpu
	cpuCores := runtime.NumCPU()

	// Adjust the parallel num based on the amount of data
	switch {
	case rowCount <= sqlbase.LowDataThreshold:
		parallelNum = sqlbase.MaxDopForLowData
	case rowCount < sqlbase.HighDataThreshold:
		scaleFactor := (rowCount - sqlbase.LowDataThreshold) / (sqlbase.HighDataThreshold - sqlbase.LowDataThreshold)
		parallelNum = uint32(2 + scaleFactor*(pTagCount-2))
		if parallelNum > sqlbase.MaxDopForHighData {
			parallelNum = sqlbase.MaxDopForHighData
		}
	default:
		// If the number of rows exceeds the high threshold, set the parallel num to the number of PTags
		parallelNum = uint32(pTagCount)
	}

	// Adjust the parallel num based on the wait memory and thread
	v, err := mem.VirtualMemory()
	if err != nil {
		m.SetTsDop(sqlbase.DefaultDop)
		return
	}
	availableRAM := v.Free
	eachParallelMemory := (rowCount / float64(parallelNum)) * float64(allColsWidth)
	maxParallelNum := float64(availableRAM) / eachParallelMemory

	// The parallel num cannot exceed the minimum number of devices and CPU cores
	parallelNum = uint32(math.Min(float64(parallelNum), math.Min(float64(cpuCores), maxParallelNum)))
	if parallelNum > m.tsDop {
		m.tsDop = parallelNum
	}
}

// GetTsDop is used to get degree of parallelism
func (m *Memo) GetTsDop() uint32 {
	return m.tsDop
}

// SetTsDop is used to set degree of parallelism
func (m *Memo) SetTsDop(num uint32) {
	m.tsDop = num
}

// InsideOutOptHelper records agg functions and projections which can push down ts engine side.
type InsideOutOptHelper struct {
	Aggs     []AggregationsItem
	Grouping opt.ColSet

	// AggArgs records the mapping between agg types and parameter's columnID
	// When agg is sum/count/avg and the parameter is the projection of the relational engine,
	// the agg cannot be optimized by push-down
	AggArgs []AggArgHelper

	// ProEngine records the execution engine of each projection.
	// When the projection layer is in time series,
	// it will build the projection layer on tsScan.
	// When the projection layer is in relation,
	// it will build the projection layer on inner join.
	ProEngine []tree.EngineType

	Projections ProjectionsExpr
	Passthrough opt.ColSet
}

// AggArgHelper records agg function operator and its argument's column ID
type AggArgHelper struct {
	AggOp    opt.Operator
	ArgColID opt.ColumnID
}
