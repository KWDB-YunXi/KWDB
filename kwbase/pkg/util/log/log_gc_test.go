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

package log

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
)

func TestGC(t *testing.T) {
	s := ScopeWithoutShowLogs(t)
	defer s.Close(t)

	setFlags()

	testLogGC(t, &mainLog, Infof)
}

func TestSecondaryGC(t *testing.T) {
	s := ScopeWithoutShowLogs(t)
	defer s.Close(t)

	setFlags()

	tmpDir, err := ioutil.TempDir(mainLog.logDir.String(), "gctest")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !t.Failed() {
			// If the test failed, we'll want to keep the artifacts for
			// troubleshooting.
			_ = os.RemoveAll(tmpDir)
		}
	}()
	tmpDirName := DirName{name: tmpDir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := NewSecondaryLogger(ctx, &tmpDirName, "woo", false /*enableGc*/, false /*syncWrites*/, true /*msgCount*/)
	defer l.Close()

	testLogGC(t, &l.logger, l.Logf)
}

func testLogGC(
	t *testing.T,
	logger *loggerT,
	logFn func(ctx context.Context, format string, args ...interface{}),
) {
	logging.mu.Lock()
	logging.mu.disableDaemons = true
	defer func(previous bool) {
		logging.mu.Lock()
		logging.mu.disableDaemons = previous
		logging.mu.Unlock()
	}(logging.mu.disableDaemons)
	logging.mu.Unlock()

	const newLogFiles = 20

	// Prevent writes to stderr from being sent to log files which would screw up
	// the expected number of log file calculation below.
	mainLog.noStderrRedirect = true

	// Ensure the main log has at least one entry. This serves two
	// purposes. One is to serve as baseline for the file size. The
	// other is to ensure that the TestLogScope does not erase the
	// temporary directory, preventing further investigation.
	Infof(context.Background(), "0")

	// Make an entry in the target logger. This ensures That there is at
	// least one file in the target directory for the logger being
	// tested.
	logFn(context.Background(), "0")

	// Check that the file was created.
	allFilesOriginal, err := logger.listLogFiles()
	if err != nil {
		t.Fatal(err)
	}
	if e, a := 1, len(allFilesOriginal); e != a {
		t.Fatalf("expected %d files, but found %d", e, a)
	}

	// Check that the file exists, and also measure its size.
	// We'll use this as base value for the maximum combined size
	// below, to force GC.
	dir, isSet := logger.logDir.get()
	if !isSet {
		t.Fatal(errDirectoryNotSet)
	}
	stat, err := os.Stat(filepath.Join(dir, allFilesOriginal[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	// logFileSize is the size of the first log file we wrote to.
	logFileSize := stat.Size()
	const expectedFilesAfterGC = 2
	// Pick a max total size that's between 2 and 3 log files in size.
	maxTotalLogFileSize := logFileSize*expectedFilesAfterGC + logFileSize // 2

	// We want to create multiple log files below. For this we need to
	// override the size/number limits first to the values suitable for
	// the test.
	defer func(previous int64) { LogFileMaxSize = previous }(LogFileMaxSize)
	LogFileMaxSize = 1 // ensure rotation on every log write
	defer func(previous int64) {
		atomic.StoreInt64(&LogFilesCombinedMaxSize, previous)
	}(LogFilesCombinedMaxSize)
	atomic.StoreInt64(&LogFilesCombinedMaxSize, maxTotalLogFileSize)

	// Create the number of expected log files.
	for i := 1; i < newLogFiles; i++ {
		logFn(context.Background(), "%d", i)
		Flush()
	}

	allFilesBefore, err := logger.listLogFiles()
	if err != nil {
		t.Fatal(err)
	}
	if e, a := newLogFiles, len(allFilesBefore); e != a {
		t.Fatalf("expected %d files, but found %d", e, a)
	}

	// Re-enable GC, so that the GC daemon can pick up the files.
	logging.mu.Lock()
	logging.mu.disableDaemons = false
	logging.mu.Unlock()
	// Start the GC daemon, using a context that will terminate it
	// at the end of the test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go logger.gcDaemon(ctx)

	// Emit a log line which will rotate the files and trigger GC.
	logFn(context.Background(), "final")
	Flush()

	succeedsSoon(t, func() error {
		allFilesAfter, err := logger.listLogFiles()
		if err != nil {
			return err
		}
		if e, a := expectedFilesAfterGC, len(allFilesAfter); e != a {
			return fmt.Errorf("expected %d files, but found %d", e, a)
		}
		return nil
	})
}

// succeedsSoon is a simplified version of testutils.SucceedsSoon.
// The main implementation cannot be used here because of
// an import cycle.
func succeedsSoon(t *testing.T, fn func() error) {
	t.Helper()
	tBegin := timeutil.Now()
	deadline := tBegin.Add(5 * time.Second)
	var lastErr error
	for wait := time.Duration(1); timeutil.Now().Before(deadline); wait *= 2 {
		lastErr = fn()
		if lastErr == nil {
			return
		}
		if wait > time.Second {
			wait = time.Second
		}
		if timeutil.Since(tBegin) > 3*time.Second {
			InfofDepth(context.Background(), 2, "SucceedsSoon: %+v", lastErr)
		}
		time.Sleep(wait)
	}
	if lastErr != nil {
		t.Fatalf("condition failed to evalute: %+v", lastErr)
	}
}
