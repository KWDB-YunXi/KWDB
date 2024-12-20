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

package ts

import "gitee.com/kwbasedb/kwbase/pkg/util/metric"

var (
	// Storage metrics.
	metaWriteSamples = metric.Metadata{
		Name:        "timeseries.write.samples",
		Help:        "Total number of metric samples written to disk",
		Measurement: "Metric Samples",
		Unit:        metric.Unit_COUNT,
	}
	metaWriteBytes = metric.Metadata{
		Name:        "timeseries.write.bytes",
		Help:        "Total size in bytes of metric samples written to disk",
		Measurement: "Storage",
		Unit:        metric.Unit_BYTES,
	}
	metaWriteErrors = metric.Metadata{
		Name:        "timeseries.write.errors",
		Help:        "Total errors encountered while attempting to write metrics to disk",
		Measurement: "Errors",
		Unit:        metric.Unit_COUNT,
	}
)

// TimeSeriesMetrics contains metrics relevant to the time series system.
type TimeSeriesMetrics struct {
	WriteSamples *metric.Counter
	WriteBytes   *metric.Counter
	WriteErrors  *metric.Counter
}

// NewTimeSeriesMetrics creates a new instance of TimeSeriesMetrics.
func NewTimeSeriesMetrics() *TimeSeriesMetrics {
	return &TimeSeriesMetrics{
		WriteSamples: metric.NewCounter(metaWriteSamples),
		WriteBytes:   metric.NewCounter(metaWriteBytes),
		WriteErrors:  metric.NewCounter(metaWriteErrors),
	}
}
