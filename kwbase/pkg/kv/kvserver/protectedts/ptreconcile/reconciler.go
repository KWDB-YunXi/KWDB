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

// Package ptreconcile provides logic to reconcile protected timestamp records
// with state associated with their metadata.
package ptreconcile

import (
	"context"
	"math/rand"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/protectedts"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/protectedts/ptpb"
	"gitee.com/kwbasedb/kwbase/pkg/settings"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
)

// ReconcileInterval is the interval between two generations of the reports.
// When set to zero - disables the report generation.
var ReconcileInterval = settings.RegisterPublicNonNegativeDurationSetting(
	"kv.protectedts.reconciliation.interval",
	"the frequency for reconciling jobs with protected timestamp records",
	5*time.Minute,
)

// StatusFunc is used to check on the status of a Record based on its Meta
// field.
type StatusFunc func(
	ctx context.Context, txn *kv.Txn, meta []byte,
) (shouldRemove bool, _ error)

// StatusFuncs maps from MetaType to a StatusFunc.
type StatusFuncs map[string]StatusFunc

// Config configures a Reconciler.
type Config struct {
	Settings *cluster.Settings
	// Stores is used to ensure that we only run the reconciliation loop on
	Stores  *kvserver.Stores
	DB      *kv.DB
	Storage protectedts.Storage
	Cache   protectedts.Cache

	// We want a map from metaType to a function which determines whether we
	// should clean it up.
	StatusFuncs StatusFuncs
}

// Reconciler runs an a loop to reconcile the protected timestamps with external
// state. Each record's status is determined using the record's meta type and
// meta in conjunction with the configured StatusFunc.
type Reconciler struct {
	settings    *cluster.Settings
	localStores *kvserver.Stores
	db          *kv.DB
	cache       protectedts.Cache
	pts         protectedts.Storage
	metrics     Metrics
	statusFuncs StatusFuncs
}

// NewReconciler constructs a Reconciler.
func NewReconciler(cfg Config) *Reconciler {
	return &Reconciler{
		settings:    cfg.Settings,
		localStores: cfg.Stores,
		db:          cfg.DB,
		cache:       cfg.Cache,
		pts:         cfg.Storage,
		metrics:     makeMetrics(),
		statusFuncs: cfg.StatusFuncs,
	}
}

// Metrics returns the Reconciler's metrics.
func (r *Reconciler) Metrics() *Metrics {
	return &r.metrics
}

// Start will start the Reconciler.
func (r *Reconciler) Start(ctx context.Context, stopper *stop.Stopper) error {
	return stopper.RunAsyncTask(ctx, "protectedts-reconciliation", func(ctx context.Context) {
		r.run(ctx, stopper)
	})
}

func (r *Reconciler) run(ctx context.Context, stopper *stop.Stopper) {
	reconcileIntervalChanged := make(chan struct{}, 1)
	ReconcileInterval.SetOnChange(&r.settings.SV, func() {
		select {
		case reconcileIntervalChanged <- struct{}{}:
		default:
		}
	})
	lastReconciled := time.Time{}
	getInterval := func() time.Duration {
		interval := ReconcileInterval.Get(&r.settings.SV)
		const jitterFrac = .1
		return time.Duration(float64(interval) * (1 + (rand.Float64()-.5)*jitterFrac))
	}
	timer := timeutil.NewTimer()
	for {
		timer.Reset(timeutil.Until(lastReconciled.Add(getInterval())))
		select {
		case <-timer.C:
			timer.Read = true
			r.reconcile(ctx)
			lastReconciled = timeutil.Now()
		case <-reconcileIntervalChanged:
			// Go back around again.
		case <-stopper.ShouldQuiesce():
			return
		case <-ctx.Done():
			return
		}
	}
}

func (r *Reconciler) isMeta1Leaseholder(ctx context.Context, now hlc.Timestamp) (bool, error) {
	return r.localStores.IsMeta1Leaseholder(now)
}

func (r *Reconciler) reconcile(ctx context.Context) {
	now := r.db.Clock().Now()
	isLeaseholder, err := r.isMeta1Leaseholder(ctx, now)
	if err != nil {
		log.Errorf(ctx, "failed to determine whether the local store contains the meta1 lease: %v", err)
		return
	}
	if !isLeaseholder {
		return
	}
	if err := r.cache.Refresh(ctx, now); err != nil {
		log.Errorf(ctx, "failed to refresh the protected timestamp cache to %v: %v", now, err)
		return
	}
	r.cache.Iterate(ctx, keys.MinKey, keys.MaxKey, func(rec *ptpb.Record) (wantMore bool) {
		task, ok := r.statusFuncs[rec.MetaType]
		if !ok {
			// NB: We don't expect to ever hit this case outside of testing.
			log.Infof(ctx, "found protected timestamp record with unknown meta type %q, skipping", rec.MetaType)
			return true
		}
		var didRemove bool
		if err := r.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) (err error) {
			didRemove = false // reset for retries
			shouldRemove, err := task(ctx, txn, rec.Meta)
			if err != nil {
				return err
			}
			if !shouldRemove {
				return nil
			}
			err = r.pts.Release(ctx, txn, rec.ID)
			if err != nil && err != protectedts.ErrNotExists {
				return err
			}
			didRemove = true
			return nil
		}); err != nil {
			r.metrics.ReconciliationErrors.Inc(1)
			log.Errorf(ctx, "failed to reconcile protected timestamp with id %s: %v",
				rec.ID.String(), err)
		} else {
			r.metrics.RecordsProcessed.Inc(1)
			if didRemove {
				r.metrics.RecordsRemoved.Inc(1)
			}
		}
		return true
	})
	r.metrics.ReconcilationRuns.Inc(1)
}
