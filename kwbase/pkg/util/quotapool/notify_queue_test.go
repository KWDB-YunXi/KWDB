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

package quotapool

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
)

func BenchmarkNotifyQueue(b *testing.B) {
	testNotifyQueue(b, b.N)
}

func TestNotifyQueue(t *testing.T) {
	testNotifyQueue(t, 10000)
}

type op bool

const (
	enqueue op = true
	dequeue op = false
)

func testNotifyQueue(t testing.TB, N int) {
	b, _ := t.(*testing.B)
	var q notifyQueue
	initializeNotifyQueue(&q)
	n := q.peek()
	assert.Nil(t, n)
	q.dequeue()
	assert.Equal(t, 0, int(q.len))
	chans := make([]chan struct{}, N)
	for i := 0; i < N; i++ {
		chans[i] = make(chan struct{})
	}
	in := chans
	out := make([]chan struct{}, 0, N)
	ops := make([]op, (4*N)/3)
	for i := 0; i < N; i++ {
		ops[i] = enqueue
	}
	rand.Shuffle(len(ops), func(i, j int) {
		ops[i], ops[j] = ops[j], ops[i]
	})
	if b != nil {
		b.ResetTimer()
	}
	l := 0 // only used if b == nil
	for _, op := range ops {
		switch op {
		case enqueue:
			q.enqueue(in[0])
			in = in[1:]
			if b == nil {
				l++
			}
		case dequeue:
			// Only test Peek if we're not benchmarking.
			if b == nil {
				if n := q.peek(); n != nil {
					out = append(out, n.c)
					q.dequeue()
					l--
					assert.Equal(t, l, int(q.len))
				}
			} else {
				if n := q.peek(); n != nil {
					out = append(out, n.c)
					q.dequeue()
				}
			}
		}
	}
	for n := q.peek(); n != nil; n = q.peek() {
		out = append(out, n.c)
		q.dequeue()
	}
	if b != nil {
		b.StopTimer()
	}
	assert.EqualValues(t, chans, out)
}
