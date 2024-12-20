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

package sqlsmith

func (s *Smither) coin() bool {
	return s.rnd.Intn(2) == 0
}

func (s *Smither) d6() int {
	return s.rnd.Intn(6) + 1
}

func (s *Smither) d9() int {
	return s.rnd.Intn(9) + 1
}

func (s *Smither) d100() int {
	return s.rnd.Intn(100) + 1
}

// geom returns a sample from a geometric distribution whose mean is 1 / (1 -
// p). Its return value is >= 1. p must satisfy 0 < p < 1. For example, pass
// .5 to this function (whose mean would thus be 2), and 50% of the time this
// function will return 1, 25% will return 2, 12.5% return 3, 6.25% will return
// 4, etc. See: https://en.wikipedia.org/wiki/Geometric_distribution.
func (s *Smither) geom(p float64) int {
	if p <= 0 || p >= 1 {
		panic("bad p")
	}
	count := 1
	for s.rnd.Float64() < p {
		count++
	}
	return count
}

// sample invokes fn mean number of times (but at most n times) with a
// geometric distribution. The i argument to fn will be unique each time and
// randomly chosen to be between 0 and n-1, inclusive. This can be used to pick
// on average mean samples from a list. If n is <= 0, fn is never invoked.
func (s *Smither) sample(n, mean int, fn func(i int)) {
	if n <= 0 {
		return
	}
	perms := s.rnd.Perm(n)
	m := float64(mean)
	p := (m - 1) / m
	k := s.geom(p)
	if k > n {
		k = n
	}
	for ki := 0; ki < k; ki++ {
		fn(perms[ki])
	}
}

const letters = "abcdefghijklmnopqrstuvwxyz"

// randString generates a random string with a target length using characters
// from the input alphabet string.
func (s *Smither) randString(length int, alphabet string) string {
	buf := make([]byte, length)
	for i := range buf {
		buf[i] = alphabet[s.rnd.Intn(len(alphabet))]
	}
	return string(buf)
}
