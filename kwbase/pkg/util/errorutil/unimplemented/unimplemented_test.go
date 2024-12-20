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

package unimplemented

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/errors"
)

func TestUnimplemented(t *testing.T) {
	testData := []struct {
		err        error
		expMsg     string
		expFeature string
		expHint    string
		expIssue   int
	}{
		// disabling all the test cases because they rely on the URL which is not proper to expose.
		// {New("woo", "waa"), "unimplemented: waa", "woo", "", 0},
		// {Newf("woo", "hello %s", "world"), "unimplemented: hello world", "woo", "", 0},
		// {NewWithDepthf(1, "woo", "hello %s", "world"), "unimplemented: hello world", "woo", "", 0},
		// {NewWithIssue(123, "waa"), "unimplemented: waa", "", "", 123},
		// {NewWithIssuef(123, "hello %s", "world"), "unimplemented: hello world", "", "", 123},
		// {NewWithIssueHint(123, "waa", "woo"), "unimplemented: waa", "", "woo", 123},
		// {NewWithIssueDetail(123, "waa", "woo"), "unimplemented: woo", "waa", "", 123},
		// {NewWithIssueDetailf(123, "waa", "hello %s", "world"), "unimplemented: hello world", "waa", "", 123},
	}

	for i, test := range testData {
		t.Run(fmt.Sprintf("%d: %v", i, test.err), func(t *testing.T) {
			if test.err.Error() != test.expMsg {
				t.Errorf("expected %q, got %q", test.expMsg, test.err.Error())
			}

			hints := errors.GetAllHints(test.err)
			found := 0
			for _, hint := range hints {
				if test.expHint != "" && hint == test.expHint {
					found |= 1
				}
				if test.expIssue != 0 {
					ref := fmt.Sprintf("%s\nSee: %s",
						errors.UnimplementedErrorHint, MakeURL(test.expIssue))
					if hint == ref {
						found |= 2
					}
				}
				if strings.HasPrefix(hint, errors.UnimplementedErrorHint) {
					found |= 4
				}
			}
			if test.expHint != "" && found&1 == 0 {
				t.Errorf("expected hint %q, not found\n%+v", test.expHint, hints)
			}
			if test.expIssue != 0 && found&2 == 0 {
				t.Errorf("expected issue ref url to %d in link, not found\n%+v", test.expIssue, hints)
			}
			if found&4 == 0 {
				t.Errorf("expected standard hint introduction %q, not found\n%+v",
					errors.UnimplementedErrorHint, hints)
			}

			links := errors.GetAllIssueLinks(test.err)
			if len(links) != 1 {
				t.Errorf("expected 1 issue link, got %+v", links)
			} else {
				if links[0].Detail != test.expFeature {
					t.Errorf("expected link detail %q, got %q", test.expFeature, links[0].Detail)
				}

				if test.expIssue != 0 {
					url := MakeURL(test.expIssue)
					if links[0].IssueURL != url {
						t.Errorf("expected link url %q, got %q", url, links[0].IssueURL)
					}
				}
			}

			keys := errors.GetTelemetryKeys(test.err)
			if len(keys) != 1 {
				t.Errorf("expected 1 telemetry key, got %+v", keys)
			} else {
				expKey := test.expFeature
				if test.expIssue > 0 {
					if expKey != "" {
						expKey = fmt.Sprintf("#%d.%s", test.expIssue, expKey)
					} else {
						expKey = fmt.Sprintf("#%d", test.expIssue)
					}
				}
				if keys[0] != expKey {
					t.Errorf("expected key %q, got %q", expKey, keys[0])
				}
			}

			if t.Failed() {
				t.Logf("while inspecting error: %+v", test.err)
			}
		})
	}
}
