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

package kvserver

import (
	"context"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/rpc"
	"gitee.com/kwbasedb/kwbase/pkg/rpc/nodedialer"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/util"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/netutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"gitee.com/kwbasedb/kwbase/pkg/util/tracing"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
	"github.com/pkg/errors"
)

func TestRaftTransportStartNewQueue(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)

	st := cluster.MakeTestingClusterSettings()
	rpcC := rpc.NewContext(log.AmbientContext{}, &base.Config{Insecure: true},
		hlc.NewClock(hlc.UnixNano, 500*time.Millisecond), stopper, st)
	rpcC.ClusterID.Set(context.TODO(), uuid.MakeV4())

	// mrs := &dummyMultiRaftServer{}

	grpcServer := rpc.NewServer(rpcC)
	// RegisterMultiRaftServer(grpcServer, mrs)

	var addr net.Addr

	resolver := func(roachpb.NodeID) (net.Addr, error) {
		if addr == nil {
			return nil, errors.New("no addr yet") // should not happen in this test
		}
		return addr, nil
	}

	tp := NewRaftTransport(
		log.AmbientContext{Tracer: tracing.NewTracer()},
		cluster.MakeTestingClusterSettings(),
		nodedialer.New(rpcC, resolver),
		grpcServer,
		stopper,
	)

	ln, err := netutil.ListenAndServeGRPC(stopper, grpcServer, &util.UnresolvedAddr{NetworkField: "tcp", AddressField: "localhost:0"})
	if err != nil {
		t.Fatal(err)
	}

	addr = ln.Addr()

	defer func() {
		if ln != nil {
			_ = ln.Close()
		}
	}()

	_, existingQueue := tp.getQueue(1, rpc.SystemClass)
	if existingQueue {
		t.Fatal("queue already exists")
	}
	timeout := time.Duration(rand.Int63n(int64(5 * time.Millisecond)))
	log.Infof(ctx, "running test with a ctx cancellation of %s", timeout)
	ctxBoom, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		<-time.After(timeout)
		_ = ln.Close()
		ln = nil
		wg.Done()
	}()
	var stats raftTransportStats
	tp.startProcessNewQueue(ctxBoom, 1, rpc.SystemClass, &stats)

	wg.Wait()
}
