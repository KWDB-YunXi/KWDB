// Copyright 2013 The Go Authors. All rights reserved.
// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in licenses/BSD-golang.txt.

// Portions of this file are additionally subject to the following
// license and copyright.
//
// Copyright 2016 The Cockroach Authors.
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

// Package leaktest provides tools to detect leaked goroutines in tests.
// To use it, call "defer leaktest.AfterTest(t)()" at the beginning of each
// test that may use goroutines.
package leaktest

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"github.com/petermattis/goid"
)

// interestingGoroutines returns all goroutines we care about for the purpose
// of leak checking. It excludes testing or runtime ones.
func interestingGoroutines() map[int64]string {
	buf := make([]byte, 2<<20)
	buf = buf[:runtime.Stack(buf, true)]
	gs := make(map[int64]string)
	for _, g := range strings.Split(string(buf), "\n\n") {
		sl := strings.SplitN(g, "\n", 2)
		if len(sl) != 2 {
			continue
		}
		stack := strings.TrimSpace(sl[1])
		if strings.HasPrefix(stack, "testing.RunTests") {
			continue
		}

		if stack == "" ||
			// Ignore HTTP keep alives
			strings.Contains(stack, ").readLoop(") ||
			strings.Contains(stack, ").writeLoop(") ||
			// Ignore the raven client, which is created lazily on first use.
			strings.Contains(stack, "raven-go.(*Client).Capture") ||
			// Seems to be gccgo specific.
			(runtime.Compiler == "gccgo" && strings.Contains(stack, "testing.T.Parallel")) ||
			// Below are the stacks ignored by the upstream leaktest code.
			strings.Contains(stack, "testing.Main(") ||
			strings.Contains(stack, "testing.tRunner(") ||
			strings.Contains(stack, "runtime.goexit") ||
			strings.Contains(stack, "created by runtime.gc") ||
			strings.Contains(stack, "interestingGoroutines") ||
			strings.Contains(stack, "runtime.MHeap_Scavenger") ||
			strings.Contains(stack, "signal.signal_recv") ||
			strings.Contains(stack, "sigterm.handler") ||
			strings.Contains(stack, "runtime_mcall") ||
			strings.Contains(stack, "goroutine in C code") ||
			strings.Contains(stack, "runtime.CPUProfile") ||
			strings.Contains(stack, "txnScannerScheduler") ||
			strings.Contains(stack, "txnReplayerScheduler") {
			continue
		}
		gs[goid.ExtractGID([]byte(g))] = g
	}
	return gs
}

// Set once a test leaks goroutines so that further tests don't attempt to
// detect leaks any more. Once a tests leaks, it has soiled the process beyond
// repair: even though other tests would take a snapshot of goroutines at the
// beginning that would include the previously-leaked goroutines, those leaked
// goroutines can spin up other goroutines at random times and these would be
// mis-attributed as leaked by the currently-running test.
var leakDetectorDisabled uint32

// AfterTest snapshots the currently-running goroutines and returns a
// function to be run at the end of tests to see whether any
// goroutines leaked.
func AfterTest(t testing.TB) func() {
	if atomic.LoadUint32(&leakDetectorDisabled) != 0 {
		return func() {}
	}
	orig := interestingGoroutines()
	return func() {
		// If there was a panic, "leaked" goroutines are expected.
		if r := recover(); r != nil {
			panic(r)
		}

		// If the test already failed, we don't pile on any more errors but we check
		// to see if the leak detector should be disabled for future tests.
		if t.Failed() {
			if err := diffGoroutines(orig); err != nil {
				atomic.StoreUint32(&leakDetectorDisabled, 1)
			}
			return
		}

		// Loop, waiting for goroutines to shut down.
		// Wait up to 5 seconds, but finish as quickly as possible.
		deadline := timeutil.Now().Add(5 * time.Second)
		for {
			if err := diffGoroutines(orig); err != nil {
				if timeutil.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
					continue
				}
				atomic.StoreUint32(&leakDetectorDisabled, 1)
				t.Error(err)
			}
			break
		}
	}
}

// diffGoroutines compares the current goroutines with the base snapshort and
// returns an error if they differ.
func diffGoroutines(base map[int64]string) error {
	var leaked []string
	for id, stack := range interestingGoroutines() {
		if _, ok := base[id]; !ok {
			leaked = append(leaked, stack)
		}
	}
	if len(leaked) == 0 {
		return nil
	}

	sort.Strings(leaked)
	var b strings.Builder
	for _, g := range leaked {
		b.WriteString(fmt.Sprintf("Leaked goroutine: %v\n\n", g))
	}
	return fmt.Errorf(b.String())
}
