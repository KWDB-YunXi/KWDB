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

package cli

import (
	"bytes"
	"io/ioutil"
	"path/filepath"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/spf13/pflag"
)

const testdataPath = "testdata/merge_logs"

type testCase struct {
	name  string
	flags []string
	args  []string
}

var cases = []testCase{
	{
		name: "1.all",
		args: []string{"testdata/merge_logs/1/*/*"},
	},
	{
		name:  "1.filter-program",
		args:  []string{"testdata/merge_logs/1/*/*"},
		flags: []string{"--program-filter", "not-kwbase"},
	},
	{
		name:  "1.seek-past-end-of-file",
		args:  []string{"testdata/merge_logs/1/*/*"},
		flags: []string{"--from", "181130 22:15:07.525317"},
	},
	{
		name:  "1.filter-message",
		args:  []string{"testdata/merge_logs/1/*/*"},
		flags: []string{"--filter", "gossip"},
	},
	{
		name: "2.multiple-files-from-node",
		args: []string{"testdata/merge_logs/2/*/*"},
	},
	{
		name:  "2.skip-file",
		args:  []string{"testdata/merge_logs/2/*/*"},
		flags: []string{"--from", "181130 22:15:07.525316"},
	},
	{
		name: "2.remove-duplicates",
		args: []string{
			"testdata/merge_logs/2/1.logs/kwbase.test-0001.ubuntu.2018-11-30T22_06_47Z.004130.log",
			"testdata/merge_logs/2/1.logs/kwbase.test-0001.ubuntu.2018-11-30T22_14_47Z.004130.log",
			"testdata/merge_logs/2/2.logs/kwbase.stderr",
			"testdata/merge_logs/2/2.logs/kwbase.test-0002.ubuntu.2018-11-30T22_06_47Z.003959.log",
			"testdata/merge_logs/2/2.logs/kwbase.test-0002.ubuntu.2018-11-30T22_06_47Z.003959.log",
			"testdata/merge_logs/2/2.logs/roachprod.log",
		},
		flags: []string{"--from", "181130 22:15:07.525316"},
	},
	{
		name:  "3.non-standard",
		args:  []string{"testdata/merge_logs/3/*/*"},
		flags: []string{"--file-pattern", ".*", "--prefix", ""},
	},
	{
		// Prints only lines that match the filter (if no submatches).
		name:  "4.filter",
		args:  []string{"testdata/merge_logs/4/*"},
		flags: []string{"--file-pattern", ".*", "--filter", "3:0"},
	},
	{
		// Prints only the submatch.
		name:  "4.filter-submatch",
		args:  []string{"testdata/merge_logs/4/*"},
		flags: []string{"--file-pattern", ".*", "--filter", "(3:)0"},
	},
	{
		// Prints only the submatches.
		name:  "4.filter-submatch-double",
		args:  []string{"testdata/merge_logs/4/*"},
		flags: []string{"--file-pattern", ".*", "--filter", "(3):(0)"},
	},
	{
		// Simple grep for a panic line only.
		name:  "4.filter-npe",
		args:  []string{"testdata/merge_logs/4/npe.log"},
		flags: []string{"--file-pattern", ".*", "--filter", `(panic: .*)`},
	},
	{
		// Grep for a panic and a few lines more. This is often not so useful
		// because there's lots of recover() and re-panic happening; the real
		// source of the panic is harder to find.
		name:  "4.filter-npe-with-context",
		args:  []string{"testdata/merge_logs/4/npe.log"},
		flags: []string{"--file-pattern", ".*", "--filter", `(?m)(panic:.(?:.*\n){0,5})`},
	},
	{
		// This regexp attempts to find the source of the panic, essentially by
		// matching on:
		//
		// panic(...
		//      stack
		// anything
		//      stack
		// not-a-panic
		//      stack
		//
		// This will skip past the recover-panic extra stacks at the top which
		// usually alternate with panic().
		name:  "4.filter-npe-origin-stack-only",
		args:  []string{"testdata/merge_logs/4/npe-repanic.log"}, // (?:panic\(.*)*
		flags: []string{"--file-pattern", ".*", "--filter", `(?m)^(panic\(.*\n.*\n.*\n.*\n[^p].*)`},
	}}

func (c testCase) run(t *testing.T) {
	outBuf := bytes.Buffer{}
	debugMergeLogsCommand.Flags().Visit(func(f *pflag.Flag) {
		if err := f.Value.Set(f.DefValue); err != nil {
			t.Fatalf("Failed to set flag to default: %v", err)
		}
	})
	debugMergeLogsCommand.SetOutput(&outBuf)
	if err := debugMergeLogsCommand.ParseFlags(c.flags); err != nil {
		t.Fatalf("Failed to set flags: %v", err)
	}
	if err := debugMergeLogsCommand.RunE(debugMergeLogsCommand, c.args); err != nil {
		t.Fatalf("Failed to run command: %v", err)
	}
	// The expected output lives in filepath.Join(testdataPath, "results", testCase.name)
	resultFile := filepath.Join(testdataPath, "results", c.name)
	expected, err := ioutil.ReadFile(resultFile)
	if err != nil {
		t.Errorf("Failed to read expected result from %v: %v", resultFile, err)
	}
	if !bytes.Equal(expected, outBuf.Bytes()) {
		t.Fatalf("Result does not match expected. Got %d bytes, expected %d. got:\n%v\nexpected:\n%v",
			len(outBuf.String()), len(expected), outBuf.String(), string(expected))
	}
}

func TestDebugMergeLogs(t *testing.T) {
	defer leaktest.AfterTest(t)()
	for _, c := range cases {
		t.Run(c.name, c.run)
	}
}
