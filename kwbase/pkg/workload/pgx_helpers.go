// Copyright 2018 The Cockroach Authors.
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

package workload

import (
	"context"
	gosql "database/sql"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/cockroach-go/crdb"
	"github.com/jackc/pgx"
	"golang.org/x/sync/errgroup"
)

// MultiConnPool maintains a set of pgx ConnPools (to different servers).
type MultiConnPool struct {
	Pools []*pgx.ConnPool
	// Atomic counter used by Get().
	counter uint32
}

// MultiConnPoolCfg encapsulates the knobs passed to NewMultiConnPool.
type MultiConnPoolCfg struct {
	// MaxTotalConnections is the total maximum number of connections across all
	// pools.
	MaxTotalConnections int

	// MaxConnsPerPool is the maximum number of connections in any single pool.
	// Limiting this is useful especially for prepared statements, which are
	// prepared on each connection inside a pool (serially).
	// If 0, there is no per-pool maximum (other than the total maximum number of
	// connections which still applies).
	MaxConnsPerPool int
}

// NewMultiConnPool creates a new MultiConnPool.
//
// Each URL gets one or more pools, and each pool has at most MaxConnsPerPool
// connections.
//
// The pools have approximately the same number of max connections, adding up to
// MaxTotalConnections.
func NewMultiConnPool(cfg MultiConnPoolCfg, urls ...string) (*MultiConnPool, error) {
	m := &MultiConnPool{}
	connsPerURL := distribute(cfg.MaxTotalConnections, len(urls))
	maxConnsPerPool := cfg.MaxConnsPerPool
	if maxConnsPerPool == 0 {
		maxConnsPerPool = cfg.MaxTotalConnections
	}

	var warmupConns [][]*pgx.Conn
	for i := range urls {
		connCfg, err := pgx.ParseConnectionString(urls[i])
		if err != nil {
			return nil, err
		}

		connsPerPool := distributeMax(connsPerURL[i], maxConnsPerPool)
		for _, numConns := range connsPerPool {
			p, err := pgx.NewConnPool(pgx.ConnPoolConfig{
				ConnConfig:     connCfg,
				MaxConnections: numConns,
			})
			if err != nil {
				return nil, err
			}

			warmupConns = append(warmupConns, make([]*pgx.Conn, numConns))
			m.Pools = append(m.Pools, p)
		}
	}

	// "Warm up" the pools so we don't have to establish connections later (which
	// would affect the observed latencies of the first requests, especially when
	// prepared statements are used). We do this by
	// acquiring connections (in parallel), then releasing them back to the
	// pool.
	var g errgroup.Group
	// Limit concurrent connection establishment. Allowing this to run
	// at maximum parallelism would trigger syn flood protection on the
	// host, which combined with any packet loss could cause Acquire to
	// return an error and fail the whole function. The value 100 is
	// chosen because it is less than the default value for SOMAXCONN
	// (128).
	sem := make(chan struct{}, 100)
	for i, p := range m.Pools {
		p := p
		conns := warmupConns[i]
		for j := range conns {
			j := j
			sem <- struct{}{}
			g.Go(func() error {
				var err error
				conns[j], err = p.Acquire()
				<-sem
				return err
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	for i, p := range m.Pools {
		for _, c := range warmupConns[i] {
			p.Release(c)
		}
	}

	return m, nil
}

// Get returns one of the pools, in round-robin manner.
func (m *MultiConnPool) Get() *pgx.ConnPool {
	if len(m.Pools) == 1 {
		return m.Pools[0]
	}
	i := atomic.AddUint32(&m.counter, 1) - 1
	return m.Pools[i%uint32(len(m.Pools))]
}

// PrepareEx prepares the given statement on all the pools.
func (m *MultiConnPool) PrepareEx(
	ctx context.Context, name, sql string, opts *pgx.PrepareExOptions,
) (*pgx.PreparedStatement, error) {
	var res *pgx.PreparedStatement
	var once sync.Once
	var g errgroup.Group
	for _, p := range m.Pools {
		p := p
		g.Go(func() error {
			ps, err := p.PrepareEx(ctx, name, sql, opts)
			if err == nil {
				// It doesn't matter which PreparedStatement we return, they should
				// contain the same information.
				once.Do(func() { res = ps })
			}
			return err
		})
	}
	err := g.Wait()
	return res, err
}

// Close closes all the pools.
func (m *MultiConnPool) Close() {
	for _, p := range m.Pools {
		p.Close()
	}
}

// PgxTx is a thin wrapper that implements the kwdb.Tx interface, allowing pgx
// transactions to be used with ExecuteInTx. The kwbase-go library has native
// support for pgx in kwdb/pgx, but only for pgx v4. KWDB is stuck for now using
// pgx v3, as v4 needs Go modules.
type PgxTx pgx.Tx

var _ kwdb.Tx = &PgxTx{}

// ExecContext is part of the kwdb.Tx interface.
func (tx *PgxTx) ExecContext(
	ctx context.Context, sql string, args ...interface{},
) (gosql.Result, error) {
	_, err := (*pgx.Tx)(tx).ExecEx(ctx, sql, nil /* QueryExOptions */, args...)
	// kwdb.ExecuteInTx doesn't actually care about the Result, just the error.
	return nil, err
}

// Commit is part of the kwdb.Tx interface.
func (tx *PgxTx) Commit() error {
	return (*pgx.Tx)(tx).Commit()
}

// Rollback is part of the kwdb.Tx interface.
func (tx *PgxTx) Rollback() error {
	return (*pgx.Tx)(tx).Rollback()
}

// distribute returns a slice of <num> integers that add up to <total> and are
// within +/-1 of each other.
func distribute(total, num int) []int {
	res := make([]int, num)
	for i := range res {
		// Use the average number of remaining connections.
		div := len(res) - i
		res[i] = (total + div/2) / div
		total -= res[i]
	}
	return res
}

// distributeMax returns a slice of integers that are at most `max` and add up
// to <total>. The slice is as short as possible and the values are within +/-1
// of each other.
func distributeMax(total, max int) []int {
	return distribute(total, (total+max-1)/max)
}
