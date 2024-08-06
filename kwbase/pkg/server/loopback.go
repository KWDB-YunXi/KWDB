// Copyright 2020 The Cockroach Authors.
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

package server

import (
	"context"
	"net"
	"sync"

	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"github.com/cockroachdb/cmux"
	"github.com/cockroachdb/errors"
)

// loopbackListener implements a local listener
// that delivers net.Conns via its Connect() method
// based on the other side calls to its Accept() method.
type loopbackListener struct {
	stopper *stop.Stopper

	closeOnce sync.Once
	active    chan struct{}

	// requests are tokens from the Connect() method to the
	// Accept() method.
	requests chan struct{}
	// conns are responses from the Accept() method
	// to the Connect() method.
	conns chan net.Conn
}

var _ net.Listener = (*loopbackListener)(nil)

// note that we need to use cmux.ErrListenerClosed as base (leaf)
// error so that it is recognized as special case in
// netutil.IsClosedConnection.
var errLocalListenerClosed = errors.Wrap(cmux.ErrListenerClosed, "loopback listener")

// Accept waits for and returns the next connection to the listener.
func (l *loopbackListener) Accept() (conn net.Conn, err error) {
	select {
	case <-l.stopper.ShouldQuiesce():
		return nil, errLocalListenerClosed
	case <-l.active:
		return nil, errLocalListenerClosed
	case <-l.requests:
	}
	c1, c2 := net.Pipe()
	select {
	case l.conns <- c1:
		return c2, nil
	case <-l.stopper.ShouldQuiesce():
	case <-l.active:
	}
	err = errLocalListenerClosed
	err = errors.CombineErrors(err, c1.Close())
	err = errors.CombineErrors(err, c2.Close())
	return nil, err
}

// Close closes the listener.
// Any blocked Accept operations will be unblocked and return errors.
func (l *loopbackListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.active)
	})
	return nil
}

// Addr returns the listener's network address.
func (l *loopbackListener) Addr() net.Addr {
	return loopbackAddr{}
}

// Connect signals the Accept method that a conn is needed.
func (l *loopbackListener) Connect(ctx context.Context) (net.Conn, error) {
	// Send request to acceptor.
	select {
	case <-l.stopper.ShouldQuiesce():
		return nil, errLocalListenerClosed
	case <-l.active:
		return nil, errLocalListenerClosed
	case l.requests <- struct{}{}:
	}
	// Get conn from acceptor.
	select {
	case <-l.stopper.ShouldQuiesce():
		return nil, errLocalListenerClosed
	case <-l.active:
		return nil, errLocalListenerClosed
	case conn := <-l.conns:
		return conn, nil
	}
}

func newLoopbackListener(ctx context.Context, stopper *stop.Stopper) *loopbackListener {
	return &loopbackListener{
		stopper:  stopper,
		active:   make(chan struct{}),
		requests: make(chan struct{}),
		conns:    make(chan net.Conn),
	}
}

type loopbackAddr struct{}

var _ net.Addr = loopbackAddr{}

func (loopbackAddr) Network() string { return "pipe" }
func (loopbackAddr) String() string  { return "loopback" }
