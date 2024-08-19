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

package sql

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/gossip"
	"gitee.com/kwbasedb/kwbase/pkg/jobs/jobspb"
	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvclient/kvcoord"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/rpc"
	"gitee.com/kwbasedb/kwbase/pkg/rpc/nodedialer"
	"gitee.com/kwbasedb/kwbase/pkg/server/serverpb"
	"gitee.com/kwbasedb/kwbase/pkg/settings"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/distsql"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/hashrouter/api"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/optbuilder"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/physicalplan"
	"gitee.com/kwbasedb/kwbase/pkg/sql/physicalplan/replicaoracle"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/util"
	"gitee.com/kwbasedb/kwbase/pkg/util/encoding"
	"gitee.com/kwbasedb/kwbase/pkg/util/envutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/protoutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
	"github.com/cockroachdb/errors"
)

// DistSQLPlanner is used to generate distributed plans from logical
// plans. A rough overview of the process:
//
//   - the plan is based on a planNode tree (in the future it will be based on an
//     intermediate representation tree). Only a subset of the possible trees is
//     supported (this can be checked via CheckSupport).
//
//   - we generate a PhysicalPlan for the planNode tree recursively. The
//     PhysicalPlan consists of a network of processors and streams, with a set
//     of unconnected "result routers". The PhysicalPlan also has information on
//     ordering and on the mapping planNode columns to columns in the result
//     streams (all result routers output streams with the same schema).
//
//     The PhysicalPlan for a scanNode leaf consists of TableReaders, one for each node
//     that has one or more ranges.
//
//   - for each an internal planNode we start with the plan of the child node(s)
//     and add processing stages (connected to the result routers of the children
//     node).
type DistSQLPlanner struct {
	// planVersion is the version of DistSQL targeted by the plan we're building.
	// This is currently only assigned to the node's current DistSQL version and
	// is used to skip incompatible nodes when mapping spans.
	planVersion execinfrapb.DistSQLVersion

	st *cluster.Settings
	// The node descriptor for the gateway node that initiated this query.
	nodeDesc     roachpb.NodeDescriptor
	stopper      *stop.Stopper
	distSQLSrv   *distsql.ServerImpl
	spanResolver physicalplan.SpanResolver

	// metadataTestTolerance is the minimum level required to plan metadata test
	// processors.
	metadataTestTolerance execinfra.MetadataTestLevel

	// runnerChan is used to send out requests (for running SetupFlow RPCs) to a
	// pool of workers.
	runnerChan chan runnerRequest

	// gossip handle used to check node version compatibility and to construct
	// the spanResolver.
	gossip *gossip.Gossip

	nodeDialer *nodedialer.Dialer

	// nodeHealth encapsulates the various node health checks to avoid planning
	// on unhealthy nodes.
	nodeHealth distSQLNodeHealth

	// distSender is used to construct the spanResolver upon SetNodeDesc.
	distSender *kvcoord.DistSender
	// rpcCtx is used to construct the spanResolver upon SetNodeDesc.
	rpcCtx        *rpc.Context
	gatewayNodeID roachpb.NodeID
}

// UintSlice is uint32 slice
type UintSlice []uint32

// Len is implement of Interface.
func (s UintSlice) Len() int {
	return len(s)
}

// Less is implement of Interface.
func (s UintSlice) Less(i, j int) bool {
	return s[i] < s[j]
}

// Swap is implement of Interface.
func (s UintSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// ReplicaOraclePolicy controls which policy the physical planner uses to choose
// a replica for a given range. It is exported so that it may be overwritten
// during initialization by CCL code to enable follower reads.
var ReplicaOraclePolicy = replicaoracle.BinPackingChoice

// If true, the plan diagram (in JSON) is logged for each plan (used for
// debugging).
var logPlanDiagram = envutil.EnvOrDefaultBool("KWBASE_DISTSQL_LOG_PLAN", false)

// If true, for index joins we instantiate a join reader on every node that
// has a stream (usually from a table reader). If false, there is a single join
// reader.
var distributeIndexJoin = settings.RegisterBoolSetting(
	"sql.distsql.distribute_index_joins",
	"if set, for index joins we instantiate a join reader on every node that has a "+
		"stream; if not set, we use a single join reader",
	true,
)

var planMergeJoins = settings.RegisterBoolSetting(
	"sql.distsql.merge_joins.enabled",
	"if set, we plan merge joins when possible",
	true,
)

// livenessProvider provides just the methods of storage.NodeLiveness that the
// DistSQLPlanner needs, to avoid importing all of storage.
type livenessProvider interface {
	IsLive(roachpb.NodeID) (bool, error)
}

// NewDistSQLPlanner initializes a DistSQLPlanner.
//
// nodeDesc is the descriptor of the node on which this planner runs. It is used
// to favor itself and other close-by nodes when planning. An empty descriptor
// can be passed to aid bootstrapping, but then SetNodeDesc() needs to be called
// before this planner is used.
func NewDistSQLPlanner(
	ctx context.Context,
	planVersion execinfrapb.DistSQLVersion,
	st *cluster.Settings,
	nodeDesc roachpb.NodeDescriptor,
	rpcCtx *rpc.Context,
	distSQLSrv *distsql.ServerImpl,
	distSender *kvcoord.DistSender,
	gossip *gossip.Gossip,
	stopper *stop.Stopper,
	liveness livenessProvider,
	nodeDialer *nodedialer.Dialer,
) *DistSQLPlanner {
	if liveness == nil {
		log.Fatal(ctx, "must specify liveness")
	}
	dsp := &DistSQLPlanner{
		planVersion: planVersion,
		st:          st,
		nodeDesc:    nodeDesc,
		stopper:     stopper,
		distSQLSrv:  distSQLSrv,
		gossip:      gossip,
		nodeDialer:  nodeDialer,
		nodeHealth: distSQLNodeHealth{
			gossip:     gossip,
			connHealth: nodeDialer.ConnHealth,
		},
		distSender:            distSender,
		rpcCtx:                rpcCtx,
		metadataTestTolerance: execinfra.NoExplain,
	}
	dsp.nodeHealth.isLive = liveness.IsLive
	dsp.gatewayNodeID = nodeDesc.NodeID

	dsp.initRunners()
	return dsp
}

func (dsp *DistSQLPlanner) shouldPlanTestMetadata() bool {
	return dsp.distSQLSrv.TestingKnobs.MetadataTestLevel >= dsp.metadataTestTolerance
}

// SetNodeDesc sets the planner's node descriptor.
// The first call to SetNodeDesc leads to the construction of the SpanResolver.
func (dsp *DistSQLPlanner) SetNodeDesc(desc roachpb.NodeDescriptor) {
	dsp.nodeDesc = desc
	if dsp.spanResolver == nil {
		sr := physicalplan.NewSpanResolver(dsp.st, dsp.distSender, dsp.gossip, desc,
			dsp.rpcCtx, ReplicaOraclePolicy)
		dsp.SetSpanResolver(sr)
	}
}

// SetSpanResolver switches to a different SpanResolver. It is the caller's
// responsibility to make sure the DistSQLPlanner is not in use.
func (dsp *DistSQLPlanner) SetSpanResolver(spanResolver physicalplan.SpanResolver) {
	dsp.spanResolver = spanResolver
}

// distSQLExprCheckVisitor is a tree.Visitor that checks if expressions
// contain things not supported by distSQL (like subqueries).
type distSQLExprCheckVisitor struct {
	err error
}

var _ tree.Visitor = &distSQLExprCheckVisitor{}

func (v *distSQLExprCheckVisitor) VisitPre(expr tree.Expr) (recurse bool, newExpr tree.Expr) {
	if v.err != nil {
		return false, expr
	}
	switch t := expr.(type) {
	case *tree.FuncExpr:
		if t.IsDistSQLBlacklist() {
			v.err = newQueryNotSupportedErrorf("function %s cannot be executed with distsql", t)
			return false, expr
		}
	case *tree.DOid:
		v.err = newQueryNotSupportedError("OID expressions are not supported by distsql")
		return false, expr
	case *tree.CastExpr:
		if t.Type.Family() == types.OidFamily {
			v.err = newQueryNotSupportedErrorf("cast to %s is not supported by distsql", t.Type)
			return false, expr
		}
	}
	return true, expr
}

func (v *distSQLExprCheckVisitor) VisitPost(expr tree.Expr) tree.Expr { return expr }

// checkExpr verifies that an expression doesn't contain things that are not yet
// supported by distSQL, like subqueries.
func (dsp *DistSQLPlanner) checkExpr(expr tree.Expr) error {
	if expr == nil {
		return nil
	}
	v := distSQLExprCheckVisitor{}
	tree.WalkExprConst(&v, expr)
	return v.err
}

type distRecommendation int

const (
	// cannotDistribute indicates that a plan cannot be distributed.
	cannotDistribute distRecommendation = iota

	// shouldNotDistribute indicates that a plan could suffer if distributed.
	shouldNotDistribute

	// canDistribute indicates that a plan will probably not benefit but will
	// probably not suffer if distributed.
	canDistribute

	// shouldDistribute indicates that a plan will likely benefit if distributed.
	shouldDistribute
)

// compose returns the recommendation for a plan given recommendations for two
// parts of it: if we shouldNotDistribute either part, then we
// shouldNotDistribute the overall plan either.
func (a distRecommendation) compose(b distRecommendation) distRecommendation {
	if a == cannotDistribute || b == cannotDistribute {
		return cannotDistribute
	}
	if a == shouldNotDistribute || b == shouldNotDistribute {
		return shouldNotDistribute
	}
	if a == shouldDistribute || b == shouldDistribute {
		return shouldDistribute
	}
	return canDistribute
}

type queryNotSupportedError struct {
	msg string
}

func (e *queryNotSupportedError) Error() string {
	return e.msg
}

func newQueryNotSupportedError(msg string) error {
	return &queryNotSupportedError{msg: msg}
}

func newQueryNotSupportedErrorf(format string, args ...interface{}) error {
	return &queryNotSupportedError{msg: fmt.Sprintf(format, args...)}
}

// planNodeNotSupportedErr is the catch-all error value returned from
// checkSupportForNode when a planNode type does not support distributed
// execution.
var planNodeNotSupportedErr = newQueryNotSupportedError("unsupported node")

var cannotDistributeRowLevelLockingErr = newQueryNotSupportedError(
	"scans with row-level locking are not supported by distsql",
)

// mustWrapNode returns true if a node has no DistSQL-processor equivalent.
// This must be kept in sync with createPlanForNode.
// TODO(jordan): refactor these to use the observer pattern to avoid duplication.
func (dsp *DistSQLPlanner) mustWrapNode(planCtx *PlanningCtx, node planNode) bool {
	switch n := node.(type) {
	// Keep these cases alphabetized, please!
	case *distinctNode:
	case *exportNode:
	case *filterNode:
	case *groupNode:
	case *indexJoinNode:
	case *joinNode:
	case *limitNode:
	case *lookupJoinNode:
	case *ordinalityNode:
	case *projectSetNode:
	case *renderNode:
	case *scanNode:
	case *tsScanNode:
	case *synchronizerNode:
	case *sortNode:
	case *unaryNode:
	case *unionNode:
	case *valuesNode:
		// This is unfortunately duplicated by createPlanForNode, and must be kept
		// in sync with its implementation.
		if !n.specifiedInQuery || planCtx.isLocal || planCtx.noEvalSubqueries {
			return true
		}
		return false
	case *windowNode:
	case *zeroNode:
	case *zigzagJoinNode:
	default:
		return true
	}
	return false
}

// checkSupportForNode returns a distRecommendation (as described above) or
// cannotDistribute and an error if the plan subtree is not distributable.
// The error doesn't indicate complete failure - it's instead the reason that
// this plan couldn't be distributed.
// TODO(radu): add tests for this.
func (dsp *DistSQLPlanner) checkSupportForNode(node planNode) (distRecommendation, error) {
	switch n := node.(type) {
	// Keep these cases alphabetized, please!
	case *distinctNode:
		return dsp.checkSupportForNode(n.plan)

	case *exportNode:
		return dsp.checkSupportForNode(n.source)

	case *filterNode:
		if err := dsp.checkExpr(n.filter); err != nil {
			return cannotDistribute, err
		}
		return dsp.checkSupportForNode(n.source.plan)

	case *groupNode:
		rec, err := dsp.checkSupportForNode(n.plan)
		if err != nil {
			return cannotDistribute, err
		}
		// Distribute aggregations if possible.
		return rec.compose(shouldDistribute), nil
	case *synchronizerNode:
		rec, err := dsp.checkSupportForNode(n.plan)
		if err != nil {
			return cannotDistribute, err
		}
		// Distribute aggregations if possible.
		return rec.compose(shouldDistribute), nil
	case *indexJoinNode:
		// n.table doesn't have meaningful spans, but we need to check support (e.g.
		// for any filtering expression).
		if _, err := dsp.checkSupportForNode(n.table); err != nil {
			return cannotDistribute, err
		}
		return dsp.checkSupportForNode(n.input)

	case *joinNode:
		if err := dsp.checkExpr(n.pred.onCond); err != nil {
			return cannotDistribute, err
		}
		recLeft, err := dsp.checkSupportForNode(n.left.plan)
		if err != nil {
			return cannotDistribute, err
		}
		recRight, err := dsp.checkSupportForNode(n.right.plan)
		if err != nil {
			return cannotDistribute, err
		}
		// If either the left or the right side can benefit from distribution, we
		// should distribute.
		rec := recLeft.compose(recRight)
		// If we can do a hash join, we distribute if possible.
		if len(n.pred.leftEqualityIndices) > 0 {
			rec = rec.compose(shouldDistribute)
		}
		return rec, nil

	case *limitNode:
		if err := dsp.checkExpr(n.countExpr); err != nil {
			return cannotDistribute, err
		}
		if err := dsp.checkExpr(n.offsetExpr); err != nil {
			return cannotDistribute, err
		}
		return dsp.checkSupportForNode(n.plan)

	case *lookupJoinNode:
		if err := dsp.checkExpr(n.onCond); err != nil {
			return cannotDistribute, err
		}
		if _, err := dsp.checkSupportForNode(n.input); err != nil {
			return cannotDistribute, err
		}
		return shouldDistribute, nil

	case *projectSetNode:
		return dsp.checkSupportForNode(n.source)

	case *renderNode:
		for _, e := range n.render {
			if err := dsp.checkExpr(e); err != nil {
				return cannotDistribute, err
			}
		}
		return dsp.checkSupportForNode(n.source.plan)

	case *scanNode:
		if n.lockingStrength != sqlbase.ScanLockingStrength_FOR_NONE {
			// Scans that are performing row-level locking cannot currently be
			// distributed because their locks would not be propagated back to
			// the root transaction coordinator.
			// TODO(nvanbenschoten): lift this restriction.
			return cannotDistribute, cannotDistributeRowLevelLockingErr
		}

		// Although we don't yet recommend distributing plans where soft limits
		// propagate to scan nodes because we don't have infrastructure to only
		// plan for a few ranges at a time, the propagation of the soft limits
		// to scan nodes has been added in 20.1 release, so to keep the
		// previous behavior we continue to ignore the soft limits for now.
		// TODO(yuzefovich): pay attention to the soft limits.
		rec := canDistribute
		// We recommend running scans distributed if we have a filtering
		// expression or if we have a full table scan.
		if n.filter != nil {
			if err := dsp.checkExpr(n.filter); err != nil {
				return cannotDistribute, err
			}
			rec = rec.compose(shouldDistribute)
		}
		// Check if we are doing a full scan.
		if len(n.spans) == 1 && n.spans[0].EqualValue(n.desc.IndexSpan(n.index.ID)) {
			rec = rec.compose(shouldDistribute)
		}
		return rec, nil

	case *sortNode:
		rec, err := dsp.checkSupportForNode(n.plan)
		if err != nil {
			return cannotDistribute, err
		}
		// If we have to sort, distribute the query.
		rec = rec.compose(shouldDistribute)
		return rec, nil

	case *unaryNode:
		return canDistribute, nil

	case *unionNode:
		recLeft, err := dsp.checkSupportForNode(n.left)
		if err != nil {
			return cannotDistribute, err
		}
		recRight, err := dsp.checkSupportForNode(n.right)
		if err != nil {
			return cannotDistribute, err
		}
		return recLeft.compose(recRight), nil

	case *valuesNode:
		if !n.specifiedInQuery {
			// This condition indicates that the valuesNode was created by planning,
			// not by the user, like the way vtables are expanded into valuesNodes. We
			// don't want to distribute queries like this across the network.
			return cannotDistribute, newQueryNotSupportedErrorf("unsupported valuesNode, not specified in query")
		}

		for _, tuple := range n.tuples {
			for _, expr := range tuple {
				if err := dsp.checkExpr(expr); err != nil {
					return cannotDistribute, err
				}
			}
		}
		return canDistribute, nil

	case *windowNode:
		return dsp.checkSupportForNode(n.plan)

	case *zeroNode:
		return canDistribute, nil

	case *zigzagJoinNode:
		if err := dsp.checkExpr(n.onCond); err != nil {
			return cannotDistribute, err
		}
		return shouldDistribute, nil

	case *tsScanNode:
		return shouldDistribute, nil

	case *tsInsertNode:
		return shouldDistribute, nil

	case *tsDeleteNode:
		if n.wrongPTag {
			return cannotDistribute, nil
		}
		return shouldDistribute, nil

	case *tsTagUpdateNode:
		if n.wrongPTag {
			return cannotDistribute, nil
		}
		return shouldDistribute, nil

	case *tsDDLNode:
		return shouldDistribute, nil

	case *operateDataNode:
		return shouldDistribute, nil

	case *tsInsertSelectNode:
		return dsp.checkSupportForNode(n.plan)
	default:
		return cannotDistribute, planNodeNotSupportedErr
	}
}

// PlanningCtx contains data used and updated throughout the planning process of
// a single query.
type PlanningCtx struct {
	ctx             context.Context
	ExtendedEvalCtx *extendedEvalContext
	spanIter        physicalplan.SpanResolverIterator
	// NodeAddresses contains addresses for all NodeIDs that are referenced by any
	// PhysicalPlan we generate with this context.
	// Nodes that fail a health check have empty addresses.
	NodeAddresses map[roachpb.NodeID]string

	// isLocal is set to true if we're planning this query on a single node.
	isLocal bool
	planner *planner
	// ignoreClose, when set to true, will prevent the closing of the planner's
	// current plan. Only the top-level query needs to close it, but everything
	// else (like sub- and postqueries, or EXPLAIN ANALYZE) should set this to
	// true to avoid double closes of the planNode tree.
	ignoreClose bool
	stmtType    tree.StatementType
	// planDepth is set to the current depth of the planNode tree. It's used to
	// keep track of whether it's valid to run a root node in a special fast path
	// mode.
	planDepth int

	// noEvalSubqueries indicates that the plan expects any subqueries to not
	// be replaced by evaluation. Should only be set by EXPLAIN.
	noEvalSubqueries bool

	// If set, a diagram for the plan will be generated and passed to this
	// function.
	saveDiagram func(execinfrapb.FlowDiagram)
	// If set, the diagram passed to saveDiagram will show the types of each
	// stream.
	saveDiagramShowInputTypes bool

	// IsTS is set true when there has tsTableReader
	IsTS bool
}

var _ physicalplan.ExprContext = &PlanningCtx{}

// EvalContext returns the associated EvalContext, or nil if there isn't one.
func (p *PlanningCtx) EvalContext() *tree.EvalContext {
	if p.ExtendedEvalCtx == nil {
		return nil
	}
	return &p.ExtendedEvalCtx.EvalContext
}

// IsLocal returns true if this PlanningCtx is being used to plan a query that
// has no remote flows.
func (p *PlanningCtx) IsLocal() bool {
	return p.isLocal
}

// EvaluateSubqueries returns true if this plan requires subqueries be fully
// executed before trying to marshal. This is normally true except for in the
// case of EXPLAIN queries, which ultimately want to describe the subquery that
// will run, without actually running it.
func (p *PlanningCtx) EvaluateSubqueries() bool {
	return !p.noEvalSubqueries
}

// IsTs return true if there has tsTableReader.
func (p *PlanningCtx) IsTs() bool {
	return p.IsTS
}

// GetTsDop is used to get tsParallelExecDegree num in timing scenarios
func (p *PlanningCtx) GetTsDop() int32 {
	if !p.IsTS || p.planner == nil {
		return sqlbase.DefaultDop
	}

	settingsTmp := p.planner.extendedEvalCtx.Settings
	if settingsTmp == nil {
		return sqlbase.DefaultDop
	}
	// If a specific TimeSeries parallel tsParallelExecDegree is set, return it.
	if tsDop := opt.TSParallelDegree.Get(&settingsTmp.SV); tsDop > 0 {
		return int32(tsDop)
	}

	// Returns the parallel num estimated based on statistics
	// and system resource(number of wait threads in the execution pool)
	if memo := p.planner.curPlan.mem; memo != nil {
		waitThreadNum, err := p.planner.ExecCfg().TsEngine.TSGetWaitThreadNum()
		if err != nil {
			return sqlbase.DefaultDop
		}
		return int32(math.Min(float64(waitThreadNum), float64(memo.GetTsDop())))
	}
	return sqlbase.DefaultDop
}

// sanityCheckAddresses returns an error if the same address is used by two
// nodes.
func (p *PlanningCtx) sanityCheckAddresses() error {
	inverted := make(map[string]roachpb.NodeID)
	for nodeID, addr := range p.NodeAddresses {
		if otherNodeID, ok := inverted[addr]; ok {
			return errors.Errorf(
				"different nodes %d and %d with the same address '%s'", nodeID, otherNodeID, addr)
		}
		inverted[addr] = nodeID
	}
	return nil
}

// PhysicalPlan is a partial physical plan which corresponds to a planNode
// (partial in that it can correspond to a planNode subtree and not necessarily
// to the entire planNode for a given query).
//
// It augments physicalplan.PhysicalPlan with information relating the physical
// plan to a planNode subtree.
//
// These plans are built recursively on a planNode tree.
type PhysicalPlan struct {
	physicalplan.PhysicalPlan

	// PlanToStreamColMap maps planNode columns (see planColumns()) to columns in
	// the result streams. These stream indices correspond to the streams
	// referenced in ResultTypes.
	//
	// Note that in some cases, not all columns in the result streams are
	// referenced in the map; for example, columns that are only required for
	// stream merges in downstream input synchronizers are not included here.
	// (This is due to some processors not being configurable to output only
	// certain columns and will be fixed.)
	//
	// Conversely, in some cases not all planNode columns have a corresponding
	// result stream column (these map to index -1); this is the case for scanNode
	// and indexJoinNode where not all columns in the table are actually used in
	// the plan, but are kept for possible use downstream (e.g., sorting).
	//
	// When the query is run, the output processor's PlanToStreamColMap is used
	// by DistSQLReceiver to create an implicit projection on the processor's
	// output for client consumption (see DistSQLReceiver.Push()). Therefore,
	// "invisible" columns (e.g., columns required for merge ordering) will not
	// be output.
	PlanToStreamColMap []int
}

// makePlanToStreamColMap initializes a new PhysicalPlan.PlanToStreamColMap. The
// columns that are present in the result stream(s) should be set in the map.
func makePlanToStreamColMap(numCols int) []int {
	m := make([]int, numCols)
	for i := 0; i < numCols; i++ {
		m[i] = -1
	}
	return m
}

// identityMap returns the slice {0, 1, 2, ..., numCols-1}.
// buf can be optionally provided as a buffer.
func identityMap(buf []int, numCols int) []int {
	buf = buf[:0]
	for i := 0; i < numCols; i++ {
		buf = append(buf, i)
	}
	return buf
}

// identityMapInPlace returns the modified slice such that it contains
// {0, 1, ..., len(slice)-1}.
func identityMapInPlace(slice []int) []int {
	for i := range slice {
		slice[i] = i
	}
	return slice
}

// SpanPartition is the intersection between a set of spans for a certain
// operation (e.g table scan) and the set of ranges owned by a given node.
type SpanPartition struct {
	Node  roachpb.NodeID
	Spans roachpb.Spans
}

type distSQLNodeHealth struct {
	gossip     *gossip.Gossip
	isLive     func(roachpb.NodeID) (bool, error)
	connHealth func(roachpb.NodeID, rpc.ConnectionClass) error
}

// GetNodeDescriptor gets a node descriptor by node ID.
func (dsp *DistSQLPlanner) GetNodeDescriptor(
	nodeID roachpb.NodeID,
) (*roachpb.NodeDescriptor, error) {
	return dsp.gossip.GetNodeDescriptor(nodeID)
}

func (h *distSQLNodeHealth) check(ctx context.Context, nodeID roachpb.NodeID) error {
	{
		// NB: as of #22658, ConnHealth does not work as expected; see the
		// comment within. We still keep this code for now because in
		// practice, once the node is down it will prevent using this node
		// 90% of the time (it gets used around once per second as an
		// artifact of rpcContext's reconnection mechanism at the time of
		// writing). This is better than having it used in 100% of cases
		// (until the liveness check below kicks in).
		err := h.connHealth(nodeID, rpc.DefaultClass)
		if err != nil && err != rpc.ErrNotHeartbeated {
			// This host is known to be unhealthy. Don't use it (use the gateway
			// instead). Note: this can never happen for our nodeID (which
			// always has its address in the nodeMap).
			log.VEventf(ctx, 1, "marking n%d as unhealthy for this plan: %v", nodeID, err)
			return err
		}
	}
	{
		live, err := h.isLive(nodeID)
		if err == nil && !live {
			err = pgerror.Newf(pgcode.CannotConnectNow,
				"node n%d is not live", errors.Safe(nodeID))
		}
		if err != nil {
			return pgerror.Wrapf(err, pgcode.CannotConnectNow,
				"not using n%d due to liveness", errors.Safe(nodeID))
		}
	}

	// Check that the node is not draining.
	drainingInfo := &execinfrapb.DistSQLDrainingInfo{}
	if err := h.gossip.GetInfoProto(gossip.MakeDistSQLDrainingKey(nodeID), drainingInfo); err != nil {
		// Because draining info has no expiration, an error
		// implies that we have not yet received a node's
		// draining information. Since this information is
		// written on startup, the most likely scenario is
		// that the node is ready. We therefore return no
		// error.
		// TODO(ajwerner): Determine the expected error types and only filter those.
		return nil //nolint:returnerrcheck
	}

	if drainingInfo.Draining {
		errMsg := fmt.Sprintf("not using n%d because it is draining", nodeID)
		log.VEvent(ctx, 1, errMsg)
		return errors.New(errMsg)
	}

	return nil
}

// PartitionSpans finds out which nodes are owners for ranges touching the
// given spans, and splits the spans according to owning nodes. The result is a
// set of SpanPartitions (guaranteed one for each relevant node), which form a
// partitioning of the spans (i.e. they are non-overlapping and their union is
// exactly the original set of spans).
//
// PartitionSpans does its best to not assign ranges on nodes that are known to
// either be unhealthy or running an incompatible version. The ranges owned by
// such nodes are assigned to the gateway.
func (dsp *DistSQLPlanner) PartitionSpans(
	planCtx *PlanningCtx, spans roachpb.Spans,
) ([]SpanPartition, error) {
	if len(spans) == 0 {
		panic("no spans")
	}
	ctx := planCtx.ctx
	partitions := make([]SpanPartition, 0, 1)
	if planCtx.isLocal {
		// If we're planning locally, map all spans to the local node.
		partitions = append(partitions,
			SpanPartition{dsp.nodeDesc.NodeID, spans})
		return partitions, nil
	}
	// nodeMap maps a nodeID to an index inside the partitions array.
	nodeMap := make(map[roachpb.NodeID]int)
	// nodeVerCompatMap maintains info about which nodes advertise DistSQL
	// versions compatible with this plan and which ones don't.
	nodeVerCompatMap := make(map[roachpb.NodeID]bool)
	it := planCtx.spanIter
	for _, span := range spans {
		// rspan is the span we are currently partitioning.
		var rspan roachpb.RSpan
		var err error
		if rspan.Key, err = keys.Addr(span.Key); err != nil {
			return nil, err
		}
		if rspan.EndKey, err = keys.Addr(span.EndKey); err != nil {
			return nil, err
		}

		var lastNodeID roachpb.NodeID
		// lastKey maintains the EndKey of the last piece of `span`.
		lastKey := rspan.Key
		if log.V(1) {
			log.Infof(ctx, "partitioning span %s", span)
		}
		// We break up rspan into its individual ranges (which may or
		// may not be on separate nodes). We then create "partitioned
		// spans" using the end keys of these individual ranges.
		for it.Seek(ctx, span, kvcoord.Ascending); ; it.Next(ctx) {
			if !it.Valid() {
				return nil, it.Error()
			}
			replInfo, err := it.ReplicaInfo(ctx)
			if err != nil {
				return nil, err
			}
			desc := it.Desc()
			if log.V(1) {
				descCpy := desc // don't let desc escape
				log.Infof(ctx, "lastKey: %s desc: %s", lastKey, &descCpy)
			}

			if !desc.ContainsKey(lastKey) {
				// This range must contain the last range's EndKey.
				log.Fatalf(
					ctx, "next range %v doesn't cover last end key %v. Partitions: %#v",
					desc.RSpan(), lastKey, partitions,
				)
			}

			// Limit the end key to the end of the span we are resolving.
			endKey := desc.EndKey
			if rspan.EndKey.Less(endKey) {
				endKey = rspan.EndKey
			}

			nodeID := replInfo.NodeDesc.NodeID
			partitionIdx, inNodeMap := nodeMap[nodeID]
			if !inNodeMap {
				// This is the first time we are seeing nodeID for these spans. Check
				// its health.
				addr, inAddrMap := planCtx.NodeAddresses[nodeID]
				if !inAddrMap {
					addr = replInfo.NodeDesc.Address.String()
					if err := dsp.nodeHealth.check(ctx, nodeID); err != nil {
						addr = ""
					}
					if addr != "" {
						planCtx.NodeAddresses[nodeID] = addr
					}
				}
				compat := true
				if addr != "" {
					// Check if the node's DistSQL version is compatible with this plan.
					// If it isn't, we'll use the gateway.
					var ok bool
					if compat, ok = nodeVerCompatMap[nodeID]; !ok {
						compat = dsp.nodeVersionIsCompatible(nodeID, dsp.planVersion)
						nodeVerCompatMap[nodeID] = compat
					}
				}
				// If the node is unhealthy or its DistSQL version is incompatible, use
				// the gateway to process this span instead of the unhealthy host.
				// An empty address indicates an unhealthy host.
				if addr == "" || !compat {
					log.Eventf(ctx, "not planning on node %d. unhealthy: %t, incompatible version: %t",
						nodeID, addr == "", !compat)
					nodeID = dsp.nodeDesc.NodeID
					partitionIdx, inNodeMap = nodeMap[nodeID]
				}

				if !inNodeMap {
					partitionIdx = len(partitions)
					partitions = append(partitions, SpanPartition{Node: nodeID})
					nodeMap[nodeID] = partitionIdx
				}
			}
			partition := &partitions[partitionIdx]

			if lastNodeID == nodeID {
				// Two consecutive ranges on the same node, merge the spans.
				partition.Spans[len(partition.Spans)-1].EndKey = endKey.AsRawKey()
			} else {
				partition.Spans = append(partition.Spans, roachpb.Span{
					Key:    lastKey.AsRawKey(),
					EndKey: endKey.AsRawKey(),
				})
			}

			if !endKey.Less(rspan.EndKey) {
				// Done.
				break
			}

			lastKey = endKey
			lastNodeID = nodeID
		}
	}
	return partitions, nil
}

// nodeVersionIsCompatible decides whether a particular node's DistSQL version
// is compatible with planVer. It uses gossip to find out the node's version
// range.
func (dsp *DistSQLPlanner) nodeVersionIsCompatible(
	nodeID roachpb.NodeID, planVer execinfrapb.DistSQLVersion,
) bool {
	var v execinfrapb.DistSQLVersionGossipInfo
	if err := dsp.gossip.GetInfoProto(gossip.MakeDistSQLNodeVersionKey(nodeID), &v); err != nil {
		return false
	}
	return distsql.FlowVerIsCompatible(dsp.planVersion, v.MinAcceptedVersion, v.Version)
}

func getIndexIdx(n *scanNode) (uint32, error) {
	if n.index.ID == n.desc.PrimaryIndex.ID {
		return 0, nil
	}
	for i := range n.desc.Indexes {
		if n.index.ID == n.desc.Indexes[i].ID {
			// IndexIdx is 1 based (0 means primary index).
			return uint32(i + 1), nil
		}
	}
	return 0, errors.Errorf("invalid scanNode index %v (table %s)", n.index, n.desc.Name)
}

// initTableReaderSpec initializes a TableReaderSpec/PostProcessSpec that
// corresponds to a scanNode, except for the Spans and OutputColumns.
func initTableReaderSpec(
	n *scanNode, planCtx *PlanningCtx, indexVarMap []int,
) (*execinfrapb.TableReaderSpec, execinfrapb.PostProcessSpec, error) {
	s := physicalplan.NewTableReaderSpec()
	*s = execinfrapb.TableReaderSpec{
		Table:             *n.desc.TableDesc(),
		Reverse:           n.reverse,
		IsCheck:           n.isCheck,
		Visibility:        n.colCfg.visibility.toDistSQLScanVisibility(),
		LockingStrength:   n.lockingStrength,
		LockingWaitPolicy: n.lockingWaitPolicy,

		// Retain the capacity of the spans slice.
		Spans: s.Spans[:0],
	}
	indexIdx, err := getIndexIdx(n)
	if err != nil {
		return nil, execinfrapb.PostProcessSpec{}, err
	}
	s.IndexIdx = indexIdx

	// When a TableReader is running scrub checks, do not allow a
	// post-processor. This is because the outgoing stream is a fixed
	// format (rowexec.ScrubTypes).
	if n.isCheck {
		return s, execinfrapb.PostProcessSpec{}, nil
	}

	filter, err := physicalplan.MakeExpression(n.filter, planCtx, indexVarMap, true, false)
	if err != nil {
		return nil, execinfrapb.PostProcessSpec{}, err
	}
	post := execinfrapb.PostProcessSpec{
		Filter: filter,
	}

	if n.hardLimit != 0 {
		post.Limit = uint64(n.hardLimit)
	} else if n.softLimit != 0 {
		s.LimitHint = n.softLimit
	}
	return s, post, nil
}

// scanNodeOrdinal returns the index of a column with the given ID.
func tableOrdinal(
	desc *sqlbase.ImmutableTableDescriptor, colID sqlbase.ColumnID, visibility scanVisibility,
) int {
	for i := range desc.Columns {
		if desc.Columns[i].ID == colID {
			return i
		}
	}
	if visibility == publicAndNonPublicColumns {
		offset := len(desc.Columns)
		for i, col := range desc.MutationColumns() {
			if col.ID == colID {
				return offset + i
			}
		}
	}
	panic(fmt.Sprintf("column %d not in desc.Columns", colID))
}

// getScanNodeToTableOrdinalMap returns a map from scan node column ordinal to
// table reader column ordinal. Returns nil if the map is identity.
//
// scanNodes can have columns set up in a few different ways, depending on the
// colCfg. The heuristic planner always creates scanNodes with all public
// columns (even if some of them aren't even in the index we are scanning).
// The optimizer creates scanNodes with a specific set of wanted columns; in
// this case we have to create a map from scanNode column ordinal to table
// column ordinal (which is what the TableReader uses).
func getScanNodeToTableOrdinalMap(n *scanNode) []int {
	if n.colCfg.wantedColumns == nil {
		return nil
	}
	if n.colCfg.addUnwantedAsHidden {
		panic("addUnwantedAsHidden not supported")
	}
	res := make([]int, len(n.cols))
	for i := range res {
		res[i] = tableOrdinal(n.desc, n.cols[i].ID, n.colCfg.visibility)
	}
	return res
}

// getOutputColumnsFromScanNode returns the indices of the columns that are
// returned by a scanNode.
// If remap is not nil, the column ordinals are remapped accordingly.
func getOutputColumnsFromScanNode(n *scanNode, remap []int) []uint32 {
	outputColumns := make([]uint32, 0, n.valNeededForCol.Len())
	// TODO(radu): if we have a scan with a filter, valNeededForCol will include
	// the columns needed for the filter, even if they aren't needed for the
	// next stage.
	n.valNeededForCol.ForEach(func(i int) {
		if remap != nil {
			i = remap[i]
		}
		outputColumns = append(outputColumns, uint32(i))
	})
	return outputColumns
}

// convertOrdering maps the columns in props.ordering to the output columns of a
// processor.
func (dsp *DistSQLPlanner) convertOrdering(
	reqOrdering ReqOrdering, planToStreamColMap []int,
) execinfrapb.Ordering {
	if len(reqOrdering) == 0 {
		return execinfrapb.Ordering{}
	}
	result := execinfrapb.Ordering{
		Columns: make([]execinfrapb.Ordering_Column, len(reqOrdering)),
	}
	for i, o := range reqOrdering {
		streamColIdx := o.ColIdx
		if planToStreamColMap != nil {
			streamColIdx = planToStreamColMap[o.ColIdx]
		}
		if streamColIdx == -1 {
			panic("column in ordering not part of processor output")
		}
		result.Columns[i].ColIdx = uint32(streamColIdx)
		dir := execinfrapb.Ordering_Column_ASC
		if o.Direction == encoding.Descending {
			dir = execinfrapb.Ordering_Column_DESC
		}
		result.Columns[i].Direction = dir
	}
	return result
}

// getNodeIDForScan retrieves the node ID where the single table reader should
// reside for a limited scan. Ideally this is the lease holder for the first
// range in the specified spans. But if that node is unhealthy or incompatible,
// we use the gateway node instead.
func (dsp *DistSQLPlanner) getNodeIDForScan(
	planCtx *PlanningCtx, spans []roachpb.Span, reverse bool,
) (roachpb.NodeID, error) {
	if len(spans) == 0 {
		panic("no spans")
	}

	// Determine the node ID for the first range to be scanned.
	it := planCtx.spanIter
	if reverse {
		it.Seek(planCtx.ctx, spans[len(spans)-1], kvcoord.Descending)
	} else {
		it.Seek(planCtx.ctx, spans[0], kvcoord.Ascending)
	}
	if !it.Valid() {
		return 0, it.Error()
	}
	replInfo, err := it.ReplicaInfo(planCtx.ctx)
	if err != nil {
		return 0, err
	}

	nodeID := replInfo.NodeDesc.NodeID
	if err := dsp.CheckNodeHealthAndVersion(planCtx, replInfo.NodeDesc); err != nil {
		log.Eventf(planCtx.ctx, "not planning on node %d. %v", nodeID, err)
		return dsp.nodeDesc.NodeID, nil
	}
	return nodeID, nil
}

// CheckNodeHealthAndVersion adds the node to planCtx if it is healthy and
// has a compatible version. An error is returned otherwise.
func (dsp *DistSQLPlanner) CheckNodeHealthAndVersion(
	planCtx *PlanningCtx, desc *roachpb.NodeDescriptor,
) error {
	nodeID := desc.NodeID
	var err error

	if err = dsp.nodeHealth.check(planCtx.ctx, nodeID); err != nil {
		err = errors.New("unhealthy")
	} else if !dsp.nodeVersionIsCompatible(nodeID, dsp.planVersion) {
		err = errors.New("incompatible version")
	} else {
		planCtx.NodeAddresses[nodeID] = desc.Address.String()
	}
	return err
}

// GetAllDistNodesInfo gets the distributed nodes after performing healthyCheck on the nodes.
func (dsp *DistSQLPlanner) GetAllDistNodesInfo(
	planCtx *PlanningCtx, status serverpb.StatusServer, allowDisaster bool,
) ([]roachpb.NodeDescriptor, []roachpb.NodeDescriptor, []error, error) {
	nodes := make([]roachpb.NodeDescriptor, 0)
	failureNodes := make([]roachpb.NodeDescriptor, 0)
	failureNodesErrs := make([]error, 0)
	nodeStatus, err := status.Nodes(planCtx.ctx, &serverpb.NodesRequest{})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, n := range nodeStatus.Nodes {
		if err := dsp.CheckNodeHealthAndVersion(planCtx, &n.Desc); err != nil {
			failureNodes = append(failureNodes, n.Desc)
			failureNodesErrs = append(failureNodesErrs, err)
		} else {
			nodes = append(nodes, n.Desc)
		}
	}
	if !allowDisaster && len(failureNodes) != 0 {
		var errStringBf bytes.Buffer
		for i := 0; i < len(failureNodes); i++ {
			s := fmt.Sprintf("Problem occurs on node %d."+"Err: %s\n", failureNodes[i].NodeID, failureNodesErrs[i])
			errStringBf.WriteString(s)
		}
		return nil, nil, nil, errors.Newf(errStringBf.String())
	}
	return nodes, failureNodes, failureNodesErrs, nil
}

// createTableReaders generates a plan consisting of table reader processors,
// one for each node that has spans that we are reading.
// overridesResultColumns is optional.
func (dsp *DistSQLPlanner) createTableReaders(
	planCtx *PlanningCtx, n *scanNode, overrideResultColumns []sqlbase.ColumnID,
) (PhysicalPlan, error) {

	scanNodeToTableOrdinalMap := getScanNodeToTableOrdinalMap(n)

	spec, post, err := initTableReaderSpec(n, planCtx, scanNodeToTableOrdinalMap)
	if err != nil {
		return PhysicalPlan{}, err
	}
	var spanPartitions []SpanPartition
	if planCtx.isLocal {
		spanPartitions = []SpanPartition{{dsp.nodeDesc.NodeID, n.spans}}
	} else if n.hardLimit == 0 {
		// No hard limit - plan all table readers where their data live. Note
		// that we're ignoring soft limits for now since the TableReader will
		// still read too eagerly in the soft limit case. To prevent this we'll
		// need a new mechanism on the execution side to modulate table reads.
		// TODO(yuzefovich): add that mechanism.
		spanPartitions, err = dsp.PartitionSpans(planCtx, n.spans)
		if err != nil {
			return PhysicalPlan{}, err
		}
	} else {
		// If the scan has a hard limit, use a single TableReader to avoid
		// reading more rows than necessary.
		nodeID, err := dsp.getNodeIDForScan(planCtx, n.spans, n.reverse)
		if err != nil {
			return PhysicalPlan{}, err
		}
		spanPartitions = []SpanPartition{{nodeID, n.spans}}
	}

	var p PhysicalPlan
	stageID := p.NewStageID()

	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(spanPartitions))
	p.Processors = make([]physicalplan.Processor, 0, len(spanPartitions))

	returnMutations := n.colCfg.visibility == publicAndNonPublicColumns

	for i, sp := range spanPartitions {
		var tr *execinfrapb.TableReaderSpec
		if i == 0 {
			// For the first span partition, we can just directly use the spec we made
			// above.
			tr = spec
		} else {
			// For the rest, we have to copy the spec into a fresh spec.
			tr = physicalplan.NewTableReaderSpec()
			// Grab the Spans field of the new spec, and reuse it in case the pooled
			// TableReaderSpec we got has pre-allocated Spans memory.
			newSpansSlice := tr.Spans
			*tr = *spec
			tr.Spans = newSpansSlice
		}
		for j := range sp.Spans {
			tr.Spans = append(tr.Spans, execinfrapb.TableReaderSpan{Span: sp.Spans[j]})
		}

		tr.MaxResults = n.maxResults
		p.TotalEstimatedScannedRows += n.estimatedRowCount
		if n.estimatedRowCount > p.MaxEstimatedRowCount {
			p.MaxEstimatedRowCount = n.estimatedRowCount
		}

		proc := physicalplan.Processor{
			Node: sp.Node,
			Spec: execinfrapb.ProcessorSpec{
				Core:    execinfrapb.ProcessorCoreUnion{TableReader: tr},
				Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID: stageID,
			},
		}

		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}

	if len(p.ResultRouters) > 1 && len(n.reqOrdering) > 0 {
		// Make a note of the fact that we have to maintain a certain ordering
		// between the parallel streams.
		//
		// This information is taken into account by the AddProjection call below:
		// specifically, it will make sure these columns are kept even if they are
		// not in the projection (e.g. "SELECT v FROM kv ORDER BY k").
		p.SetMergeOrdering(dsp.convertOrdering(n.reqOrdering, scanNodeToTableOrdinalMap))
	}

	var typs []types.T
	if returnMutations {
		typs = make([]types.T, 0, len(n.desc.Columns)+len(n.desc.MutationColumns()))
	} else {
		typs = make([]types.T, 0, len(n.desc.Columns))
	}
	for i := range n.desc.Columns {
		typs = append(typs, n.desc.Columns[i].Type)
	}
	if returnMutations {
		for _, col := range n.desc.MutationColumns() {
			typs = append(typs, col.Type)
		}
	}
	p.SetLastStagePost(post, typs)

	var outCols []uint32
	if overrideResultColumns == nil {
		outCols = getOutputColumnsFromScanNode(n, scanNodeToTableOrdinalMap)
	} else {
		outCols = make([]uint32, len(overrideResultColumns))
		for i, id := range overrideResultColumns {
			outCols[i] = uint32(tableOrdinal(n.desc, id, n.colCfg.visibility))
		}
	}
	planToStreamColMap := make([]int, len(n.cols))
	descColumnIDs := make([]sqlbase.ColumnID, 0, len(n.desc.Columns))
	for i := range n.desc.Columns {
		descColumnIDs = append(descColumnIDs, n.desc.Columns[i].ID)
	}
	if returnMutations {
		for _, c := range n.desc.MutationColumns() {
			descColumnIDs = append(descColumnIDs, c.ID)
		}
	}
	for i := range planToStreamColMap {
		planToStreamColMap[i] = -1
		for j, c := range outCols {
			if descColumnIDs[c] == n.cols[i].ID {
				planToStreamColMap[i] = j
				break
			}
		}
	}
	p.AddProjection(outCols)

	p.PlanToStreamColMap = planToStreamColMap
	return p, nil
}

func makeResultTypeForAggFuncs(aggTyps [][]types.T, aggFns []int32) ([]types.T, error) {
	outTyps := make([]types.T, len(aggFns))

	for i := range aggFns {
		// Set the output type of the aggregate.
		switch execinfrapb.AggregatorSpec_Func(aggFns[i]) {
		case execinfrapb.AggregatorSpec_COUNT_ROWS, execinfrapb.AggregatorSpec_COUNT:
			// TODO(jordan): this is a somewhat of a hack. The aggregate functions
			// should come with their own output types, somehow.
			outTyps[i] = *types.Int
		case
			execinfrapb.AggregatorSpec_ANY_NOT_NULL,
			execinfrapb.AggregatorSpec_AVG,
			execinfrapb.AggregatorSpec_SUM,
			execinfrapb.AggregatorSpec_SUM_INT,
			execinfrapb.AggregatorSpec_MIN,
			execinfrapb.AggregatorSpec_MAX,
			execinfrapb.AggregatorSpec_BOOL_AND,
			execinfrapb.AggregatorSpec_FIRST,
			execinfrapb.AggregatorSpec_LAST,
			execinfrapb.AggregatorSpec_LAST_ROW,
			execinfrapb.AggregatorSpec_FIRST_ROW,
			execinfrapb.AggregatorSpec_BOOL_OR:
			// Output types are the input types for now.
			outTyps[i] = aggTyps[i][0]
		case
			execinfrapb.AggregatorSpec_LASTTS,
			execinfrapb.AggregatorSpec_FIRSTTS,
			execinfrapb.AggregatorSpec_LAST_ROW_TS,
			execinfrapb.AggregatorSpec_FIRST_ROW_TS:
			outTyps[i] = *types.TimestampTZ
		default:
			return nil, errors.Errorf("unsupported columnar aggregate function %s", execinfrapb.AggregatorSpec_Func(aggFns[i]).String())
		}
	}

	return outTyps, nil
}

// add tsColIndex in add-delete columns, init in createReaders,
// record indexs of column after add of delete columns
type tsColIndex struct {
	idx     int                // index of column
	colType sqlbase.ColumnType // type of column
}

// buildTSStatisticColsAndTSColMap build tsCols and tsColMap by cols
//
// Parameters:
// - n: ts scan node
// - columnIDSet: column id set
//
// Returns:
// - ts column meta
// - ts column index map, key is column id(logical id)
// - statistic scan output column index array
// - scan output column id array
// - err: Description of the error, if any
func buildTSStatisticColsAndTSColMap(
	n *tsScanNode, columnIDSet opt.ColSet,
) ([]sqlbase.TSCol, map[sqlbase.ColumnID]tsColIndex, []int, []int) {
	tsCols, tsColMap, resColsOld := buildTSColsAndTSColMap(n, columnIDSet)

	resCols := make([]int, 0)
	for i := 0; i < len(n.ScanAggArray); i++ {
		resCols = append(resCols, i+1)
	}

	return tsCols, tsColMap, resCols, resColsOld
}

// build tsCols and tsColMap by cols
//
// Parameters:
// - n: ts scan node
// - columnIDSet: column id set
//
// Returns:
// - ts column meta
// - ts column index map, key is column id(logical id)
// - scan output column id array
// - err: Description of the error, if any
func buildTSColsAndTSColMap(
	n *tsScanNode, columnIDSet opt.ColSet,
) ([]sqlbase.TSCol, map[sqlbase.ColumnID]tsColIndex, []int) {
	tsCols := make([]sqlbase.TSCol, 0)
	tsColMap := make(map[sqlbase.ColumnID]tsColIndex)
	for i := 0; i < n.Table.ColumnCount(); i++ {
		if col, ok := n.Table.Column(i).(*sqlbase.ColumnDescriptor); ok {
			col.TsCol.Nullable = n.Table.Column(i).IsNullable()
			if !col.IsTagCol() {
				tsCols = append(tsCols, col.TsCol)
				tsColMap[col.ID] = tsColIndex{len(tsCols) - 1, col.TsCol.ColumnType}
			}
		}
	}
	for i := 0; i < n.Table.ColumnCount(); i++ {
		if col, ok := n.Table.Column(i).(*sqlbase.ColumnDescriptor); ok {
			col.TsCol.Nullable = n.Table.Column(i).IsNullable()
			if col.IsTagCol() {
				tsCols = append(tsCols, col.TsCol)
				tsColMap[col.ID] = tsColIndex{len(tsCols) - 1, col.TsCol.ColumnType}
			}
		}
	}

	resCols := make([]int, 0)
	for _, resCol := range n.resultColumns {
		if columnIDSet.Contains(opt.ColumnID(resCol.PGAttributeNum)) {
			if index, ok := tsColMap[resCol.PGAttributeNum]; ok {
				resCols = append(resCols, index.idx)
			} else {
				panic("not find result column")
			}
		} else {
			panic("not find result column")
		}
	}
	return tsCols, tsColMap, resCols
}

// init PhysicalPlan
func (p *PhysicalPlan) initPhyPlanForTsReaders(
	colMetas []sqlbase.TSCol, n *tsScanNode, nodeIDs []roachpb.NodeID,
) error {
	tr := execinfrapb.TSReaderSpec{TableID: uint64(n.Table.ID()), TsSpans: n.tsSpans, TableVersion: n.Table.GetTSVersion()}
	for i := range colMetas {
		tr.ColMetas = append(tr.ColMetas, &colMetas[i])
	}
	pIdxStart := physicalplan.ProcessorIdx(len(p.Processors))
	for _, resultProc := range p.ResultRouters {
		nodeID := p.Processors[resultProc].Node
		proc := physicalplan.Processor{
			Node: nodeID,
			TSSpec: execinfrapb.TSProcessorSpec{
				Input: []execinfrapb.InputSyncSpec{{
					// The other fields will be filled in by mergeResultStreams.
					ColumnTypes: p.ResultTypes,
				}},
				Output: []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
			},
			ExecInTSEngine: true,
		}

		var reader = new(execinfrapb.TSReaderSpec)
		*reader = tr
		proc.TSSpec.Core.TableReader = reader

		p.AddProcessor(proc)
	}
	// Connect the streams.
	for bucket := 0; bucket < len(p.ResultRouters); bucket++ {
		pIdx := pIdxStart + physicalplan.ProcessorIdx(bucket)
		p.MergeResultStreams(p.ResultRouters, 0, p.MergeOrdering, pIdx, 0, false /* forceSerialization */)
	}

	// Set the new result routers.
	for i := 0; i < len(p.ResultRouters); i++ {
		p.ResultRouters[i] = pIdxStart + physicalplan.ProcessorIdx(i)
	}

	p.GateNoopInput = len(nodeIDs)
	p.TsOperator = execinfrapb.OperatorType_TsSelect
	return nil
}

// init PhysicalPlan for statistic reader
func (p *PhysicalPlan) initPhyPlanForStatisticReaders(
	spec execinfrapb.TSStatisticReaderSpec,
) error {
	pIdxStart := physicalplan.ProcessorIdx(len(p.Processors))
	for _, resultProc := range p.ResultRouters {
		nodeID := p.Processors[resultProc].Node
		proc := physicalplan.Processor{
			Node: nodeID,
			TSSpec: execinfrapb.TSProcessorSpec{
				Input: []execinfrapb.InputSyncSpec{{
					// The other fields will be filled in by mergeResultStreams.
					ColumnTypes: p.ResultTypes,
				}},
				Output: []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
			},
			ExecInTSEngine: true,
		}
		var reader = new(execinfrapb.TSStatisticReaderSpec)
		*reader = spec
		proc.TSSpec.Core.StatisticReader = reader

		p.AddProcessor(proc)
	}
	// Connect the streams.
	for bucket := 0; bucket < len(p.ResultRouters); bucket++ {
		pIdx := pIdxStart + physicalplan.ProcessorIdx(bucket)
		p.MergeResultStreams(p.ResultRouters, 0, p.MergeOrdering, pIdx, 0, false /* forceSerialization */)
	}

	// Set the new result routers.
	for i := 0; i < len(p.ResultRouters); i++ {
		p.ResultRouters[i] = pIdxStart + physicalplan.ProcessorIdx(i)
	}

	p.GateNoopInput = len(p.ResultRouters)
	p.TsOperator = execinfrapb.OperatorType_TsSelect
	return nil
}

// getPrimaryTag get primary tag values struct
//
// Parameters:
// - primaryTagValues: primary tag filter array
// - tsColMap: ts column index and meta  map
//
// Returns:
// - TSTagReaderSpec_TagValueArray array
func getPrimaryTag(
	primaryTagValues map[uint32][]string, tsColMap map[sqlbase.ColumnID]tsColIndex,
) []execinfrapb.TSTagReaderSpec_TagValueArray {
	var ptagFilter []execinfrapb.TSTagReaderSpec_TagValueArray
	if len(primaryTagValues) > 0 {
		idList := make(UintSlice, 0, len(primaryTagValues))
		for key := range primaryTagValues {
			idList = append(idList, key)
		}
		sort.Sort(idList)
		for _, id := range idList {
			if val, ok := primaryTagValues[id]; ok {
				// ZDP-33457: drop and alter column need to make new colidx, replace old idx with new idx
				if idx, ok1 := tsColMap[sqlbase.ColumnID(id)]; ok1 {
					ptagFilter = append(ptagFilter, execinfrapb.TSTagReaderSpec_TagValueArray{Colid: uint32(idx.idx), TagValues: val})
				} else {
					ptagFilter = append(ptagFilter, execinfrapb.TSTagReaderSpec_TagValueArray{Colid: id - 1, TagValues: val})
				}
			}
		}
	}

	return ptagFilter
}

// buildPhyPlanForTagReaders construct TSTagReadera and add to Proceccor.
// tableID is the id of table that needs to be scanned.
//
// Parameters:
// - planCtx: context
// - n: ts scan node
// - nodeIDs: node id array
// - colMetas: ts column meta
// - tsColMap: ts column index and type map, key is ts column id
// - resCols: scan output column id array
// - typs: column output types
// - descColumnIDs: column physical id
//
// Returns:
// - err: Description of the error, if any
func (p *PhysicalPlan) buildPhyPlanForTagReaders(
	planCtx *PlanningCtx,
	n *tsScanNode,
	nodeIDs []roachpb.NodeID,
	colMetas []sqlbase.TSCol,
	tsColMap map[sqlbase.ColumnID]tsColIndex,
	resCols []int,
	typs []types.T,
	descColumnIDs []sqlbase.ColumnID,
) error {
	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(nodeIDs))
	p.Processors = make([]physicalplan.Processor, 0, len(nodeIDs))

	// when the tag filter not push to TagFilterArray of tsScanNode, need to deal with filter.
	if n.HintType == keys.TagOnlyHint && n.filter != nil {
		n.TagFilterArray = append(n.TagFilterArray, n.filter)
	}

	// deal tag filter
	post := execinfrapb.TSPostProcessSpec{
		Filter: physicalplan.MakeTSExpressionForArray(n.TagFilterArray, planCtx, resCols),
	}

	// deal primary tag filter values
	// primary tag need to be sort.
	ptagFilter := getPrimaryTag(n.PrimaryTagValues, tsColMap)

	for i, nodeID := range nodeIDs {
		proc := physicalplan.Processor{
			Node: nodeID,
			TSSpec: execinfrapb.TSProcessorSpec{
				Core: execinfrapb.TSProcessorCoreUnion{
					TagReader: &execinfrapb.TSTagReaderSpec{
						TableID:      uint64(n.Table.ID()),
						ColMetas:     colMetas,
						AccessMode:   n.AccessMode,
						PrimaryTags:  ptagFilter,
						TableVersion: n.Table.GetTSVersion(),
					},
				},
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
				}},
			},
			ExecInTSEngine: true,
		}
		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}

	// tags output
	var outCols []uint32
	n.ScanSource.ForEach(func(i int) {
		outCols = append(outCols, uint32(i-1))
		if col, ok := tsColMap[sqlbase.ColumnID(n.Table.Column(i-1).ColID())]; ok {
			if col.colType == sqlbase.ColumnType_TYPE_PTAG || col.colType == sqlbase.ColumnType_TYPE_TAG {
				post.OutputColumns = append(post.OutputColumns, uint32(col.idx))
			}
		}
	})

	post.Projection = true
	p.SetLastStageTSPost(post, typs)
	p.AddTSProjection(outCols, false, true)

	// add output types for last ts engine processor
	p.addTSOutputType(true)

	if n.HintType == keys.TagOnlyHint {
		p.PlanToStreamColMap = getPlanToStreamColMapForReader(outCols, n.resultColumns, descColumnIDs)
	}

	return nil
}

// addTSOutputType add output types for ts processor
func (p *PhysicalPlan) addTSOutputType(force bool) {
	for _, idx := range p.ResultRouters {
		if force || p.Processors[idx].ExecInTSEngine {
			p.Processors[idx].TSSpec.Post.OutputTypes = make([]types.Family, len(p.ResultTypes))
			for i, typ := range p.ResultTypes {
				p.Processors[idx].TSSpec.Post.OutputTypes[i] = typ.InternalType.Family
			}
		}
	}
}

// getPlanToStreamColMapForReader add plan to stream col map
func getPlanToStreamColMapForReader(
	outCols []uint32, resultColumns sqlbase.ResultColumns, descColumnIDs []sqlbase.ColumnID,
) []int {
	planToStreamColMap := make([]int, len(outCols))

	for i := range planToStreamColMap {
		planToStreamColMap[i] = -1
		for j, c := range outCols {
			if descColumnIDs[c] == resultColumns[i].PGAttributeNum {
				planToStreamColMap[i] = j
				break
			}
		}
	}
	return planToStreamColMap
}

// buildPhyPlanForTSStatisticReaders construct TSTagReadera and add to Proceccor.
// tableID is the id of table that needs to be scanned.
//
// Parameters:
// - planCtx: context
// - n: ts scan node
// - colMetas: ts column meta
// - resCols: scan output column id array
// - typs: column output types
// - tsColMap: ts column index and type map, key is ts column id
//
// Returns:
// - err: Description of the error, if any
func (p *PhysicalPlan) buildPhyPlanForTSStatisticReaders(
	planCtx *PlanningCtx,
	n *tsScanNode,
	colMetas []sqlbase.TSCol,
	resCols []int,
	typs []types.T,
	tsColMap map[sqlbase.ColumnID]tsColIndex,
) error {
	var scanCols []uint32
	var scanAgg []int32
	for i, agg := range n.ScanAggArray {
		if tsColIndex1, ok := tsColMap[sqlbase.ColumnID(agg.ColID)]; ok {
			scanCols = append(scanCols, uint32(tsColIndex1.idx))
		}
		scanAgg = append(scanAgg, int32(n.ScanAggArray[i].AggTyp))
	}

	tr := execinfrapb.TSStatisticReaderSpec{
		TableID: uint64(n.Table.ID()), TsSpans: n.tsSpans, Cols: scanCols, AggTypes: scanAgg, TsCols: colMetas,
		TableVersion: n.Table.GetTSVersion()}

	//construct TSReaderSpec
	err := p.initPhyPlanForStatisticReaders(tr)
	if err != nil {
		return err
	}

	post, err1 := initPostSpecForTSReaders(planCtx, n, resCols)
	if err1 != nil {
		return err1
	}

	planToStreamColMap := make([]int, len(n.ScanAggArray))
	post.Renders = make([]string, len(n.ScanAggArray))
	// new output cols type
	argTypeArray := make([][]types.T, len(n.ScanAggArray))
	// agg func type
	funcType := make([]int32, len(n.ScanAggArray))
	// new output cols
	outCols := make([]uint32, len(n.ScanAggArray))
	for i := range n.ScanAggArray {
		planToStreamColMap[i] = i
		argTypeArray[i] = []types.T{typs[uint32(n.ScanAggArray[i].ColID)-1]}
		funcType[i] = int32(n.ScanAggArray[i].AggTyp)
		outCols[i] = uint32(i)
		post.Renders[i] = fmt.Sprintf("@%d", i+1)
	}
	outTypes, typErr := makeResultTypeForAggFuncs(argTypeArray, funcType)
	if typErr != nil {
		panic(typErr)
	}
	post.Projection = false
	p.SetLastStageTSPost(post, outTypes)

	p.AddTSProjection(outCols, true, false)
	p.PlanToStreamColMap = planToStreamColMap
	return nil
}

// buildPhyPlanForTSReaders construct TSTagReadera and add to Proceccor.
// tableID is the id of table that needs to be scanned.
//
// Parameters:
// - planCtx: context
// - n: ts scan node
// - nodeIDs: node id array
// - colMetas: ts column meta
// - tsColMap: ts column index and type map, key is ts column id
// - resCols: scan output column id array
// - typs: column output types
// - descColumnIDs: column physical id
//
// Returns:
// - err: Description of the error, if any
func (p *PhysicalPlan) buildPhyPlanForTSReaders(
	planCtx *PlanningCtx,
	n *tsScanNode,
	nodeIDs []roachpb.NodeID,
	colMetas []sqlbase.TSCol,
	tsColMap map[sqlbase.ColumnID]tsColIndex,
	resCols []int,
	typs []types.T,
	descColumnIDs []sqlbase.ColumnID,
) error {
	useStatistic := len(n.ScanAggArray) > 0
	//construct TSReaderSpec
	err := p.initPhyPlanForTsReaders(colMetas, n, nodeIDs)
	if err != nil {
		return err
	}

	post, err1 := initPostSpecForTSReaders(planCtx, n, resCols)
	if err1 != nil {
		return err1
	}

	outCols, planToStreamColMap, outTypes := buildPostSpecForTSReaders(n, typs, descColumnIDs, tsColMap, &post)
	p.SetLastStageTSPost(post, outTypes)

	p.AddTSProjection(outCols, useStatistic, false)
	p.PlanToStreamColMap = planToStreamColMap
	return nil
}

// init TSPostProcessSpec
func initPostSpecForTSReaders(
	planCtx *PlanningCtx, n *tsScanNode, resCols []int,
) (execinfrapb.TSPostProcessSpec, error) {
	filter, err := physicalplan.MakeTSExpression(n.filter, planCtx, resCols)
	if err != nil {
		return execinfrapb.TSPostProcessSpec{}, err
	}

	post := execinfrapb.TSPostProcessSpec{
		Filter: filter.Expr,
	}

	return post, nil
}

// build TSPostProcessSpec
func buildPostSpecForTSReaders(
	n *tsScanNode,
	typs []types.T,
	descColumnIDs []sqlbase.ColumnID,
	tsColMap map[sqlbase.ColumnID]tsColIndex,
	post *execinfrapb.TSPostProcessSpec,
) ([]uint32, []int, []types.T) {
	var outCols []uint32
	var planToStreamColMap []int

	n.ScanSource.ForEach(func(i int) {
		outCols = append(outCols, uint32(i-1))
	})

	// tsCols
	for i := 0; i < len(n.resultColumns); i++ {
		if col, ok := tsColMap[n.resultColumns[i].PGAttributeNum]; ok {
			post.OutputColumns = append(post.OutputColumns, uint32(col.idx))
		}
	}

	post.Projection = true
	planToStreamColMap = getPlanToStreamColMapForReader(outCols, n.resultColumns, descColumnIDs)
	return outCols, planToStreamColMap, typs
}

func (dsp *DistSQLPlanner) createTSReaders(
	planCtx *PlanningCtx, n *tsScanNode,
) (PhysicalPlan, error) {
	planCtx.IsTS = true
	var p PhysicalPlan
	var columnIDSet opt.ColSet
	descColumnIDs := make([]sqlbase.ColumnID, 0)
	var typs = make([]types.T, 0)
	for i := 0; i < n.Table.DeletableColumnCount(); i++ {
		col := n.Table.Column(i)
		columnIDSet.Add(opt.ColumnID(col.ColID()))
		descColumnIDs = append(descColumnIDs, sqlbase.ColumnID(col.ColID()))
		typs = append(typs, *col.DatumType())
	}

	var nodeIDs []roachpb.NodeID
	hashRouter, err := api.GetHashRouterWithTable(uint32(n.Table.GetParentID()), uint32(n.Table.ID()), false, planCtx.planner.txn)
	if err != nil {
		return PhysicalPlan{}, err
	}
	mpp := planCtx.ExtendedEvalCtx.ExecCfg.StartMode == StartSingleReplica || planCtx.ExtendedEvalCtx.ExecCfg.StartMode == StartSingleNode

	nodeIDs, err = hashRouter.GetLeaseHolderNodeIDs(planCtx.ctx, mpp)
	if err != nil {
		return PhysicalPlan{}, err
	}
	sort.Slice(nodeIDs, func(i, j int) bool {
		return nodeIDs[i] < nodeIDs[j]
	})

	// statistic reader exists
	if len(n.ScanAggArray) > 0 {
		tsColMetas, tsColMap, resCols, resColsOld := buildTSStatisticColsAndTSColMap(n, columnIDSet)

		//construct TSTagReaderSpec
		err = p.buildPhyPlanForTagReaders(planCtx, n, nodeIDs, tsColMetas, tsColMap, resColsOld, typs, descColumnIDs)
		if err != nil {
			return PhysicalPlan{}, err
		}

		if n.HintType == keys.TagOnlyHint {
			return p, nil
		}

		//construct TSStatisticReader
		err = p.buildPhyPlanForTSStatisticReaders(planCtx, n, tsColMetas, resCols, typs, tsColMap)
		if err != nil {
			return PhysicalPlan{}, err
		}
		return p, nil
	}

	tsColMetas, tsColMap, resCols := buildTSColsAndTSColMap(n, columnIDSet)

	//construct TSTagReaderSpec
	err = p.buildPhyPlanForTagReaders(planCtx, n, nodeIDs, tsColMetas, tsColMap, resCols, typs, descColumnIDs)
	if err != nil {
		return PhysicalPlan{}, err
	}
	if n.HintType == keys.TagOnlyHint {
		return p, nil
	}

	//construct TSReaderSpec
	err = p.buildPhyPlanForTSReaders(planCtx, n, nodeIDs, tsColMetas, tsColMap, resCols, typs, descColumnIDs)
	if err != nil {
		return PhysicalPlan{}, err
	}

	return p, nil
}

// createTSInsertSelect
// plan: node of select
// 1. add Noop to plan for get data
// 2. construct tsInserSelSpec to exec ts insert
func (dsp *DistSQLPlanner) createTSInsertSelect(
	plan *PhysicalPlan, n *tsInsertSelectNode, planCtx *PlanningCtx,
) error {
	thisNodeID := dsp.nodeDesc.NodeID
	// add noop to subNode of tsInsertSelectNode
	plan.AddNoopToTsProcessors(thisNodeID, true, true)

	stageID := plan.NewStageID()
	var tsInsertSel = &execinfrapb.TsInsertSelSpec{TargetTableId: n.TableID, DbId: n.DBID, Cols: n.Cols, ColIdxs: n.ColIdxs, ChildName: n.TableName, TableType: n.TableType, NotSetInputsToDrain: plan.InlcudeApplyJoin}

	proc := physicalplan.Processor{
		Node: dsp.nodeDesc.NodeID,
		Spec: execinfrapb.ProcessorSpec{
			Core: execinfrapb.ProcessorCoreUnion{TsInsertSelect: tsInsertSel},
			Input: []execinfrapb.InputSyncSpec{{
				Type:        execinfrapb.InputSyncSpec_UNORDERED,
				ColumnTypes: plan.ResultTypes,
			}},
			Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
			StageID: stageID,
		},
	}
	pIdx := plan.AddProcessor(proc)

	plan.Streams = append(plan.Streams, physicalplan.Stream{
		SourceProcessor:  plan.ResultRouters[0],
		DestProcessor:    pIdx,
		SourceRouterSlot: 0,
		DestInput:        0,
	})

	plan.ResultRouters = []physicalplan.ProcessorIdx{pIdx}
	plan.TsOperator = execinfrapb.OperatorType_TsInsertSelect

	return nil
}

// createTSInsert construct tsInsertSpec and processors.
func (dsp *DistSQLPlanner) createTSInsert(
	planCtx *PlanningCtx, n *tsInsertNode,
) (PhysicalPlan, error) {
	var p PhysicalPlan
	stageID := p.NewStageID()

	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(n.nodeIDs))
	p.Processors = make([]physicalplan.Processor, 0, len(n.nodeIDs))

	// Construct a processor for the payload of each node.
	for i := 0; i < len(n.nodeIDs); i++ {
		var tsInsert = &execinfrapb.TsInsertProSpec{}
		tsInsert.PayLoad = make([][]byte, len(n.allNodePayloadInfos[i]))
		tsInsert.RowNums = make([]uint32, len(n.allNodePayloadInfos[i]))
		tsInsert.PrimaryTagKey = make([][]byte, len(n.allNodePayloadInfos[i]))
		for j := range n.allNodePayloadInfos[i] {
			tsInsert.PayLoad[j] = n.allNodePayloadInfos[i][j].Payload
			tsInsert.RowNums[j] = n.allNodePayloadInfos[i][j].RowNum
			tsInsert.PrimaryTagKey[j] = n.allNodePayloadInfos[i][j].PrimaryTagKey
		}
		proc := physicalplan.Processor{
			Node: n.nodeIDs[i],
			Spec: execinfrapb.ProcessorSpec{
				Core:    execinfrapb.ProcessorCoreUnion{TsInsert: tsInsert},
				Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID: stageID,
			},
		}
		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}

	p.GateNoopInput = len(n.nodeIDs)
	p.TsOperator = execinfrapb.OperatorType_TsInsert

	return p, nil
}

func (dsp *DistSQLPlanner) createTSDelete(
	planCtx *PlanningCtx, n *tsDeleteNode,
) (PhysicalPlan, error) {
	var p PhysicalPlan
	stageID := p.NewStageID()
	var nodeIDs []roachpb.NodeID
	entityGroups := make(map[int32][]*api.EntityRangeGroup)
	if (planCtx.ExtendedEvalCtx.ExecCfg.StartMode == StartSingleReplica || planCtx.ExtendedEvalCtx.ExecCfg.StartMode == StartSingleNode) && n.delTyp == uint8(execinfrapb.OperatorType_TsDeleteMultiEntitiesData) {
		// build table's hash hook
		hashRouter, err := api.GetHashRouterWithTable(0, uint32(n.tableID), false, planCtx.planner.txn)
		if err != nil {
			return p, err
		}
		entityRGs := hashRouter.GetAllGroups(planCtx.ctx)
		for i := range entityRGs {
			entityGroups[int32(entityRGs[i].LeaseHolder.NodeID)] = append(entityGroups[int32(entityRGs[i].LeaseHolder.NodeID)], &entityRGs[i])
		}
		for nodeID := range entityGroups {
			nodeIDs = append(nodeIDs, roachpb.NodeID(nodeID))
		}
	} else {
		nodeIDs = n.nodeIDs
	}

	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(nodeIDs))
	p.Processors = make([]physicalplan.Processor, 0, len(nodeIDs))

	// Construct a processor for the payload of each node
	for i := 0; i < len(nodeIDs); i++ {
		var tsDelete = &execinfrapb.TsDeleteProSpec{
			TsOperator:     execinfrapb.OperatorType(n.delTyp),
			TableId:        n.tableID,
			RangeGroupId:   n.groupID,
			PrimaryTagKeys: n.primaryTagKey,
			PrimaryTags:    n.primaryTagValue,
			Spans:          n.spans,
		}
		if len(entityGroups) != 0 {
			nodeGroups := entityGroups[int32(nodeIDs[i])]
			tsDelete.EntityGroups = make([]execinfrapb.DeleteEntityGroup, len(nodeGroups))
			for i, group := range nodeGroups {
				tsDelete.EntityGroups[i].GroupId = uint64(group.GroupID)
				for _, par := range group.Partitions {
					tsDelete.EntityGroups[i].Partitions = append(tsDelete.EntityGroups[i].Partitions, &par)
				}
			}
		}

		proc := physicalplan.Processor{
			Node: nodeIDs[i],
			Spec: execinfrapb.ProcessorSpec{
				Core:    execinfrapb.ProcessorCoreUnion{TsDelete: tsDelete},
				Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID: stageID,
			},
		}
		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}

	p.GateNoopInput = len(nodeIDs)
	p.TsOperator = execinfrapb.OperatorType(n.delTyp)

	return p, nil
}

func (dsp *DistSQLPlanner) createTSTagUpdate(
	planCtx *PlanningCtx, n *tsTagUpdateNode,
) (PhysicalPlan, error) {
	var p PhysicalPlan
	stageID := p.NewStageID()

	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(n.nodeIDs))
	p.Processors = make([]physicalplan.Processor, 0, len(n.nodeIDs))

	// Construct a processor for the payload of each node
	for i := 0; i < len(n.nodeIDs); i++ {
		var tsTagUpdate = &execinfrapb.TsTagUpdateProSpec{}
		tsTagUpdate.TsOperator = execinfrapb.OperatorType_TsUpdateTag
		//tsTagUpdate.TsOperator = execinfrapb.OperatorType(n.operateTyp)
		tsTagUpdate.TableId = n.tableID
		tsTagUpdate.RangeGroupId = n.groupID
		tsTagUpdate.PrimaryTagKeys = n.primaryTagKey
		tsTagUpdate.PrimaryTags = n.TagValue
		tsTagUpdate.Tags = n.TagValue

		proc := physicalplan.Processor{
			Node: n.nodeIDs[i],
			Spec: execinfrapb.ProcessorSpec{
				Core:    execinfrapb.ProcessorCoreUnion{TsTagUpdate: tsTagUpdate},
				Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID: stageID,
			},
		}
		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx
	}

	p.GateNoopInput = len(n.nodeIDs)
	p.TsOperator = execinfrapb.OperatorType_TsUpdateTag

	return p, nil
}

func (dsp *DistSQLPlanner) operateTSData(
	planCtx *PlanningCtx, n *operateDataNode,
) (PhysicalPlan, error) {
	var p PhysicalPlan
	var clearInfos []sqlbase.DeleteMeMsg
	var endTime int64
	var comPressTime int64
	stageID := p.NewStageID()
	tsPro := &execinfrapb.TsProSpec{}
	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(n.nodeID))
	p.Processors = make([]physicalplan.Processor, 0, len(n.nodeID))

	p.GateNoopInput = len(n.nodeID)

	tsPro.TsOperator = execinfrapb.OperatorType_TsDeleteExpiredData
	if n.operateType == Compress {
		tsPro.TsOperator = execinfrapb.OperatorType_TsCompressTsTable
	}
	for _, table := range n.desc {
		if table.TsTable.Lifetime == 0 || table.TsTable.Lifetime == InvalidLifetime {
			endTime = math.MinInt64
		} else {
			endTime = timeutil.Now().Unix() - int64(table.TsTable.Lifetime)
		}
		if table.TsTable.ActiveTime == 0 {
			comPressTime = math.MinInt64
		} else {
			comPressTime = timeutil.Now().Unix() - int64(table.TsTable.ActiveTime)
		}
		clearInfo := sqlbase.DeleteMeMsg{
			TableID:    uint32(table.ID),
			StartTs:    math.MinInt64,
			EndTs:      endTime,
			CompressTs: comPressTime,
			TsVersion:  uint32(table.TsTable.GetTsVersion()),
		}
		clearInfos = append(clearInfos, clearInfo)
	}
	tsPro.DropMEInfo = clearInfos
	p.TsOperator = tsPro.TsOperator
	// create processor for each node
	for i := range n.nodeID {
		proc := physicalplan.Processor{
			Node: n.nodeID[i],
			Spec: execinfrapb.ProcessorSpec{
				Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID: stageID,
			},
		}
		proc.Spec.Core = execinfrapb.ProcessorCoreUnion{TsPro: tsPro}
		pIdx := p.AddProcessor(proc)

		//I don't know what the key represents temporarily, starting from 0
		p.ResultRouters[i] = pIdx
	}
	return p, nil
}

func (dsp *DistSQLPlanner) createTSDDL(planCtx *PlanningCtx, n *tsDDLNode) (PhysicalPlan, error) {
	var p PhysicalPlan
	stageID := p.NewStageID()

	p.ResultRouters = make([]physicalplan.ProcessorIdx, len(n.nodeID))
	p.Processors = make([]physicalplan.Processor, 0, len(n.nodeID))

	p.GateNoopInput = len(n.nodeID)
	// Construct a processor for the payload of each node
	for i := 0; i < len(n.nodeID); i++ {
		proc := physicalplan.Processor{
			Node: n.nodeID[i],
			Spec: execinfrapb.ProcessorSpec{
				Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
				StageID: stageID,
			},
		}

		switch n.d.Type {
		case createKwdbTsTable:
			//If modified, the function SQL needs to be synchronously modified MakeNormalTSTableMeta and kvsserver.makeNormalTSTableMeta
			createKObjectTable := makeKObjectTableForTs(n.d)
			meta, err := protoutil.Marshal(&createKObjectTable)
			if err != nil {
				panic(err.Error())
			}
			var tsCreate = &execinfrapb.TsCreateTableProSpec{}
			tsCreate.Meta = meta
			tsCreate.TsTableID = uint64(n.d.SNTable.ID)
			proc.Spec.Core = execinfrapb.ProcessorCoreUnion{TsCreate: tsCreate}
			p.TsOperator = execinfrapb.OperatorType_TsCreateTable
		case dropKwdbTsTable, dropKwdbInsTable, dropKwdbTsDatabase:
			var tsDrop = &execinfrapb.TsProSpec{}
			tsDrop.DropMEInfo = n.d.DropMEInfo
			dropInsTable := n.d.Type == dropKwdbInsTable
			tsDrop.TsOperator = execinfrapb.OperatorType_TsDropTsTable
			if dropInsTable {
				tsDrop.TsOperator = execinfrapb.OperatorType_TsDropDeleteEntities
			}
			proc.Spec.Core = execinfrapb.ProcessorCoreUnion{TsPro: tsDrop}
			p.TsOperator = tsDrop.TsOperator
		case alterKwdbAddTag, alterKwdbAddColumn, alterKwdbDropTag, alterKwdbDropColumn, alterKwdbAlterTagType, alterKwdbAlterColumnType:
			var tsAlterColumn = &execinfrapb.TsAlterProSpec{}
			col := n.d.AlterTag
			tsColumn := sqlbase.KWDBKTSColumn{
				ColumnId:           uint32(col.ID),
				Name:               col.Name,
				Nullable:           col.Nullable,
				StorageType:        col.TsCol.StorageType,
				StorageLen:         col.TsCol.StorageLen,
				VariableLengthType: col.TsCol.VariableLengthType,
				ColType:            col.TsCol.ColumnType,
			}
			colMeta, err := protoutil.Marshal(&tsColumn)
			if err != nil {
				panic(err.Error())
			}
			switch n.d.Type {
			case alterKwdbAlterTagType, alterKwdbAlterColumnType:
				oriCol := n.d.OriginColumn
				oriTSCol := sqlbase.KWDBKTSColumn{
					ColumnId:           uint32(oriCol.ID),
					Name:               oriCol.Name,
					Nullable:           oriCol.Nullable,
					StorageType:        oriCol.TsCol.StorageType,
					StorageLen:         oriCol.TsCol.StorageLen,
					VariableLengthType: oriCol.TsCol.VariableLengthType,
					ColType:            oriCol.TsCol.ColumnType,
				}
				oriColMeta, err := protoutil.Marshal(&oriTSCol)
				if err != nil {
					panic(err.Error())
				}
				tsAlterColumn.OriginalCol = oriColMeta
			}
			switch n.d.Type {
			case alterKwdbAddTag, alterKwdbAddColumn:
				tsAlterColumn.TsOperator = execinfrapb.OperatorType_TsAddColumn
			case alterKwdbAlterColumnType, alterKwdbAlterTagType:
				tsAlterColumn.TsOperator = execinfrapb.OperatorType_TsAlterType
			default:
				tsAlterColumn.TsOperator = execinfrapb.OperatorType_TsDropColumn
			}

			if n.txnEvent == txnRollback {
				tsAlterColumn.TsOperator = execinfrapb.OperatorType_TsRollback
			}
			if n.txnEvent == txnCommit {
				tsAlterColumn.TsOperator = execinfrapb.OperatorType_TsCommit
			}

			tsAlterColumn.Column = colMeta
			tsAlterColumn.TsTableID = uint64(n.d.SNTable.ID)
			tsAlterColumn.TsVersion = uint32(n.d.SNTable.TsTable.GetTsVersion())
			tsAlterColumn.TxnID = n.txnID
			proc.Spec.Core = execinfrapb.ProcessorCoreUnion{TsAlter: tsAlterColumn}
			p.TsOperator = tsAlterColumn.TsOperator
		case alterKwdbAlterPartitionInterval:
			var tsAlter = &execinfrapb.TsAlterProSpec{}
			tsAlter.TsOperator = execinfrapb.OperatorType_TsAlterPartitionInterval
			tsAlter.TsTableID = uint64(n.d.SNTable.ID)
			tsAlter.PartitionInterval = n.d.SNTable.TsTable.PartitionInterval
			proc.Spec.Core = execinfrapb.ProcessorCoreUnion{TsAlter: tsAlter}
			p.TsOperator = tsAlter.TsOperator
		case alterCompressInterval:
			var tsAlter = &execinfrapb.TsAlterProSpec{}
			tsAlter.TsOperator = execinfrapb.OperatorType_TsAlterCompressInterval
			tsAlter.CompressInterval = []byte(n.compressInterval)
			proc.Spec.Core = execinfrapb.ProcessorCoreUnion{TsAlter: tsAlter}
			p.TsOperator = tsAlter.TsOperator
		default:
			panic("can not get ts ddl type")
		}
		pIdx := p.AddProcessor(proc)
		p.ResultRouters[i] = pIdx //I don't know what the key represents temporarily, starting from 0
	}

	return p, nil
}

// MakeNormalTSTableMeta construct the normal time-series table meta.
// Note :
//
//	This function should be the same with kvserver.makeNormalTSTableMeta.
//	When modifying this function, modify kvserver.makeNormalTSTableMeta
//	at the same time.
func MakeNormalTSTableMeta(table *sqlbase.TableDescriptor) ([]byte, error) {
	syncDetail := jobspb.SyncMetaCacheDetails{
		Type:    createKwdbTsTable,
		SNTable: *table,
	}
	createKObjectTable := makeKObjectTableForTs(syncDetail)

	var meta []byte
	meta, err := protoutil.Marshal(&createKObjectTable)
	if err != nil {
		return nil, err
	}
	return meta, nil
}

// selectRenders takes a PhysicalPlan that produces the results corresponding to
// the select data source (a n.source) and updates it to produce results
// corresponding to the render node itself. An evaluator stage is added if the
// render node has any expressions which are not just simple column references.
func (dsp *DistSQLPlanner) selectRenders(
	p *PhysicalPlan, n *renderNode, planCtx *PlanningCtx,
) error {
	typs, err := getTypesForPlanResult(n, nil /* planToStreamColMap */)
	if err != nil {
		return err
	}

	if p.SelfCanExecInTSEngine(n.engine == tree.EngineTypeTimeseries) {
		err = p.AddTSRendering(n.render, planCtx, p.PlanToStreamColMap, typs)
		if err != nil {
			return err
		}
	} else {
		if p.ChildIsExecInTSEngine() {
			p.AddTSTableReader()
		}

		// If this layer or its children can not be executed in ts engine,
		// this layer can not be executed in ts engine.
		// In this case, p.SelfCanExecInTSEngine(n.engine == tree.EngineTypeTimeseries) is always false.
		err = p.AddRendering(n.render, planCtx, p.PlanToStreamColMap, typs, false)
		if err != nil {
			return err
		}
	}

	p.PlanToStreamColMap = identityMap(p.PlanToStreamColMap, len(n.render))
	return nil
}

// addSorters adds sorters corresponding to a sortNode and updates the plan to
// reflect the sort node.
func (dsp *DistSQLPlanner) addSorters(p *PhysicalPlan, n *sortNode) {
	// Sorting is needed; we add a stage of sorting processors.
	ordering := execinfrapb.ConvertToMappedSpecOrdering(n.ordering, p.PlanToStreamColMap)

	if p.SelfCanExecInTSEngine(n.engine == tree.EngineTypeTimeseries) {
		kwdbordering := execinfrapb.ConvertToMappedSpecOrdering(n.ordering, p.PlanToStreamColMap)
		p.AddTSNoGroupingStage(
			execinfrapb.TSProcessorCoreUnion{
				Sorter: &execinfrapb.SorterSpec{
					OutputOrdering:   kwdbordering,
					OrderingMatchLen: uint32(n.alreadyOrderedPrefix),
				},
			},
			execinfrapb.TSPostProcessSpec{},
			p.ResultTypes,
			ordering,
		)
	} else {
		p.AddNoGroupingStage(
			execinfrapb.ProcessorCoreUnion{
				Sorter: &execinfrapb.SorterSpec{
					OutputOrdering:   ordering,
					OrderingMatchLen: uint32(n.alreadyOrderedPrefix),
				},
			},
			execinfrapb.PostProcessSpec{},
			p.ResultTypes,
			ordering,
		)
	}
}

// checkHas check aggregator spec has in all spec
func checkHas(
	src execinfrapb.AggregatorSpec_Aggregation,
	dst []execinfrapb.AggregatorSpec_Aggregation,
	reIndex *uint32,
) bool {
	for j, prevLocalAgg := range dst {
		if src.Equals(prevLocalAgg) {
			// Found existing, equivalent local agg. Map the relative index (i)
			// for the current local agg to the absolute index (j) of the existing local agg.
			*reIndex = uint32(j)
			return true
		}
	}

	return false
}

// getFinalAggFuncAndType get final agg function and type
// Each aggregation can have multiple aggregations in the local/final stages. We concatenate all these into
// localAggs/finalAggs. finalIdx is the index of the final aggregation with respect to all final aggregations.
// Parameters:
// - aggregations: array of AggregatorSpec
// - localAggs: [out] all local agg
// - tsFinalAggs: [out] all ts engine final agg for parallel
// - finalAggs: [out] all final agg
// - inputTypes: child processor output column type
// - needRender: need render flag
// - finalIdxMap: final column index map
// - finalPreRenderTypes: [out] if need render, this is render column type
// - intermediateTypes: [out] local agg column type
// - tsFinalTypes: [out] ts engine final agg column type
// - statisticIndex: agg use statistic column index, statistic reader output sum(a), count(a) as single column
//
// Returns:
// - err if has error
func getFinalAggFuncAndType(
	aggregations []execinfrapb.AggregatorSpec_Aggregation,
	localAggs *[]execinfrapb.AggregatorSpec_Aggregation,
	tsFinalAggs *[]execinfrapb.AggregatorSpec_Aggregation,
	finalAggs *[]execinfrapb.AggregatorSpec_Aggregation,
	inputTypes []types.T,
	needRender bool,
	finalIdxMap *[]uint32,
	finalPreRenderTypes *[]*types.T,
	intermediateTypes *[]types.T,
	tsFinalTypes *[]types.T,
	statisticIndex opt.StatisticIndex,
) error {
	finalIdx := 0
	for k, e := range aggregations {
		info := physicalplan.DistAggregationTable[e.Func]

		// relToAbsLocalIdx maps each local stage for the given aggregation e to its final index in localAggs.
		// This is necessary since we de-duplicate equivalent local aggregations and need to correspond the
		// one copy of local aggregation required by the final stage to its input, which is specified as a
		// relative local stage index (see `Aggregations` in aggregators_func.go).
		// We use a slice here instead of a map because we have a small, bounded domain to map and runtime hash
		// operations are relatively expensive.
		relToAbsLocalIdx := make([]uint32, len(info.LocalStage))
		var relToAbsTSFinalIdx []uint32
		if tsFinalAggs != nil && tsFinalTypes != nil {
			relToAbsTSFinalIdx = make([]uint32, len(info.MiddleStage))
		}

		// Note the planNode first feeds the input (inputTypes) into the local aggregators.
		for i, localFunc := range info.LocalStage {
			localAgg := execinfrapb.AggregatorSpec_Aggregation{
				Func: localFunc, FilterColIdx: e.FilterColIdx,
			}
			//use statistic , local need use final agg
			if len(statisticIndex) > 0 {
				info1 := physicalplan.DistAggregationTable[localFunc]
				localAgg.Func = info1.FinalStage[0].Fn
				localFunc = info1.FinalStage[0].Fn
				localAgg.ColIdx = statisticIndex[k]
				e.ColIdx = statisticIndex[k]
			} else {
				localAgg.ColIdx = e.ColIdx
			}

			if !checkHas(localAgg, *localAggs, &relToAbsLocalIdx[i]) {
				// Append the new local aggregation and map to its index in localAggs.
				relToAbsLocalIdx[i] = uint32(len(*localAggs))
				*localAggs = append(*localAggs, localAgg)

				// Keep track of the new local aggregation's output type.
				argTypes := make([]types.T, len(e.ColIdx))
				for j, c := range localAgg.ColIdx {
					argTypes[j] = inputTypes[c]
				}

				_, outputType, err := execinfrapb.GetAggregateInfo(localFunc, argTypes...)
				if err != nil {
					return err
				}
				*intermediateTypes = append(*intermediateTypes, *outputType)
			}
		}

		if tsFinalAggs != nil && tsFinalTypes != nil {
			for i, tsFinalInfo := range info.MiddleStage {
				// The input of the final aggregators is specified as the relative indices of the local aggregation values.
				// We need to map these to the corresponding absolute indices in localAggs.
				// argIdxs consists of the absolute indices in localAggs.
				argIdxs := make([]uint32, len(tsFinalInfo.LocalIdxs))
				for m, relIdx := range tsFinalInfo.LocalIdxs {
					argIdxs[m] = relToAbsLocalIdx[relIdx]
				}

				tsFinalAgg := execinfrapb.AggregatorSpec_Aggregation{Func: tsFinalInfo.Fn, ColIdx: argIdxs}

				if !checkHas(tsFinalAgg, *tsFinalAggs, &relToAbsTSFinalIdx[i]) {
					// Append the new local aggregation and map to its index in localAggs.
					relToAbsTSFinalIdx[i] = uint32(len(*tsFinalAggs))
					*tsFinalAggs = append(*tsFinalAggs, tsFinalAgg)

					// Keep track of the new local aggregation's output type.
					argTypes := make([]types.T, len(argIdxs))
					for j, c := range tsFinalAgg.ColIdx {
						argTypes[j] = (*intermediateTypes)[c]
					}

					_, outputType, err := execinfrapb.GetAggregateInfo(tsFinalAgg.Func, argTypes...)
					if err != nil {
						return err
					}
					*tsFinalTypes = append(*tsFinalTypes, *outputType)
				}
			}
		} else {
			relToAbsTSFinalIdx = relToAbsLocalIdx
		}

		for _, finalInfo := range info.FinalStage {
			// The input of the final aggregators is specified as the relative indices of the local aggregation values.
			// We need to map these to the corresponding absolute indices in localAggs.
			// argIdxs consists of the absolute indices in localAggs.
			argIdxs := make([]uint32, len(finalInfo.LocalIdxs))
			for i, relIdx := range finalInfo.LocalIdxs {
				argIdxs[i] = relToAbsTSFinalIdx[relIdx]
			}

			finalAgg := execinfrapb.AggregatorSpec_Aggregation{Func: finalInfo.Fn, ColIdx: argIdxs}

			// Append the final agg if there is no existing equivalent.
			if !checkHas(finalAgg, *finalAggs, &(*finalIdxMap)[finalIdx]) {
				(*finalIdxMap)[finalIdx] = uint32(len(*finalAggs))
				*finalAggs = append(*finalAggs, finalAgg)

				if needRender {
					argTypes := make([]types.T, len(finalInfo.LocalIdxs))
					for i := range finalInfo.LocalIdxs {
						// Map the corresponding local aggregation output types for the current aggregation e.
						argTypes[i] = (*intermediateTypes)[argIdxs[i]]
					}
					_, outputType, err := execinfrapb.GetAggregateInfo(finalInfo.Fn, argTypes...)
					if err != nil {
						return err
					}
					*finalPreRenderTypes = append(*finalPreRenderTypes, outputType)
				}
			}
			finalIdx++
		}
	}
	return nil
}

// getTwoStageAggCount get local agg  final agg and need render
func getTwoStageAggCount(aggs []execinfrapb.AggregatorSpec_Aggregation) (int, int, int, bool) {
	nFirst := 0
	nTSFinal := 0
	nFinal := 0
	needRender := false
	for _, e := range aggs {
		info := physicalplan.DistAggregationTable[e.Func]
		nFirst += len(info.LocalStage)
		nTSFinal += len(info.MiddleStage)
		nFinal += len(info.FinalStage)
		if info.FinalRendering != nil {
			needRender = true
		}
	}

	return nFirst, nTSFinal, nFinal, needRender
}

// getLocalAggAndTypeAndFinalGroupCols get local agg \ local agg type \ final group cols \ final ordered group cols
func getLocalAggAndTypeAndFinalGroupCols(
	p *PhysicalPlan,
	groupCols []uint32,
	orderedGroupColSet util.FastIntSet,
	oldGroupCols []int,
	localAggs *[]execinfrapb.AggregatorSpec_Aggregation,
	localAggsTypes *[]types.T,
	finalGroupCols *[]uint32,
	finalOrderedGroupCols *[]uint32,
) {
	for i, groupColIdx := range groupCols {
		agg := execinfrapb.AggregatorSpec_Aggregation{
			Func:   execinfrapb.AggregatorSpec_ANY_NOT_NULL,
			ColIdx: []uint32{groupColIdx},
		}
		// See if there already is an aggregation like the one we want to add.
		var idx uint32
		if !checkHas(agg, *localAggs, &idx) {
			// Not already there, add it.
			idx = uint32(len(*localAggs))
			*localAggs = append(*localAggs, agg)
			*localAggsTypes = append(*localAggsTypes, p.ResultTypes[groupColIdx])
		}
		(*finalGroupCols)[i] = idx
		if orderedGroupColSet.Contains(oldGroupCols[i]) {
			*finalOrderedGroupCols = append(*finalOrderedGroupCols, idx)
		}
	}
}

// Create the merge ordering for the local stage
func createMergeOrderingForLocalStage(
	groupColOrdering sqlbase.ColumnOrdering, oldGroupCols []int, finalGroupCols []uint32,
) ([]execinfrapb.Ordering_Column, error) {
	ordCols := make([]execinfrapb.Ordering_Column, len(groupColOrdering))
	for i, o := range groupColOrdering {
		// Find the group column.
		found := false
		for j, col := range oldGroupCols {
			if col == o.ColIdx {
				ordCols[i].ColIdx = finalGroupCols[j]
				found = true
				break
			}
		}

		if !found {
			return nil, errors.AssertionFailedf("group column ordering contains non-grouping column %d", o.ColIdx)
		}

		if o.Direction == encoding.Descending {
			ordCols[i].Direction = execinfrapb.Ordering_Column_DESC
		} else {
			ordCols[i].Direction = execinfrapb.Ordering_Column_ASC
		}
	}

	return ordCols, nil
}

// getTwiceAggregatorRender get agg render eg avg -> sum/count
func getTwiceAggregatorRender(
	planCtx *PlanningCtx,
	aggregations []execinfrapb.AggregatorSpec_Aggregation,
	finalPreRenderTypes []*types.T,
	finalIdxMap []uint32,
	canLocal bool,
) ([]execinfrapb.Expression, error) {
	// Build rendering expressions.
	renderExprs := make([]execinfrapb.Expression, len(aggregations))
	h := tree.MakeTypesOnlyIndexedVarHelper(finalPreRenderTypes)
	// finalIdx is an index inside finalAggs. It is used to keep track of the finalAggs results
	// that correspond to each aggregation.
	finalIdx := 0
	for i, e := range aggregations {
		info := physicalplan.DistAggregationTable[e.Func]
		if info.FinalRendering == nil {
			// mappedIdx corresponds to the index location of the result for this final aggregation in finalAggs.
			// This is necessary since we re-use final aggregations if they are equivalent across and within stages.
			mappedIdx := int(finalIdxMap[finalIdx])
			var err error
			renderExprs[i], err = physicalplan.MakeExpression(
				h.IndexedVar(mappedIdx), planCtx, nil /* indexVarMap */, canLocal, false)
			if err != nil {
				return nil, err
			}
		} else {
			// We have multiple final aggregation values that we need to be mapped to their corresponding index in
			// finalAggs for FinalRendering.
			mappedIdxs := make([]int, len(info.FinalStage))
			for j := range info.FinalStage {
				mappedIdxs[j] = int(finalIdxMap[finalIdx+j])
			}
			// Map the final aggregation values to their corresponding indices.
			expr, err := info.FinalRendering(&h, mappedIdxs)
			if err != nil {
				return nil, err
			}

			renderExprs[i], err = physicalplan.MakeExpression(expr, planCtx, nil /* indexVarMap */, canLocal, false)
			if err != nil {
				return nil, err
			}
		}
		finalIdx += len(info.FinalStage)
	}
	return renderExprs, nil
}

// addSynchronizerForAgg add ts engine twice agg spec
func addSynchronizerForAgg(
	planCtx *PlanningCtx,
	p *PhysicalPlan,
	intermediateTypes []types.T,
	ordCols []execinfrapb.Ordering_Column,
	post *execinfrapb.TSPostProcessSpec,
) {
	// construct Synchronizer spec
	localAggsSpec := execinfrapb.TSSynchronizerSpec{
		Degree: planCtx.GetTsDop(),
	}

	if post == nil {
		// construct Synchronizer post spec
		tsPost := execinfrapb.TSPostProcessSpec{}

		// add ts post spec output type from intermediateTypes
		tsPost.OutputTypes = make([]types.Family, len(intermediateTypes))
		for i, typ := range intermediateTypes {
			tsPost.OutputTypes[i] = typ.InternalType.Family
		}
		// add Synchronizer spec for all node
		p.AddTSNoGroupingStage(
			execinfrapb.TSProcessorCoreUnion{Synchronizer: &localAggsSpec},
			tsPost,
			intermediateTypes,
			execinfrapb.Ordering{Columns: ordCols},
		)
	} else {
		// add Synchronizer spec for all node
		p.AddTSNoGroupingStage(
			execinfrapb.TSProcessorCoreUnion{Synchronizer: &localAggsSpec},
			*post,
			intermediateTypes,
			execinfrapb.Ordering{Columns: ordCols},
		)
	}
}

// setupChildDistributionStrategy setup child distribution strategy
func setupChildDistributionStrategy(
	p *PhysicalPlan, typ execinfrapb.OutputRouterSpec_Type, groupCols []uint32,
) {
	// set up child pass through
	for _, resultProc := range p.ResultRouters {
		if p.Processors[resultProc].ExecInTSEngine {
			p.Processors[resultProc].TSSpec.Output[0] = execinfrapb.OutputRouterSpec{
				Type:        typ,
				HashColumns: groupCols,
			}
		} else {
			p.Processors[resultProc].Spec.Output[0] = execinfrapb.OutputRouterSpec{
				Type:        typ,
				HashColumns: groupCols,
			}
		}
	}
}

// addTwiceAggregators add twice stage aggregator spec
func (dsp *DistSQLPlanner) addTwiceAggregators(
	planCtx *PlanningCtx,
	p *PhysicalPlan,
	aggType execinfrapb.AggregatorSpec_Type,
	groupColOrdering sqlbase.ColumnOrdering,
	aggregations []execinfrapb.AggregatorSpec_Aggregation,
	groupCols []uint32,
	oldGroupCols []int,
	orderedGroupCols []uint32,
	orderedGroupColSet util.FastIntSet,
	statisticIndex opt.StatisticIndex,
) (execinfrapb.AggregatorSpec, execinfrapb.PostProcessSpec, error) {
	var finalAggsSpec execinfrapb.AggregatorSpec
	var finalAggsPost execinfrapb.PostProcessSpec
	// Some aggregations might need multiple aggregation as part of their local and final stages
	// (along with a final render expression to combine the multiple aggregations into a single result).
	//
	// Count the total number of aggregation in the local/final
	// stages and keep track of whether any of them needs a final rendering.
	nLocalAgg, _, nFinalAgg, needRender := getTwoStageAggCount(aggregations)

	// We alloc the maximum possible number of unique local and final aggregations but do not initialize
	// any aggregations since we can de-duplicate equivalent local and final aggregations.
	localAggs := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nLocalAgg+len(groupCols))
	intermediateTypes := make([]types.T, 0, nLocalAgg+len(groupCols))
	finalAggs := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nFinalAgg)
	// finalIdxMap maps the index i of the final aggregation (with respect to the i-th final aggregation
	// out of all final aggregations) to its index in the finalAggs slice.
	finalIdxMap := make([]uint32, nFinalAgg)

	// finalPreRenderTypes is passed to an IndexVarHelper which helps type-check the indexed variables
	// passed into FinalRendering for some aggregations. This has a 1-1 mapping to finalAggs
	var finalPreRenderTypes []*types.T
	if needRender {
		finalPreRenderTypes = make([]*types.T, 0, nFinalAgg)
	}

	if err := getFinalAggFuncAndType(aggregations, &localAggs, nil, &finalAggs, p.ResultTypes, needRender,
		&finalIdxMap, &finalPreRenderTypes, &intermediateTypes, nil, statisticIndex); err != nil {
		return finalAggsSpec, finalAggsPost, err
	}

	// In queries like SELECT min(v) FROM kv GROUP BY k, not all group columns appear in the rendering.
	// Add IDENT expressions for them, as they need to be part of the output of the local stage for the
	// final stage to know about them.
	finalGroupCols := make([]uint32, len(groupCols))
	finalOrderedGroupCols := make([]uint32, 0, len(orderedGroupCols))
	getLocalAggAndTypeAndFinalGroupCols(p, groupCols, orderedGroupColSet, oldGroupCols,
		&localAggs, &intermediateTypes, &finalGroupCols, &finalOrderedGroupCols)

	// Create the merge ordering for the local stage (this will be maintained for results going into the final stage).
	ordCols, err := createMergeOrderingForLocalStage(groupColOrdering, oldGroupCols, finalGroupCols)
	if err != nil {
		return finalAggsSpec, finalAggsPost, err
	}

	finalAggsSpec = execinfrapb.AggregatorSpec{
		Type:             aggType,
		Aggregations:     finalAggs,
		GroupCols:        finalGroupCols,
		OrderedGroupCols: finalOrderedGroupCols,
	}

	localAggsSpec := execinfrapb.AggregatorSpec{
		Type:             aggType,
		Aggregations:     localAggs,
		GroupCols:        groupCols,
		OrderedGroupCols: orderedGroupCols,
	}

	p.AddNoGroupingStage(
		execinfrapb.ProcessorCoreUnion{Aggregator: &localAggsSpec},
		execinfrapb.PostProcessSpec{},
		intermediateTypes,
		execinfrapb.Ordering{Columns: ordCols},
	)

	if needRender {
		// Build rendering expressions.
		renderExprs, err := getTwiceAggregatorRender(planCtx, aggregations, finalPreRenderTypes, finalIdxMap, false)
		if err != nil {
			return finalAggsSpec, finalAggsPost, err
		}

		finalAggsPost.RenderExprs = renderExprs
	} else if len(finalAggs) < len(aggregations) {
		// We have removed some duplicates, so we need to add a projection.
		finalAggsPost.Projection = true
		finalAggsPost.OutputColumns = finalIdxMap
	}

	return finalAggsSpec, finalAggsPost, nil
}

// addTwoStageAggForTS add two stage aggregator spec
// Parameters:
// - planCtx: context
// - p: PhysicalPlan
// - aggType: AggregatorSpec_Type
// - aggregations: array of AggregatorSpec
// - groupCols: group by col index
// - orderedGroupCols: ordered group by col index
// - orderedGroupColSet: ordered group by col set
// - n: group node
// - addTSTwiceAgg: add ts twice agg flag
//
// Returns:
// - final agg spec
// - final agg post
// - err if has error
// if add synchronizer we need have twice agg
// eg          :count(e1)          | last(e1)/first/last_row/first_row | avg(e1)
// first    agg:count(e1)          | last(e1), lastts(e1)              | sum_int(e1),count(e1)
// synchronizer:add                | add                               | add
// twice    agg:sum_int(count(e1)) | last(last(e1), lastts(e1))        | avg(sum_int(e1)/count(e1))(add render sum_int/count)
// aggPush is true when need special operator for tsbs.
func (dsp *DistSQLPlanner) addTwoStageAggForTS(
	planCtx *PlanningCtx,
	p *PhysicalPlan,
	aggType execinfrapb.AggregatorSpec_Type,
	aggregations []execinfrapb.AggregatorSpec_Aggregation,
	groupCols []uint32,
	orderedGroupCols []uint32,
	orderedGroupColSet util.FastIntSet,
	n *groupNode,
	addTSTwiceAgg bool,
) (execinfrapb.AggregatorSpec, execinfrapb.PostProcessSpec, error) {
	var finalAggsSpec execinfrapb.AggregatorSpec
	var finalAggsPost execinfrapb.PostProcessSpec
	// Some aggregations might need multiple aggregation as part of their local and final stages
	// (along with a final render expression to combine the multiple aggregations into a single result).
	//
	// Count the total number of aggregation in the local/final
	// stages and keep track of whether any of them needs a final rendering.
	nLocalAgg, nTSFinalAgg, nFinalAgg, needRender := getTwoStageAggCount(aggregations)

	// We alloc the maximum possible number of unique local and final aggregations but do not initialize
	// any aggregations since we can de-duplicate equivalent local and final aggregations.
	localAggs := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nLocalAgg+len(groupCols))
	intermediateTypes := make([]types.T, 0, nLocalAgg+len(groupCols))

	// ts engine final agg and agg type after parallel execute
	tsFinalAggs := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nTSFinalAgg+len(groupCols))
	tsAggsTypes := make([]types.T, 0, nTSFinalAgg+len(groupCols))

	finalAggs := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nFinalAgg)
	// finalIdxMap maps the index i of the final aggregation (with respect to the i-th final aggregation
	// out of all final aggregations) to its index in the finalAggs slice.
	finalIdxMap := make([]uint32, nFinalAgg)

	// finalPreRenderTypes is passed to an IndexVarHelper which helps type-check the indexed variables
	// passed into FinalRendering for some aggregations. This has a 1-1 mapping to finalAggs
	var finalPreRenderTypes []*types.T
	if needRender {
		finalPreRenderTypes = make([]*types.T, 0, nFinalAgg)
	}

	if addTSTwiceAgg {
		if err := getFinalAggFuncAndType(aggregations, &localAggs, &tsFinalAggs, &finalAggs, p.ResultTypes, needRender,
			&finalIdxMap, &finalPreRenderTypes, &intermediateTypes, &tsAggsTypes, n.statisticIndex); err != nil {
			return finalAggsSpec, finalAggsPost, err
		}
	} else {
		if err := getFinalAggFuncAndType(aggregations, &localAggs, nil, &finalAggs, p.ResultTypes, needRender,
			&finalIdxMap, &finalPreRenderTypes, &intermediateTypes, nil, n.statisticIndex); err != nil {
			return finalAggsSpec, finalAggsPost, err
		}
	}

	// In queries like SELECT min(v) FROM kv GROUP BY k, not all group columns appear in the rendering.
	// Add IDENT expressions for them, as they need to be part of the output of the local stage for the
	// final stage to know about them.
	finalGroupCols := make([]uint32, len(groupCols))
	finalOrderedGroupCols := make([]uint32, 0, len(orderedGroupCols))
	getLocalAggAndTypeAndFinalGroupCols(p, groupCols, orderedGroupColSet, n.groupCols,
		&localAggs, &intermediateTypes, &finalGroupCols, &finalOrderedGroupCols)

	// Create the merge ordering for the local stage (this will be maintained for results going into the final stage).
	ordCols, err := createMergeOrderingForLocalStage(n.groupColOrdering, n.groupCols, finalGroupCols)
	if err != nil {
		return finalAggsSpec, finalAggsPost, err
	}

	finalAggsSpec = execinfrapb.AggregatorSpec{
		Type:             aggType,
		Aggregations:     finalAggs,
		GroupCols:        finalGroupCols,
		OrderedGroupCols: finalOrderedGroupCols,
	}

	localAggsSpec := execinfrapb.AggregatorSpec{
		Type:             aggType,
		Aggregations:     localAggs,
		GroupCols:        groupCols,
		OrderedGroupCols: orderedGroupCols,
		AggPushDown:      n.aggPushDown,
	}

	// construct Synchronizer post spec
	tsPost := execinfrapb.TSPostProcessSpec{}
	// add ts post spec output type from intermediateTypes
	tsPost.OutputTypes = make([]types.Family, len(intermediateTypes))
	for i, typ := range intermediateTypes {
		tsPost.OutputTypes[i] = typ.InternalType.Family
	}
	// add ts local agg
	p.AddTSNoGroupingStage(
		execinfrapb.TSProcessorCoreUnion{Aggregator: &localAggsSpec},
		tsPost,
		intermediateTypes,
		execinfrapb.Ordering{Columns: ordCols},
	)
	pushDownProcessorToTSReader(p, n.aggPushDown, true)

	// add parallel processor
	if n.addSynchronizer {
		addSynchronizerForAgg(planCtx, p, intermediateTypes, ordCols, &tsPost)

		// add ts final agg processor
		if addTSTwiceAgg {
			tsFinialAggsSpec := execinfrapb.AggregatorSpec{
				Type:             aggType,
				Aggregations:     tsFinalAggs,
				GroupCols:        finalGroupCols,        // group col from final col idx
				OrderedGroupCols: finalOrderedGroupCols, // order cols from final col idx
				AggPushDown:      false,                 // must false
			}

			tsPost.OutputTypes = make([]types.Family, len(tsAggsTypes))
			for i, typ := range tsAggsTypes {
				tsPost.OutputTypes[i] = typ.InternalType.Family
			}

			// add twice stage agg for parallel
			p.AddTSNoGroupingStage(
				execinfrapb.TSProcessorCoreUnion{Aggregator: &tsFinialAggsSpec},
				tsPost,
				tsAggsTypes,
				execinfrapb.Ordering{Columns: ordCols},
			)
		}
	}

	if needRender {
		// Build rendering expressions.
		renderExprs, err := getTwiceAggregatorRender(planCtx, aggregations, finalPreRenderTypes, finalIdxMap, false)
		if err != nil {
			return finalAggsSpec, finalAggsPost, err
		}

		finalAggsPost.RenderExprs = renderExprs
	} else if len(finalAggs) < len(aggregations) {
		// We have removed some duplicates, so we need to add a projection.
		finalAggsPost.Projection = true
		finalAggsPost.OutputColumns = finalIdxMap
	}

	return finalAggsSpec, finalAggsPost, nil
}

// getAggFuncAndType get plan agg function and type
// Parameters:
// - planCtx: ctx
// - funcs: are the aggregation functions that the renders use
// - aggFuncs: agg funcInfos
// - planToStreamColMap: maps planNode columns (see planColumns()) to columns in the result streams
// - canLocal: can exec on local plan
// - statisticIndex: the column index of agg func used from statistic reader
// Returns:
// - result:
// - the array of AggregatorSpec
// - the array of aggregate function result type
// - can not distribute execute
// - if has error, errors
func getAggFuncAndType(
	planCtx *PlanningCtx,
	funcs []*aggregateFuncHolder,
	aggFuncs *opt.AggFuncNames,
	planToStreamColMap []int,
	canLocal bool,
	statisticIndex opt.StatisticIndex,
) ([]execinfrapb.AggregatorSpec_Aggregation, [][]types.T, bool, error) {
	aggCount := len(funcs) + len(*aggFuncs)
	aggs := make([]execinfrapb.AggregatorSpec_Aggregation, aggCount)
	aggColTyps := make([][]types.T, aggCount)
	canNotDist := false
	var position int
	for i, fholder := range funcs {
		i = position
		position++
		// Convert the aggregate function to the enum value with the same string representation.
		replaceCountRows := false
		funcStr := strings.ToUpper(fholder.funcName)
		// use ts engine statistic reader need change count_rows to count(ts)
		if len(statisticIndex) > 0 && funcStr == "COUNT_ROWS" {
			funcStr = "COUNT"
			replaceCountRows = true
		}
		funcIdx, ok := execinfrapb.AggregatorSpec_Func_value[funcStr]
		if !ok {
			return aggs, aggColTyps, false, errors.Errorf("unknown aggregate %s", funcStr)
		}
		aggs[i].Func = execinfrapb.AggregatorSpec_Func(funcIdx)
		aggs[i].Distinct = fholder.isDistinct()
		if len(statisticIndex) > 0 {
			if replaceCountRows { // this agg function is count_row that has no param
				// count_row covert to count (ts), so force add ts column that in the head
				aggs[i].ColIdx = append(aggs[i].ColIdx, statisticIndex[i][0])
			} else {
				for j := range fholder.argRenderIdxs {
					aggs[i].ColIdx = append(aggs[i].ColIdx, statisticIndex[i][j])
				}
			}
		} else {
			for _, renderIdx := range fholder.argRenderIdxs {
				aggs[i].ColIdx = append(aggs[i].ColIdx, uint32(planToStreamColMap[renderIdx]))
			}
		}

		if fholder.hasFilter() {
			col := uint32(planToStreamColMap[fholder.filterRenderIdx])
			aggs[i].FilterColIdx = &col
		}
		aggs[i].Arguments = make([]execinfrapb.Expression, len(fholder.arguments))
		aggColTyps[i] = make([]types.T, len(fholder.arguments))
		for j, argument := range fholder.arguments {
			var err error
			aggs[i].Arguments[j], err = physicalplan.MakeExpression(argument, planCtx, nil, canLocal, false)

			if err != nil {
				return aggs, aggColTyps, false, err
			}
			aggColTyps[i][j] = *argument.ResolvedType()
		}

		if fholder.funcName == optbuilder.Gapfillinternal {
			canNotDist = true
		}

		if fholder.funcName == optbuilder.Interpolate {
			funcStr = strings.ToUpper((*aggFuncs)[0])
			*aggFuncs = (*aggFuncs)[1:]
			funcIdx, ok = execinfrapb.AggregatorSpec_Func_value[funcStr]
			if !ok {
				return aggs, aggColTyps, canNotDist, errors.Errorf("unknown aggregate %s", funcStr)
			}
			aggs[position].Func = execinfrapb.AggregatorSpec_Func(funcIdx)
			aggs[position].ColIdx = append(aggs[position].ColIdx, uint32(fholder.argRenderIdxs[0]))
			if strings.ToLower(funcStr) == sqlbase.LastAgg || strings.ToLower(funcStr) == sqlbase.LastRowAgg ||
				strings.ToLower(funcStr) == sqlbase.FirstAgg || strings.ToLower(funcStr) == sqlbase.FirstRowAgg {
				for _, v := range aggs {
					if v.Func == execinfrapb.AggregatorSpec_TIME_BUCKET_GAPFILL_INTERNAL {
						aggs[position].ColIdx = append(aggs[position].ColIdx, v.ColIdx[0])
					}
				}
			}
			position++
		}

		if aggs[i].Func == execinfrapb.AggregatorSpec_TIME_BUCKET_GAPFILL_INTERNAL {
			canNotDist = true
		}
	}

	return aggs, aggColTyps, canNotDist, nil
}

// addDistinct add distinct
func (dsp *DistSQLPlanner) addDistinct(
	aggregations []execinfrapb.AggregatorSpec_Aggregation, p *PhysicalPlan, plan planNode,
) {
	for _, e := range aggregations {
		if !e.Distinct {
			// child can push ts engine, so must add relational noop
			if p.ChildIsExecInTSEngine() {
				p.AddNoGroupingStage(
					execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{}},
					execinfrapb.PostProcessSpec{},
					p.ResultTypes,
					p.MergeOrdering,
				)
			}
			return
		}
	}

	var distinctColumnsSet util.FastIntSet
	for _, e := range aggregations {
		for _, colIdx := range e.ColIdx {
			distinctColumnsSet.Add(int(colIdx))
		}
	}
	if distinctColumnsSet.Len() > 0 {
		// We only need to plan distinct processors if we have non-empty
		// set of argument columns.
		distinctColumns := make([]uint32, 0, distinctColumnsSet.Len())
		distinctColumnsSet.ForEach(func(i int) {
			distinctColumns = append(distinctColumns, uint32(i))
		})
		ordering := dsp.convertOrdering(planReqOrdering(plan), p.PlanToStreamColMap).Columns
		orderedColumns := make([]uint32, 0, len(ordering))
		for _, ord := range ordering {
			if distinctColumnsSet.Contains(int(ord.ColIdx)) {
				// Ordered columns must be a subset of distinct columns, so
				// we only include such into orderedColumns slice.
				orderedColumns = append(orderedColumns, ord.ColIdx)
			}
		}
		sort.Slice(orderedColumns, func(i, j int) bool { return orderedColumns[i] < orderedColumns[j] })
		sort.Slice(distinctColumns, func(i, j int) bool { return distinctColumns[i] < distinctColumns[j] })
		distinctSpec := execinfrapb.ProcessorCoreUnion{
			Distinct: &execinfrapb.DistinctSpec{
				OrderedColumns:  orderedColumns,
				DistinctColumns: distinctColumns,
			},
		}
		if p.ChildIsExecInTSEngine() {
			// push down distinct, and then add noop
			var res []types.Family
			for _, typ := range p.ResultTypes {
				res = append(res, typ.InternalType.Family)
			}
			// add ts distinct
			p.AddTSNoGroupingStage(
				execinfrapb.TSProcessorCoreUnion{Distinct: &execinfrapb.DistinctSpec{
					OrderedColumns:  orderedColumns,
					DistinctColumns: distinctColumns,
				}},
				execinfrapb.TSPostProcessSpec{OutputTypes: res},
				p.ResultTypes,
				execinfrapb.Ordering{},
			)
			// add noop
			p.AddNoGroupingStage(
				execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{}},
				execinfrapb.PostProcessSpec{},
				p.ResultTypes, p.MergeOrdering,
			)
		} else {
			// Add distinct processors local to each existing current result processor.
			p.AddNoGroupingStage(distinctSpec, execinfrapb.PostProcessSpec{}, p.ResultTypes, p.MergeOrdering)
		}
	}
}

// getFinalColumnType get final column result type
func getFinalColumnType(
	p *PhysicalPlan,
	aggregations []execinfrapb.AggregatorSpec_Aggregation,
	aggregationsColumnTypes [][]types.T,
) ([]types.T, error) {
	finalOutTypes := make([]types.T, len(aggregations))
	for i, agg := range aggregations {
		argTypes := make([]types.T, len(agg.ColIdx)+len(agg.Arguments))
		for j, c := range agg.ColIdx {
			argTypes[j] = p.ResultTypes[c]
		}
		for j, argumentColumnType := range aggregationsColumnTypes[i] {
			argTypes[len(agg.ColIdx)+j] = argumentColumnType
		}
		var err error
		_, returnTyp, err := execinfrapb.GetAggregateInfo(agg.Func, argTypes...)
		if err != nil {
			return finalOutTypes, err
		}
		finalOutTypes[i] = *returnTyp
	}
	return finalOutTypes, nil
}

// checkIsMultiState check is multi state
func checkIsMultiState(
	prevStageNode roachpb.NodeID, aggregations []execinfrapb.AggregatorSpec_Aggregation,
) bool {
	multiStage := prevStageNode == 0
	if multiStage {
		for _, e := range aggregations {
			if e.Distinct {
				multiStage = false
				break
			}
			// Check that the function supports a local stage.
			if _, ok := physicalplan.DistAggregationTable[e.Func]; !ok {
				multiStage = false
				break
			}
		}
	}

	return multiStage
}

// getPreStageNodeID get prev stage node id, if has different, id is 0
func getPreStageNodeID(p *PhysicalPlan) roachpb.NodeID {
	prevStageNode := p.Processors[p.ResultRouters[0]].Node
	for i := 1; i < len(p.ResultRouters); i++ {
		if n := p.Processors[p.ResultRouters[i]].Node; n != prevStageNode {
			prevStageNode = 0
			break
		}
	}

	return prevStageNode
}

// connectStreamForMultiAgg connect local agg and final agg spec stream
func connectStreamForMultiAgg(p *PhysicalPlan, pIdxStart physicalplan.ProcessorIdx, tsEngine bool) {
	for bucket := 0; bucket < len(p.ResultRouters); bucket++ {
		pIdx := pIdxStart + physicalplan.ProcessorIdx(bucket)
		srcRouterSlot := bucket
		if tsEngine {
			srcRouterSlot = 0
		}
		p.MergeResultStreams(p.ResultRouters, srcRouterSlot, p.MergeOrdering, pIdx, 0, false /* forceSerialization */)
	}
}

// setupNewResultRouter setup new result router
func setupNewResultRouter(p *PhysicalPlan, pIdxStart physicalplan.ProcessorIdx) {
	for i := 0; i < len(p.ResultRouters); i++ {
		p.ResultRouters[i] = pIdxStart + physicalplan.ProcessorIdx(i)
	}
}

// addRelationalFinalAggStateSpec add relational final agg state spec to physical
func addRelationalFinalAggStateSpec(
	planCtx *PlanningCtx,
	p *PhysicalPlan,
	finalAggsSpec execinfrapb.AggregatorSpec,
	finalAggsPost execinfrapb.PostProcessSpec,
	stageID int32,
) {
	for _, resultProc := range p.ResultRouters {
		proc := physicalplan.Processor{
			Node: p.Processors[resultProc].Node,
			Spec: execinfrapb.ProcessorSpec{
				Input: []execinfrapb.InputSyncSpec{{
					// The other fields will be filled in by mergeResultStreams.
					ColumnTypes: p.ResultTypes,
				}},
				Core: execinfrapb.ProcessorCoreUnion{Aggregator: &finalAggsSpec},
				Post: finalAggsPost,
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
				}},
				StageID: stageID,
			},
		}
		p.AddProcessor(proc)
	}
}

// setupMultiAggFinalState set up relational engine multiple node agg final state
func (dsp *DistSQLPlanner) setupMultiAggFinalState(
	planCtx *PlanningCtx,
	p *PhysicalPlan,
	finalOutTypes []types.T,
	reqOrdering *ReqOrdering,
	finalAggsSpec execinfrapb.AggregatorSpec,
	finalAggsPost execinfrapb.PostProcessSpec,
) {
	// ts engine compute twice agg , relational engine compute third agg
	// Set up the output routers from the previous stage.
	setupChildDistributionStrategy(p, execinfrapb.OutputRouterSpec_BY_HASH, finalAggsSpec.GroupCols)

	// We have one final stage processor for each result router. This is a
	// somewhat arbitrary decision; we could have a different number of nodes
	// working on the final stage.
	pIdxStart := physicalplan.ProcessorIdx(len(p.Processors))

	addRelationalFinalAggStateSpec(planCtx, p, finalAggsSpec, finalAggsPost, p.NewStageID())

	// Connect the streams.
	connectStreamForMultiAgg(p, pIdxStart, false)

	// Set the new result routers.
	setupNewResultRouter(p, pIdxStart)

	p.ResultTypes = finalOutTypes
	p.SetMergeOrdering(dsp.convertOrdering(*reqOrdering, p.PlanToStreamColMap))
}

// setupMultiAggFinalStateForTS set up ts engine multiple node agg final state
func (dsp *DistSQLPlanner) setupMultiAggFinalStateForTS(
	p *PhysicalPlan,
	finalOutTypes []types.T,
	reqOrdering *ReqOrdering,
	reordering bool,
	coreUnion execinfrapb.TSProcessorCoreUnion,
	post execinfrapb.TSPostProcessSpec,
) {
	// set up child pass through
	setupChildDistributionStrategy(p, execinfrapb.OutputRouterSpec_PASS_THROUGH, nil)

	// We have one final stage processor for each result router. This is a
	// somewhat arbitrary decision; we could have a different number of nodes
	// working on the final stage.
	pIdxStart := physicalplan.ProcessorIdx(len(p.Processors))
	for _, resultProc := range p.ResultRouters {
		proc := physicalplan.Processor{
			Node: p.Processors[resultProc].Node,
			TSSpec: execinfrapb.TSProcessorSpec{
				Input: []execinfrapb.InputSyncSpec{{
					// The other fields will be filled in by mergeResultStreams.
					ColumnTypes: p.ResultTypes,
				}},
				Core: coreUnion,
				Post: post,
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
				}},
			},
			ExecInTSEngine: true,
		}
		p.AddProcessor(proc)
	}

	// Connect the streams.
	connectStreamForMultiAgg(p, pIdxStart, true)

	// Set the new result routers.
	setupNewResultRouter(p, pIdxStart)

	p.ResultTypes = finalOutTypes
	if reordering {
		p.SetMergeOrdering(dsp.convertOrdering(*reqOrdering, p.PlanToStreamColMap))
	}
}

// addSingleGroupStateForTS  add single group state for ts engine
func (dsp *DistSQLPlanner) addSingleGroupStateForTS(
	p *PhysicalPlan,
	prevStageNode roachpb.NodeID,
	core execinfrapb.TSProcessorCoreUnion,
	post execinfrapb.TSPostProcessSpec,
	finalOutTypes []types.T,
) {
	node := dsp.nodeDesc.NodeID
	if prevStageNode != 0 {
		node = prevStageNode
	}
	p.AddTSSingleGroupStage(node, core, post, finalOutTypes)
}

// addSingleGroupState add single group state for relational
func (dsp *DistSQLPlanner) addSingleGroupState(
	p *PhysicalPlan,
	prevStageNode roachpb.NodeID,
	finalAggsSpec execinfrapb.AggregatorSpec,
	finalAggsPost execinfrapb.PostProcessSpec,
	finalOutTypes []types.T,
) {
	node := dsp.nodeDesc.NodeID
	if prevStageNode != 0 {
		node = prevStageNode
	}
	p.AddSingleGroupStage(
		node,
		execinfrapb.ProcessorCoreUnion{Aggregator: &finalAggsSpec},
		finalAggsPost,
		finalOutTypes,
	)
}

// getPhysicalGroupCols get physical group cols from logical group cols
func getPhysicalGroupCols(
	p *PhysicalPlan, groupCols []int, statisticIndex opt.StatisticIndex,
) []uint32 {
	dstGroupCols := make([]uint32, len(groupCols))
	for i, idx := range groupCols {
		if len(statisticIndex) > 0 {
			dstGroupCols[i] = statisticIndex[i][0]
		} else {
			dstGroupCols[i] = uint32(p.PlanToStreamColMap[idx])
		}
	}

	return dstGroupCols
}

// getPhysicalOrderedGroupColsAndMap get physical ordered group cols and map from logical group cols
func getPhysicalOrderedGroupColsAndMap(
	p *PhysicalPlan, groupColOrdering sqlbase.ColumnOrdering,
) ([]uint32, util.FastIntSet) {
	orderedGroupCols := make([]uint32, len(groupColOrdering))
	var orderedGroupColSet util.FastIntSet
	for i, c := range groupColOrdering {
		orderedGroupCols[i] = uint32(p.PlanToStreamColMap[c.ColIdx])
		orderedGroupColSet.Add(c.ColIdx)
	}
	return orderedGroupCols, orderedGroupColSet
}

// addAggregators adds aggregators corresponding to a groupNode and updates the plan to
// reflect the groupNode. An evaluator stage is added if necessary.
// Invariants assumed:
//   - There is strictly no "pre-evaluation" necessary. If the given query is
//     'SELECT COUNT(k), v + w FROM kv GROUP BY v + w', the evaluation of the first
//     'v + w' is done at the source of the groupNode.
//   - We only operate on the following expressions:
//   - ONLY aggregation functions, with arguments pre-evaluated. So for
//     COUNT(k + v), we assume a stream of evaluated 'k + v' values.
//   - Expressions that CONTAIN an aggregation function, e.g. 'COUNT(k) + 1'.
//     This is evaluated in the post aggregation evaluator attached after.
//   - Expressions that also appear verbatim in the GROUP BY expressions.
//     For 'SELECT k GROUP BY k', the aggregation function added is IDENT,
//     therefore k just passes through unchanged.
//     All other expressions simply pass through unchanged, for e.g. '1' in
//     'SELECT 1 GROUP BY k'.
func (dsp *DistSQLPlanner) addAggregators(
	planCtx *PlanningCtx, p *PhysicalPlan, n *groupNode,
) error {
	for _, function := range n.funcs {
		if function.funcName == optbuilder.Gapfill && n.reqOrdering == nil {
			return errors.Errorf("%s must use ordered", optbuilder.Gapfill)
		}
	}

	// get agg spec
	aggregations, aggregationsColumnTypes, canNotDist, err := getAggFuncAndType(planCtx, n.funcs, n.aggFuncs,
		p.PlanToStreamColMap, !(n.engine == tree.EngineTypeTimeseries) && len(p.ResultRouters) == 1, opt.StatisticIndex{})
	if err != nil {
		return err
	}

	// get agg col output type
	finalOutTypes, err1 := getFinalColumnType(p, aggregations, aggregationsColumnTypes)
	if err1 != nil {
		return err1
	}

	aggType := execinfrapb.AggregatorSpec_NON_SCALAR
	if n.isScalar {
		aggType = execinfrapb.AggregatorSpec_SCALAR
	}

	groupCols := getPhysicalGroupCols(p, n.groupCols, opt.StatisticIndex{})
	orderedGroupCols, orderedGroupColSet := getPhysicalOrderedGroupColsAndMap(p, n.groupColOrdering)

	// We can have a local stage of distinct processors if all aggregation
	// functions are distinct.
	dsp.addDistinct(aggregations, p, n.plan)

	// Check if the previous stage is all on one node.
	prevStageNode := getPreStageNodeID(p)

	// We either have a local stage on each stream followed by a final stage, or
	// just a final stage. We only use a local stage if:
	//  - the previous stage is distributed on multiple nodes, and
	//  - all aggregation functions support it, and
	//  - no function is performing distinct aggregation.
	//  TODO(radu): we could relax this by splitting the aggregation into two
	//  different paths and joining on the results.
	multiStage := checkIsMultiState(prevStageNode, aggregations)

	var finalAggsSpec execinfrapb.AggregatorSpec
	var finalAggsPost execinfrapb.PostProcessSpec

	if !multiStage {
		finalAggsSpec = execinfrapb.AggregatorSpec{
			Type:             aggType,
			Aggregations:     aggregations,
			GroupCols:        groupCols,
			OrderedGroupCols: orderedGroupCols,
		}
	} else {
		finalAggsSpec, finalAggsPost, err = dsp.addTwiceAggregators(planCtx, p, aggType, n.groupColOrdering, aggregations,
			groupCols, n.groupCols, orderedGroupCols, orderedGroupColSet, opt.StatisticIndex{})
		if err != nil {
			return err
		}
	}

	// Set up the final stage.
	// Update p.PlanToStreamColMap; we will have a simple 1-to-1 mapping of
	// planNode columns to stream columns because the aggregator
	// has been programmed to produce the same columns as the groupNode.
	p.PlanToStreamColMap = identityMap(p.PlanToStreamColMap, len(aggregations))

	// notNeedDist is a special identifier used for Interpolate aggregate functions.
	// Interpolate agg should not dist.
	notNeedDist := false
	if canNotDist && p.Processors[0].TSSpec.Core.TagReader != nil {
		notNeedDist = true
	}
	if len(finalAggsSpec.GroupCols) == 0 || len(p.ResultRouters) == 1 || notNeedDist {
		// No GROUP BY, or we have a single stream. Use a single final aggregator.
		// If the previous stage was all on a single node, put the final
		// aggregator there. Otherwise, bring the results back on this node.
		dsp.addSingleGroupState(p, prevStageNode, finalAggsSpec, finalAggsPost, finalOutTypes)
	} else {
		// We distribute (by group columns) to multiple processors.
		dsp.setupMultiAggFinalState(planCtx, p, finalOutTypes, &n.reqOrdering, finalAggsSpec, finalAggsPost)
	}

	return nil
}

// addSynchronizerForTS construct synchronizer processor
func (dsp *DistSQLPlanner) addSynchronizerForTS(p *PhysicalPlan, degree int32) error {
	if !p.ChildIsExecInTSEngine() {
		return nil
	}
	p.SynchronizerChildRouters = append(p.SynchronizerChildRouters, p.ResultRouters...)
	TSPost := execinfrapb.TSPostProcessSpec{}

	TSCoreUnion := execinfrapb.TSProcessorCoreUnion{Synchronizer: &execinfrapb.TSSynchronizerSpec{
		Degree: degree,
	}}

	p.AddTSNoGroupingStage(TSCoreUnion,
		TSPost,
		p.ResultTypes,
		execinfrapb.Ordering{},
	)
	return nil
}

// addTSAggregators add ts engine agg processor
func (dsp *DistSQLPlanner) addTSAggregators(
	planCtx *PlanningCtx, p *PhysicalPlan, n *groupNode,
) error {
	// get agg spec and agg column type
	aggregations, aggregationsColumnTypes, _, err := getAggFuncAndType(planCtx, n.funcs, n.aggFuncs,
		p.PlanToStreamColMap, false, n.statisticIndex)
	if err != nil {
		return err
	}

	// the special operator not support distinct.
	for _, v := range aggregations {
		if v.Distinct {
			n.aggPushDown = false
		}
	}

	// get agg col output type
	finalOutTypes, err1 := getFinalColumnType(p, aggregations, aggregationsColumnTypes)
	if err1 != nil {
		return err1
	}

	aggType := execinfrapb.AggregatorSpec_NON_SCALAR
	if n.isScalar {
		aggType = execinfrapb.AggregatorSpec_SCALAR
	}

	groupCols := getPhysicalGroupCols(p, n.groupCols, n.statisticIndex)
	orderedGroupCols, orderedGroupColSet := getPhysicalOrderedGroupColsAndMap(p, n.groupColOrdering)

	// Check if the previous stage is all on one node.
	prevStageNode := getPreStageNodeID(p)

	var finalAggsSpec execinfrapb.AggregatorSpec
	var finalAggsPost execinfrapb.PostProcessSpec

	coreUnion := execinfrapb.TSProcessorCoreUnion{Aggregator: &execinfrapb.AggregatorSpec{
		Type:             aggType,
		Aggregations:     aggregations,
		GroupCols:        groupCols,
		OrderedGroupCols: orderedGroupCols,
		AggPushDown:      n.aggPushDown,
	}}

	// Set up the final stage.
	// Update p.PlanToStreamColMap; we will have a simple 1-to-1 mapping of
	// planNode columns to stream columns because the aggregator
	// has been programmed to produce the same columns as the groupNode.
	p.PlanToStreamColMap = identityMap(p.PlanToStreamColMap, len(aggregations))

	if len(p.ResultRouters) == 1 {
		// add synchronizer need parallel execute, so need local agg and finial agg
		if n.addSynchronizer {
			// add local agg
			finalAggsSpec, finalAggsPost, err = dsp.addTwoStageAggForTS(planCtx, p, aggType, aggregations, groupCols,
				orderedGroupCols, orderedGroupColSet, n, false)
			if err != nil {
				return err
			}

			// add twice agg for ts engine
			var tsFinalAggsPost execinfrapb.TSPostProcessSpec
			for _, val := range finalAggsPost.RenderExprs {
				tsFinalAggsPost.Renders = append(tsFinalAggsPost.Renders, val.String())
			}
			tsFinalAggsPost.Projection = finalAggsPost.Projection
			tsFinalAggsPost.OutputColumns = finalAggsPost.OutputColumns
			dsp.addSingleGroupStateForTS(p, prevStageNode, execinfrapb.TSProcessorCoreUnion{Aggregator: &finalAggsSpec},
				tsFinalAggsPost, finalOutTypes)
		} else {
			// No GROUP BY, or we have a single stream. Use a single final aggregator.
			// If the previous stage was all on a single node, put the final
			// aggregator there. Otherwise, bring the results back on this node.
			dsp.addSingleGroupStateForTS(p, prevStageNode, coreUnion, execinfrapb.TSPostProcessSpec{}, finalOutTypes)
			pushDownProcessorToTSReader(p, n.aggPushDown, true)
		}
	} else {
		// Check whether twice aggregation is necessary in time series engine
		needsTSTwiceAgg, err := needsTSTwiceAggregation(n, planCtx)
		if err != nil {
			return err
		}
		// add twice agg local
		finalAggsSpec, finalAggsPost, err = dsp.addTwoStageAggForTS(planCtx, p, aggType,
			aggregations, groupCols, orderedGroupCols, orderedGroupColSet, n, needsTSTwiceAgg)
		if err != nil {
			return err
		}

		p.AddTSTableReader()

		// get all data to gateway node compute agg
		if 0 == len(finalAggsSpec.GroupCols) {
			dsp.addSingleGroupState(p, prevStageNode, finalAggsSpec, finalAggsPost, finalOutTypes)
		} else {
			// ts engine compute twice agg , relational engine compute third agg
			dsp.setupMultiAggFinalState(planCtx, p, finalOutTypes, &n.reqOrdering, finalAggsSpec, finalAggsPost)
		}
	}

	return nil
}

func (dsp *DistSQLPlanner) createPlanForIndexJoin(
	planCtx *PlanningCtx, n *indexJoinNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.input)
	if err != nil {
		return PhysicalPlan{}, err
	}

	// In "index-join mode", the join reader assumes that the PK cols are a prefix
	// of the input stream columns (see #40749). We need a projection to make that
	// happen. The other columns are not used by the join reader.
	pkCols := make([]uint32, len(n.keyCols))
	for i := range n.keyCols {
		streamColOrd := plan.PlanToStreamColMap[n.keyCols[i]]
		if streamColOrd == -1 {
			panic("key column not in planToStreamColMap")
		}
		pkCols[i] = uint32(streamColOrd)
	}
	plan.AddProjection(pkCols)

	joinReaderSpec := execinfrapb.JoinReaderSpec{
		Table:             *n.table.desc.TableDesc(),
		IndexIdx:          0,
		Visibility:        n.table.colCfg.visibility.toDistSQLScanVisibility(),
		LockingStrength:   n.table.lockingStrength,
		LockingWaitPolicy: n.table.lockingWaitPolicy,
	}

	filter, err := physicalplan.MakeExpression(
		n.table.filter, planCtx, nil /* indexVarMap */, false, false)
	if err != nil {
		return PhysicalPlan{}, err
	}
	post := execinfrapb.PostProcessSpec{
		Filter:     filter,
		Projection: true,
	}

	// Calculate the output columns from n.cols.
	post.OutputColumns = make([]uint32, len(n.cols))
	plan.PlanToStreamColMap = identityMap(plan.PlanToStreamColMap, len(n.cols))

	for i := range n.cols {
		ord := tableOrdinal(n.table.desc, n.cols[i].ID, n.table.colCfg.visibility)
		post.OutputColumns[i] = uint32(ord)
	}

	colTypes, err := getTypesForPlanResult(n, plan.PlanToStreamColMap)
	if err != nil {
		return PhysicalPlan{}, err
	}
	if distributeIndexJoin.Get(&dsp.st.SV) && len(plan.ResultRouters) > 1 {
		// Instantiate one join reader for every stream.
		plan.AddNoGroupingStage(
			execinfrapb.ProcessorCoreUnion{JoinReader: &joinReaderSpec},
			post,
			colTypes,
			dsp.convertOrdering(n.reqOrdering, plan.PlanToStreamColMap),
		)
	} else {
		// Use a single join reader (if there is a single stream, on that node; if
		// not, on the gateway node).
		node := dsp.nodeDesc.NodeID
		if len(plan.ResultRouters) == 1 {
			node = plan.Processors[plan.ResultRouters[0]].Node
		}
		plan.AddSingleGroupStage(
			node,
			execinfrapb.ProcessorCoreUnion{JoinReader: &joinReaderSpec},
			post,
			colTypes,
		)
	}
	return plan, nil
}

// createPlanForLookupJoin creates a distributed plan for a lookupJoinNode.
// Note that this is a separate code path from the experimental path which
// converts joins to lookup joins.
func (dsp *DistSQLPlanner) createPlanForLookupJoin(
	planCtx *PlanningCtx, n *lookupJoinNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.input)
	if err != nil {
		return PhysicalPlan{}, err
	}

	joinReaderSpec := execinfrapb.JoinReaderSpec{
		Table:             *n.table.desc.TableDesc(),
		Type:              n.joinType,
		Visibility:        n.table.colCfg.visibility.toDistSQLScanVisibility(),
		LockingStrength:   n.table.lockingStrength,
		LockingWaitPolicy: n.table.lockingWaitPolicy,
	}
	joinReaderSpec.IndexIdx, err = getIndexIdx(n.table)
	if err != nil {
		return PhysicalPlan{}, err
	}
	joinReaderSpec.LookupColumns = make([]uint32, len(n.eqCols))
	for i, col := range n.eqCols {
		if plan.PlanToStreamColMap[col] == -1 {
			panic("lookup column not in planToStreamColMap")
		}
		joinReaderSpec.LookupColumns[i] = uint32(plan.PlanToStreamColMap[col])
	}
	joinReaderSpec.LookupColumnsAreKey = n.eqColsAreKey

	// The n.table node can be configured with an arbitrary set of columns. Apply
	// the corresponding projection.
	// The internal schema of the join reader is:
	//    <input columns>... <table columns>...
	numLeftCols := len(plan.ResultTypes)
	numOutCols := numLeftCols + len(n.table.cols)
	post := execinfrapb.PostProcessSpec{Projection: true}

	post.OutputColumns = make([]uint32, numOutCols)
	colTypes := make([]types.T, numOutCols)

	for i := 0; i < numLeftCols; i++ {
		colTypes[i] = plan.ResultTypes[i]
		post.OutputColumns[i] = uint32(i)
	}
	for i := range n.table.cols {
		colTypes[numLeftCols+i] = n.table.cols[i].Type
		ord := tableOrdinal(n.table.desc, n.table.cols[i].ID, n.table.colCfg.visibility)
		post.OutputColumns[numLeftCols+i] = uint32(numLeftCols + ord)
	}

	// Map the columns of the lookupJoinNode to the result streams of the
	// JoinReader.
	numInputNodeCols := len(planColumns(n.input))
	planToStreamColMap := makePlanToStreamColMap(numInputNodeCols + len(n.table.cols))
	copy(planToStreamColMap, plan.PlanToStreamColMap)
	for i := range n.table.cols {
		planToStreamColMap[numInputNodeCols+i] = numLeftCols + i
	}

	// Set the ON condition.
	if n.onCond != nil {
		// Note that (regardless of the join type or the OutputColumns projection)
		// the ON condition refers to the input columns with var indexes 0 to
		// numInputNodeCols-1 and to table columns with var indexes starting from
		// numInputNodeCols.
		indexVarMap := makePlanToStreamColMap(numInputNodeCols + len(n.table.cols))
		copy(indexVarMap, plan.PlanToStreamColMap)
		for i := range n.table.cols {
			indexVarMap[numInputNodeCols+i] = int(post.OutputColumns[numLeftCols+i])
		}
		var err error
		joinReaderSpec.OnExpr, err = physicalplan.MakeExpression(
			n.onCond, planCtx, indexVarMap, true, false,
		)
		if err != nil {
			return PhysicalPlan{}, err
		}
	}

	if n.joinType == sqlbase.LeftSemiJoin || n.joinType == sqlbase.LeftAntiJoin {
		// For anti/semi join, we only produce the input columns.
		planToStreamColMap = planToStreamColMap[:numInputNodeCols]
		post.OutputColumns = post.OutputColumns[:numInputNodeCols]
		colTypes = colTypes[:numInputNodeCols]
	}

	// Instantiate one join reader for every stream.
	plan.AddNoGroupingStage(
		execinfrapb.ProcessorCoreUnion{JoinReader: &joinReaderSpec},
		post,
		colTypes,
		dsp.convertOrdering(planReqOrdering(n), planToStreamColMap),
	)
	plan.PlanToStreamColMap = planToStreamColMap
	return plan, nil
}

// createPlanForZigzagJoin creates a distributed plan for a zigzagJoinNode.
func (dsp *DistSQLPlanner) createPlanForZigzagJoin(
	planCtx *PlanningCtx, n *zigzagJoinNode,
) (plan PhysicalPlan, err error) {

	tables := make([]sqlbase.TableDescriptor, len(n.sides))
	indexOrdinals := make([]uint32, len(n.sides))
	cols := make([]execinfrapb.Columns, len(n.sides))
	numStreamCols := 0
	for i, side := range n.sides {
		tables[i] = *side.scan.desc.TableDesc()
		indexOrdinals[i], err = getIndexIdx(side.scan)
		if err != nil {
			return PhysicalPlan{}, err
		}

		cols[i].Columns = make([]uint32, len(side.eqCols))
		for j, col := range side.eqCols {
			cols[i].Columns[j] = uint32(col)
		}

		numStreamCols += len(side.scan.desc.Columns)
	}

	// The zigzag join node only represents inner joins, so hardcode Type to
	// InnerJoin.
	zigzagJoinerSpec := execinfrapb.ZigzagJoinerSpec{
		Tables:        tables,
		IndexOrdinals: indexOrdinals,
		EqColumns:     cols,
		Type:          sqlbase.InnerJoin,
	}
	zigzagJoinerSpec.FixedValues = make([]*execinfrapb.ValuesCoreSpec, len(n.sides))

	// The fixed values are represented as a Values node with one tuple.
	for i := range n.sides {
		valuesPlan, err := dsp.createPlanForValues(planCtx, n.sides[i].fixedVals)
		if err != nil {
			return PhysicalPlan{}, err
		}
		zigzagJoinerSpec.FixedValues[i] = valuesPlan.PhysicalPlan.Processors[0].Spec.Core.Values
	}

	// The internal schema of the zigzag joiner is:
	//    <side 1 table columns> ... <side 2 table columns> ...
	// with only the columns in the specified index populated.
	//
	// The schema of the zigzagJoinNode is:
	//    <side 1 index columns> ... <side 2 index columns> ...
	// so the planToStreamColMap has to basically map index ordinals
	// to table ordinals.
	post := execinfrapb.PostProcessSpec{Projection: true}
	numOutCols := len(n.columns)

	post.OutputColumns = make([]uint32, numOutCols)
	colTypes := make([]types.T, numOutCols)
	planToStreamColMap := makePlanToStreamColMap(numOutCols)
	colOffset := 0
	i := 0

	// Populate post.OutputColumns (the implicit projection), result colTypes,
	// and the planToStreamColMap for index columns from all sides.
	for _, side := range n.sides {
		// Note that the side's scanNode only contains the columns from that
		// index that are also in n.columns. This is because we generated
		// colCfg.wantedColumns for only the necessary columns in
		// opt/exec/execbuilder/relational_builder.go, similar to lookup joins.
		for colIdx := range side.scan.cols {
			ord := tableOrdinal(side.scan.desc, side.scan.cols[colIdx].ID, side.scan.colCfg.visibility)
			post.OutputColumns[i] = uint32(colOffset + ord)
			colTypes[i] = side.scan.cols[colIdx].Type
			planToStreamColMap[i] = i

			i++
		}

		colOffset += len(side.scan.desc.Columns)
	}

	// Figure out the node where this zigzag joiner goes.
	//
	// TODO(itsbilal): Add support for restricting the Zigzag joiner
	// to a certain set of spans (similar to the InterleavedReaderJoiner)
	// on one side. Once that's done, we can split this processor across
	// multiple nodes here. Until then, schedule on the current node.
	nodeID := dsp.nodeDesc.NodeID

	stageID := plan.NewStageID()
	// Set the ON condition.
	if n.onCond != nil {
		// Note that the ON condition refers to the *internal* columns of the
		// processor (before the OutputColumns projection).
		indexVarMap := makePlanToStreamColMap(len(n.columns))
		for i := range n.columns {
			indexVarMap[i] = int(post.OutputColumns[i])
		}
		zigzagJoinerSpec.OnExpr, err = physicalplan.MakeExpression(
			n.onCond, planCtx, indexVarMap, true, false,
		)
		if err != nil {
			return PhysicalPlan{}, err
		}
	}

	// Build the PhysicalPlan.
	proc := physicalplan.Processor{
		Node: nodeID,
		Spec: execinfrapb.ProcessorSpec{
			Core:    execinfrapb.ProcessorCoreUnion{ZigzagJoiner: &zigzagJoinerSpec},
			Post:    post,
			Output:  []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
			StageID: stageID,
		},
	}

	plan.Processors = append(plan.Processors, proc)

	// Each result router correspond to each of the processors we appended.
	plan.ResultRouters = []physicalplan.ProcessorIdx{physicalplan.ProcessorIdx(0)}

	plan.PlanToStreamColMap = planToStreamColMap
	plan.ResultTypes = colTypes

	return plan, nil
}

// createPlanForSynchronizer creates a distributed plan for a SynchronizerNode.
func (dsp *DistSQLPlanner) createPlanForSynchronizer(
	planCtx *PlanningCtx, n *synchronizerNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.plan)
	if err != nil {
		return PhysicalPlan{}, err
	}

	if err := dsp.addSynchronizerForTS(&plan, planCtx.GetTsDop()); err != nil {
		return PhysicalPlan{}, err
	}
	return plan, nil
}

// createPlanForGroup creates a distributed plan for a group node.
func (dsp *DistSQLPlanner) createPlanForGroup(
	planCtx *PlanningCtx, n *groupNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.plan)
	if err != nil {
		return PhysicalPlan{}, err
	}

	if plan.SelfCanExecInTSEngine(n.engine == tree.EngineTypeTimeseries) {
		err = dsp.addTSAggregators(planCtx, &plan, n)
	} else {
		err = dsp.addAggregators(planCtx, &plan, n)
	}

	if err != nil {
		return PhysicalPlan{}, err
	}

	return plan, nil
}

// getTypesForPlanResult returns the types of the elements in the result streams
// of a plan that corresponds to a given planNode. If planToStreamColMap is nil,
// a 1-1 mapping is assumed.
func getTypesForPlanResult(node planNode, planToStreamColMap []int) ([]types.T, error) {
	nodeColumns := planColumns(node)
	if planToStreamColMap == nil {
		// No remapping.
		colTypes := make([]types.T, len(nodeColumns))
		for i := range nodeColumns {
			colTypes[i] = *nodeColumns[i].Typ
		}
		return colTypes, nil
	}
	numCols := 0
	for _, streamCol := range planToStreamColMap {
		if numCols <= streamCol {
			numCols = streamCol + 1
		}
	}
	colTypes := make([]types.T, numCols)
	for nodeCol, streamCol := range planToStreamColMap {
		if streamCol != -1 {
			colTypes[streamCol] = *nodeColumns[nodeCol].Typ
		}
	}
	return colTypes, nil
}

func (dsp *DistSQLPlanner) createPlanForJoin(
	planCtx *PlanningCtx, n *joinNode,
) (PhysicalPlan, error) {
	// See if we can create an interleave join plan.
	if planInterleavedJoins.Get(&dsp.st.SV) {
		plan, ok, err := dsp.tryCreatePlanForInterleavedJoin(planCtx, n)
		if err != nil {
			return PhysicalPlan{}, err
		}
		// An interleave join plan could be used. Return it.
		if ok {
			return plan, nil
		}
	}

	// Outline of the planning process for joins:
	//
	//  - We create PhysicalPlans for the left and right side. Each plan has a set
	//    of output routers with result that will serve as input for the join.
	//
	//  - We merge the list of processors and streams into a single plan. We keep
	//    track of the output routers for the left and right results.
	//
	//  - We add a set of joiner processors (say K of them).
	//
	//  - We configure the left and right output routers to send results to
	//    these joiners, distributing rows by hash (on the join equality columns).
	//    We are thus breaking up all input rows into K buckets such that rows
	//    that match on the equality columns end up in the same bucket. If there
	//    are no equality columns, we cannot distribute rows so we use a single
	//    joiner.
	//
	//  - The routers of the joiner processors are the result routers of the plan.

	leftPlan, err := dsp.createPlanForNode(planCtx, n.left.plan)
	if err != nil {
		return PhysicalPlan{}, err
	}

	if leftPlan.ChildIsExecInTSEngine() {
		leftPlan.AddNoopToTsProcessors(dsp.nodeDesc.NodeID, planCtx.IsLocal(), false)
	}

	rightPlan, err := dsp.createPlanForNode(planCtx, n.right.plan)
	if err != nil {
		return PhysicalPlan{}, err
	}

	if rightPlan.ChildIsExecInTSEngine() {
		rightPlan.AddNoopToTsProcessors(dsp.nodeDesc.NodeID, planCtx.IsLocal(), false)
	}

	// Nodes where we will run the join processors.
	var nodes []roachpb.NodeID

	// We initialize these properties of the joiner. They will then be used to
	// fill in the processor spec. See descriptions for HashJoinerSpec.
	var leftEqCols, rightEqCols []uint32
	var leftMergeOrd, rightMergeOrd execinfrapb.Ordering
	joinType := n.joinType

	// Figure out the left and right types.
	leftTypes := leftPlan.ResultTypes
	rightTypes := rightPlan.ResultTypes

	// Set up the equality columns.
	if numEq := len(n.pred.leftEqualityIndices); numEq != 0 {
		leftEqCols = eqCols(n.pred.leftEqualityIndices, leftPlan.PlanToStreamColMap)
		rightEqCols = eqCols(n.pred.rightEqualityIndices, rightPlan.PlanToStreamColMap)
	}

	var p PhysicalPlan
	var leftRouters, rightRouters []physicalplan.ProcessorIdx
	p.PhysicalPlan, leftRouters, rightRouters = physicalplan.MergePlans(
		&leftPlan.PhysicalPlan, &rightPlan.PhysicalPlan,
	)

	// Set up the output columns.
	if numEq := len(n.pred.leftEqualityIndices); numEq != 0 {
		nodes = findJoinProcessorNodes(leftRouters, rightRouters, p.Processors)

		if planMergeJoins.Get(&dsp.st.SV) && len(n.mergeJoinOrdering) > 0 {
			// TODO(radu): we currently only use merge joins when we have an ordering on
			// all equality columns. We should relax this by either:
			//  - implementing a hybrid hash/merge processor which implements merge
			//    logic on the columns we have an ordering on, and within each merge
			//    group uses a hashmap on the remaining columns
			//  - or: adding a sort processor to complete the order
			if len(n.mergeJoinOrdering) == len(n.pred.leftEqualityIndices) {
				// Excellent! We can use the merge joiner.
				leftMergeOrd = distsqlOrdering(n.mergeJoinOrdering, leftEqCols)
				rightMergeOrd = distsqlOrdering(n.mergeJoinOrdering, rightEqCols)
			}
		}
	} else {
		// Without column equality, we cannot distribute the join. Run a
		// single processor.
		nodes = []roachpb.NodeID{dsp.nodeDesc.NodeID}

		// If either side has a single stream, put the processor on that node. We
		// prefer the left side because that is processed first by the hash joiner.
		if len(leftRouters) == 1 {
			nodes[0] = p.Processors[leftRouters[0]].Node
		} else if len(rightRouters) == 1 {
			nodes[0] = p.Processors[rightRouters[0]].Node
		}
	}

	leftMap := leftPlan.PlanToStreamColMap
	rightMap := rightPlan.PlanToStreamColMap
	post, joinToStreamColMap := joinOutColumns(n, leftMap, rightMap, len(leftPlan.ResultTypes))
	onExpr, err := remapOnExpr(planCtx, n, leftMap, rightMap)
	if err != nil {
		return PhysicalPlan{}, err
	}

	// Create the Core spec.
	var core execinfrapb.ProcessorCoreUnion
	if leftMergeOrd.Columns == nil {
		core.HashJoiner = &execinfrapb.HashJoinerSpec{
			LeftEqColumns:        leftEqCols,
			RightEqColumns:       rightEqCols,
			OnExpr:               onExpr,
			Type:                 joinType,
			LeftEqColumnsAreKey:  n.pred.leftEqKey,
			RightEqColumnsAreKey: n.pred.rightEqKey,
		}
	} else {
		core.MergeJoiner = &execinfrapb.MergeJoinerSpec{
			LeftOrdering:         leftMergeOrd,
			RightOrdering:        rightMergeOrd,
			OnExpr:               onExpr,
			Type:                 joinType,
			LeftEqColumnsAreKey:  n.pred.leftEqKey,
			RightEqColumnsAreKey: n.pred.rightEqKey,
		}
	}
	p.AddNoopForJoinToAgent(nodes, leftRouters, rightRouters, dsp.nodeDesc.NodeID, &leftTypes, &rightTypes)
	p.AddJoinStage(
		nodes, core, post, leftEqCols, rightEqCols, leftTypes, rightTypes,
		leftMergeOrd, rightMergeOrd, leftRouters, rightRouters,
	)

	p.PlanToStreamColMap = joinToStreamColMap
	p.ResultTypes, err = getTypesForPlanResult(n, joinToStreamColMap)
	if err != nil {
		return PhysicalPlan{}, err
	}

	// Joiners may guarantee an ordering to outputs, so we ensure that
	// ordering is propagated through the input synchronizer of the next stage.
	// We can propagate the ordering from either side, we use the left side here.
	// Note that n.props only has a non-empty ordering for inner joins, where it
	// uses the mergeJoinOrdering.
	p.SetMergeOrdering(dsp.convertOrdering(n.reqOrdering, p.PlanToStreamColMap))
	return p, nil
}

func (dsp *DistSQLPlanner) createPlanForNode(
	planCtx *PlanningCtx, node planNode,
) (plan PhysicalPlan, err error) {
	planCtx.planDepth++

	switch n := node.(type) {
	// Keep these cases alphabetized, please!
	case *distinctNode:
		plan, err = dsp.createPlanForDistinct(planCtx, n)

	case *exportNode:
		plan, err = dsp.createPlanForExport(planCtx, n)

	case *filterNode:
		plan, err = dsp.createPlanForNode(planCtx, n.source.plan)
		if err != nil {
			return PhysicalPlan{}, err
		}

		if err := plan.AddFilter(n.filter, planCtx, plan.PlanToStreamColMap, n.engine == tree.EngineTypeTimeseries); err != nil {
			return PhysicalPlan{}, err
		}
	case *synchronizerNode:
		plan, err = dsp.createPlanForSynchronizer(planCtx, n)
	case *groupNode:
		plan, err = dsp.createPlanForGroup(planCtx, n)
	case *indexJoinNode:
		plan, err = dsp.createPlanForIndexJoin(planCtx, n)

	case *joinNode:
		plan, err = dsp.createPlanForJoin(planCtx, n)

	case *limitNode:
		plan, err = dsp.createPlanForNode(planCtx, n.plan)
		if err != nil {
			return PhysicalPlan{}, err
		}
		if err := n.evalLimit(planCtx.EvalContext()); err != nil {
			return PhysicalPlan{}, err
		}
		if err := plan.AddLimit(n.count, n.offset, planCtx, dsp.nodeDesc.NodeID, n.engine == tree.EngineTypeTimeseries); err != nil {
			return PhysicalPlan{}, err
		}
		pushDownProcessorToTSReader(&plan, n.canOpt, false)

	case *lookupJoinNode:
		plan, err = dsp.createPlanForLookupJoin(planCtx, n)

	case *ordinalityNode:
		plan, err = dsp.createPlanForOrdinality(planCtx, n)

	case *projectSetNode:
		plan, err = dsp.createPlanForProjectSet(planCtx, n)

	case *renderNode:
		plan, err = dsp.createPlanForNode(planCtx, n.source.plan)
		if err != nil {
			return PhysicalPlan{}, err
		}
		err = dsp.selectRenders(&plan, n, planCtx)
		if err != nil {
			return PhysicalPlan{}, err
		}

	case *scanNode:
		plan, err = dsp.createTableReaders(planCtx, n, nil)

	case *tsScanNode:
		plan, err = dsp.createTSReaders(planCtx, n)
		if err != nil {
			return plan, err
		}
		if planCtx.IsLocal() {
			// add output types
			plan.addTSOutputType(false)

			// add synchronizer
			if err = dsp.addSynchronizerForTS(&plan, planCtx.GetTsDop()); err != nil {
				return PhysicalPlan{}, err
			}

			// add output types
			plan.addTSOutputType(false)

			// add noop for gateway node to collect all data from all node
			plan.AddNoopToTsProcessors(dsp.nodeDesc.NodeID, true, false)
		}

	case *tsInsertNode:
		plan, err = dsp.createTSInsert(planCtx, n)

	case *tsInsertSelectNode:
		plan, err = dsp.createPlanForNode(planCtx, n.plan)
		if err != nil {
			return PhysicalPlan{}, err
		}
		err = dsp.createTSInsertSelect(&plan, n, planCtx)
		if err != nil {
			return PhysicalPlan{}, err
		}

	case *tsDeleteNode:
		if n.wrongPTag {
			plan, err = dsp.wrapPlan(planCtx, n)
		} else {
			plan, err = dsp.createTSDelete(planCtx, n)
		}

	case *tsTagUpdateNode:
		if n.wrongPTag {
			plan, err = dsp.wrapPlan(planCtx, n)
		} else {
			plan, err = dsp.createTSTagUpdate(planCtx, n)
		}

	case *tsDDLNode:
		plan, err = dsp.createTSDDL(planCtx, n)

	case *operateDataNode:
		plan, err = dsp.operateTSData(planCtx, n)

	case *sortNode:
		plan, err = dsp.createPlanForNode(planCtx, n.plan)
		if err != nil {
			return PhysicalPlan{}, err
		}

		dsp.addSorters(&plan, n)

	case *unaryNode:
		plan, err = dsp.createPlanForUnary(planCtx, n)

	case *unionNode:
		plan, err = dsp.createPlanForSetOp(planCtx, n)

	case *valuesNode:
		// Just like in checkSupportForNode, if a valuesNode wasn't specified in
		// the query, it means that it was autogenerated for things that we don't
		// want to be distributing, like populating values from a virtual table. So,
		// we wrap the plan instead.
		//
		// If the plan is local, we also wrap the plan to avoid pointless
		// serialization of the values, and also to avoid situations in which
		// expressions within the valuesNode were not distributable in the first
		// place.
		//
		// Finally, if noEvalSubqueries is set, it means that nothing has replaced
		// the subqueries with their results yet, which again means that we can't
		// plan a DistSQL values node, which requires that all expressions be
		// evaluatable.
		//
		// NB: If you change this conditional, you must also change it in
		// checkSupportForNode!
		if !n.specifiedInQuery || planCtx.isLocal || planCtx.noEvalSubqueries {
			plan, err = dsp.wrapPlan(planCtx, n)
		} else {
			plan, err = dsp.createPlanForValues(planCtx, n)
		}

	case *windowNode:
		plan, err = dsp.createPlanForWindow(planCtx, n)

	case *zeroNode:
		plan, err = dsp.createPlanForZero(planCtx, n)

	case *zigzagJoinNode:
		plan, err = dsp.createPlanForZigzagJoin(planCtx, n)
	//case *pipeGroupNode:
	//	plan, err = dsp.createPlanForPipeGroup(planCtx, n)

	default:
		// Can't handle a node? We wrap it and continue on our way.
		plan, err = dsp.wrapPlan(planCtx, n)
	}
	if err != nil {
		return plan, err
	}

	// add output types for ts processor
	plan.addTSOutputType(false)

	if dsp.shouldPlanTestMetadata() {
		if err := plan.CheckLastStagePost(); err != nil {
			log.Fatal(planCtx.ctx, err)
		}
		plan.AddNoGroupingStageWithCoreFunc(
			func(_ int, _ *physicalplan.Processor) execinfrapb.ProcessorCoreUnion {
				return execinfrapb.ProcessorCoreUnion{
					MetadataTestSender: &execinfrapb.MetadataTestSenderSpec{
						ID: uuid.MakeV4().String(),
					},
				}
			},
			execinfrapb.PostProcessSpec{},
			plan.ResultTypes,
			plan.MergeOrdering,
		)
	}

	return plan, err
}

// wrapPlan produces a DistSQL processor for an arbitrary planNode. This is
// invoked when a particular planNode can't be distributed for some reason. It
// will create a planNodeToRowSource wrapper for the sub-tree that's not
// plannable by DistSQL. If that sub-tree has DistSQL-plannable sources, they
// will be planned by DistSQL and connected to the wrapper.
func (dsp *DistSQLPlanner) wrapPlan(planCtx *PlanningCtx, n planNode) (PhysicalPlan, error) {
	useFastPath := planCtx.planDepth == 1 && planCtx.stmtType == tree.RowsAffected

	// First, we search the planNode tree we're trying to wrap for the first
	// DistSQL-enabled planNode in the tree. If we find one, we ask the planner to
	// continue the DistSQL planning recursion on that planNode.
	seenTop := false
	nParents := uint32(0)
	var p PhysicalPlan
	var nodeID = dsp.nodeDesc.NodeID
	var query string
	// This will be set to first DistSQL-enabled planNode we find, if any. We'll
	// modify its parent later to connect its source to the DistSQL-planned
	// subtree.
	var firstNotWrapped planNode
	if err := walkPlan(planCtx.ctx, n, planObserver{
		enterNode: func(ctx context.Context, nodeName string, plan planNode) (bool, error) {
			switch plan.(type) {
			case *explainDistSQLNode, *explainPlanNode, *explainVecNode:
				// Don't continue recursing into explain nodes - they need to be left
				// alone since they handle their own planning later.
				return false, nil
			}
			if !seenTop {
				// We know we're wrapping the first node, so ignore it.
				seenTop = true
				return true, nil
			}
			var err error
			// Continue walking until we find a node that has a DistSQL
			// representation - that's when we'll quit the wrapping process and hand
			// control of planning back to the DistSQL physical planner.
			if !dsp.mustWrapNode(planCtx, plan) {
				firstNotWrapped = plan
				p, err = dsp.createPlanForNode(planCtx, plan)
				if err != nil {
					return false, err
				}
				nParents++
				return false, nil
			}
			return true, nil
		},
	}); err != nil {
		return PhysicalPlan{}, err
	}
	if nParents > 1 {
		return PhysicalPlan{}, errors.Errorf("can't wrap plan %v %T with more than one input", n, n)
	}

	// Copy the evalCtx.
	evalCtx := *planCtx.ExtendedEvalCtx
	if nodeID == dsp.nodeDesc.NodeID {
		// We permit the planNodeToRowSource to trigger the wrapped planNode's fast
		// path if its the very first node in the flow, and if the statement type we're
		// expecting is in fact RowsAffected. RowsAffected statements return a single
		// row with the number of rows affected by the statement, and are the only
		// types of statement where it's valid to invoke a plan's fast path.
		wrapper, err := makePlanNodeToRowSource(n,
			runParams{
				extendedEvalCtx: &evalCtx,
				p:               planCtx.planner,
			},
			useFastPath,
		)
		if err != nil {
			return PhysicalPlan{}, err
		}
		wrapper.firstNotWrapped = firstNotWrapped

		idx := uint32(len(p.LocalProcessors))
		p.LocalProcessors = append(p.LocalProcessors, wrapper)
		p.LocalProcessorIndexes = append(p.LocalProcessorIndexes, &idx)
		var input []execinfrapb.InputSyncSpec
		if firstNotWrapped != nil {
			// We found a DistSQL-plannable subtree - create an input spec for it.
			input = []execinfrapb.InputSyncSpec{{
				Type:        execinfrapb.InputSyncSpec_UNORDERED,
				ColumnTypes: p.ResultTypes,
			}}
		}
		name := nodeName(n)
		if name == "apply-join" {
			p.InlcudeApplyJoin = true
		}
		if planCtx.IsLocal() {
			// if subprocess of apply-join is processor of time series, need to add Noop-processor
			// createPlanForNode handle hash-join(inner-join) to add noop
			p.AddNoopToTsProcessors(dsp.nodeDesc.NodeID, true, false)
		}
		proc := physicalplan.Processor{
			Node: dsp.nodeDesc.NodeID,
			Spec: execinfrapb.ProcessorSpec{
				Input: input,
				Core: execinfrapb.ProcessorCoreUnion{LocalPlanNode: &execinfrapb.LocalPlanNodeSpec{
					RowSourceIdx: &idx,
					NumInputs:    &nParents,
					Name:         &name,
				}},
				Post: execinfrapb.PostProcessSpec{},
				Output: []execinfrapb.OutputRouterSpec{{
					Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
				}},
				StageID: p.NewStageID(),
			},
		}
		pIdx := p.AddProcessor(proc)
		p.ResultTypes = wrapper.outputTypes
		p.PlanToStreamColMap = identityMapInPlace(make([]int, len(p.ResultTypes)))
		if firstNotWrapped != nil {
			// If we found a DistSQL-plannable subtree, we need to add a result stream
			// between it and the physicalPlan we're creating here.
			p.MergeResultStreams(p.ResultRouters, 0, p.MergeOrdering, pIdx, 0, false /* forceSerialization */)
		}
		// ResultRouters gets overwritten each time we add a new PhysicalPlan. We will
		// just have a single result router, since local processors aren't
		// distributed, so make sure that p.ResultRouters has at least 1 slot and
		// write the new processor index there.
		if cap(p.ResultRouters) < 1 {
			p.ResultRouters = make([]physicalplan.ProcessorIdx, 1)
		} else {
			p.ResultRouters = p.ResultRouters[:1]
		}
		p.ResultRouters[0] = pIdx
		return p, nil
	}
	planCtx.isLocal = false
	proc := physicalplan.Processor{
		Node: nodeID,
		Spec: execinfrapb.ProcessorSpec{
			Input: []execinfrapb.InputSyncSpec{},
			Core: execinfrapb.ProcessorCoreUnion{RemotePlanNode: &execinfrapb.RemotePlanNodeSpec{
				Query: query,
			}},
			Post: execinfrapb.PostProcessSpec{},
			Output: []execinfrapb.OutputRouterSpec{{
				Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
			}},
			StageID: p.NewStageID(),
		},
	}
	pIdx := p.AddProcessor(proc)
	nodeColumns := planColumns(n)

	colTypes := make([]types.T, len(nodeColumns))
	for i := range nodeColumns {
		colTypes[i] = *nodeColumns[i].Typ
	}
	p.ResultTypes = colTypes
	p.PlanToStreamColMap = identityMapInPlace(make([]int, len(p.ResultTypes)))
	if firstNotWrapped != nil {
		// If we found a DistSQL-plannable subtree, we need to add a result stream
		// between it and the physicalPlan we're creating here.
		p.MergeResultStreams(p.ResultRouters, 0, p.MergeOrdering, pIdx, 0, false /* forceSerialization */)
	}
	// ResultRouters gets overwritten each time we add a new PhysicalPlan. We will
	// just have a single result router, since local processors aren't
	// distributed, so make sure that p.ResultRouters has at least 1 slot and
	// write the new processor index there.
	if cap(p.ResultRouters) < 1 {
		p.ResultRouters = make([]physicalplan.ProcessorIdx, 1)
	} else {
		p.ResultRouters = p.ResultRouters[:1]
	}
	p.ResultRouters[0] = pIdx
	return p, nil
}

// createValuesPlan creates a plan with a single Values processor
// located on the gateway node and initialized with given numRows
// and rawBytes that need to be precomputed beforehand.
func (dsp *DistSQLPlanner) createValuesPlan(
	resultTypes []types.T, numRows int, rawBytes [][]byte,
) (PhysicalPlan, error) {
	numColumns := len(resultTypes)
	s := execinfrapb.ValuesCoreSpec{
		Columns: make([]execinfrapb.DatumInfo, numColumns),
	}

	for i, t := range resultTypes {
		s.Columns[i].Encoding = sqlbase.DatumEncoding_VALUE
		s.Columns[i].Type = t
	}

	s.NumRows = uint64(numRows)
	s.RawBytes = rawBytes

	plan := physicalplan.PhysicalPlan{
		Processors: []physicalplan.Processor{{
			// TODO: find a better node to place processor at
			Node: dsp.nodeDesc.NodeID,
			Spec: execinfrapb.ProcessorSpec{
				Core:   execinfrapb.ProcessorCoreUnion{Values: &s},
				Output: []execinfrapb.OutputRouterSpec{{Type: execinfrapb.OutputRouterSpec_PASS_THROUGH}},
			},
		}},
		ResultRouters: []physicalplan.ProcessorIdx{0},
		ResultTypes:   resultTypes,
	}

	return PhysicalPlan{
		PhysicalPlan:       plan,
		PlanToStreamColMap: identityMapInPlace(make([]int, numColumns)),
	}, nil
}

func (dsp *DistSQLPlanner) createPlanForValues(
	planCtx *PlanningCtx, n *valuesNode,
) (PhysicalPlan, error) {
	params := runParams{
		ctx:             planCtx.ctx,
		extendedEvalCtx: planCtx.ExtendedEvalCtx,
	}
	colTypes, err := getTypesForPlanResult(n, nil /* planToStreamColMap */)
	if err != nil {
		return PhysicalPlan{}, err
	}

	if err := n.startExec(params); err != nil {
		return PhysicalPlan{}, err
	}
	defer n.Close(planCtx.ctx)

	var a sqlbase.DatumAlloc

	numRows := n.rows.Len()
	rawBytes := make([][]byte, numRows)
	for i := 0; i < numRows; i++ {
		if next, err := n.Next(runParams{ctx: planCtx.ctx}); !next {
			return PhysicalPlan{}, err
		}

		var buf []byte
		datums := n.Values()
		for j := range n.columns {
			var err error
			datum := sqlbase.DatumToEncDatum(&colTypes[j], datums[j])
			buf, err = datum.Encode(&colTypes[j], &a, sqlbase.DatumEncoding_VALUE, buf)
			if err != nil {
				return PhysicalPlan{}, err
			}
		}
		rawBytes[i] = buf
	}
	physicalPlan, err := dsp.createValuesPlan(colTypes, numRows, rawBytes)

	return physicalPlan, err
}

func (dsp *DistSQLPlanner) createPlanForUnary(
	planCtx *PlanningCtx, n *unaryNode,
) (PhysicalPlan, error) {
	colTypes, err := getTypesForPlanResult(n, nil /* planToStreamColMap */)
	if err != nil {
		return PhysicalPlan{}, err
	}

	physicalPlan, err := dsp.createValuesPlan(colTypes, 1 /* numRows */, nil /* rawBytes */)
	return physicalPlan, err
}

func (dsp *DistSQLPlanner) createPlanForZero(
	planCtx *PlanningCtx, n *zeroNode,
) (PhysicalPlan, error) {
	colTypes, err := getTypesForPlanResult(n, nil /* planToStreamColMap */)
	if err != nil {
		return PhysicalPlan{}, err
	}

	physicalPlan, err := dsp.createValuesPlan(colTypes, 0 /* numRows */, nil /* rawBytes */)
	return physicalPlan, err
}

func createDistinctSpec(n *distinctNode, cols []int) *execinfrapb.DistinctSpec {
	var orderedColumns []uint32
	if !n.columnsInOrder.Empty() {
		orderedColumns = make([]uint32, 0, n.columnsInOrder.Len())
		for i, ok := n.columnsInOrder.Next(0); ok; i, ok = n.columnsInOrder.Next(i + 1) {
			orderedColumns = append(orderedColumns, uint32(cols[i]))
		}
	}

	var distinctColumns []uint32
	if !n.distinctOnColIdxs.Empty() {
		for planCol, streamCol := range cols {
			if streamCol != -1 && n.distinctOnColIdxs.Contains(planCol) {
				distinctColumns = append(distinctColumns, uint32(streamCol))
			}
		}
	} else {
		// If no distinct columns were specified, run distinct on the entire row.
		for planCol := range planColumns(n) {
			if streamCol := cols[planCol]; streamCol != -1 {
				distinctColumns = append(distinctColumns, uint32(streamCol))
			}
		}
	}

	return &execinfrapb.DistinctSpec{
		OrderedColumns:   orderedColumns,
		DistinctColumns:  distinctColumns,
		NullsAreDistinct: n.nullsAreDistinct,
		ErrorOnDup:       n.errorOnDup,
	}
}

func (dsp *DistSQLPlanner) createPlanForDistinct(
	planCtx *PlanningCtx, n *distinctNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.plan)
	if err != nil {
		return PhysicalPlan{}, err
	}

	distinctSpec := execinfrapb.ProcessorCoreUnion{
		Distinct: createDistinctSpec(n, plan.PlanToStreamColMap),
	}

	if plan.SelfCanExecInTSEngine(n.engine == tree.EngineTypeTimeseries) {
		plan.AddTSNoGroupingStage(
			execinfrapb.TSProcessorCoreUnion{Distinct: distinctSpec.Distinct},
			execinfrapb.TSPostProcessSpec{}, plan.ResultTypes, plan.MergeOrdering)
		if len(plan.ResultRouters) > 1 {
			// add output types for ts processor
			plan.addTSOutputType(false)

			// add ts engine data receiver
			plan.AddTSTableReader()
		}
	} else {
		// TODO(arjun): This is potentially memory inefficient if we don't have any sorted columns.

		// Add distinct processors local to each existing current result processor.
		plan.AddNoGroupingStage(distinctSpec, execinfrapb.PostProcessSpec{}, plan.ResultTypes, plan.MergeOrdering)
	}

	if len(plan.ResultRouters) == 1 {
		return plan, nil
	}

	// TODO(arjun): We could distribute this final stage by hash.
	plan.AddSingleGroupStage(dsp.nodeDesc.NodeID, distinctSpec, execinfrapb.PostProcessSpec{}, plan.ResultTypes)

	return plan, nil
}

func (dsp *DistSQLPlanner) createPlanForOrdinality(
	planCtx *PlanningCtx, n *ordinalityNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.source)
	if err != nil {
		return PhysicalPlan{}, err
	}

	// add noop to receive Data from AE
	if plan.ChildIsExecInTSEngine() {
		plan.AddNoGroupingStage(
			execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{}},
			execinfrapb.PostProcessSpec{},
			plan.ResultTypes,
			plan.MergeOrdering,
		)
	}
	ordinalitySpec := execinfrapb.ProcessorCoreUnion{
		Ordinality: &execinfrapb.OrdinalitySpec{},
	}

	plan.PlanToStreamColMap = append(plan.PlanToStreamColMap, len(plan.ResultTypes))
	outputTypes := append(plan.ResultTypes, *types.Int)

	// WITH ORDINALITY never gets distributed so that the gateway node can
	// always number each row in order.
	plan.AddSingleGroupStage(dsp.nodeDesc.NodeID, ordinalitySpec, execinfrapb.PostProcessSpec{}, outputTypes)

	return plan, nil
}

func createProjectSetSpec(
	planCtx *PlanningCtx, n *projectSetNode, indexVarMap []int,
) (*execinfrapb.ProjectSetSpec, error) {
	spec := execinfrapb.ProjectSetSpec{
		Exprs:            make([]execinfrapb.Expression, len(n.exprs)),
		GeneratedColumns: make([]types.T, len(n.columns)-n.numColsInSource),
		NumColsPerGen:    make([]uint32, len(n.exprs)),
	}
	for i, expr := range n.exprs {
		var err error
		spec.Exprs[i], err = physicalplan.MakeExpression(expr, planCtx, indexVarMap, true, false)
		if err != nil {
			return nil, err
		}
	}
	for i, col := range n.columns[n.numColsInSource:] {
		spec.GeneratedColumns[i] = *col.Typ
	}
	for i, n := range n.numColsPerGen {
		spec.NumColsPerGen[i] = uint32(n)
	}
	return &spec, nil
}

func (dsp *DistSQLPlanner) createPlanForProjectSet(
	planCtx *PlanningCtx, n *projectSetNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.source)
	if err != nil {
		return PhysicalPlan{}, err
	}
	numResults := len(plan.ResultTypes)

	indexVarMap := makePlanToStreamColMap(len(n.columns))
	copy(indexVarMap, plan.PlanToStreamColMap)

	// Create the project set processor spec.
	projectSetSpec, err := createProjectSetSpec(planCtx, n, indexVarMap)
	if err != nil {
		return PhysicalPlan{}, err
	}
	spec := execinfrapb.ProcessorCoreUnion{
		ProjectSet: projectSetSpec,
	}

	// Since ProjectSet tends to be a late stage which produces more rows than its
	// source, we opt to perform it only on the gateway node. If we encounter
	// cases in the future where this is non-optimal (perhaps if its output is
	// filtered), we could try to detect these cases and use AddNoGroupingStage
	// instead.
	outputTypes := append(plan.ResultTypes, projectSetSpec.GeneratedColumns...)
	plan.AddSingleGroupStage(dsp.nodeDesc.NodeID, spec, execinfrapb.PostProcessSpec{}, outputTypes)

	// Add generated columns to PlanToStreamColMap.
	for i := range projectSetSpec.GeneratedColumns {
		plan.PlanToStreamColMap = append(plan.PlanToStreamColMap, numResults+i)
	}
	return plan, nil
}

// isOnlyOnGateway returns true if a physical plan is executed entirely on the
// gateway node.
func (dsp *DistSQLPlanner) isOnlyOnGateway(plan *PhysicalPlan) bool {
	if len(plan.ResultRouters) == 1 {
		processorIdx := plan.ResultRouters[0]
		if plan.Processors[processorIdx].Node == dsp.nodeDesc.NodeID {
			return true
		}
	}
	return false
}

// TODO(abhimadan): Refactor this function to reduce the UNION vs
// EXCEPT/INTERSECT and DISTINCT vs ALL branching.
//
// createPlanForSetOp creates a physical plan for "set operations". UNION plans
// are created by merging the left and right plans together, and INTERSECT and
// EXCEPT plans are created by performing a special type of join on the left and
// right sides. In the UNION DISTINCT case, a distinct stage is placed after the
// plans are merged, and in the INTERSECT/EXCEPT DISTINCT cases, distinct stages
// are added as the inputs of the join stage. In all DISTINCT cases, an
// additional distinct stage is placed at the end of the left and right plans if
// there are multiple nodes involved in the query, to reduce the amount of
// unnecessary network I/O.
//
// Examples (single node):
//
//   - Query: ( VALUES (1), (2), (2) ) UNION ( VALUES (2), (3) )
//     Plan:
//     VALUES        VALUES
//     |             |
//     -------------
//     |
//     DISTINCT
//
//   - Query: ( VALUES (1), (2), (2) ) INTERSECT ALL ( VALUES (2), (3) )
//     Plan:
//     VALUES        VALUES
//     |             |
//     -------------
//     |
//     JOIN
//
//   - Query: ( VALUES (1), (2), (2) ) EXCEPT ( VALUES (2), (3) )
//     Plan:
//     VALUES        VALUES
//     |             |
//     DISTINCT       DISTINCT
//     |             |
//     -------------
//     |
//     JOIN
func (dsp *DistSQLPlanner) createPlanForSetOp(
	planCtx *PlanningCtx, n *unionNode,
) (PhysicalPlan, error) {
	leftLogicalPlan := n.left
	leftPlan, err := dsp.createPlanForNode(planCtx, n.left)
	if err != nil {
		return PhysicalPlan{}, err
	}
	rightLogicalPlan := n.right
	rightPlan, err := dsp.createPlanForNode(planCtx, n.right)
	if err != nil {
		return PhysicalPlan{}, err
	}
	if n.inverted {
		leftPlan, rightPlan = rightPlan, leftPlan
		leftLogicalPlan, rightLogicalPlan = rightLogicalPlan, leftLogicalPlan
	}
	childPhysicalPlans := []*PhysicalPlan{&leftPlan, &rightPlan}

	// Check that the left and right side PlanToStreamColMaps are equivalent.
	// TODO(solon): Are there any valid UNION/INTERSECT/EXCEPT cases where these
	// differ? If we encounter any, we could handle them by adding a projection on
	// the unioned columns on each side, similar to how we handle mismatched
	// ResultTypes.
	if !reflect.DeepEqual(leftPlan.PlanToStreamColMap, rightPlan.PlanToStreamColMap) {
		return PhysicalPlan{}, errors.Errorf(
			"planToStreamColMap mismatch: %v, %v", leftPlan.PlanToStreamColMap,
			rightPlan.PlanToStreamColMap)
	}
	planToStreamColMap := leftPlan.PlanToStreamColMap
	streamCols := make([]uint32, 0, len(planToStreamColMap))
	for _, streamCol := range planToStreamColMap {
		if streamCol < 0 {
			continue
		}
		streamCols = append(streamCols, uint32(streamCol))
	}

	var distinctSpecs [2]execinfrapb.ProcessorCoreUnion

	if !n.all {
		var distinctOrds [2]execinfrapb.Ordering
		distinctOrds[0] = execinfrapb.ConvertToMappedSpecOrdering(
			planReqOrdering(leftLogicalPlan), leftPlan.PlanToStreamColMap,
		)
		distinctOrds[1] = execinfrapb.ConvertToMappedSpecOrdering(
			planReqOrdering(rightLogicalPlan), rightPlan.PlanToStreamColMap,
		)

		// Build distinct processor specs for the left and right child plans.
		//
		// Note there is the potential for further network I/O optimization here
		// in the UNION case, since rows are not deduplicated between left and right
		// until the single group stage. In the worst case (total duplication), this
		// causes double the amount of data to be streamed as necessary.
		for side, plan := range childPhysicalPlans {
			sortCols := make([]uint32, len(distinctOrds[side].Columns))
			for i, ord := range distinctOrds[side].Columns {
				sortCols[i] = ord.ColIdx
			}
			distinctSpec := &distinctSpecs[side]
			distinctSpec.Distinct = &execinfrapb.DistinctSpec{
				DistinctColumns: streamCols,
				OrderedColumns:  sortCols,
			}
			if !dsp.isOnlyOnGateway(plan) {
				// TODO(solon): We could skip this stage if there is a strong key on
				// the result columns.
				plan.AddNoGroupingStage(
					*distinctSpec, execinfrapb.PostProcessSpec{}, plan.ResultTypes, distinctOrds[side])
				plan.AddProjection(streamCols)
			}
		}
	}

	var p PhysicalPlan
	p.SetRowEstimates(&leftPlan.PhysicalPlan, &rightPlan.PhysicalPlan)

	// Merge the plans' PlanToStreamColMap, which we know are equivalent.
	p.PlanToStreamColMap = planToStreamColMap

	// Merge the plans' result types and merge ordering.
	resultTypes, err := physicalplan.MergeResultTypes(leftPlan.ResultTypes, rightPlan.ResultTypes)
	if err != nil {
		return PhysicalPlan{}, err
	}

	if len(leftPlan.MergeOrdering.Columns) != 0 || len(rightPlan.MergeOrdering.Columns) != 0 {
		return PhysicalPlan{}, errors.AssertionFailedf("set op inputs should have no orderings")
	}

	// TODO(radu): for INTERSECT and EXCEPT, the mergeOrdering should be set when
	// we can use merge joiners below. The optimizer needs to be modified to take
	// advantage of this optimization and pass down merge orderings. Tracked by
	// #40797.
	var mergeOrdering execinfrapb.Ordering

	// Merge processors, streams, result routers, and stage counter.
	var leftRouters, rightRouters []physicalplan.ProcessorIdx
	p.PhysicalPlan, leftRouters, rightRouters = physicalplan.MergePlans(
		&leftPlan.PhysicalPlan, &rightPlan.PhysicalPlan)

	p.AddNoopForJoinToAgent(findJoinProcessorNodes(leftRouters, rightRouters, p.Processors),
		leftRouters, rightRouters, dsp.nodeDesc.NodeID, &leftPlan.ResultTypes, &rightPlan.ResultTypes)

	if n.unionType == tree.UnionOp {
		// We just need to append the left and right streams together, so append
		// the left and right output routers.
		p.ResultRouters = append(leftRouters, rightRouters...)

		p.ResultTypes = resultTypes
		p.SetMergeOrdering(mergeOrdering)

		if !n.all {
			// TODO(abhimadan): use columns from mergeOrdering to fill in the
			// OrderingColumns field in DistinctSpec once the unused columns
			// are projected out.
			distinctSpec := execinfrapb.ProcessorCoreUnion{
				Distinct: &execinfrapb.DistinctSpec{DistinctColumns: streamCols},
			}
			p.AddSingleGroupStage(
				dsp.nodeDesc.NodeID, distinctSpec, execinfrapb.PostProcessSpec{}, p.ResultTypes)
		} else {
			// With UNION ALL, we can end up with multiple streams on the same node.
			// We don't want to have unnecessary routers and cross-node streams, so
			// merge these streams now.
			//
			// More importantly, we need to guarantee that if everything is planned
			// on a single node (which is always the case when there are mutations),
			// we can fuse everything so there are no concurrent KV operations (see
			// #40487, #41307).
			//
			// Furthermore, in order to disable auto-parallelism that could occur
			// when merging multiple streams on the same node, we force the
			// serialization of the merge operation (otherwise, it would be
			// possible that we have a source of unbounded parallelism, see #51548).
			p.EnsureSingleStreamPerNode(true /* forceSerialization */, false, dsp.gatewayNodeID)

			// UNION ALL is special: it doesn't have any required downstream
			// processor, so its two inputs might have different post-processing
			// which would violate an assumption later down the line. Check for this
			// condition and add a no-op stage if it exists.
			if err := p.CheckLastStagePost(); err != nil {
				p.AddSingleGroupStage(
					dsp.nodeDesc.NodeID,
					execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{}},
					execinfrapb.PostProcessSpec{},
					p.ResultTypes,
				)
			}
		}
	} else {
		// We plan INTERSECT and EXCEPT queries with joiners. Get the appropriate
		// join type.
		joinType := distsqlSetOpJoinType(n.unionType)

		// Nodes where we will run the join processors.
		nodes := findJoinProcessorNodes(leftRouters, rightRouters, p.Processors)

		// Set up the equality columns.
		eqCols := streamCols

		// Project the left-side columns only.
		post := execinfrapb.PostProcessSpec{Projection: true}
		post.OutputColumns = make([]uint32, len(streamCols))
		copy(post.OutputColumns, streamCols)

		// Create the Core spec.
		//
		// TODO(radu): we currently only use merge joins when we have an ordering on
		// all equality columns. We should relax this by either:
		//  - implementing a hybrid hash/merge processor which implements merge
		//    logic on the columns we have an ordering on, and within each merge
		//    group uses a hashmap on the remaining columns
		//  - or: adding a sort processor to complete the order
		var core execinfrapb.ProcessorCoreUnion
		if !planMergeJoins.Get(&dsp.st.SV) || len(mergeOrdering.Columns) < len(streamCols) {
			core.HashJoiner = &execinfrapb.HashJoinerSpec{
				LeftEqColumns:  eqCols,
				RightEqColumns: eqCols,
				Type:           joinType,
			}
		} else {
			core.MergeJoiner = &execinfrapb.MergeJoinerSpec{
				LeftOrdering:  mergeOrdering,
				RightOrdering: mergeOrdering,
				Type:          joinType,
				NullEquality:  true,
			}
		}

		if n.all {
			p.AddJoinStage(
				nodes, core, post, eqCols, eqCols,
				leftPlan.ResultTypes, rightPlan.ResultTypes,
				leftPlan.MergeOrdering, rightPlan.MergeOrdering,
				leftRouters, rightRouters,
			)
		} else {
			p.AddDistinctSetOpStage(
				nodes, core, distinctSpecs[:], post, eqCols,
				leftPlan.ResultTypes, rightPlan.ResultTypes,
				leftPlan.MergeOrdering, rightPlan.MergeOrdering,
				leftRouters, rightRouters,
			)
		}

		// An EXCEPT ALL is like a left outer join, so there is no guaranteed ordering.
		if n.unionType == tree.ExceptOp {
			mergeOrdering = execinfrapb.Ordering{}
		}

		p.ResultTypes = resultTypes
		p.SetMergeOrdering(mergeOrdering)
	}
	return p, nil
}

// createPlanForWindow creates a physical plan for computing window functions.
// We add a new stage of windower processors for each different partitioning
// scheme found in the query's window functions.
func (dsp *DistSQLPlanner) createPlanForWindow(
	planCtx *PlanningCtx, n *windowNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.plan)
	if err != nil {
		return PhysicalPlan{}, err
	}
	numWindowFuncProcessed := 0
	windowPlanState := createWindowPlanState(n, planCtx, &plan)
	// Each iteration of this loop adds a new stage of windowers. The steps taken:
	// 1. find a set of unprocessed window functions that have the same PARTITION BY
	//    clause. All of these will be computed using the single stage of windowers.
	// 2. a) populate output types of the current stage of windowers. All input
	//       columns are being passed through, and windower will append output
	//       columns for each window function processed at the stage.
	//    b) create specs for all window functions in the set.
	// 3. decide whether to put windowers on a single or on multiple nodes.
	//    a) if we're putting windowers on multiple nodes, we'll put them onto
	//       every node that participated in the previous stage. We leverage hash
	//       routers to partition the data based on PARTITION BY clause of window
	//       functions in the set.
	for numWindowFuncProcessed < len(n.funcs) {
		samePartitionFuncs, partitionIdxs := windowPlanState.findUnprocessedWindowFnsWithSamePartition()
		numWindowFuncProcessed += len(samePartitionFuncs)
		windowerSpec := execinfrapb.WindowerSpec{
			PartitionBy: partitionIdxs,
			WindowFns:   make([]execinfrapb.WindowerSpec_WindowFn, len(samePartitionFuncs)),
		}

		newResultTypes := make([]types.T, len(plan.ResultTypes)+len(samePartitionFuncs))
		copy(newResultTypes, plan.ResultTypes)
		for windowFnSpecIdx, windowFn := range samePartitionFuncs {
			windowFnSpec, outputType, err := windowPlanState.createWindowFnSpec(windowFn)
			if err != nil {
				return PhysicalPlan{}, err
			}
			newResultTypes[windowFn.outputColIdx] = *outputType
			windowerSpec.WindowFns[windowFnSpecIdx] = windowFnSpec
		}

		// Check if the previous stage is all on one node.
		prevStageNode := plan.Processors[plan.ResultRouters[0]].Node
		for i := 1; i < len(plan.ResultRouters); i++ {
			if n := plan.Processors[plan.ResultRouters[i]].Node; n != prevStageNode {
				prevStageNode = 0
				break
			}
		}

		// Get all nodes from the previous stage.
		nodes := getNodesOfRouters(plan.ResultRouters, plan.Processors)
		if len(partitionIdxs) == 0 || len(nodes) == 1 {
			// No PARTITION BY or we have a single node. Use a single windower.
			// If the previous stage was all on a single node, put the windower
			// there. Otherwise, bring the results back on this node.
			node := dsp.nodeDesc.NodeID
			if len(nodes) == 1 {
				node = nodes[0]
			}
			plan.AddSingleGroupStage(
				node,
				execinfrapb.ProcessorCoreUnion{Windower: &windowerSpec},
				execinfrapb.PostProcessSpec{},
				newResultTypes,
			)
		} else {
			plan.AddNoopToTsProcessors(dsp.nodeDesc.NodeID, false, false)
			// Set up the output routers from the previous stage.
			// We use hash routers with hashing on the columns
			// from PARTITION BY clause of window functions
			// we're processing in the current stage.
			for _, resultProc := range plan.ResultRouters {
				plan.Processors[resultProc].Spec.Output[0] = execinfrapb.OutputRouterSpec{
					Type:        execinfrapb.OutputRouterSpec_BY_HASH,
					HashColumns: partitionIdxs,
				}
			}
			stageID := plan.NewStageID()

			// We put a windower on each node and we connect it
			// with all hash routers from the previous stage in
			// a such way that each node has its designated
			// SourceRouterSlot - namely, position in which
			// a node appears in nodes.
			prevStageRouters := plan.ResultRouters
			plan.ResultRouters = make([]physicalplan.ProcessorIdx, 0, len(nodes))
			for bucket, nodeID := range nodes {
				proc := physicalplan.Processor{
					Node: nodeID,
					Spec: execinfrapb.ProcessorSpec{
						Input: []execinfrapb.InputSyncSpec{{
							Type:        execinfrapb.InputSyncSpec_UNORDERED,
							ColumnTypes: plan.ResultTypes,
						}},
						Core: execinfrapb.ProcessorCoreUnion{Windower: &windowerSpec},
						Post: execinfrapb.PostProcessSpec{},
						Output: []execinfrapb.OutputRouterSpec{{
							Type: execinfrapb.OutputRouterSpec_PASS_THROUGH,
						}},
						StageID: stageID,
					},
				}
				pIdx := plan.AddProcessor(proc)

				for _, router := range prevStageRouters {
					plan.Streams = append(plan.Streams, physicalplan.Stream{
						SourceProcessor:  router,
						SourceRouterSlot: bucket,
						DestProcessor:    pIdx,
						DestInput:        0,
					})
				}
				plan.ResultRouters = append(plan.ResultRouters, pIdx)
			}

			plan.ResultTypes = newResultTypes
		}
	}

	// We definitely added columns throughout all the stages of windowers, so we
	// need to update PlanToStreamColMap. We need to update the map before adding
	// rendering or projection because it is used there.
	plan.PlanToStreamColMap = identityMap(plan.PlanToStreamColMap, len(plan.ResultTypes))

	// windowers do not guarantee maintaining the order at the moment, so we
	// reset MergeOrdering. There shouldn't be an ordering here, but we reset it
	// defensively (see #35179).
	plan.SetMergeOrdering(execinfrapb.Ordering{})

	// After all window functions are computed, we need to add rendering or
	// projection.
	if err := windowPlanState.addRenderingOrProjection(); err != nil {
		return PhysicalPlan{}, err
	}

	if len(plan.ResultTypes) != len(plan.PlanToStreamColMap) {
		// We added/removed columns while rendering or projecting, so we need to
		// update PlanToStreamColMap.
		plan.PlanToStreamColMap = identityMap(plan.PlanToStreamColMap, len(plan.ResultTypes))
	}

	return plan, nil
}

// createPlanForExport creates a physical plan for EXPORT.
// We add a new stage of CSVWriter processors to the input plan.
func (dsp *DistSQLPlanner) createPlanForExport(
	planCtx *PlanningCtx, n *exportNode,
) (PhysicalPlan, error) {
	plan, err := dsp.createPlanForNode(planCtx, n.source)
	if err != nil {
		return PhysicalPlan{}, err
	}

	core := execinfrapb.ProcessorCoreUnion{CSVWriter: &execinfrapb.CSVWriterSpec{
		Destination: n.fileName,
		NamePattern: exportFilePatternDefault,
		Options:     n.expOpts.csvOpts,
		ChunkRows:   int64(n.expOpts.chunkSize),
		QueryName:   n.queryName,
		OnlyMeta:    n.expOpts.onlyMeta,
		OnlyData:    n.expOpts.onlyData,
		IsTS:        n.isTS,
	}}
	var resTypes []types.T
	if n.isTS {
		resTypes = make([]types.T, len(sqlbase.ExportTsColumns))
		for i := range sqlbase.ExportTsColumns {
			resTypes[i] = *sqlbase.ExportTsColumns[i].Typ
		}
	} else {
		resTypes = make([]types.T, len(sqlbase.ExportColumns))
		for i := range sqlbase.ExportColumns {
			resTypes[i] = *sqlbase.ExportColumns[i].Typ
		}
	}

	//switch t := event.GetValue().(type) {
	//case *roachpb.RangeFeedValue:
	maybeScan := n.source
	switch sc := maybeScan.(type) {
	case *scanNode:
		if n.expOpts.colName {
			colNames := make([]string, len(sc.resultColumns))
			for id, col := range sc.resultColumns {
				colNames[id] = col.Name
			}
			core.CSVWriter.ColNames = colNames
		}
	case *synchronizerNode:
		if n.expOpts.colName {
			colNames := make([]string, len(sc.columns))
			for id, col := range sc.columns {
				colNames[id] = col.Name
			}
			core.CSVWriter.ColNames = colNames
		}
	case *renderNode:

		if n.expOpts.colName {
			colNames := make([]string, len(sc.columns))
			for id, col := range sc.columns {
				colNames[id] = col.Name
			}
			core.CSVWriter.ColNames = colNames
		}
	default:
		log.Infof(context.Background(), "default maybeScan.(type), %v", sc)
	}

	plan.AddNoGroupingStage(
		core, execinfrapb.PostProcessSpec{}, resTypes, execinfrapb.Ordering{},
	)
	// The CSVWriter produces the same columns as the EXPORT statement.
	if n.isTS {
		plan.PlanToStreamColMap = identityMap(plan.PlanToStreamColMap, len(sqlbase.ExportTsColumns))
	} else {
		plan.PlanToStreamColMap = identityMap(plan.PlanToStreamColMap, len(sqlbase.ExportColumns))
	}
	return plan, nil
}

// NewPlanningCtx returns a new PlanningCtx.
func (dsp *DistSQLPlanner) NewPlanningCtx(
	ctx context.Context, evalCtx *extendedEvalContext, txn *kv.Txn,
) *PlanningCtx {
	planCtx := dsp.newLocalPlanningCtx(ctx, evalCtx)
	planCtx.spanIter = dsp.spanResolver.NewSpanResolverIterator(txn)
	planCtx.NodeAddresses = make(map[roachpb.NodeID]string)
	planCtx.NodeAddresses[dsp.nodeDesc.NodeID] = dsp.nodeDesc.Address.String()
	return planCtx
}

// newLocalPlanningCtx is a lightweight version of NewPlanningCtx that can be
// used when the caller knows plans will only be run on one node.
func (dsp *DistSQLPlanner) newLocalPlanningCtx(
	ctx context.Context, evalCtx *extendedEvalContext,
) *PlanningCtx {
	return &PlanningCtx{
		ctx:             ctx,
		ExtendedEvalCtx: evalCtx,
	}
}

// FinalizePlan adds a final "result" stage if necessary and populates the
// endpoints of the plan.
func (dsp *DistSQLPlanner) FinalizePlan(planCtx *PlanningCtx, plan *PhysicalPlan) {
	// Find all MetadataTestSenders in the plan, so that the MetadataTestReceiver
	// knows how many sender IDs it should expect.
	var metadataSenders []string
	for _, proc := range plan.Processors {
		if proc.Spec.Core.MetadataTestSender != nil {
			metadataSenders = append(metadataSenders, proc.Spec.Core.MetadataTestSender.ID)
		}
	}
	thisNodeID := dsp.nodeDesc.NodeID
	// If we don't already have a single result router on this node, add a final
	// stage.
	for i, idx := range plan.ResultRouters {
		if plan.Processors[idx].ExecInTSEngine && plan.Processors[idx].Node != thisNodeID {
			plan.AddNoopImplementation(
				execinfrapb.PostProcessSpec{}, idx, i, nil, &plan.ResultTypes,
			)
		}
	}

	plan.SetTSEngineReturnEncode()

	if len(plan.ResultRouters) != 1 ||
		plan.Processors[plan.ResultRouters[0]].Node != thisNodeID || plan.ChildIsExecInTSEngine() {
		var thisPost execinfrapb.PostProcessSpec
		plan.AddSingleGroupStage(
			thisNodeID,
			execinfrapb.ProcessorCoreUnion{Noop: &execinfrapb.NoopCoreSpec{
				InputNum:   uint32(plan.GateNoopInput),
				TsOperator: plan.TsOperator,
			}},
			thisPost,
			plan.ResultTypes,
		)
		if len(plan.ResultRouters) != 1 {
			panic(fmt.Sprintf("%d results after single group stage", len(plan.ResultRouters)))
		}
		plan.Processors[plan.ResultRouters[0]].LogicalSequenceID = []uint64{0}
	}

	if len(metadataSenders) > 0 {
		plan.AddSingleGroupStage(
			thisNodeID,
			execinfrapb.ProcessorCoreUnion{
				MetadataTestReceiver: &execinfrapb.MetadataTestReceiverSpec{
					SenderIDs: metadataSenders,
				},
			},
			execinfrapb.PostProcessSpec{},
			plan.ResultTypes,
		)
		plan.Processors[plan.ResultRouters[0]].LogicalSequenceID = []uint64{0}
	}
	// Set up the endpoints for p.streams.
	plan.PopulateEndpoints(planCtx.NodeAddresses)
	// Single router and can not execute on ts engine
	// 1.Check processor is no-op and render filter all nil
	// 2.Traverse whether all inputs under noop are execute on ts engine
	var subdue bool
	if planCtx.planner != nil {
		if planCtx.planner.curPlan.subqueryPlans != nil {
			if len(planCtx.planner.curPlan.subqueryPlans) != 0 {
				subdue = true
			}
		}
	}
	var closes int
	if plan.Processors[plan.ResultRouters[0]].Spec.Core.Noop != nil &&
		plan.Processors[plan.ResultRouters[0]].Spec.Post.RenderExprs == nil &&
		plan.Processors[plan.ResultRouters[0]].Spec.Post.Filter.Empty() &&
		!subdue {
		plan.AllProcessorsExecInTSEngine = true
		for _, input := range plan.Processors[plan.ResultRouters[0]].Spec.Input {
			for _, stream := range input.Streams {
				if stream.Type != execinfrapb.StreamEndpointType_QUEUE {
					plan.AllProcessorsExecInTSEngine = false
					break
				}
				closes++
			}
			if !plan.AllProcessorsExecInTSEngine {
				break
			}
		}
	} else {
		plan.AllProcessorsExecInTSEngine = false
	}

	if planCtx.ExtendedEvalCtx.IsInternalSQL {
		plan.AllProcessorsExecInTSEngine = false
	}

	if plan.AllProcessorsExecInTSEngine {
		plan.Closes = closes
	}

	// Set up the endpoint for the final result.
	finalOut := &plan.Processors[plan.ResultRouters[0]].Spec.Output[0]
	finalOut.Streams = append(finalOut.Streams, execinfrapb.StreamEndpointSpec{
		Type: execinfrapb.StreamEndpointType_SYNC_RESPONSE,
	})

	// Assign processor IDs.
	for i := range plan.Processors {
		if plan.Processors[i].ExecInTSEngine {
			plan.Processors[i].TSSpec.Post.Commandlimit = uint32(planCtx.EvalContext().SessionData.CommandLimit)
			plan.Processors[i].TSSpec.ProcessorID = int32(i)
		} else {
			plan.Processors[i].Spec.ProcessorID = int32(i)
		}
	}
	planCtx.EvalContext().SessionData.CommandLimit = 0
}

// FinalizeTopicPlan adds a final "result" stage if necessary and populates the
// endpoints of the plan.
func (dsp *DistSQLPlanner) FinalizeTopicPlan(planCtx *PlanningCtx, plan *PhysicalPlan) {
	// Find all MetadataTestSenders in the plan, so that the MetadataTestReceiver
	// knows how many sender IDs it should expect.
	var metadataSenders []string
	for _, proc := range plan.Processors {
		if proc.Spec.Core.MetadataTestSender != nil {
			metadataSenders = append(metadataSenders, proc.Spec.Core.MetadataTestSender.ID)
		}
	}
	// Set up the endpoints for p.streams.
	plan.PopulateEndpoints(planCtx.NodeAddresses)

	// Assign processor IDs.
	for i := range plan.Processors {
		if plan.Processors[i].ExecInTSEngine {
			plan.Processors[i].TSSpec.ProcessorID = int32(i)
		} else {
			plan.Processors[i].Spec.ProcessorID = int32(i)
		}
	}
}

func makeTableReaderSpans(spans roachpb.Spans) []execinfrapb.TableReaderSpan {
	trSpans := make([]execinfrapb.TableReaderSpan, len(spans))
	for i, span := range spans {
		trSpans[i].Span = span
	}

	return trSpans
}

// pushDownAggToTSReader push (Aggregator, Sorter) and it's post into tsTableReader.
// p is physical plan.
// canOpt is true when need to optimize agg or limit order by.
// isAgg is true when there is agg.
func pushDownProcessorToTSReader(p *PhysicalPlan, canOpt bool, isAgg bool) {
	tsReaderCount := 0
	if canOpt {
		for k, v := range p.Processors {
			if v.TSSpec.Core.TableReader != nil {
				if isAgg {
					p.Processors[k].TSSpec.Core.TableReader.Aggregator = p.Processors[p.ResultRouters[0]].TSSpec.Core.Aggregator
					p.Processors[k].TSSpec.Core.TableReader.AggregatorPost = &p.Processors[p.ResultRouters[0]].TSSpec.Post
				} else {
					var order *execinfrapb.SorterSpec
					var limit, offset uint32
					if p.ChildIsExecInTSEngine() {
						order = p.Processors[p.ResultRouters[0]].TSSpec.Core.Sorter
						limit = p.Processors[p.ResultRouters[0]].TSSpec.Post.Limit
						offset = p.Processors[p.ResultRouters[0]].TSSpec.Post.Offset
					} else {
						for i := p.ResultRouters[0]; i >= 0; i-- {
							if p.Processors[i].Spec.Core.Sorter != nil {
								order = p.Processors[i].Spec.Core.Sorter
								break
							}
						}
						limit = uint32(p.Processors[p.ResultRouters[0]].Spec.Post.Limit)
						offset = uint32(p.Processors[p.ResultRouters[0]].Spec.Post.Offset)
					}
					p.Processors[k].TSSpec.Post.Limit = limit
					p.Processors[k].TSSpec.Post.Offset = offset
					if order != nil {
						p.Processors[k].TSSpec.Core.TableReader.Sorter = &order.OutputOrdering
					}
				}
				tsReaderCount++
			} else if tsReaderCount != 0 {
				break
			}
		}
	}
}

// needsTSTwiceAggregation determines whether a twice aggregation is necessary
// when querying time-series data. This function assesses whether all primary tag columns
// are used as grouping columns in the query, which dictates if a second aggregation pass is required.
//
// Parameters:
// - n: *groupNode representing the grouping and aggregation operation in the query plan.
// - planCtx: *PlanningCtx providing context such as the transaction and evaluation context for the query.
//
// Returns:
// - bool: Indicates whether a second aggregation is needed.
// - error: Error encountered during the process, if any.
//
// Examples:
//
//   - SELECT count(*) FROM t3 GROUP BY c1, c2;
//     Here, if c1 and c2 are all primary tag columns (PTAGs), this function should return false,
//     indicating no second aggregation is required as all PTAGs are used in the grouping.
//
//   - For a query with a subquery:
//     SELECT max(k_timestamp) FROM (SELECT k_timestamp, c1, c2 FROM t3 WHERE e1 > 1) AS new_table GROUP BY c1, c2;
//     Similar to the first example, if c1 and c2 are all PTAGs, this function would also return false
//     for the same reason: the grouping covers all primary tag columns.
func needsTSTwiceAggregation(n *groupNode, planCtx *PlanningCtx) (bool, error) {
	var primaryTagColIDs, groupColIDs util.FastIntSet
	needsTSTwiceAgg := true

	// No grouping by condition requires two aggregates
	if len(n.groupCols) == 0 {
		return needsTSTwiceAgg, nil
	}
	// Get grouping columns physical id
	inputCols := planColumns(n.plan)
	for _, colIdx := range n.groupCols {
		col := inputCols[colIdx]
		groupColIDs.Add(int(col.PGAttributeNum))
	}

	// Handle different types of input plans
	// (joinNode,etc cannot be pushed, so don't check)
	switch inputPlan := n.plan.(type) {
	case *tsScanNode:
		for i := 0; i < inputPlan.Table.ColumnCount(); i++ {
			col := inputPlan.Table.Column(i)
			if col.IsPrimaryTagCol() {
				primaryTagColIDs.Add(int(col.ColID()))
			}
		}
		return !primaryTagColIDs.Equals(groupColIDs), nil
	case *renderNode, *filterNode:
		var tabID sqlbase.ID
		for _, colIdx := range n.groupCols {
			col := inputCols[colIdx]
			// Grouping columns have relational expression
			if col.TableID == sqlbase.InvalidID {
				return needsTSTwiceAgg, nil
			}
			tabID = col.TableID
		}
		tableDesc, err := sqlbase.GetTableDescFromID(planCtx.ctx, planCtx.ExtendedEvalCtx.Txn, tabID)
		if err != nil {
			return needsTSTwiceAgg, err
		}
		for _, col := range tableDesc.Columns {
			if col.IsPrimaryTagCol() {
				primaryTagColIDs.Add(int(col.ColID()))
			}
		}
		return !primaryTagColIDs.Equals(groupColIDs), nil
	default:
		// Other cases don't come to mind temporarily, so return needsTSTwiceAgg
		return needsTSTwiceAgg, nil
	}

	return needsTSTwiceAgg, nil
}