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

package main

import (
	"context"
	"net/http"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/ts/tspb"
	"gitee.com/kwbasedb/kwbase/pkg/util/httputil"
)

// tsQueryType represents the type of the time series query to retrieve. In
// most cases, tests are verifying either the "total" or "rate" metrics, so
// this enum type simplifies the API of tspb.Query.
type tsQueryType int

const (
	// total indicates to query the total of the metric. Specifically,
	// downsampler will be average, aggregator will be sum, and derivative will
	// be none.
	total tsQueryType = iota
	// rate indicates to query the rate of change of the metric. Specifically,
	// downsampler will be average, aggregator will be sum, and derivative will
	// be non-negative derivative.
	rate
)

type tsQuery struct {
	name      string
	queryType tsQueryType
}

func mustGetMetrics(
	t *test, adminURL string, start, end time.Time, tsQueries []tsQuery,
) tspb.TimeSeriesQueryResponse {
	response, err := getMetrics(adminURL, start, end, tsQueries)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func getMetrics(
	adminURL string, start, end time.Time, tsQueries []tsQuery,
) (tspb.TimeSeriesQueryResponse, error) {
	url := "http://" + adminURL + "/ts/query"
	queries := make([]tspb.Query, len(tsQueries))
	for i := 0; i < len(tsQueries); i++ {
		switch tsQueries[i].queryType {
		case total:
			queries[i] = tspb.Query{
				Name:             tsQueries[i].name,
				Downsampler:      tspb.TimeSeriesQueryAggregator_AVG.Enum(),
				SourceAggregator: tspb.TimeSeriesQueryAggregator_SUM.Enum(),
			}
		case rate:
			queries[i] = tspb.Query{
				Name:             tsQueries[i].name,
				Downsampler:      tspb.TimeSeriesQueryAggregator_AVG.Enum(),
				SourceAggregator: tspb.TimeSeriesQueryAggregator_SUM.Enum(),
				Derivative:       tspb.TimeSeriesQueryDerivative_NON_NEGATIVE_DERIVATIVE.Enum(),
			}
		default:
			panic("unexpected")
		}
	}
	request := tspb.TimeSeriesQueryRequest{
		StartNanos: start.UnixNano(),
		EndNanos:   end.UnixNano(),
		// Ask for one minute intervals. We can't just ask for the whole hour
		// because the time series query system does not support downsampling
		// offsets.
		SampleNanos: (1 * time.Minute).Nanoseconds(),
		Queries:     queries,
	}
	var response tspb.TimeSeriesQueryResponse
	err := httputil.PostJSON(http.Client{Timeout: 500 * time.Millisecond}, url, &request, &response)
	return response, err

}

func verifyTxnPerSecond(
	ctx context.Context,
	c *cluster,
	t *test,
	adminNode nodeListOption,
	start, end time.Time,
	txnTarget, maxPercentTimeUnderTarget float64,
) {
	// Query needed information over the timespan of the query.
	adminURL := c.ExternalAdminUIAddr(ctx, adminNode)[0]
	response := mustGetMetrics(t, adminURL, start, end, []tsQuery{
		{name: "cr.node.txn.commits", queryType: rate},
		{name: "cr.node.txn.commits", queryType: total},
	})

	// Drop the first two minutes of datapoints as a "ramp-up" period.
	perMinute := response.Results[0].Datapoints[2:]
	cumulative := response.Results[1].Datapoints[2:]

	// Check average txns per second over the entire test was above the target.
	totalTxns := cumulative[len(cumulative)-1].Value - cumulative[0].Value
	avgTxnPerSec := totalTxns / float64(end.Sub(start)/time.Second)

	if avgTxnPerSec < txnTarget {
		t.Fatalf("average txns per second %f was under target %f", avgTxnPerSec, txnTarget)
	} else {
		t.l.Printf("average txns per second: %f", avgTxnPerSec)
	}

	// Verify that less than the specified limit of each individual one minute
	// period was underneath the target.
	minutesBelowTarget := 0.0
	for _, dp := range perMinute {
		if dp.Value < txnTarget {
			minutesBelowTarget++
		}
	}
	if perc := minutesBelowTarget / float64(len(perMinute)); perc > maxPercentTimeUnderTarget {
		t.Fatalf(
			"spent %f%% of time below target of %f txn/s, wanted no more than %f%%",
			perc*100, txnTarget, maxPercentTimeUnderTarget*100,
		)
	} else {
		t.l.Printf("spent %f%% of time below target of %f txn/s", perc*100, txnTarget)
	}
}

func verifyLookupsPerSec(
	ctx context.Context,
	c *cluster,
	t *test,
	adminNode nodeListOption,
	start, end time.Time,
	rangeLookupsTarget float64,
) {
	// Query needed information over the timespan of the query.
	adminURL := c.ExternalAdminUIAddr(ctx, adminNode)[0]
	response := mustGetMetrics(t, adminURL, start, end, []tsQuery{
		{name: "cr.node.distsender.rangelookups", queryType: rate},
	})

	// Drop the first two minutes of datapoints as a "ramp-up" period.
	perMinute := response.Results[0].Datapoints[2:]

	// Verify that each individual one minute periods were below the target.
	for _, dp := range perMinute {
		if dp.Value > rangeLookupsTarget {
			t.Fatalf("Found minute interval with %f lookup/sec above target of %f lookup/sec\n", dp.Value, rangeLookupsTarget)
		} else {
			t.l.Printf("Found minute interval with %f lookup/sec\n", dp.Value)
		}
	}
}
