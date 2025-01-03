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

package sql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/security"
	"gitee.com/kwbasedb/kwbase/pkg/server/serverpb"
	"gitee.com/kwbasedb/kwbase/pkg/server/telemetry"
	"gitee.com/kwbasedb/kwbase/pkg/settings"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/schema"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sessiondata"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqltelemetry"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlutil"
	"gitee.com/kwbasedb/kwbase/pkg/util"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/metric"
	"gitee.com/kwbasedb/kwbase/pkg/util/retry"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/uint128"
	"github.com/cockroachdb/errors"
	io_prometheus_client "github.com/prometheus/client_model/go"
)

// TempObjectCleanupInterval is a ClusterSetting controlling how often
// temporary objects get cleaned up.
var TempObjectCleanupInterval = settings.RegisterPublicDurationSetting(
	"sql.temp_object_cleaner.cleanup_interval",
	"how often to clean up orphaned temporary objects",
	30*time.Minute,
)

var (
	temporaryObjectCleanerActiveCleanersMetric = metric.Metadata{
		Name:        "sql.temp_object_cleaner.active_cleaners",
		Help:        "number of cleaner tasks currently running on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_GAUGE,
	}
	temporaryObjectCleanerSchemasToDeleteMetric = metric.Metadata{
		Name:        "sql.temp_object_cleaner.schemas_to_delete",
		Help:        "number of schemas to be deleted by the temp object cleaner on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
	temporaryObjectCleanerSchemasDeletionErrorMetric = metric.Metadata{
		Name:        "sql.temp_object_cleaner.schemas_deletion_error",
		Help:        "number of errored schema deletions by the temp object cleaner on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
	temporaryObjectCleanerSchemasDeletionSuccessMetric = metric.Metadata{
		Name:        "sql.temp_object_cleaner.schemas_deletion_success",
		Help:        "number of successful schema deletions by the temp object cleaner on this node",
		Measurement: "Count",
		Unit:        metric.Unit_COUNT,
		MetricType:  io_prometheus_client.MetricType_COUNTER,
	}
)

// TemporarySchemaNameForRestorePrefix is the prefix name of the schema we
// synthesize during a full cluster restore. All temporary objects being
// restored are remapped to belong to this schema allowing the reconciliation
// job to gracefully clean up these objects when it runs.
const TemporarySchemaNameForRestorePrefix string = "pg_temp_0_"

func (p *planner) getOrCreateTemporarySchema(
	ctx context.Context, dbID sqlbase.ID,
) (sqlbase.ID, error) {
	tempSchemaName := p.TemporarySchemaName()
	sKey := sqlbase.NewSchemaKey(dbID, tempSchemaName)
	schemaID, err := GetDescriptorID(ctx, p.txn, sKey)
	if err != nil {
		return sqlbase.InvalidID, err
	} else if schemaID == sqlbase.InvalidID {
		// The temporary schema has not been created yet.
		id, err := GenerateUniqueDescID(ctx, p.ExecCfg().DB)
		if err != nil {
			return sqlbase.InvalidID, err
		}
		if err := p.CreateSchemaNamespaceEntry(ctx, sKey.Key(), id); err != nil {
			return sqlbase.InvalidID, err
		}
		p.sessionDataMutator.SetTemporarySchemaName(sKey.Name())
		return id, nil
	}
	return schemaID, nil
}

// CreateSchemaNamespaceEntry creates an entry for the schema in the
// system.namespace table.
func (p *planner) CreateSchemaNamespaceEntry(
	ctx context.Context, schemaNameKey roachpb.Key, schemaID sqlbase.ID,
) error {
	if p.ExtendedEvalContext().Tracing.KVTracingEnabled() {
		log.VEventf(ctx, 2, "CPut %s -> %d", schemaNameKey, schemaID)
	}

	b := &kv.Batch{}
	b.CPut(schemaNameKey, schemaID, nil)

	return p.txn.Run(ctx, b)
}

// temporarySchemaName returns the session specific temporary schema name given
// the sessionID. When the session creates a temporary object for the first
// time, it must create a schema with the name returned by this function.
func temporarySchemaName(sessionID ClusterWideID) string {
	return fmt.Sprintf("pg_temp_%d_%d", sessionID.Hi, sessionID.Lo)
}

// temporarySchemaSessionID returns the sessionID of the given temporary schema.
func temporarySchemaSessionID(scName string) (bool, ClusterWideID, error) {
	if !strings.HasPrefix(scName, "pg_temp_") {
		return false, ClusterWideID{}, nil
	}
	parts := strings.Split(scName, "_")
	if len(parts) != 4 {
		return false, ClusterWideID{}, errors.Errorf("malformed temp schema name %s", scName)
	}
	hi, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return false, ClusterWideID{}, err
	}
	lo, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return false, ClusterWideID{}, err
	}
	return true, ClusterWideID{uint128.Uint128{Hi: hi, Lo: lo}}, nil
}

// getTemporaryObjectNames returns all the temporary objects under the
// temporary schema of the given dbID.
func getTemporaryObjectNames(
	ctx context.Context, txn *kv.Txn, dbID sqlbase.ID, tempSchemaName string,
) (TableNames, error) {
	dbDesc, err := MustGetDatabaseDescByID(ctx, txn, dbID)
	if err != nil {
		return nil, err
	}
	a := UncachedPhysicalAccessor{}
	return a.GetObjectNames(
		ctx,
		txn,
		dbDesc,
		tempSchemaName,
		tree.DatabaseListFlags{CommonLookupFlags: tree.CommonLookupFlags{Required: false}},
	)
}

// cleanupSessionTempObjects removes all temporary objects (tables, sequences,
// views, temporary schema) created by the session.
func cleanupSessionTempObjects(
	ctx context.Context,
	settings *cluster.Settings,
	db *kv.DB,
	ie sqlutil.InternalExecutor,
	sessionID ClusterWideID,
) error {
	tempSchemaName := temporarySchemaName(sessionID)
	return db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Explicitly set the system config trigger, since we may write to the
		// namespace table first.
		if err := txn.SetSystemConfigTrigger(); err != nil {
			return err
		}
		// We are going to read all database descriptor IDs, then for each database
		// we will drop all the objects under the temporary schema.
		dbIDs, err := GetAllDatabaseDescriptorIDs(ctx, txn)
		if err != nil {
			return err
		}
		for _, id := range dbIDs {
			if err := cleanupSchemaObjects(
				ctx,
				settings,
				txn,
				ie,
				id,
				tempSchemaName,
			); err != nil {
				return err
			}
			// Even if no objects were found under the temporary schema, the schema
			// itself may still exist (eg. a temporary table was created and then
			// dropped). So we remove the namespace table entry of the temporary
			// schema.
			if err := sqlbase.RemoveSchemaNamespaceEntry(ctx, txn, id, tempSchemaName); err != nil {
				return err
			}
		}
		return nil
	})
}

// cleanupSchemaObjects removes all objects that is located within a dbID and schema.
func cleanupSchemaObjects(
	ctx context.Context,
	settings *cluster.Settings,
	txn *kv.Txn,
	ie sqlutil.InternalExecutor,
	dbID sqlbase.ID,
	schemaName string,
) error {
	tbNames, err := getTemporaryObjectNames(ctx, txn, dbID, schemaName)
	if err != nil {
		return err
	}
	a := UncachedPhysicalAccessor{}

	searchPath := sqlbase.DefaultSearchPath.WithTemporarySchemaName(schemaName)
	override := sqlbase.InternalExecutorSessionDataOverride{
		SearchPath: &searchPath,
		User:       security.RootUser,
	}

	// TODO(andrei): We might want to accelerate the deletion of this data.
	var tables sqlbase.IDs
	var views sqlbase.IDs
	var sequences sqlbase.IDs

	descsByID := make(map[sqlbase.ID]*TableDescriptor, len(tbNames))
	tblNamesByID := make(map[sqlbase.ID]tree.TableName, len(tbNames))
	for _, tbName := range tbNames {
		objDesc, err := a.GetObjectDesc(
			ctx,
			txn,
			settings,
			&tbName,
			tree.ObjectLookupFlagsWithRequired(),
		)
		if err != nil {
			return err
		}
		desc := objDesc.TableDesc()

		descsByID[desc.ID] = desc
		tblNamesByID[desc.ID] = tbName

		if desc.SequenceOpts != nil {
			sequences = append(sequences, desc.ID)
		} else if desc.ViewQuery != "" {
			views = append(views, desc.ID)
		} else {
			tables = append(tables, desc.ID)
		}
	}

	for _, toDelete := range []struct {
		// typeName is the type of table being deleted, e.g. view, table, sequence
		typeName string
		// ids represents which ids we wish to remove.
		ids sqlbase.IDs
		// preHook is used to perform any operations needed before calling
		// delete on all the given ids.
		preHook func(sqlbase.ID) error
	}{
		// Drop views before tables to avoid deleting required dependencies.
		{"VIEW", views, nil},
		{"TABLE", tables, nil},
		// Drop sequences after tables, because then we reduce the amount of work
		// that may be needed to drop indices.
		{
			"SEQUENCE",
			sequences,
			func(id sqlbase.ID) error {
				desc := descsByID[id]
				// For any dependent tables, we need to drop the sequence dependencies.
				// This can happen if a permanent table references a temporary table.
				for _, d := range desc.DependedOnBy {
					// We have already cleaned out anything we are depended on if we've seen
					// the descriptor already.
					if _, ok := descsByID[d.ID]; ok {
						continue
					}
					dTableDesc, err := sqlbase.GetTableDescFromID(ctx, txn, d.ID)
					if err != nil {
						return err
					}
					db, err := sqlbase.GetDatabaseDescFromID(ctx, txn, dTableDesc.GetParentID())
					if err != nil {
						return err
					}
					schema, err := schema.ResolveNameByID(
						ctx,
						txn,
						dTableDesc.GetParentID(),
						dTableDesc.GetParentSchemaID(),
					)
					if err != nil {
						return err
					}
					dependentColIDs := util.MakeFastIntSet()
					for _, colID := range d.ColumnIDs {
						dependentColIDs.Add(int(colID))
					}
					for _, col := range dTableDesc.Columns {
						if dependentColIDs.Contains(int(col.ID)) {
							tbName := tree.MakeTableNameWithSchema(
								tree.Name(db.Name),
								tree.Name(schema),
								tree.Name(dTableDesc.Name),
							)
							_, err = ie.ExecEx(
								ctx,
								"delete-temp-dependent-col",
								txn,
								override,
								fmt.Sprintf(
									"ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT",
									tbName.FQString(),
									tree.NameString(col.Name),
								),
							)
							if err != nil {
								return err
							}
						}
					}
				}
				return nil
			},
		},
	} {
		if len(toDelete.ids) > 0 {
			if toDelete.preHook != nil {
				for _, id := range toDelete.ids {
					if err := toDelete.preHook(id); err != nil {
						return err
					}
				}
			}

			var query strings.Builder
			query.WriteString("DROP ")
			query.WriteString(toDelete.typeName)

			for i, id := range toDelete.ids {
				tbName := tblNamesByID[id]
				if i != 0 {
					query.WriteString(",")
				}
				query.WriteString(" ")
				query.WriteString(tbName.FQString())
			}
			query.WriteString(" CASCADE")
			_, err = ie.ExecEx(ctx, "delete-temp-"+toDelete.typeName, txn, override, query.String())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// isMeta1LeaseholderFunc helps us avoid an import into pkg/storage.
type isMeta1LeaseholderFunc func(hlc.Timestamp) (bool, error)

// TemporaryObjectCleaner is a background thread job that periodically
// cleans up orphaned temporary objects by sessions which did not close
// down cleanly.
type TemporaryObjectCleaner struct {
	settings                         *cluster.Settings
	db                               *kv.DB
	makeSessionBoundInternalExecutor sqlutil.SessionBoundInternalExecutorFactory
	statusServer                     serverpb.StatusServer
	isMeta1LeaseholderFunc           isMeta1LeaseholderFunc
	testingKnobs                     ExecutorTestingKnobs
	metrics                          *temporaryObjectCleanerMetrics
}

// temporaryObjectCleanerMetrics are the metrics for TemporaryObjectCleaner
type temporaryObjectCleanerMetrics struct {
	ActiveCleaners         *metric.Gauge
	SchemasToDelete        *metric.Counter
	SchemasDeletionError   *metric.Counter
	SchemasDeletionSuccess *metric.Counter
}

var _ metric.Struct = (*temporaryObjectCleanerMetrics)(nil)

// MetricStruct implements the metrics.Struct interface.
func (m *temporaryObjectCleanerMetrics) MetricStruct() {}

// NewTemporaryObjectCleaner initializes the TemporaryObjectCleaner with the
// required arguments, but does not start it.
func NewTemporaryObjectCleaner(
	settings *cluster.Settings,
	db *kv.DB,
	registry *metric.Registry,
	makeSessionBoundInternalExecutor sqlutil.SessionBoundInternalExecutorFactory,
	statusServer serverpb.StatusServer,
	isMeta1LeaseholderFunc isMeta1LeaseholderFunc,
	testingKnobs ExecutorTestingKnobs,
) *TemporaryObjectCleaner {
	metrics := makeTemporaryObjectCleanerMetrics()
	registry.AddMetricStruct(metrics)
	return &TemporaryObjectCleaner{
		settings:                         settings,
		db:                               db,
		makeSessionBoundInternalExecutor: makeSessionBoundInternalExecutor,
		statusServer:                     statusServer,
		isMeta1LeaseholderFunc:           isMeta1LeaseholderFunc,
		testingKnobs:                     testingKnobs,
		metrics:                          metrics,
	}
}

// makeTemporaryObjectCleanerMetrics makes the metrics for the TemporaryObjectCleaner.
func makeTemporaryObjectCleanerMetrics() *temporaryObjectCleanerMetrics {
	return &temporaryObjectCleanerMetrics{
		ActiveCleaners:         metric.NewGauge(temporaryObjectCleanerActiveCleanersMetric),
		SchemasToDelete:        metric.NewCounter(temporaryObjectCleanerSchemasToDeleteMetric),
		SchemasDeletionError:   metric.NewCounter(temporaryObjectCleanerSchemasDeletionErrorMetric),
		SchemasDeletionSuccess: metric.NewCounter(temporaryObjectCleanerSchemasDeletionSuccessMetric),
	}
}

// doTemporaryObjectCleanup performs the actual cleanup.
func (c *TemporaryObjectCleaner) doTemporaryObjectCleanup(
	ctx context.Context, closerCh <-chan struct{},
) error {
	defer log.Infof(ctx, "completed temporary object cleanup job")
	// Wrap the retry functionality with the default arguments.
	retryFunc := func(ctx context.Context, do func() error) error {
		return retry.WithMaxAttempts(
			ctx,
			retry.Options{
				InitialBackoff: 1 * time.Second,
				MaxBackoff:     1 * time.Minute,
				Multiplier:     2,
				Closer:         closerCh,
			},
			5, // maxAttempts
			func() error {
				err := do()
				if err != nil {
					log.Warningf(ctx, "error during schema cleanup, retrying: %v", err)
				}
				return err
			},
		)
	}

	// We only want to perform the cleanup if we are holding the meta1 lease.
	// This ensures only one server can perform the job at a time.
	isLeaseholder, err := c.isMeta1LeaseholderFunc(c.db.Clock().Now())
	if err != nil {
		return err
	}
	if !isLeaseholder {
		log.Infof(ctx, "skipping temporary object cleanup run as it is not the leaseholder")
		return nil
	}

	c.metrics.ActiveCleaners.Inc(1)
	defer c.metrics.ActiveCleaners.Dec(1)

	log.Infof(ctx, "running temporary object cleanup background job")
	txn := kv.NewTxn(ctx, c.db, 0)

	// Build a set of all session IDs with temporary objects.
	var dbIDs []sqlbase.ID
	if err := retryFunc(ctx, func() error {
		var err error
		dbIDs, err = GetAllDatabaseDescriptorIDs(ctx, txn)
		return err
	}); err != nil {
		return err
	}

	sessionIDs := make(map[ClusterWideID]struct{})
	for _, dbID := range dbIDs {
		var schemaNames map[sqlbase.ID]string
		if err := retryFunc(ctx, func() error {
			var err error
			schemaNames, err = schema.GetForDatabase(ctx, txn, dbID)
			return err
		}); err != nil {
			return err
		}
		for _, scName := range schemaNames {
			isTempSchema, sessionID, err := temporarySchemaSessionID(scName)
			if err != nil {
				// This should not cause an error.
				log.Warningf(ctx, "could not parse %q as temporary schema name", scName)
				continue
			}
			if isTempSchema {
				sessionIDs[sessionID] = struct{}{}
			}
		}
	}
	log.Infof(ctx, "found %d temporary schemas", len(sessionIDs))

	if len(sessionIDs) == 0 {
		log.Infof(ctx, "early exiting temporary schema cleaner as no temporary schemas were found")
		return nil
	}

	// Get active sessions.
	var response *serverpb.ListSessionsResponse
	if err := retryFunc(ctx, func() error {
		var err error
		response, err = c.statusServer.ListSessions(
			ctx,
			&serverpb.ListSessionsRequest{},
		)
		return err
	}); err != nil {
		return err
	}
	activeSessions := make(map[uint128.Uint128]struct{})
	for _, session := range response.Sessions {
		activeSessions[uint128.FromBytes(session.ID)] = struct{}{}
	}

	// Clean up temporary data for inactive sessions.
	ie := c.makeSessionBoundInternalExecutor(ctx, &sessiondata.SessionData{})
	for sessionID := range sessionIDs {
		if _, ok := activeSessions[sessionID.Uint128]; !ok {
			log.Eventf(ctx, "cleaning up temporary object for session %q", sessionID)
			c.metrics.SchemasToDelete.Inc(1)

			// Reset the session data with the appropriate sessionID such that we can resolve
			// the given schema correctly.
			if err := retryFunc(ctx, func() error {
				return cleanupSessionTempObjects(
					ctx,
					c.settings,
					c.db,
					ie,
					sessionID,
				)
			}); err != nil {
				// Log error but continue trying to delete the rest.
				log.Warningf(ctx, "failed to clean temp objects under session %q: %v", sessionID, err)
				c.metrics.SchemasDeletionError.Inc(1)
			} else {
				c.metrics.SchemasDeletionSuccess.Inc(1)
				telemetry.Inc(sqltelemetry.TempObjectCleanerDeletionCounter)
			}
		} else {
			log.Eventf(ctx, "not cleaning up %q as session is still active", sessionID)
		}
	}

	return nil
}

// Start initializes the background thread which periodically cleans up leftover temporary objects.
func (c *TemporaryObjectCleaner) Start(ctx context.Context, stopper *stop.Stopper) {
	stopper.RunWorker(ctx, func(ctx context.Context) {
		nextTick := timeutil.Now()
		for {
			nextTickCh := time.After(nextTick.Sub(timeutil.Now()))
			if c.testingKnobs.TempObjectsCleanupCh != nil {
				nextTickCh = c.testingKnobs.TempObjectsCleanupCh
			}

			select {
			case <-nextTickCh:
				if err := c.doTemporaryObjectCleanup(ctx, stopper.ShouldQuiesce()); err != nil {
					log.Warningf(ctx, "failed to clean temp objects: %v", err)
				}
			case <-stopper.ShouldQuiesce():
				return
			case <-ctx.Done():
				return
			}
			if c.testingKnobs.OnTempObjectsCleanupDone != nil {
				c.testingKnobs.OnTempObjectsCleanupDone()
			}
			nextTick = nextTick.Add(TempObjectCleanupInterval.Get(&c.settings.SV))
			log.Infof(ctx, "temporary object cleaner next scheduled to run at %s", nextTick)
		}
	})
}
