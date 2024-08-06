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

package concurrency_test

import (
	"strconv"
	"strings"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/concurrency/lock"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage/enginepb"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/uint128"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
	"github.com/cockroachdb/datadriven"
)

func nextUUID(counter *uint32) uuid.UUID {
	*counter = *counter + 1
	hi := uint64(*counter) << 32
	return uuid.FromUint128(uint128.Uint128{Hi: hi})
}

func scanTimestamp(t *testing.T, d *datadriven.TestData) hlc.Timestamp {
	return scanTimestampWithName(t, d, "ts")
}

func scanTimestampWithName(t *testing.T, d *datadriven.TestData, name string) hlc.Timestamp {
	var ts hlc.Timestamp
	var tsS string
	d.ScanArgs(t, name, &tsS)
	parts := strings.Split(tsS, ",")

	// Find the wall time part.
	tsW, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		d.Fatalf(t, "%v", err)
	}
	ts.WallTime = tsW

	// Find the logical part, if there is one.
	var tsL int64
	if len(parts) > 1 {
		tsL, err = strconv.ParseInt(parts[1], 10, 32)
		if err != nil {
			d.Fatalf(t, "%v", err)
		}
	}
	ts.Logical = int32(tsL)
	return ts
}

func scanLockDurability(t *testing.T, d *datadriven.TestData) lock.Durability {
	var durS string
	d.ScanArgs(t, "dur", &durS)
	switch durS {
	case "r":
		return lock.Replicated
	case "u":
		return lock.Unreplicated
	default:
		d.Fatalf(t, "unknown lock durability: %s", durS)
		return 0
	}
}

func scanSingleRequest(
	t *testing.T, d *datadriven.TestData, line string, txns map[string]*roachpb.Transaction,
) roachpb.Request {
	cmd, cmdArgs, err := datadriven.ParseLine(line)
	if err != nil {
		d.Fatalf(t, "error parsing single request: %v", err)
		return nil
	}

	fields := make(map[string]string, len(cmdArgs))
	for _, cmdArg := range cmdArgs {
		if len(cmdArg.Vals) != 1 {
			d.Fatalf(t, "unexpected command values: %+v", cmdArg)
			return nil
		}
		fields[cmdArg.Key] = cmdArg.Vals[0]
	}
	mustGetField := func(f string) string {
		v, ok := fields[f]
		if !ok {
			d.Fatalf(t, "missing required field: %s", f)
		}
		return v
	}
	maybeGetSeq := func() enginepb.TxnSeq {
		s, ok := fields["seq"]
		if !ok {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			d.Fatalf(t, "could not parse seq num: %v", err)
		}
		return enginepb.TxnSeq(n)
	}

	switch cmd {
	case "get":
		var r roachpb.GetRequest
		r.Sequence = maybeGetSeq()
		r.Key = roachpb.Key(mustGetField("key"))
		return &r

	case "scan":
		var r roachpb.ScanRequest
		r.Sequence = maybeGetSeq()
		r.Key = roachpb.Key(mustGetField("key"))
		if v, ok := fields["endkey"]; ok {
			r.EndKey = roachpb.Key(v)
		}
		return &r

	case "put":
		var r roachpb.PutRequest
		r.Sequence = maybeGetSeq()
		r.Key = roachpb.Key(mustGetField("key"))
		r.Value.SetString(mustGetField("value"))
		return &r

	case "resolve-intent":
		var r roachpb.ResolveIntentRequest
		r.IntentTxn = txns[mustGetField("txn")].TxnMeta
		r.Key = roachpb.Key(mustGetField("key"))
		r.Status = parseTxnStatus(t, d, mustGetField("status"))
		return &r

	case "request-lease":
		var r roachpb.RequestLeaseRequest
		return &r

	default:
		d.Fatalf(t, "unknown request type: %s", cmd)
		return nil
	}
}

func scanTxnStatus(t *testing.T, d *datadriven.TestData) (roachpb.TransactionStatus, string) {
	var statusStr string
	d.ScanArgs(t, "status", &statusStr)
	status := parseTxnStatus(t, d, statusStr)
	var verb string
	switch status {
	case roachpb.COMMITTED:
		verb = "committing"
	case roachpb.ABORTED:
		verb = "aborting"
	case roachpb.PENDING:
		verb = "increasing timestamp of"
	default:
		d.Fatalf(t, "unknown txn status: %s", status)
	}
	return status, verb
}

func parseTxnStatus(t *testing.T, d *datadriven.TestData, s string) roachpb.TransactionStatus {
	switch s {
	case "committed":
		return roachpb.COMMITTED
	case "aborted":
		return roachpb.ABORTED
	case "pending":
		return roachpb.PENDING
	default:
		d.Fatalf(t, "unknown txn status: %s", s)
		return 0
	}
}
