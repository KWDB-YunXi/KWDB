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

package lang

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/cockroachdb/datadriven"
)

func TestScanner(t *testing.T) {
	datadriven.RunTest(t, "testdata/scanner", func(t *testing.T, d *datadriven.TestData) string {
		// Only scan command supported.
		if d.Cmd != "scan" {
			t.FailNow()
		}

		// Check for "fail=count" command arg, which indicates reader failure
		// test case.
		count := -1
		for _, arg := range d.CmdArgs {
			if arg.Key != "fail" || len(arg.Vals) != 1 {
				t.FailNow()
			}
			count, _ = strconv.Atoi(arg.Vals[0])
		}

		r := io.Reader(strings.NewReader(d.Input))
		if count != -1 {
			// Wrap the reader in readers that will fail once the specified
			// count of bytes have been read.
			r = &errorReader{r: iotest.OneByteReader(r), count: count}
		}
		s := NewScanner(r)

		var buf bytes.Buffer
		for {
			tok := s.Scan()
			if tok == EOF {
				break
			}

			fmt.Fprintf(&buf, "(%v %s)\n", tok, s.Literal())

			if tok == ERROR {
				break
			}
		}

		return buf.String()
	})
}

// Separate test case for whitespace, since some editors normalize whitespace
// in the data driven test case file.
func TestScannerWhitespace(t *testing.T) {
	// Use various ASCII whitespace chars + Unicode whitespace chars.
	ws := " \t\r\n \u00A0\u1680"
	s := NewScanner(strings.NewReader(ws))
	tok := s.Scan()
	if tok != WHITESPACE {
		t.Fatalf("expected whitespace, found %v", tok)
	}
	if s.Literal() != ws {
		t.Fatal("whitespace did not match")
	}
}

// errorReader returns io.ErrClosedPipe after count reads.
type errorReader struct {
	r     io.Reader
	count int
}

func (r *errorReader) Read(p []byte) (int, error) {
	r.count--
	if r.count <= 0 {
		return 0, io.ErrClosedPipe
	}
	return r.r.Read(p)
}
