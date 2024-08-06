// Copyright 2019 The Cockroach Authors.
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

package kvcoord

import (
	"context"
	"sync"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"github.com/cockroachdb/errors"
)

// lockedSender is like a client.Sender but requires the caller to hold the
// TxnCoordSender lock to send requests.
type lockedSender interface {
	// SendLocked sends the batch request and receives a batch response. It
	// requires that the TxnCoordSender lock be held when called, but this lock
	// is not held for the entire duration of the call. Instead, the lock is
	// released immediately before the batch is sent to a lower-level Sender and
	// is re-acquired when the response is returned.
	// WARNING: because the lock is released when calling this method and
	// re-acquired before it returned, callers cannot rely on a single mutual
	// exclusion zone mainted across the call.
	SendLocked(context.Context, roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error)
}

// txnLockGatekeeper is a lockedSender that sits at the bottom of the
// TxnCoordSender's interceptor stack and handles unlocking the TxnCoordSender's
// mutex when sending a request and locking the TxnCoordSender's mutex when
// receiving a response. It allows the entire txnInterceptor stack to operate
// under lock without needing to worry about unlocking at the correct time.
type txnLockGatekeeper struct {
	wrapped kv.Sender
	mu      sync.Locker // shared with TxnCoordSender

	// If set, concurrent requests are allowed. If not set, concurrent requests
	// result in an assertion error. Only leaf transactions are supposed allow
	// concurrent requests - leaves don't restart the transaction and they don't
	// bump the read timestamp through refreshes.
	allowConcurrentRequests bool
	// requestInFlight is set while a request is being processed by the wrapped
	// sender. Used to detect and prevent concurrent txn use.
	requestInFlight bool
}

// SendLocked implements the lockedSender interface.
func (gs *txnLockGatekeeper) SendLocked(
	ctx context.Context, ba roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	// If so configured, protect against concurrent use of the txn. Concurrent
	// requests don't work generally because of races between clients sending
	// requests and the TxnCoordSender restarting the transaction, and also
	// concurrent requests are not compatible with the span refresher in
	// particular since refreshing is invalid if done concurrently with requests
	// in flight whose spans haven't been accounted for.
	//
	// As a special case, allow for async rollbacks and heartbeats to be sent
	// whenever.
	if !gs.allowConcurrentRequests {
		asyncRequest := ba.IsSingleAbortTxnRequest() || ba.IsSingleHeartbeatTxnRequest()
		if !asyncRequest {
			if gs.requestInFlight {
				return nil, roachpb.NewError(
					errors.AssertionFailedf("concurrent txn use detected. ba: %s", ba))
			}
			gs.requestInFlight = true
			defer func() {
				gs.requestInFlight = false
			}()
		}
	}

	// Note the funky locking here: we unlock for the duration of the call and the
	// lock again.
	gs.mu.Unlock()
	defer gs.mu.Lock()
	return gs.wrapped.Send(ctx, ba)
}
