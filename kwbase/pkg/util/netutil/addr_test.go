// Copyright 2014 The Cockroach Authors.
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

package netutil_test

import (
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/netutil"
)

func TestSplitHostPort(t *testing.T) {
	testData := []struct {
		v           string
		defaultPort string
		addr        string
		port        string
		err         string
	}{
		{"127.0.0.1", "", "127.0.0.1", "", ""},
		{"127.0.0.1:123", "", "127.0.0.1", "123", ""},
		{"127.0.0.1", "123", "127.0.0.1", "123", ""},
		{"127.0.0.1:456", "123", "127.0.0.1", "456", ""},
		{"[::1]", "", "::1", "", ""},
		{"[::1]:123", "", "::1", "123", ""},
		{"[::1]", "123", "::1", "123", ""},
		{"[::1]:456", "123", "::1", "456", ""},
		{"::1", "", "", "", "invalid address format: \"::1\""},
		{"[123", "", "", "", `address \[123:: missing ']' in address`},
	}

	for _, test := range testData {
		t.Run(test.v+"/"+test.defaultPort, func(t *testing.T) {
			addr, port, err := netutil.SplitHostPort(test.v, test.defaultPort)
			if !testutils.IsError(err, test.err) {
				t.Fatalf("error: expected %q, got: %+v", test.err, err)
			}
			if test.err != "" {
				return
			}
			if addr != test.addr {
				t.Errorf("addr: expected %q, got %q", test.addr, addr)
			}
			if port != test.port {
				t.Errorf("addr: expected %q, got %q", test.port, port)
			}
		})
	}
}
