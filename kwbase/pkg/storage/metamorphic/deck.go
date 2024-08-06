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

package metamorphic

import (
	"math/rand"

	"gitee.com/kwbasedb/kwbase/pkg/util/syncutil"
)

// deck is a random number generator that generates numbers in the range
// [0,len(weights)-1] where the probability of i is
// weights(i)/sum(weights). Unlike Weighted, the weights are specified as
// integers and used in a deck-of-cards style random number selection which
// ensures that each element is returned with a desired frequency within the
// size of the deck.
type deck struct {
	rng *rand.Rand
	mu  struct {
		syncutil.Mutex
		index int
		deck  []int
	}
}

// newDeck returns a new deck random number generator.
func newDeck(rng *rand.Rand, weights ...int) *deck {
	var sum int
	for i := range weights {
		sum += weights[i]
	}
	expandedDeck := make([]int, 0, sum)
	for i := range weights {
		for j := 0; j < weights[i]; j++ {
			expandedDeck = append(expandedDeck, i)
		}
	}
	d := &deck{
		rng: rng,
	}
	d.mu.index = len(expandedDeck)
	d.mu.deck = expandedDeck
	return d
}

// Int returns a random number in the range [0,len(weights)-1] where the
// probability of i is weights(i)/sum(weights).
func (d *deck) Int() int {
	d.mu.Lock()
	if d.mu.index == len(d.mu.deck) {
		d.rng.Shuffle(len(d.mu.deck), func(i, j int) {
			d.mu.deck[i], d.mu.deck[j] = d.mu.deck[j], d.mu.deck[i]
		})
		d.mu.index = 0
	}
	result := d.mu.deck[d.mu.index]
	d.mu.index++
	d.mu.Unlock()
	return result
}
