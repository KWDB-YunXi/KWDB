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

// Package closedts houses the interfaces and basic definitions used by the
// various components of the closed timestamp subsystems.
//
// The following diagram illustrates how these components fit together. In
// running operation, the components are grouped in a container.Container
// (intended as a pass-around per-instance Singleton).
// Replicas proposing commands talk to the Tracker; replicas trying to serve
// follower reads talk to the Provider, which receives closed timestamp updates
// for the local node and its peers.
//
//                             Node 1 | Node 2
//                                    |
// +---------+  Close  +-----------+  |  +-----------+
// | Tracker |<--------|           |  |  |           |
// +-----+---+         | +-------+ |  |  | +-------+ |  CanServe
//       ^             | |Storage| |  |  | |Storage| |<---------+
//       |             | --------+ |  |  | +-------+ |          |
//       |Track        |           |  |  |           |     +----+----+
//       |             | Provider  |  |  | Provider  |     | Follower|
//       |             +-----------+  |  +-----------+     | Replica |
//       |                 ^                  ^            +----+----+
//       |                 |Subscribe         |Notify           |
//       |                 |                  |                 |
// +---------+             |      Request     |                 |
// |Proposing| Refresh +---+----+ <------ +---+-----+  Request  |
// | Replica |<--------| Server |         | Clients |<----------+
// +---------+         +--------+ ------> +---------+  EnsureClient
//                                  CT
package closedts

import (
	"context"

	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/closedts/ctpb"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"google.golang.org/grpc"
)

// ReleaseFunc is a closure returned from Track which is used to record the
// LeaseAppliedIndex (LAI) given to a tracked proposal. The supplied epoch must
// match that of the lease under which the proposal was proposed.
type ReleaseFunc func(context.Context, ctpb.Epoch, roachpb.RangeID, ctpb.LAI)

// TrackerI is part of the machinery enabling follower reads, that is, consistent
// reads served by replicas not holding the lease (for the requested timestamp).
// This data structure keeps tabs on ongoing command evaluations (which it
// forces to successively higher timestamps) and provides closed timestamp
// updates along with a map delta of minimum Lease Applied Indexes a replica
// wishing to serve a follower read must reach in order to do so correctly.
//
// See https://gitee.com/kwbasedb/kwbase/pull/26362 for more information.
//
// The methods exposed on Tracker are safe for concurrent use.
type TrackerI interface {
	Close(next hlc.Timestamp, expCurEpoch ctpb.Epoch) (hlc.Timestamp, map[roachpb.RangeID]ctpb.LAI, bool)
	Track(ctx context.Context) (hlc.Timestamp, ReleaseFunc)
}

// A Storage holds the closed timestamps and associated MLAIs for each node. It
// additionally provides historical information about past state that it
// "compacts" regularly, and which can be introspected via the VisitAscending
// method.
//
// The data in a Storage is ephemeral, i.e. is lost during process restarts.
// Introducing a persistent storage will require some design work to make
// sure a) that the records in the storage are certifiably up to date (they
// won't be naturally, unless we add a synchronous write to each proposal)
// and b) that the proposal at each MLAI has actually been proposed. It's
// unlikely that we'll ever find it useful to introduce persistence here
// (though we want to persist historical information for recovery after
// permanent loss of quorum, but there we only need some consistent on-
// disk state; we don't need to bootstrap it into a new consistent state
// that can be updated incrementally).
type Storage interface {
	// VisitAscending visits the historical states contained within the Storage
	// in ascending closed timestamp order. Each state (Entry) is full, i.e.
	// non-incremental. The iteration stops when all states have been visited
	// or the visitor returns true.
	VisitAscending(roachpb.NodeID, func(ctpb.Entry) (done bool))
	// VisitDescending visits the historical states contained within the Storage
	// in descending closed timestamp order. Each state (Entry) is full, i.e.
	// non-incremental. The iteration stops when all states have been visited
	// or the visitor returns true.
	VisitDescending(roachpb.NodeID, func(ctpb.Entry) (done bool))
	// Add merges the given Entry into the state for the given NodeID. The first
	// Entry passed in for any given Entry.Epoch must have Entry.Full set.
	Add(roachpb.NodeID, ctpb.Entry)
	// Clear removes all closed timestamp information from the Storage. It can
	// be used to simulate the loss of information caused by a process restart.
	Clear()
}

// A Notifyee is a sink for closed timestamp updates.
type Notifyee interface {
	// Notify returns a channel into which updates are written.
	//
	// In practice, the Notifyee will be a Provider.
	Notify(roachpb.NodeID) chan<- ctpb.Entry
}

// A Producer is a source of closed timestamp updates about the local node.
type Producer interface {
	// The Subscribe method blocks and, until the context cancels, writes a
	// stream of updates to the provided channel the aggregate of which is
	// guaranteed to represent a valid (i.e. gapless) state.
	Subscribe(context.Context, chan<- ctpb.Entry)
}

// Provider is the central coordinator in the closed timestamp subsystem and the
// gatekeeper for the closed timestamp state for both local and remote nodes,
// which it handles in a symmetric fashion. It has the following tasks:
//
// 1. it accepts subscriptions for closed timestamp updates sourced from the
//    local node. Upon accepting a subscription, the subscriber first receives
//    the aggregate closed timestamp snapshot of the local node and then periodic
//    updates.
// 2. it periodically closes out timestamps on the local node and passes the
//    resulting entries to all of its subscribers.
// 3. it accepts notifications from other nodes, passing these updates through
//    to its local storage, so that
// 4. the CanServe method determines via the the underlying storage whether a
//    given read can be satisfied via follower reads.
// 5. the MaxClosed method determines via the underlying storage what the maximum
//    closed timestamp is for the specified LAI.
//    TODO(tschottdorf): This is already adding some cruft to this nice interface.
//    CanServe and MaxClosed are almost identical.
//
// Note that a Provider has no duty to immediately persist the local closed
// timestamps to the underlying storage.
type Provider interface {
	Producer
	Notifyee
	Start()
	MaxClosed(roachpb.NodeID, roachpb.RangeID, ctpb.Epoch, ctpb.LAI) hlc.Timestamp
}

// A ClientRegistry is the client component of the follower reads subsystem. It
// contacts other nodes and requests a continuous stream of closed timestamp
// updates which it relays to the Provider.
type ClientRegistry interface {
	// Request asynchronously notifies the given node that an update should be
	// emitted for the given range.
	Request(roachpb.NodeID, roachpb.RangeID)
	// EnsureClient instructs the registry to (asynchronously) request a stream
	// of closed timestamp updates from the given node.
	EnsureClient(roachpb.NodeID)
}

// CloseFn is periodically called by Producers to close out new timestamps.
// Outside of tests, it corresponds to (*Tracker).Close; see there for a
// detailed description of the semantics. The final returned boolean indicates
// whether tracked epoch matched the expCurEpoch and that returned information
// may be used.
type CloseFn func(next hlc.Timestamp, expCurEpoch ctpb.Epoch) (hlc.Timestamp, map[roachpb.RangeID]ctpb.LAI, bool)

// AsCloseFn uses the TrackerI as a CloseFn.
func AsCloseFn(t TrackerI) CloseFn {
	return func(next hlc.Timestamp, expCurEpoch ctpb.Epoch) (hlc.Timestamp, map[roachpb.RangeID]ctpb.LAI, bool) {
		return t.Close(next, expCurEpoch)
	}
}

// LiveClockFn supplies a current HLC timestamp from the local node with the
// extra constraints that the local node is live for the returned timestamp at
// the given epoch. The NodeID is passed in to make this method easier to define
// before the NodeID is known.
type LiveClockFn func(roachpb.NodeID) (liveNow hlc.Timestamp, liveEpoch ctpb.Epoch, _ error)

// RefreshFn is called by the Producer when it is asked to manually create (and
// emit) an update for a number of its replicas. The closed timestamp subsystem
// intentionally knows as little about the outside world as possible, and this
// function, injected from the outside, provides the minimal glue. Its job is
// to register a proposal for the current lease applied indexes of the replicas
// with the Tracker, so that updates for them are emitted soon thereafter.
type RefreshFn func(...roachpb.RangeID)

// A Dialer opens closed timestamp connections to receive updates from remote
// nodes.
type Dialer interface {
	Dial(context.Context, roachpb.NodeID) (BackwardsCompatibleClosedTimestampClient, error)
	Ready(roachpb.NodeID) bool // if false, Dial is likely to fail
}

// BackwardsCompatibleClosedTimestampClient is the interface implemented by
// closed-timestamp client connections. It exposes separate methods for talking
// to 20.1 and to 19.2 servers.
type BackwardsCompatibleClosedTimestampClient interface {
	// Get starts an update stream.
	Get(ctx context.Context, opts ...grpc.CallOption) (ctpb.Client, error)
	// Get192 is like Get, but uses the RPC service exposed by 19.2 clients.
	Get192(ctx context.Context, opts ...grpc.CallOption) (ctpb.Client, error)
}
