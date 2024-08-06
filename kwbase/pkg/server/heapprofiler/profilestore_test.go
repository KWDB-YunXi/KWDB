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

package heapprofiler

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/server/dumpstore"
	"github.com/stretchr/testify/assert"
)

func TestMakeFileName(t *testing.T) {
	store := dumpstore.NewStore("mydir", nil, nil)
	joy := newProfileStore(store, heapFileNamePrefix, nil)

	ts := time.Date(2020, 6, 15, 13, 19, 19, 543000000, time.UTC)
	assert.Equal(t,
		filepath.Join("mydir", "memprof.2020-06-15T13_19_19.543.123456"),
		joy.makeNewFileName(ts, 123456))

	// Also check when the millisecond part is zero. This verifies that
	// the .999 format is not used, which would cause the millisecond
	// part to be (erronously) omitted.
	ts = time.Date(2020, 6, 15, 13, 19, 19, 00000000, time.UTC)
	assert.Equal(t,
		filepath.Join("mydir", "memprof.2020-06-15T13_19_19.000.123456"),
		joy.makeNewFileName(ts, 123456))
}

func TestParseFileName(t *testing.T) {
	z := time.Time{}
	testData := []struct {
		f         string
		ts        time.Time
		heapUsage uint64
		expError  bool
	}{
		{"hello", z, 0, true},
		{"", z, 0, true},

		// Old (obsolete) formats.
		{"memprof.fraction_system_memory.000000019331059712_2020-03-04T16_58_39.54.pprof", z, 0, true},
		{"memprof.000000000030536024_2020-06-15T13_19_19.543", z, 0, true},

		// New format.
		{"memprof.2020-06-15T13_19_19.543.123456", time.Date(2020, 6, 15, 13, 19, 19, 543000000, time.UTC), 123456, false},
		// v20.2 transition formats.
		// TODO(knz): Remove in v21.1.
		{"memprof.2020-06-15T13_19_19.54.123456", time.Date(2020, 6, 15, 13, 19, 19, 540000000, time.UTC), 123456, false},
		{"memprof.2020-06-15T13_19_19.5.123456", time.Date(2020, 6, 15, 13, 19, 19, 500000000, time.UTC), 123456, false},
	}

	s := profileStore{prefix: heapFileNamePrefix}
	for _, tc := range testData {
		ok, ts, heapUsage := s.parseFileName(context.Background(), tc.f)
		if ok != !tc.expError {
			t.Fatalf("%s: expected error %v, got %v", tc.f, tc.expError, !ok)
		}

		assert.Equal(t, tc.ts, ts)
		assert.Equal(t, tc.heapUsage, heapUsage)
	}
}

func TestCleanupLastRampup(t *testing.T) {
	testData := []struct {
		startFiles []string
		maxP       int64
		cleaned    []string
		preserved  map[int]bool
	}{
		// When the directory is empty, nothing happens.
		{[]string{}, 0, []string{}, map[int]bool{}},
		{[]string{}, 5, []string{}, map[int]bool{}},
		// General case, just one file.
		{
			// Starting files.
			[]string{
				"memprof.2020-06-15T13_19_19.543.123456",
			},
			// max files to keep in upramp.
			1,
			// Nothing gets cleaned.
			[]string{},
			// The one file gets preserved.
			map[int]bool{0: true},
		},
		// General case.
		{
			// Two upramps.
			[]string{
				// First upramp.
				"memprof.2020-06-15T13_19_19.000.1",
				"memprof.2020-06-15T13_19_19.001.2",
				"memprof.2020-06-15T13_19_19.002.3",
				"memprof.2020-06-15T13_19_19.003.4",
				"memprof.2020-06-15T13_19_19.004.5",
				// Second upramp.
				"memprof.2020-06-15T13_19_19.005.1",
				"memprof.2020-06-15T13_19_19.006.10",
				"memprof.2020-06-15T13_19_19.007.20",
				"memprof.2020-06-15T13_19_19.008.100",
			},
			// max files to keep in one upramp.
			2,
			// We expect files to only get cleaned in the last upramp.
			// The deletion goes in decreasing order (the algorithm
			// starts from the end.)
			[]string{
				"memprof.2020-06-15T13_19_19.006.10",
				"memprof.2020-06-15T13_19_19.005.1",
			},
			// The last two files in the 2nd upramp are preserved.
			map[int]bool{7: true, 8: true},
		},
		// Odd case: num files to keep = 0 per upramp.  Everything gets
		// deleted _inside the last upramp_ but previous upramps are
		// unaffected. This is intended to preserve upramps that occur
		// when a process crashes.
		{
			[]string{
				// First upramp.
				"memprof.2020-06-15T13_19_19.000.1",
				"memprof.2020-06-15T13_19_19.001.2",
				"memprof.2020-06-15T13_19_19.002.3",
				"memprof.2020-06-15T13_19_19.003.4",
				"memprof.2020-06-15T13_19_19.004.5",
				// Second upramp.
				"memprof.2020-06-15T13_19_19.005.1",
				"memprof.2020-06-15T13_19_19.006.10",
				"memprof.2020-06-15T13_19_19.007.20",
				"memprof.2020-06-15T13_19_19.008.100",
			},
			// max files to keep in one upramp.
			0,
			// The deletion goes in decreasing order (the algorithm
			// starts from the end.)
			[]string{
				"memprof.2020-06-15T13_19_19.008.100",
				"memprof.2020-06-15T13_19_19.007.20",
				"memprof.2020-06-15T13_19_19.006.10",
				"memprof.2020-06-15T13_19_19.005.1",
			},
			// No file preserved.
			map[int]bool{},
		},
		// Odd case: bogus filenames "in the middle".
		{
			// Two upramps.
			[]string{
				// Unexpected junk files at beginning.
				"abc_unexpected0",
				"def_unexpected1",
				// First upramp.
				"memprof.2020-06-15T13_19_19.002.1",
				"memprof.2020-06-15T13_19_19.003.2",
				"memprof.2020-06-15T13_19_19.004.invalid",
				"memprof.2020-06-15T13_19_19.005.4",
				// Some middle junk.
				"memprof.2020-06-15T13_19_19.006.unexpected",
				// Second upramp.
				"memprof.2020-06-15T13_19_19.007.1",
				"memprof.2020-06-15T13_19_19.008.10",
				"memprof.2020-06-15T13_19_19.009.invalid",
				"memprof.2020-06-15T13_19_19.010.100",
				// Unexpected junk files at end.
				"yyy_unexpected11",
				"zzz_unexpected12",
			},
			// max files to keep in one upramp.
			2,
			// We expect files to only get cleaned in the last upramp.
			// The deletion goes in decreasing order (the algorithm
			// starts from the end.)
			[]string{
				"memprof.2020-06-15T13_19_19.007.1",
			},
			// The last two _valid_ files in the 2nd upramp are preserved.
			map[int]bool{8: true, 10: true},
		},
	}

	s := profileStore{prefix: heapFileNamePrefix}
	for i, tc := range testData {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			path, err := ioutil.TempDir("", "cleanup")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.RemoveAll(path) }()

			files := populate(t, path, tc.startFiles)

			cleaned := []string{}
			cleanupFn := func(s string) error {
				cleaned = append(cleaned, s)
				return nil
			}

			preserved := s.cleanupLastRampup(context.Background(), files, tc.maxP, cleanupFn)
			assert.EqualValues(t, tc.cleaned, cleaned)
			assert.EqualValues(t, tc.preserved, preserved)
		})
	}
}

func populate(t *testing.T, dirName string, fileNames []string) []os.FileInfo {
	for _, fn := range fileNames {
		f, err := os.Create(filepath.Join(dirName, fn))
		if err != nil {
			t.Fatal(err)
		}

		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Retrieve the file list for the remainder of the test.
	files, err := ioutil.ReadDir(dirName)
	if err != nil {
		t.Fatal(err)
	}
	return files
}
