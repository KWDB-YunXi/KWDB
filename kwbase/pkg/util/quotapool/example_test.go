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

package quotapool

import (
	"context"
	"sort"

	"gitee.com/kwbasedb/kwbase/pkg/util/ctxgroup"
)

// An example use case for AcquireFunc is a pool of workers attempting to
// acquire resources to run a heterogenous set of jobs. Imagine for example we
// have a set of workers and a list of jobs which need to be run. The function
// might be used to choose the largest job which can be run by the existing
// quantity of quota.
func ExampleIntPool_AcquireFunc() {
	const quota = 7
	const workers = 3
	qp := NewIntPool("work units", quota)
	type job struct {
		name string
		cost uint64
	}
	jobs := []*job{
		{name: "foo", cost: 3},
		{name: "bar", cost: 2},
		{name: "baz", cost: 4},
		{name: "qux", cost: 6},
		{name: "quux", cost: 3},
		{name: "quuz", cost: 3},
	}
	// sortJobs sorts the jobs in highest-to-lowest order with nil last.
	sortJobs := func() {
		sort.Slice(jobs, func(i, j int) bool {
			ij, jj := jobs[i], jobs[j]
			if ij != nil && jj != nil {
				return ij.cost > jj.cost
			}
			return ij != nil
		})
	}
	// getJob finds the largest job which can be run with the current quota.
	getJob := func(
		ctx context.Context, qp *IntPool,
	) (j *job, alloc *IntAlloc, err error) {
		alloc, err = qp.AcquireFunc(ctx, func(
			ctx context.Context, pi PoolInfo,
		) (took uint64, err error) {
			sortJobs()
			// There are no more jobs, take 0 and return.
			if jobs[0] == nil {
				return 0, nil
			}
			// Find the largest jobs which can be run.
			for i := range jobs {
				if jobs[i] == nil {
					break
				}
				if jobs[i].cost <= pi.Available {
					j, jobs[i] = jobs[i], nil
					return j.cost, nil
				}
			}
			return 0, ErrNotEnoughQuota
		})
		return j, alloc, err
	}
	runWorker := func(workerNum int) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			for {
				j, alloc, err := getJob(ctx, qp)
				if err != nil {
					return err
				}
				if j == nil {
					return nil
				}
				alloc.Release()
			}
		}
	}
	g := ctxgroup.WithContext(context.Background())
	for i := 0; i < workers; i++ {
		g.GoCtx(runWorker(i))
	}
	if err := g.Wait(); err != nil {
		panic(err)
	}
}
