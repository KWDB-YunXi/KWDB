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

package ptreconcile

import (
	"gitee.com/kwbasedb/kwbase/pkg/util/metric"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

// Metrics encapsulates the metrics exported by the Reconciler.
type Metrics struct {
	ReconcilationRuns    *metric.Counter
	RecordsProcessed     *metric.Counter
	RecordsRemoved       *metric.Counter
	ReconciliationErrors *metric.Counter
}

func makeMetrics() Metrics {
	return Metrics{
		ReconcilationRuns:    metric.NewCounter(metaReconciliationRuns),
		RecordsProcessed:     metric.NewCounter(metaRecordsProcessed),
		RecordsRemoved:       metric.NewCounter(metaRecordsRemoved),
		ReconciliationErrors: metric.NewCounter(metaReconciliationErrors),
	}
}

var _ metric.Struct = (*Metrics)(nil)

// MetricStruct makes Metrics a metric.Struct.
func (m *Metrics) MetricStruct() {}

var (
	metaReconciliationRuns = metric.Metadata{
		Name:        "kv.protectedts.reconciliation.num_runs",
		Help:        "number of successful reconciliation runs on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
	metaRecordsProcessed = metric.Metadata{
		Name:        "kv.protectedts.reconciliation.records_processed",
		Help:        "number of records processed without error during reconciliation on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
	metaRecordsRemoved = metric.Metadata{
		Name:        "kv.protectedts.reconciliation.records_removed",
		Help:        "number of records removed during reconciliation runs on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
	metaReconciliationErrors = metric.Metadata{
		Name:        "kv.protectedts.reconciliation.errors",
		Help:        "number of errors encountered during reconciliation runs on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
)
