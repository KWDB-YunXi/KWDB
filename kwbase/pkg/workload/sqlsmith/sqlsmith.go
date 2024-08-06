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

package sqlsmith

import (
	"context"
	gosql "database/sql"
	"math/rand"
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/internal/sqlsmith"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"gitee.com/kwbasedb/kwbase/pkg/workload"
	"gitee.com/kwbasedb/kwbase/pkg/workload/histogram"
	"github.com/cockroachdb/errors"
	"github.com/spf13/pflag"
)

type sqlSmith struct {
	flags     workload.Flags
	connFlags *workload.ConnFlags

	seed          int64
	tables        int
	errorSettings int
}

type errorSettingTypes int

const (
	ignoreExecErrors errorSettingTypes = iota
	returnOnInternalError
	returnOnError
)

func init() {
	workload.Register(sqlSmithMeta)
}

var sqlSmithMeta = workload.Meta{
	Name:        `sqlsmith`,
	Description: `sqlsmith is a random SQL query generator`,
	Version:     `1.0.0`,
	New: func() workload.Generator {
		g := &sqlSmith{}
		g.flags.FlagSet = pflag.NewFlagSet(`sqlsmith`, pflag.ContinueOnError)
		g.flags.Int64Var(&g.seed, `seed`, 1, `Key hash seed.`)
		g.flags.IntVar(&g.tables, `tables`, 1, `Number of tables.`)
		g.flags.IntVar(&g.errorSettings, `error-sensitivity`, 0,
			`SQLSmith's sensitivity to errors. 0=ignore all errors. 1=quit on internal errors. 2=quit on any error.`)
		g.connFlags = workload.NewConnFlags(&g.flags)
		return g
	},
}

// Meta implements the Generator interface.
func (*sqlSmith) Meta() workload.Meta { return sqlSmithMeta }

// Flags implements the Flagser interface.
func (g *sqlSmith) Flags() workload.Flags { return g.flags }

// Tables implements the Generator interface.
func (g *sqlSmith) Tables() []workload.Table {
	rng := rand.New(rand.NewSource(g.seed))
	var tables []workload.Table
	for idx := 0; idx < g.tables; idx++ {
		schema := sqlbase.RandCreateTable(rng, "table", idx)
		table := workload.Table{
			Name:   schema.Table.String(),
			Schema: tree.Serialize(schema),
		}
		// workload expects the schema to be missing the CREATE TABLE "name", so
		// drop everything before the first `(`.
		table.Schema = table.Schema[strings.Index(table.Schema, `(`):]
		tables = append(tables, table)
	}
	return tables
}

func (g *sqlSmith) handleError(err error) error {
	if err != nil {
		switch errorSettingTypes(g.errorSettings) {
		case ignoreExecErrors:
			return nil
		case returnOnInternalError:
			if strings.Contains(err.Error(), "internal error") {
				return err
			}
		case returnOnError:
			return err
		}
	}
	return nil
}

func (g *sqlSmith) validateErrorSetting() error {
	switch errorSettingTypes(g.errorSettings) {
	case ignoreExecErrors:
	case returnOnInternalError:
	case returnOnError:
	default:
		return errors.Newf("invalid value for error-sensitivity: %d", g.errorSettings)
	}
	return nil
}

// Ops implements the Opser interface.
func (g *sqlSmith) Ops(
	ctx context.Context, urls []string, reg *histogram.Registry,
) (workload.QueryLoad, error) {
	if err := g.validateErrorSetting(); err != nil {
		return workload.QueryLoad{}, err
	}
	sqlDatabase, err := workload.SanitizeUrls(g, g.connFlags.DBOverride, urls)
	if err != nil {
		return workload.QueryLoad{}, err
	}
	db, err := gosql.Open(`kwbase`, strings.Join(urls, ` `))
	if err != nil {
		return workload.QueryLoad{}, err
	}
	// Allow a maximum of concurrency+1 connections to the database.
	db.SetMaxOpenConns(g.connFlags.Concurrency + 1)
	db.SetMaxIdleConns(g.connFlags.Concurrency + 1)

	ql := workload.QueryLoad{SQLDatabase: sqlDatabase}
	for i := 0; i < g.connFlags.Concurrency; i++ {
		rng := rand.New(rand.NewSource(g.seed + int64(i)))
		smither, err := sqlsmith.NewSmither(db, rng)
		if err != nil {
			return workload.QueryLoad{}, err
		}

		hists := reg.GetHandle()
		workerFn := func(ctx context.Context) error {
			start := timeutil.Now()
			query := smither.Generate()
			elapsed := timeutil.Since(start)
			hists.Get(`generate`).Record(elapsed)

			start = timeutil.Now()
			_, err := db.ExecContext(ctx, query)
			if handledErr := g.handleError(err); handledErr != nil {
				return handledErr
			}
			elapsed = timeutil.Since(start)

			hists.Get(`exec`).Record(elapsed)

			return nil
		}
		ql.WorkerFns = append(ql.WorkerFns, workerFn)
	}
	return ql, nil
}
