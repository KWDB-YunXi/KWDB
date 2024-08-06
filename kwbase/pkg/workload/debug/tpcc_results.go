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

package debug

import (
	"fmt"
	"sort"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/workload/histogram"
	"gitee.com/kwbasedb/kwbase/pkg/workload/tpcc"
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
)

var tpccMergeResultsCmd = &cobra.Command{
	Use: "tpcc-merge-results [<hist-file> [<hist-file>...]]",
	Short: "tpcc-merge-results merges the histograms from parallel runs of " +
		"TPC-C to compute a combined result.",
	RunE: tpccMergeResults,
	Args: cobra.MinimumNArgs(1),
}

func init() {
	flags := tpccMergeResultsCmd.Flags()
	flags.Int("warehouses", 0, "number of aggregate warehouses in all of the histograms")
}

func tpccMergeResults(cmd *cobra.Command, args []string) error {
	// We want to take histograms and merge them
	warehouses, err := cmd.Flags().GetInt("warehouses")
	if err != nil {
		return errors.Wrap(err, "no warehouses flag found")
	}
	var results []*tpcc.Result

	for _, fname := range args {
		snapshots, err := histogram.DecodeSnapshots(fname)
		if err != nil {
			return errors.Wrapf(err, "failed to decode histograms at %q", fname)
		}
		results = append(results, tpcc.NewResultWithSnapshots(warehouses, 0, snapshots))
	}

	res := tpcc.MergeResults(results...)
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Duration: %.5v, Warehouses: %v, Efficiency: %.4v, tpmC: %.2f\n",
		res.Elapsed, res.ActiveWarehouses, res.Efficiency(), res.TpmC())
	_, _ = fmt.Fprintf(out, "_elapsed___ops/sec(cum)__p50(ms)__p90(ms)__p95(ms)__p99(ms)_pMax(ms)\n")

	var queries []string
	for query := range res.Cumulative {
		queries = append(queries, query)
	}
	sort.Strings(queries)
	for _, query := range queries {
		hist := res.Cumulative[query]
		_, _ = fmt.Fprintf(out, "%7.1fs %14.1f %8.1f %8.1f %8.1f %8.1f %8.1f %s\n",
			res.Elapsed.Seconds(),
			float64(hist.TotalCount())/res.Elapsed.Seconds(),
			time.Duration(hist.ValueAtQuantile(50)).Seconds()*1000,
			time.Duration(hist.ValueAtQuantile(90)).Seconds()*1000,
			time.Duration(hist.ValueAtQuantile(95)).Seconds()*1000,
			time.Duration(hist.ValueAtQuantile(99)).Seconds()*1000,
			time.Duration(hist.ValueAtQuantile(100)).Seconds()*1000,
			query,
		)
	}
	return nil
}
