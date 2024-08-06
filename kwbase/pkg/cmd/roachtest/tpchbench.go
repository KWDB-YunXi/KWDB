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

package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/httputil"
	"gitee.com/kwbasedb/kwbase/pkg/workload/querybench"
)

type tpchBenchSpec struct {
	Nodes           int
	CPUs            int
	ScaleFactor     int
	benchType       string
	url             string
	numRunsPerQuery int
	// minVersion specifies the minimum version of KWDB nodes. If omitted, it
	// will default to v19.1.0.
	minVersion string
	// maxLatency is the expected maximum time that a query will take to execute
	// needed to correctly initialize histograms.
	maxLatency time.Duration
}

// runTPCHBench runs sets of queries against CockroachDB clusters in different
// configurations.
//
// In order to run a benchmark, a TPC-H dataset must first be loaded. To reuse
// this data across runs, it is recommended to use a combination of
// `--cluster=<cluster>` and `--wipe=false` flags to limit the loading phase to
// the first run.
//
// This benchmark runs with a single load generator node running a single
// worker.
func runTPCHBench(ctx context.Context, t *test, c *cluster, b tpchBenchSpec) {
	roachNodes := c.Range(1, c.spec.NodeCount-1)
	loadNode := c.Node(c.spec.NodeCount)

	t.Status("copying binaries")
	c.Put(ctx, kwbase, "./kwbase", roachNodes)
	c.Put(ctx, workload, "./workload", loadNode)

	filename := b.benchType
	t.Status(fmt.Sprintf("downloading %s query file from %s", filename, b.url))
	if err := c.RunE(ctx, loadNode, fmt.Sprintf("curl %s > %s", b.url, filename)); err != nil {
		t.Fatal(err)
	}

	t.Status("starting nodes")
	c.Start(ctx, t, roachNodes)

	m := newMonitor(ctx, c, roachNodes)
	m.Go(func(ctx context.Context) error {
		t.Status("setting up dataset")
		err := loadTPCHDataset(ctx, t, c, b.ScaleFactor, m, roachNodes)
		if err != nil {
			return err
		}

		t.l.Printf("running %s benchmark on tpch scale-factor=%d", filename, b.ScaleFactor)

		numQueries, err := getNumQueriesInFile(filename, b.url)
		if err != nil {
			t.Fatal(err)
		}
		// maxOps flag will allow us to exit the workload once all the queries were
		// run b.numRunsPerQuery number of times.
		maxOps := b.numRunsPerQuery * numQueries

		// Run with only one worker to get best-case single-query performance.
		cmd := fmt.Sprintf(
			"./workload run querybench --db=tpch --concurrency=1 --query-file=%s "+
				"--num-runs=%d --max-ops=%d {pgurl%s} "+
				"--histograms="+perfArtifactsDir+"/stats.json --histograms-max-latency=%s",
			filename,
			b.numRunsPerQuery,
			maxOps,
			roachNodes,
			b.maxLatency.String(),
		)
		if err := c.RunE(ctx, loadNode, cmd); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	m.Wait()
}

// getNumQueriesInFile downloads a file that url points to, stores it at a
// temporary location, parses it using querybench, and deletes the file. It
// returns the number of queries in the file.
func getNumQueriesInFile(filename, url string) (int, error) {
	tempFile, err := downloadFile(filename, url)
	if err != nil {
		return 0, err
	}
	// Use closure to make linter happy about unchecked error.
	defer func() {
		_ = os.Remove(tempFile.Name())
	}()

	queries, err := querybench.GetQueries(tempFile.Name())
	if err != nil {
		return 0, err
	}
	return len(queries), nil
}

// downloadFile will download a url as a local temporary file.
func downloadFile(filename string, url string) (*os.File, error) {
	// These files may be a bit large, so give ourselves
	// some room before the timeout expires.
	httpClient := httputil.NewClientWithTimeout(30 * time.Second)
	// Get the data.
	resp, err := httpClient.Get(context.TODO(), url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Create the file.
	out, err := ioutil.TempFile(`` /* dir */, filename)
	if err != nil {
		return nil, err
	}
	defer out.Close()

	// Write the body to file.
	_, err = io.Copy(out, resp.Body)
	return out, err
}

func registerTPCHBenchSpec(r *testRegistry, b tpchBenchSpec) {
	nameParts := []string{
		"tpchbench",
		b.benchType,
		fmt.Sprintf("nodes=%d", b.Nodes),
		fmt.Sprintf("cpu=%d", b.CPUs),
		fmt.Sprintf("sf=%d", b.ScaleFactor),
	}

	// Add a load generator node.
	numNodes := b.Nodes + 1
	minVersion := b.minVersion
	if minVersion == `` {
		minVersion = "v19.1.0" // needed for import
	}

	r.Add(testSpec{
		Name:       strings.Join(nameParts, "/"),
		Owner:      OwnerSQLExec,
		Cluster:    makeClusterSpec(numNodes),
		MinVersion: minVersion,
		Run: func(ctx context.Context, t *test, c *cluster) {
			runTPCHBench(ctx, t, c, b)
		},
	})
}

func registerTPCHBench(r *testRegistry) {
	specs := []tpchBenchSpec{
		{
			Nodes:           3,
			CPUs:            4,
			ScaleFactor:     1,
			benchType:       `sql20`,
			url:             `https://raw.githubusercontent.com/kwbasedb/kwbase/master/pkg/workload/querybench/2.1-sql-20`,
			numRunsPerQuery: 3,
			maxLatency:      100 * time.Second,
		},
		{
			Nodes:           3,
			CPUs:            4,
			ScaleFactor:     1,
			benchType:       `tpch`,
			url:             `https://raw.githubusercontent.com/kwbasedb/kwbase/master/pkg/workload/querybench/tpch-queries`,
			numRunsPerQuery: 3,
			minVersion:      `v19.2.0`,
			maxLatency:      500 * time.Second,
		},
	}

	for _, b := range specs {
		registerTPCHBenchSpec(r, b)
	}
}
