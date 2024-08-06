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

package batcheval

import (
	"context"

	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/batcheval/result"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"github.com/pkg/errors"
)

func init() {
	RegisterReadOnlyCommand(roachpb.RefreshRange, DefaultDeclareKeys, RefreshRange)
}

// RefreshRange checks whether the key range specified has any values written in
// the interval [args.RefreshFrom, header.Timestamp].
func RefreshRange(
	ctx context.Context, reader storage.Reader, cArgs CommandArgs, resp roachpb.Response,
) (result.Result, error) {
	args := cArgs.Args.(*roachpb.RefreshRangeRequest)
	h := cArgs.Header

	if h.Txn == nil {
		return result.Result{}, errors.Errorf("no transaction specified to %s", args.Method())
	}

	// We're going to refresh up to the transaction's read timestamp.
	if h.Timestamp != h.Txn.WriteTimestamp {
		// We're expecting the read and write timestamp to have converged before the
		// Refresh request was sent.
		log.Fatalf(ctx, "expected provisional commit ts %s == read ts %s. txn: %s", h.Timestamp,
			h.Txn.WriteTimestamp, h.Txn)
	}
	refreshTo := h.Timestamp

	refreshFrom := args.RefreshFrom
	if refreshFrom.IsEmpty() {
		// Compatibility with 19.2 nodes, which didn't set the args.RefreshFrom field.
		refreshFrom = h.Txn.DeprecatedOrigTimestamp
	}

	// Iterate over values until we discover any value written at or after the
	// original timestamp, but before or at the current timestamp. Note that we
	// iterate inconsistently, meaning that intents - including our own - are
	// collected separately and the callback is only invoked on the latest
	// committed version. Note also that we include tombstones, which must be
	// considered as updates on refresh.
	log.VEventf(ctx, 2, "refresh %s @[%s-%s]", args.Span(), refreshFrom, refreshTo)
	intents, err := storage.MVCCIterate(
		ctx, reader, args.Key, args.EndKey, refreshTo,
		storage.MVCCScanOptions{
			Inconsistent: true,
			Tombstones:   true,
		},
		func(kv roachpb.KeyValue) (bool, error) {
			if ts := kv.Value.Timestamp; refreshFrom.LessEq(ts) {
				return true, errors.Errorf("encountered recently written key %s @%s", kv.Key, ts)
			}
			return false, nil
		})
	if err != nil {
		return result.Result{}, err
	}

	// Check if any intents which are not owned by this transaction were written
	// at or beneath the refresh timestamp.
	for _, i := range intents {
		// Ignore our own intents.
		if i.Txn.ID == h.Txn.ID {
			continue
		}
		// Return an error if an intent was written to the span.
		return result.Result{}, errors.Errorf("encountered recently written intent %s @%s",
			i.Key, i.Txn.WriteTimestamp)
	}

	return result.Result{}, nil
}
