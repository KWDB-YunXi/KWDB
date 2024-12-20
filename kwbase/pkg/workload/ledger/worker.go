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

package ledger

import (
	"context"
	gosql "database/sql"
	"math/rand"
	"strconv"
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"gitee.com/kwbasedb/kwbase/pkg/workload/histogram"
	"github.com/cockroachdb/errors"
)

type worker struct {
	config *ledger
	hists  *histogram.Histograms
	db     *gosql.DB

	rng      *rand.Rand
	deckPerm []int
	permIdx  int
}

type ledgerTx interface {
	run(config *ledger, db *gosql.DB, rng *rand.Rand) (interface{}, error)
}

type tx struct {
	ledgerTx
	weight int    // percent likelihood that each transaction type is run
	name   string // display name
}

var allTxs = [...]tx{
	{
		ledgerTx: balance{}, name: "balance",
	},
	{
		ledgerTx: withdrawal{}, name: "withdrawal",
	},
	{
		ledgerTx: deposit{}, name: "deposit",
	},
	{
		ledgerTx: reversal{}, name: "reversal",
	},
}

func initializeMix(config *ledger) error {
	config.txs = append([]tx(nil), allTxs[0:]...)
	nameToTx := make(map[string]int, len(allTxs))
	for i, tx := range config.txs {
		nameToTx[tx.name] = i
	}

	items := strings.Split(config.mix, `,`)
	totalWeight := 0
	for _, item := range items {
		kv := strings.Split(item, `=`)
		if len(kv) != 2 {
			return errors.Errorf(`Invalid mix %s: %s is not a k=v pair`, config.mix, item)
		}
		txName, weightStr := kv[0], kv[1]

		weight, err := strconv.Atoi(weightStr)
		if err != nil {
			return errors.Errorf(
				`Invalid percentage mix %s: %s is not an integer`, config.mix, weightStr)
		}

		i, ok := nameToTx[txName]
		if !ok {
			return errors.Errorf(
				`Invalid percentage mix %s: no such transaction %s`, config.mix, txName)
		}

		config.txs[i].weight = weight
		totalWeight += weight
	}

	config.deck = make([]int, 0, totalWeight)
	for i, t := range config.txs {
		for j := 0; j < t.weight; j++ {
			config.deck = append(config.deck, i)
		}
	}

	return nil
}

func (w *worker) run(ctx context.Context) error {
	if w.permIdx == len(w.deckPerm) {
		rand.Shuffle(len(w.deckPerm), func(i, j int) {
			w.deckPerm[i], w.deckPerm[j] = w.deckPerm[j], w.deckPerm[i]
		})
		w.permIdx = 0
	}
	// Move through our permutation slice until its exhausted, using each value to
	// to index into our deck of transactions, which contains indexes into the
	// txs slice.
	opIdx := w.deckPerm[w.permIdx]
	t := w.config.txs[opIdx]
	w.permIdx++

	start := timeutil.Now()
	if _, err := t.run(w.config, w.db, w.rng); err != nil {
		return errors.Wrapf(err, "error in %s", t.name)
	}
	elapsed := timeutil.Since(start)
	w.hists.Get(t.name).Record(elapsed)
	return nil
}
