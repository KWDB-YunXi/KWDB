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

package hba

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/kr/pretty"
)

func TestSpecialCharacters(t *testing.T) {
	// We use Go test cases here instead of datadriven because the input
	// strings would be stripped of whitespace or considered invalid by
	// datadriven.
	testData := []struct {
		input  string
		expErr string
	}{
		{"\"ab\tcd\"", `line 1: invalid characters in quoted string`},
		{"\"ab\fcd\"", `line 1: invalid characters in quoted string`},
		{`0 0 0 0 ` + "\f", `line 1: unsupported character: "\f"`},
		{`0 0 0 0 ` + "\x00", `line 1: unsupported character: "\x00"`},
	}

	for _, tc := range testData {
		_, err := tokenize(tc.input)
		if err == nil || err.Error() != tc.expErr {
			t.Errorf("expected:\n%s\ngot:\n%v", tc.expErr, err)
		}
	}
}

func TestScanner(t *testing.T) {
	datadriven.RunTest(t, "testdata/scan", func(t *testing.T, td *datadriven.TestData) string {
		switch td.Cmd {
		case "token":
			remaining, tok, trailingComma, err := nextToken(td.Input)
			if err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			return fmt.Sprintf("%# v %v %q", pretty.Formatter(tok), trailingComma, remaining)

		case "field":
			remaining, field, err := nextFieldExpand(td.Input)
			if err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			return fmt.Sprintf("%+v\n%q", field, remaining)

		case "file":
			tokens, err := tokenize(td.Input)
			if err != nil {
				return fmt.Sprintf("error: %v", err)
			}
			return fmt.Sprintf("%# v", pretty.Formatter(tokens))
		default:
			t.Fatalf("unknown directive: %s", td.Cmd)
		}
		return ""
	})
}
