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

package distsql

import (
	"context"
	"net"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/rpc"
	"gitee.com/kwbasedb/kwbase/pkg/rpc/nodedialer"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/flowinfra"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/distsqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/netutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

// staticAddressResolver maps execinfra.StaticNodeID to the given address.
func staticAddressResolver(addr net.Addr) nodedialer.AddressResolver {
	return func(nodeID roachpb.NodeID) (net.Addr, error) {
		if nodeID == execinfra.StaticNodeID {
			return addr, nil
		}
		return nil, errors.Errorf("node %d not found", nodeID)
	}
}

// TestOutboxInboundStreamIntegration verifies that if an inbound stream gets
// a draining status from its consumer, that status is propagated back to the
// outbox and there are no goroutine leaks.
func TestOutboxInboundStreamIntegration(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)
	ni := base.NodeIDContainer{}
	ni.Set(ctx, 1)
	st := cluster.MakeTestingClusterSettings()
	mt := execinfra.MakeDistSQLMetrics(time.Hour /* histogramWindow */)
	srv := NewServer(
		ctx,
		execinfra.ServerConfig{
			Settings: st,
			Stopper:  stopper,
			Metrics:  &mt,
			NodeID:   &ni,
		},
	)

	clock := hlc.NewClock(hlc.UnixNano, time.Nanosecond)
	rpcContext := rpc.NewInsecureTestingContext(clock, stopper)

	// We're going to serve multiple node IDs with that one context. Disable node ID checks.
	rpcContext.TestingAllowNamedRPCToAnonymousServer = true

	rpcSrv := rpc.NewServer(rpcContext)
	defer rpcSrv.Stop()

	execinfrapb.RegisterDistSQLServer(rpcSrv, srv)
	ln, err := netutil.ListenAndServeGRPC(stopper, rpcSrv, util.IsolatedTestAddr)
	if err != nil {
		t.Fatal(err)
	}

	// The outbox uses this stopper to run a goroutine.
	outboxStopper := stop.NewStopper()
	flowCtx := execinfra.FlowCtx{
		Cfg: &execinfra.ServerConfig{
			Settings:   st,
			NodeDialer: nodedialer.New(rpcContext, staticAddressResolver(ln.Addr())),
			Stopper:    outboxStopper,
		},
	}

	streamID := execinfrapb.StreamID(1)
	outbox := flowinfra.NewOutbox(&flowCtx, execinfra.StaticNodeID, execinfrapb.FlowID{}, streamID)
	outbox.Init(sqlbase.OneIntCol)

	// WaitGroup for the outbox and inbound stream. If the WaitGroup is done, no
	// goroutines were leaked. Grab the flow's waitGroup to avoid a copy warning.
	f := &flowinfra.FlowBase{}
	wg := f.GetWaitGroup()

	// Use RegisterFlow to register our consumer, which we will control.
	consumer := distsqlutils.NewRowBuffer(sqlbase.OneIntCol, nil /* rows */, distsqlutils.RowBufferArgs{})
	connectionInfo := map[execinfrapb.StreamID]*flowinfra.InboundStreamInfo{
		streamID: flowinfra.NewInboundStreamInfo(
			flowinfra.RowInboundStreamHandler{RowReceiver: consumer},
			wg,
		),
	}
	// Add to the WaitGroup counter for the inbound stream.
	wg.Add(1)
	require.NoError(
		t,
		srv.flowRegistry.RegisterFlow(ctx, execinfrapb.FlowID{}, f, connectionInfo, time.Hour /* timeout */),
	)

	outbox.Start(ctx, wg, func() {})

	// Put the consumer in draining mode, this should propagate all the way back
	// from the inbound stream to the outbox when it attempts to Push a row
	// below.
	consumer.ConsumerDone()

	row := sqlbase.EncDatumRow{sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(0)))}

	// Now push a row to the outbox's RowChannel and expect the consumer status
	// returned to be DrainRequested. This is wrapped in a SucceedsSoon because
	// the write to the row channel is asynchronous wrt the outbox sending the
	// row and getting back the updated consumer status.
	testutils.SucceedsSoon(t, func() error {
		if cs := outbox.Push(row, nil /* meta */); cs != execinfra.DrainRequested {
			return errors.Errorf("unexpected consumer status %s", cs)
		}
		return nil
	})

	// As a producer, we are now required to call ProducerDone after draining. We
	// do so now to simulate the fact that we have no more rows or metadata to
	// send.
	outbox.ProducerDone()

	// Both the outbox and the inbound stream should exit.
	wg.Wait()

	// Wait for outstanding tasks to complete. Specifically, we are waiting for
	// the outbox's drain signal listener to return.
	outboxStopper.Quiesce(ctx)
}
