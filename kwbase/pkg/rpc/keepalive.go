// Copyright 2017 The Cockroach Authors.
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

package rpc

import (
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"google.golang.org/grpc/keepalive"
)

// 10 seconds is the minimum keepalive interval permitted by gRPC.
// Setting it to a value lower than this will lead to gRPC adjusting to this
// value and annoyingly logging "Adjusting keepalive ping interval to minimum
// period of 10s". See grpc/grpc-go#2642.
const minimumClientKeepaliveInterval = 10 * time.Second

// To prevent unidirectional network partitions from keeping an unhealthy
// connection alive, we use both client-side and server-side keepalive pings.
var clientKeepalive = keepalive.ClientParameters{
	// Send periodic pings on the connection.
	Time: minimumClientKeepaliveInterval,
	// If the pings don't get a response within the timeout, we might be
	// experiencing a network partition. gRPC will close the transport-level
	// connection and all the pending RPCs (which may not have timeouts) will
	// fail eagerly. gRPC will then reconnect the transport transparently.
	Timeout: minimumClientKeepaliveInterval,
	// Do the pings even when there are no ongoing RPCs.
	PermitWithoutStream: true,
}
var serverKeepalive = keepalive.ServerParameters{
	// Send periodic pings on the connection.
	Time: base.NetworkTimeout,
	// If the pings don't get a response within the timeout, we might be
	// experiencing a network partition. gRPC will close the transport-level
	// connection and all the pending RPCs (which may not have timeouts) will
	// fail eagerly.
	Timeout: base.NetworkTimeout,
}

// By default, gRPC disconnects clients that send "too many" pings,
// but we don't really care about that, so configure the server to be
// as permissive as possible.
var serverEnforcement = keepalive.EnforcementPolicy{
	MinTime:             time.Nanosecond,
	PermitWithoutStream: true,
}

var serverTestingKeepalive = keepalive.ServerParameters{
	Time:    200 * time.Millisecond,
	Timeout: 300 * time.Millisecond,
}
