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

package util

import (
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
)

func TestEveryN(t *testing.T) {
	start := timeutil.Now()
	en := EveryN{N: time.Minute}
	testCases := []struct {
		t        time.Duration // time since start
		expected bool
	}{
		{0, true}, // the first attempt to log should always succeed
		{0, false},
		{time.Second, false},
		{time.Minute - 1, false},
		{time.Minute, true},
		{time.Minute, false},
		{time.Minute + 30*time.Second, false},
		{10 * time.Minute, true},
		{10 * time.Minute, false},
		{10*time.Minute + 59*time.Second, false},
		{11 * time.Minute, true},
	}
	for _, tc := range testCases {
		if a, e := en.ShouldProcess(start.Add(tc.t)), tc.expected; a != e {
			t.Errorf("ShouldProcess(%v) got %v, want %v", tc.t, a, e)
		}
	}
}
