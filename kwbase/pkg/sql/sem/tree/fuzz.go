// Copyright 2019 The Cockroach Authors.
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

//go:build gofuzz
// +build gofuzz

package tree

import "gitee.com/kwbasedb/kwbase/pkg/util/timeutil"

var (
	timeCtx = NewParseTimeContext(timeutil.Now())
)

func FuzzParseDDecimal(data []byte) int {
	_, err := ParseDDecimal(string(data))
	if err != nil {
		return 0
	}
	return 1
}

func FuzzParseDDate(data []byte) int {
	_, err := ParseDDate(timeCtx, string(data))
	if err != nil {
		return 0
	}
	return 1
}
