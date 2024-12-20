// Copyright 2014 The Cockroach Authors.
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

package netutil

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"gitee.com/kwbasedb/kwbase/pkg/util/syncutil"
	"github.com/cockroachdb/cmux"
	"github.com/cockroachdb/errors"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
)

// ListenAndServeGRPC creates a listener and serves the specified grpc Server
// on it, closing the listener when signaled by the stopper.
func ListenAndServeGRPC(
	stopper *stop.Stopper, server *grpc.Server, addr net.Addr,
) (net.Listener, error) {
	ln, err := net.Listen(addr.Network(), addr.String())
	if err != nil {
		return ln, err
	}

	ctx := context.TODO()

	stopper.RunWorker(ctx, func(context.Context) {
		<-stopper.ShouldQuiesce()
		FatalIfUnexpected(ln.Close())
		<-stopper.ShouldStop()
		server.Stop()
	})

	stopper.RunWorker(ctx, func(context.Context) {
		FatalIfUnexpected(server.Serve(ln))
	})
	return ln, nil
}

var httpLogger = log.NewStdLogger(log.Severity_ERROR, "httpLogger")

// Server is a thin wrapper around http.Server. See MakeServer for more detail.
type Server struct {
	*http.Server
}

// MakeServer constructs a Server that tracks active connections,
// closing them when signaled by stopper.
//
// It can serve two different purposes simultaneously:
//
// - to serve as actual HTTP server, using the .Serve(net.Listener) method.
// - to serve as plain TCP server, using the .ServeWith(...) method.
//
// The latter is used e.g. to accept SQL client connections.
//
// When the HTTP facility is not used, the Go HTTP server object is
// still used internally to maintain/register the connections via the
// ConnState() method, for convenience.
func MakeServer(stopper *stop.Stopper, tlsConfig *tls.Config, handler http.Handler) Server {
	var mu syncutil.Mutex
	activeConns := make(map[net.Conn]struct{})
	server := Server{
		Server: &http.Server{
			Handler:   handler,
			TLSConfig: tlsConfig,
			ConnState: func(conn net.Conn, state http.ConnState) {
				mu.Lock()
				switch state {
				case http.StateNew:
					activeConns[conn] = struct{}{}
				case http.StateClosed:
					delete(activeConns, conn)
				}
				mu.Unlock()
			},
			ErrorLog: httpLogger,
		},
	}

	ctx := context.TODO()

	// net/http.(*Server).Serve/http2.ConfigureServer are not thread safe with
	// respect to net/http.(*Server).TLSConfig, so we call it synchronously here.
	if err := http2.ConfigureServer(server.Server, nil); err != nil {
		log.Fatal(ctx, err)
	}

	stopper.RunWorker(ctx, func(context.Context) {
		<-stopper.ShouldStop()

		mu.Lock()
		for conn := range activeConns {
			conn.Close()
		}
		mu.Unlock()
	})

	return server
}

// ServeWith accepts connections on ln and serves them using serveConn.
func (s *Server) ServeWith(
	ctx context.Context, stopper *stop.Stopper, l net.Listener, serveConn func(net.Conn),
) error {
	// Inspired by net/http.(*Server).Serve
	var tempDelay time.Duration // how long to sleep on accept failure
	for {
		rw, e := l.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				httpLogger.Printf("http: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		go func() {
			defer stopper.Recover(ctx)
			s.Server.ConnState(rw, http.StateNew) // before Serve can return
			serveConn(rw)
			s.Server.ConnState(rw, http.StateClosed)
		}()
	}
}

// IsClosedConnection returns true if err is cmux.ErrListenerClosed,
// grpc.ErrServerStopped, io.EOF, or the net package's errClosed.
func IsClosedConnection(err error) bool {
	return errors.IsAny(err, cmux.ErrListenerClosed, grpc.ErrServerStopped, io.EOF) ||
		strings.Contains(err.Error(), "use of closed network connection")
}

// FatalIfUnexpected calls Log.Fatal(err) unless err is nil,
// cmux.ErrListenerClosed, or the net package's errClosed.
func FatalIfUnexpected(err error) {
	if err != nil && !IsClosedConnection(err) {
		log.Fatal(context.TODO(), err)
	}
}

// InitialHeartbeatFailedError indicates that while attempting a GRPC
// connection to a node, we aren't successful and have never seen a
// heartbeat over that connection before.
type InitialHeartbeatFailedError struct {
	WrappedErr error
}

var _ error = (*InitialHeartbeatFailedError)(nil)
var _ fmt.Formatter = (*InitialHeartbeatFailedError)(nil)
var _ errors.Formatter = (*InitialHeartbeatFailedError)(nil)

// Error implements error.
func (e *InitialHeartbeatFailedError) Error() string { return fmt.Sprintf("%v", e) }

// Cause implements causer.
func (e *InitialHeartbeatFailedError) Cause() error { return e.WrappedErr }

// Format implements fmt.Formatter.
func (e *InitialHeartbeatFailedError) Format(s fmt.State, verb rune) { errors.FormatError(e, s, verb) }

// FormatError implements errors.FormatError.
func (e *InitialHeartbeatFailedError) FormatError(p errors.Printer) error {
	p.Print("initial connection heartbeat failed")
	return e.WrappedErr
}

// NewInitialHeartBeatFailedError creates a new InitialHeartbeatFailedError.
func NewInitialHeartBeatFailedError(cause error) *InitialHeartbeatFailedError {
	return &InitialHeartbeatFailedError{
		WrappedErr: cause,
	}
}
