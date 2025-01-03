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

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/cmd/smithcmp/cmpconn"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"gitee.com/kwbasedb/kwbase/pkg/workload/tpcds"
	"github.com/cockroachdb/errors"
)

func registerTPCDSVec(r *testRegistry) {
	const (
		timeout                         = 5 * time.Minute
		withStatsSlowerWarningThreshold = 1.25
	)

	queriesToSkip := map[int]bool{
		// The plans for these queries contain processors with
		// core.LocalPlanNode which currently cannot be wrapped by the
		// vectorized engine, so 'vectorize' session variable will make no
		// difference.
		1:  true,
		2:  true,
		4:  true,
		11: true,
		23: true,
		24: true,
		30: true,
		31: true,
		39: true,
		45: true,
		47: true,
		57: true,
		59: true,
		64: true,
		74: true,
		75: true,
		81: true,
		95: true,

		// These queries contain unsupported function 'rollup' (#46280).
		5:  true,
		14: true,
		18: true,
		22: true,
		67: true,
		77: true,
		80: true,
	}

	queriesToSkip20_1 := map[int]bool{
		// These queries do not finish in 5 minutes on 20.1 branch.
		7:  true,
		13: true,
		17: true,
		19: true,
		25: true,
		26: true,
		29: true,
		//45: true,
		46: true,
		48: true,
		50: true,
		61: true,
		//64: true,
		66: true,
		68: true,
		72: true,
		84: true,
		85: true,
	}

	tpcdsTables := []string{
		`call_center`, `catalog_page`, `catalog_returns`, `catalog_sales`,
		`customer`, `customer_address`, `customer_demographics`, `date_dim`,
		`dbgen_version`, `household_demographics`, `income_band`, `inventory`,
		`item`, `promotion`, `reason`, `ship_mode`, `store`, `store_returns`,
		`store_sales`, `time_dim`, `warehouse`, `web_page`, `web_returns`,
		`web_sales`, `web_site`,
	}

	runTPCDSVec := func(ctx context.Context, t *test, c *cluster) {
		c.Put(ctx, kwbase, "./kwbase", c.All())
		c.Start(ctx, t)

		clusterConn := c.Conn(ctx, 1)
		disableAutoStats(t, clusterConn)
		disableVectorizeRowCountThresholdHeuristic(t, clusterConn)
		t.Status("restoring TPCDS dataset for Scale Factor 1")
		if _, err := clusterConn.Exec(
			`RESTORE DATABASE tpcds FROM 'gs://kwbase-fixtures/workload/tpcds/scalefactor=1/backup';`,
		); err != nil {
			t.Fatal(err)
		}

		if _, err := clusterConn.Exec("USE tpcds;"); err != nil {
			t.Fatal(err)
		}
		scatterTables(t, clusterConn, tpcdsTables)
		t.Status("waiting for full replication")
		waitForFullReplication(t, clusterConn)
		versionString, err := fetchCockroachVersion(ctx, c, c.Node(1)[0])
		if err != nil {
			t.Fatal(err)
		}

		// TODO(yuzefovich): it seems like if cmpconn.CompareConns hits a
		// timeout, the query actually keeps on going and the connection
		// becomes kinda stale. To go around it, we set a statement timeout
		// variable on the connections and pass in 3 x timeout into
		// CompareConns hoping that the session variable is better respected.
		// We additionally open fresh connections for each query.
		setStmtTimeout := fmt.Sprintf("SET statement_timeout='%s';", timeout)
		firstNode := c.Node(1)
		firstNodeURL := c.ExternalPGUrl(ctx, firstNode)[0]
		openNewConnections := func() (map[string]*cmpconn.Conn, func()) {
			conns := map[string]*cmpconn.Conn{}
			vecOffConn, err := cmpconn.NewConn(
				firstNodeURL, nil, nil, setStmtTimeout+"SET vectorize=off; USE tpcds;",
			)
			if err != nil {
				t.Fatal(err)
			}
			conns["vectorize=OFF"] = vecOffConn
			vecOnConn, err := cmpconn.NewConn(
				firstNodeURL, nil, nil, setStmtTimeout+"SET vectorize=on; USE tpcds;",
			)
			if err != nil {
				t.Fatal(err)
			}
			conns["vectorize=ON"] = vecOnConn
			// A sanity check that we have different values of 'vectorize'
			// session variable on two connections and that the comparator will
			// emit an error because of that difference.
			if err := cmpconn.CompareConns(ctx, timeout, conns, "", "SHOW vectorize;"); err == nil {
				t.Fatal("unexpectedly SHOW vectorize didn't trigger an error on comparison")
			}
			return conns, func() {
				vecOffConn.Close()
				vecOnConn.Close()
			}
		}

		noStatsRunTimes := make(map[int]float64)
		var errToReport error
		// We will run all queries in two scenarios: without stats and with
		// auto stats. The idea is that the plans are likely to be different,
		// so we will be testing different execution scenarios. We additionally
		// will compare the queries' run times in both scenarios and print out
		// warnings when in presence of stats we seem to be choosing worse
		// plans.
		for _, haveStats := range []bool{false, true} {
			for queryNum := 1; queryNum <= tpcds.NumQueries; queryNum++ {
				if _, toSkip := queriesToSkip[queryNum]; toSkip {
					continue
				}
				if strings.HasPrefix(versionString, "v20.1") {
					if _, toSkip := queriesToSkip20_1[queryNum]; toSkip {
						continue
					}
				}
				query, ok := tpcds.QueriesByNumber[queryNum]
				if !ok {
					continue
				}
				t.Status(fmt.Sprintf("running query %d\n", queryNum))
				// We will be opening fresh connections for every query to go
				// around issues with cancellation.
				conns, cleanup := openNewConnections()
				start := timeutil.Now()
				if err := cmpconn.CompareConns(
					ctx, 3*timeout, conns, "", query); err != nil {
					t.Status(fmt.Sprintf("encountered an error: %s\n", err))
					errToReport = errors.CombineErrors(errToReport, err)
				} else {
					runTimeInSeconds := timeutil.Since(start).Seconds()
					t.Status(
						fmt.Sprintf("[q%d] took about %.2fs to run on both configs",
							queryNum, runTimeInSeconds),
					)
					if haveStats {
						noStatsRunTime, ok := noStatsRunTimes[queryNum]
						if ok && noStatsRunTime*withStatsSlowerWarningThreshold < runTimeInSeconds {
							t.Status(fmt.Sprintf("WARNING: suboptimal plan when stats are present\n"+
								"no stats: %.2fs\twith stats: %.2fs", noStatsRunTime, runTimeInSeconds))
						}
					} else {
						noStatsRunTimes[queryNum] = runTimeInSeconds
					}
				}
				cleanup()
			}

			if !haveStats {
				createStatsFromTables(t, clusterConn, tpcdsTables)
			}
		}
		if errToReport != nil {
			t.Fatal(errToReport)
		}
	}

	r.Add(testSpec{
		Name:       "tpcdsvec",
		Owner:      OwnerSQLExec,
		Cluster:    makeClusterSpec(3),
		MinVersion: "v20.1.0",
		Run: func(ctx context.Context, t *test, c *cluster) {
			runTPCDSVec(ctx, t, c)
		},
	})
}
