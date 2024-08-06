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
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package issues

import (
	"testing"

	"github.com/stretchr/testify/require"
)

type condenseTestCase struct {
	input     string
	digest    string
	lastLines int
}

const fiveLines = `info1
info2
info3
info4
info5
`

const messagePanic = `panic: boom [recovered]
panic: boom
`

const messageFatal = `F191210 10:35:27.740990 196365 foo/bar.go:276  [some,tags] something broke, badly:

everything is broken!
`

const firstStack = `goroutine 1 [running]:
main.main.func2()
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:15 +0x43
panic(0x1061be0, 0x1087db0)
	/usr/local/Cellar/go/1.13.4/libexec/src/runtime/panic.go:679 +0x1b2
main.main()
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:17 +0x92
`
const restStack = `
goroutine 5 [sleep]:
runtime.goparkunlock(...)
	/usr/local/Cellar/go/1.13.4/libexec/src/runtime/proc.go:310
time.Sleep(0x3b9aca00)
	/usr/local/Cellar/go/1.13.4/libexec/src/runtime/time.go:105 +0x157
main.main.func1()
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:10 +0x2a
created by main.main
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:9 +0x42

goroutine 6 [sleep]:
runtime.goparkunlock(...)
	/usr/local/Cellar/go/1.13.4/libexec/src/runtime/proc.go:310
time.Sleep(0x3b9aca00)
	/usr/local/Cellar/go/1.13.4/libexec/src/runtime/time.go:105 +0x157
main.main.func1()
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:10 +0x2a
created by main.main
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:9 +0x42

goroutine 7 [runnable]:
time.Sleep(0x3b9aca00)
	/usr/local/Cellar/go/1.13.4/libexec/src/runtime/time.go:100 +0x109
main.main.func1()
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:10 +0x2a
created by main.main
	/Users/tschottdorf/go/src/github.com/tbg/goplay/panic/main.go:9 +0x42
exit status 2
`

const panic5Lines = fiveLines + messagePanic + firstStack + restStack
const fatal5Lines = fiveLines + messageFatal + firstStack + restStack

var errorCases = []condenseTestCase{
	{

		panic5Lines,
		fiveLines + messagePanic + firstStack,
		100,
	},
	{

		panic5Lines,
		"info5\n" + messagePanic + firstStack,
		1,
	},
	{

		panic5Lines,
		messagePanic + firstStack,
		0,
	},
	{
		fatal5Lines,
		fiveLines + messageFatal + firstStack,
		100,
	},
	{
		fatal5Lines,
		messageFatal + firstStack,
		0,
	},
}

var lineCases = []condenseTestCase{
	{
		``,
		``,
		5,
	},
	{
		`foo
bar
baz
`,
		`baz
`,
		1,
	},
	{
		`foo
bar
baz
`,
		`bar
baz
`,
		2,
	},
	{
		`foo
bar
baz
`,
		`foo
bar
baz
`,
		3,
	},
	{
		`foo
bar
baz
`,
		`foo
bar
baz
`,
		4,
	},
	{
		`foo
bar
baz
`,
		`foo
bar
baz
`,
		100,
	},
}

func TestCondense(t *testing.T) {
	run := func(t *testing.T, tc condenseTestCase) {
		require.Equal(t, tc.digest, CondensedMessage(tc.input).Digest(tc.lastLines))
	}

	for _, tc := range lineCases {
		t.Run("line", func(t *testing.T) {
			run(t, tc)
		})
	}
	for _, tc := range errorCases {
		t.Run("panic", func(t *testing.T) {
			run(t, tc)
		})
	}
}
