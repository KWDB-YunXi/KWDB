// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
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
	"strconv"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/clusterversion"
	"gitee.com/kwbasedb/kwbase/pkg/config"
	"gitee.com/kwbasedb/kwbase/pkg/jobs"
	"gitee.com/kwbasedb/kwbase/pkg/jobs/jobspb"
	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/sql/hashrouter/api"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/protoutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/retry"
	"github.com/pkg/errors"
)

// In order to ensure the consistency of schema changes of time-series objects,
// we implement a two-stage process for schema changes of time-series objects.
// We use the node executing the job as the coordinator to complete the
// time-series engine schema change step by step. Is not available for time-series
// objects in DDL. When we finally complete the DDL safely, the time-series objects
// are available again.
// This is different from the Online-Schema-Change for relational tables.
// We may support Online-Schema-Change for time-series objects in the future

const (
	_ = iota
	createKwdbTsTable
	createKwdbInsTable
	dropKwdbTsTable
	dropKwdbInsTable
	dropKwdbTsDatabase
	alterKwdbAddTag
	alterKwdbDropTag
	alterKwdbAlterTagType
	alterKwdbSetTagValue
	alterKwdbAddColumn
	alterKwdbDropColumn
	alterKwdbAlterColumnType
	alterKwdbAlterPartitionInterval
	// Compress compress
	Compress
	// Retention retention
	Retention
	alterCompressInterval
)

// tsSchemaChangeResumer implements the jobs.Resumer interface for syncMetaCache
// jobs. A new instance is created for each job.
type tsSchemaChangeResumer struct {
	job *jobs.Job
}

var _ jobs.Resumer = &tsSchemaChangeResumer{}

// TSSchemaChangeWorker is used to change the schema on a ts table.
type TSSchemaChangeWorker struct {
	nodeID         roachpb.NodeID
	db             *kv.DB
	leaseMgr       *LeaseManager
	p              *planner
	distSQLPlanner *DistSQLPlanner
	jobRegistry    *jobs.Registry
	// Keep a reference to the job related to this schema change
	// so that we don't need to read the job again while updating
	// the status of the job.
	job *jobs.Job
	// Caches updated by DistSQL.
	settings *cluster.Settings
	execCfg  *ExecutorConfig
	clock    *hlc.Clock
}

// Resume is part of the jobs.Resumer interface.
func (r *tsSchemaChangeResumer) Resume(
	ctx context.Context, phs interface{}, resultsCh chan<- tree.Datums,
) error {
	p := phs.(PlanHookState)
	sw := TSSchemaChangeWorker{
		nodeID:         p.ExecCfg().NodeID.Get(),
		db:             p.ExecCfg().DB,
		leaseMgr:       p.ExecCfg().LeaseManager,
		p:              p.(*planner),
		distSQLPlanner: p.DistSQLPlanner(),
		jobRegistry:    p.ExecCfg().JobRegistry,
		job:            r.job,
		settings:       p.ExecCfg().Settings,
		execCfg:        p.ExecCfg(),
		clock:          p.ExecCfg().Clock,
	}
	return sw.exec(ctx)
}

// exec executes the entire ts schema change in steps.
func (sw *TSSchemaChangeWorker) exec(ctx context.Context) error {
	d := sw.job.Details().(jobspb.SyncMetaCacheDetails)
	var syncErr error
	// we need to handle the outcome of async DDL action
	defer func() {
		// handle the intermediate state of metadata
		sw.handleResult(ctx, d, syncErr)
	}()
	// some DDL does not need AE operation
	if d.DoNothing {
		return nil
	}
	opType := getDDLOpType(d.Type)
	// make distributed exec plan and run
	// if failed, rollback WAL
	// if succeeded, commit WAL
	if syncErr = sw.makeAndRunDistPlan(ctx, d); syncErr != nil {
		log.Infof(ctx, "%s start rollback, jobID: %d", opType, sw.job.ID())
		if err := sw.retryTsTxn(ctx, txnRollback); err != nil {
			log.Infof(ctx, "%s rollback failed, reason: %s, jobID: %d", opType, err.Error(), sw.job.ID())
			return err
		}
		return syncErr
	}
	if err := sw.retryTsTxn(ctx, txnCommit); err != nil {
		return err
	}

	return nil
}

// retryTsTxn is used to rollback/commit WAL
func (sw *TSSchemaChangeWorker) retryTsTxn(ctx context.Context, event txnEvent) error {
	d := sw.job.Details().(jobspb.SyncMetaCacheDetails)
	retryOpts := retry.Options{
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
		Multiplier:     2,
		MaxRetries:     100,
	}
	var err error
	for opt := retry.Start(retryOpts); opt.Next(); {
		err = sw.sendTsTxn(ctx, d, event)
		if err == nil {
			break
		}
		log.Error(ctx, err)
	}
	return err
}

// makeKObjectTableForTs make KObjectTable for AE
// Inparam: SyncMetaCacheDetails
// OutParam: KObjectTable
func makeKObjectTableForTs(d jobspb.SyncMetaCacheDetails) sqlbase.CreateTsTable {
	var kColDescs []sqlbase.KWDBKTSColumn
	var KColumnsID []uint32

	for _, col := range d.SNTable.Columns {
		colName := tree.Name(col.Name)
		kColDesc := sqlbase.KWDBKTSColumn{
			ColumnId:           uint32(col.ID),
			Name:               colName.String(),
			Nullable:           col.IsNullable(),
			StorageType:        col.TsCol.StorageType,
			StorageLen:         col.TsCol.StorageLen,
			ColOffset:          col.TsCol.ColOffset,
			VariableLengthType: col.TsCol.VariableLengthType,
			ColType:            col.TsCol.ColumnType,
		}
		kColDescs = append(kColDescs, kColDesc)
		KColumnsID = append(KColumnsID, uint32(col.ID))
	}
	tableName := tree.Name(d.SNTable.Name)
	kObjectTable := sqlbase.KWDBTsTable{
		TsTableId:         uint64(d.SNTable.ID),
		DatabaseId:        uint32(d.SNTable.ParentID),
		LifeTime:          d.SNTable.TsTable.Lifetime,
		ActiveTime:        d.SNTable.TsTable.ActiveTime,
		KColumnsId:        KColumnsID,
		RowSize:           d.SNTable.TsTable.RowSize,
		BitmapOffset:      d.SNTable.TsTable.BitmapOffset,
		TableName:         tableName.String(),
		Sde:               d.SNTable.TsTable.Sde,
		PartitionInterval: d.SNTable.TsTable.PartitionInterval,
		TsVersion:         uint32(d.SNTable.TsTable.GetTsVersion()),
	}

	return sqlbase.CreateTsTable{
		TsTable: kObjectTable,
		KColumn: kColDescs,
	}
}

// handleResult processes metadata based on the results of distributed execution.
// If the execution is successful, it modifies the metadata and changes the intermediate state to public.
// If the execution fails, it rolls back the metadata and changes the intermediate state to public.
func (sw *TSSchemaChangeWorker) handleResult(
	ctx context.Context, d jobspb.SyncMetaCacheDetails, syncErr error,
) {
	opType := getDDLOpType(d.Type)
	log.Infof(ctx, "%s initial ddl job finished, jobID: %d", opType, sw.job.ID())
	retryOpts := retry.Options{
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
		Multiplier:     2,
	}
	p := sw.p
	log.Infof(ctx, "%s metadata retry job started, jobID: %d", opType, sw.job.ID())
	// keep trying to process metadata until success
	for opt := retry.Start(retryOpts); opt.Next(); {
		var updateErr error
		switch d.Type {
		case createKwdbTsTable:
			updateErr = p.handleCreateTSTable(ctx, d.SNTable, syncErr)
		case dropKwdbTsTable:
			updateErr = p.handleDropTsTable(ctx, d.SNTable, sw.jobRegistry, syncErr)
		case dropKwdbTsDatabase:
			updateErr = p.handleDropTsDatabase(ctx, d.Database, d.DropDBInfo, sw.jobRegistry, syncErr)
		case createKwdbInsTable:
			// prepare instance table metadata which is being created
			insTable := sqlbase.InstNameSpace{
				InstName:    d.CTable.CTable.Name,
				InstTableID: sqlbase.ID(d.CTable.CTable.Id),
				TmplTableID: d.SNTable.ID,
				DBName:      d.Database.Name,
				ChildDesc: sqlbase.ChildDesc{
					STableName: d.SNTable.Name,
					State:      sqlbase.ChildDesc_PUBLIC,
				},
			}
			updateErr = p.handleCreateInsTable(ctx, insTable, syncErr)
		case dropKwdbInsTable:
			updateErr = p.handleDropInsTable(
				ctx,
				d.DropMEInfo[0].DatabaseName,
				d.DropMEInfo[0].TableName,
				d.DropMEInfo[0].TableID,
				syncErr,
			)
		case alterKwdbAddTag, alterKwdbAddColumn:
			updateErr = p.handleAddTsColumn(ctx, d.SNTable.ID, d.AlterTag, syncErr)
		case alterKwdbDropTag, alterKwdbDropColumn:
			updateErr = p.handleDropTsColumn(ctx, d.SNTable.ID, d.AlterTag, syncErr)
		case alterKwdbAlterPartitionInterval:
			updateErr = p.handleAlterPartitionInterval(
				ctx,
				d.SNTable.ID,
				d.SNTable.TsTable.PartitionInterval,
				d.SNTable.TsTable.PartitionIntervalInput,
				syncErr,
			)
		case alterKwdbSetTagValue:
			// prepare instance table metadata being modified
			insTable := sqlbase.InstNameSpace{
				InstName:    d.SetTag.TableName,
				InstTableID: sqlbase.ID(d.SetTag.TableId),
				TmplTableID: d.SNTable.ID,
				DBName:      d.SetTag.DbName,
				ChildDesc: sqlbase.ChildDesc{
					STableName: d.SNTable.Name,
					State:      sqlbase.ChildDesc_PUBLIC,
				},
			}
			updateErr = p.handleSetTagValue(ctx, d.SNTable, insTable, syncErr)
		case alterKwdbAlterTagType, alterKwdbAlterColumnType:
			updateErr = p.handleAlterTsColumnType(ctx, d.SNTable.ID, d.AlterTag, syncErr)
		default:
		}
		if updateErr == nil {
			break
		} else {
			log.Infof(ctx, "handle metadata failed: ", updateErr)
		}
	}
	log.Infof(ctx, "%s metadata retry job finished, jobID: %d", opType, sw.job.ID())
}

// handleSetTagValue restore instance table metadata is available,
// and the time-series engine completes setting the tag value.
func (p *planner) handleSetTagValue(
	ctx context.Context, desc sqlbase.TableDescriptor, insTable sqlbase.InstNameSpace, syncErr error,
) error {
	updateErr := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		p.txn = txn
		// rewrite instance table
		if err := writeInstTableMeta(ctx, p.Txn(), []sqlbase.InstNameSpace{insTable}, true); err != nil {
			return err
		}
		if syncErr == nil {
			tableDesc := sqlbase.NewMutableExistingTableDescriptor(desc)
			tableDesc.TableType = tree.TemplateTable
			if err := p.writeTableDesc(ctx, tableDesc); err != nil {
				return err
			}
		}
		return nil
	})
	return updateErr
}

// handleAlterPartitionInterval restore time-series table metadata is available,
// and the time-series engine completes setting the PartitionInterval.
func (p *planner) handleAlterPartitionInterval(
	ctx context.Context, tableID sqlbase.ID, partitionInterval uint64, input *string, syncErr error,
) error {
	_, updateDescErr := p.ExecCfg().LeaseManager.Publish(
		ctx,
		tableID,
		func(tableDesc *sqlbase.MutableTableDescriptor) error {
			tableDesc.State = sqlbase.TableDescriptor_PUBLIC
			if syncErr == nil {
				tableDesc.TsTable.PartitionInterval = partitionInterval
				tableDesc.TsTable.PartitionIntervalInput = input
			}
			return nil
		},
		func(txn *kv.Txn) error { return nil })
	return updateDescErr
}

// handleDropTsColumn drops tag/column from time-series table metadata,
// when the time-series engine completes deleting the tag/column.
func (p *planner) handleDropTsColumn(
	ctx context.Context, tableID sqlbase.ID, tag sqlbase.ColumnDescriptor, syncErr error,
) error {
	_, updateDescErr := p.ExecCfg().LeaseManager.Publish(
		ctx,
		tableID,
		func(desc *sqlbase.MutableTableDescriptor) error {
			desc.State = sqlbase.TableDescriptor_PUBLIC
			if syncErr == nil {
				for i := range desc.Columns {
					if tag.Name == desc.Columns[i].Name {
						// Removes the tag from the Columns/ColumnNames/ColumnIDs queue.
						// The tags in the middle of the queue and at the end of the queue are processed respectively.
						if i == len(desc.Columns)-1 {
							desc.Columns = desc.Columns[:i]
							desc.Families[0].ColumnNames = desc.Families[0].ColumnNames[:i]
							desc.Families[0].ColumnIDs = desc.Families[0].ColumnIDs[:i]
						} else {
							desc.Columns = append(desc.Columns[:i], desc.Columns[i+1:]...)
							desc.Families[0].ColumnNames = append(desc.Families[0].ColumnNames[:i], desc.Families[0].ColumnNames[i+1:]...)
							desc.Families[0].ColumnIDs = append(desc.Families[0].ColumnIDs[:i], desc.Families[0].ColumnIDs[i+1:]...)
						}
						break
					}
				}
			}
			return nil
		},
		func(txn *kv.Txn) error {
			if syncErr == nil {
				if err := p.removeColumnComment(ctx, txn, tableID, tag.ID); err != nil {
					return err
				}
			}
			return nil
		})
	return updateDescErr
}

// handleAlterTsColumnType modifies tag/column type from time-series table metadata,
// when the time-series engine completes modifying the tag/column type.
func (p *planner) handleAlterTsColumnType(
	ctx context.Context, tableID sqlbase.ID, tag sqlbase.ColumnDescriptor, syncErr error,
) error {
	_, updateDescErr := p.ExecCfg().LeaseManager.Publish(
		ctx,
		tableID,
		func(tableDesc *sqlbase.MutableTableDescriptor) error {
			tableDesc.State = sqlbase.TableDescriptor_PUBLIC
			if syncErr == nil {
				// get old-version tag
				oldTag, dropped, err := tableDesc.FindColumnByName(tag.ColName())
				if err != nil {
					return err
				}
				if dropped {
					return pgerror.Newf(pgcode.ObjectNotInPrerequisiteState,
						"column %q in the middle of being dropped", tag.ColName())
				}
				// change oldTag type to new Tag type
				oldTag.Type = tag.Type
				oldTag.TsCol = tag.TsCol
			}
			return nil
		},
		func(txn *kv.Txn) error { return nil })
	return updateDescErr
}

// handleAddTsColumn add tag/column for time-series table metadata,
// when the time-series engine completes adding the tag/column.
func (p *planner) handleAddTsColumn(
	ctx context.Context, tableID sqlbase.ID, tag sqlbase.ColumnDescriptor, syncErr error,
) error {
	_, updateDescErr := p.ExecCfg().LeaseManager.Publish(
		ctx,
		tableID,
		func(tableDesc *sqlbase.MutableTableDescriptor) error {
			tableDesc.State = sqlbase.TableDescriptor_PUBLIC
			if syncErr == nil {
				// add new tag to tableDesc.ColumnDescriptors
				if tag.ID == 0 {
					tag.ID = tableDesc.GetNextColumnID()
					tableDesc.NextColumnID++
				}
				tableDesc.AddColumn(&tag)
				tableDesc.Families[0].ColumnNames = append(tableDesc.Families[0].ColumnNames, tag.Name)
				tableDesc.Families[0].ColumnIDs = append(tableDesc.Families[0].ColumnIDs, tag.ID)
			}
			return nil
		},
		func(txn *kv.Txn) error { return nil })
	return updateDescErr
}

// handleDropTsDatabase processes metadata based on the result of AE execution.
// If AE drops all the tables in this database success, delete corresponding metadata.
// If AE fails, rollback the metadata.
func (p *planner) handleDropTsDatabase(
	ctx context.Context,
	dbDesc sqlbase.DatabaseDescriptor,
	tables []sqlbase.TableDescriptor,
	jr *jobs.Registry,
	syncErr error,
) error {
	var sj *jobs.StartableJob
	updateDescErr := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		p.txn = txn
		if err := getDescriptorByID(ctx, txn, dbDesc.ID, &dbDesc); err != nil {
			if strings.Contains(err.Error(), "is not a database") {
				return nil
			}
			return err
		}
		if syncErr != nil {
			idKey := sqlbase.MakeDatabaseNameKey(ctx, p.ExecCfg().Settings, dbDesc.Name)
			// If the database fails to be dropped, roll it back
			if err := p.Txn().Put(ctx, idKey.Key(), dbDesc.ID); err != nil {
				return err
			}
			for _, desc := range tables {
				tableDesc := sqlbase.NewMutableExistingTableDescriptor(desc)
				tableDesc.State = sqlbase.TableDescriptor_PUBLIC
				if err := p.writeTableDesc(ctx, tableDesc); err != nil {
					return err
				}
			}
			return nil
		}
		if !p.ExecCfg().Settings.Version.IsActive(ctx, clusterversion.VersionSchemaChangeJob) {
			if ctx.Value(migrationSchemaChangeRequiredHint{}) == nil {
				return errSchemaChangeDisallowedInMixedState
			}
		}
		// TODO (lucy): This should probably be deleting the queued jobs for all the
		// tables being dropped, so that we don't have duplicate schema changers.
		droppedDetails := make([]jobspb.DroppedTableDetails, 0, len(tables))
		descriptorIDs := make([]sqlbase.ID, 0, len(tables))

		for _, desc := range tables {
			tableDesc := sqlbase.NewMutableExistingTableDescriptor(desc)
			droppedDetails = append(droppedDetails, jobspb.DroppedTableDetails{
				Name: desc.Name,
				ID:   desc.ID,
			})
			descriptorIDs = append(descriptorIDs, desc.ID)
			jobDesc := "handle drop table " + desc.Name
			if _, err := p.dropTableImpl(ctx, tableDesc, false, jobDesc); err != nil {
				return err
			}
		}
		jobDesc := "handle drop database " + dbDesc.Name
		jobRecord := jobs.Record{
			Description:   jobDesc,
			Username:      p.User(),
			DescriptorIDs: descriptorIDs,
			Details: jobspb.SchemaChangeDetails{
				DroppedTables:     droppedDetails,
				DroppedDatabaseID: dbDesc.ID,
				FormatVersion:     jobspb.JobResumerFormatVersion,
			},
			Progress: jobspb.SchemaChangeProgress{},
		}
		descKey := sqlbase.MakeDescMetadataKey(dbDesc.ID)

		b := &kv.Batch{}
		if p.ExtendedEvalContext().Tracing.KVTracingEnabled() {
			log.VEventf(ctx, 2, "Del %s", descKey)
		}
		b.Del(descKey)

		schemaToDelete := sqlbase.ResolvedSchema{
			ID:   keys.PublicSchemaID,
			Kind: sqlbase.SchemaPublic,
			Name: tree.PublicSchema,
		}
		if err := p.dropSchemaImpl(ctx, b, dbDesc.ID, &schemaToDelete); err != nil {
			return err
		}

		err := sqlbase.RemoveDatabaseNamespaceEntry(
			ctx, p.txn, dbDesc.Name, p.ExtendedEvalContext().Tracing.KVTracingEnabled(),
		)
		if err != nil {
			return err
		}
		// No job was created because no tables were dropped, so zone config can be
		// immediately removed.
		if len(tables) == 0 {
			zoneKeyPrefix := config.MakeZoneKeyPrefix(uint32(dbDesc.ID))
			if p.ExtendedEvalContext().Tracing.KVTracingEnabled() {
				log.VEventf(ctx, 2, "DelRange %s", zoneKeyPrefix)
			}
			// Delete the zone config entry for this database.
			b.DelRange(zoneKeyPrefix, zoneKeyPrefix.PrefixEnd(), false /* returnKeys */)
		}
		p.Tables().addUncommittedDatabase(dbDesc.Name, dbDesc.ID, dbDropped)

		sj, err = jr.CreateStartableJobWithTxn(ctx, jobRecord, p.txn, nil)
		if err != nil {
			return err
		}
		return p.txn.Run(ctx, b)
	})
	if updateDescErr == nil && sj != nil {
		if err := sj.Run(ctx); err != nil {
			if cleanupErr := sj.CleanupOnRollback(ctx); cleanupErr != nil {
				return cleanupErr
			}
			return err
		}
	}
	return updateDescErr

}

// handleDropTsTable handle result for drop template table and time series table.
// if drop table success, drop table descriptor.
// else if drop table failed, change table state to public.
func (p *planner) handleDropTsTable(
	ctx context.Context, desc sqlbase.TableDescriptor, jr *jobs.Registry, syncErr error,
) error {
	var sj *jobs.StartableJob
	updateDescErr := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		p.txn = txn
		tableDesc := sqlbase.NewMutableExistingTableDescriptor(desc)
		if syncErr != nil {
			tableDesc.State = sqlbase.TableDescriptor_PUBLIC
			return p.writeTableDesc(ctx, tableDesc)
		}
		// execute without error, then delete corresponding metadata
		jobDesc := "handle drop table " + tableDesc.Name
		if _, err := p.dropTableImpl(ctx, tableDesc, false, jobDesc); err != nil {
			return err
		}
		// Queue a new job.
		var spanList []jobspb.ResumeSpanList
		span := tableDesc.PrimaryIndexSpan()
		for i := len(tableDesc.Mutations) + len(spanList); i < len(tableDesc.Mutations); i++ {
			spanList = append(spanList,
				jobspb.ResumeSpanList{
					ResumeSpans: []roachpb.Span{span},
				},
			)
		}
		jobRecord := jobs.Record{
			Description:   jobDesc,
			Username:      p.User(),
			DescriptorIDs: sqlbase.IDs{tableDesc.GetID()},
			Details: jobspb.SchemaChangeDetails{
				TableID:        tableDesc.ID,
				MutationID:     sqlbase.InvalidMutationID,
				ResumeSpanList: spanList,
				FormatVersion:  jobspb.JobResumerFormatVersion,
			},
			Progress: jobspb.SchemaChangeProgress{},
		}
		var err error
		sj, err = jr.CreateStartableJobWithTxn(ctx, jobRecord, p.txn, nil)
		if err != nil {
			return err
		}
		return nil
	})
	if updateDescErr == nil && sj != nil {
		if err := sj.Run(ctx); err != nil {
			if cleanupErr := sj.CleanupOnRollback(ctx); cleanupErr != nil {
				return cleanupErr
			}
			return err
		}
	}
	return updateDescErr
}

// handleDropInsTable handle result for drop instance table.
// if drop table success, delete instance table from system table.
// else drop table failed, change table state to public.
func (p *planner) handleDropInsTable(
	ctx context.Context, dbName string, tableName string, tableID uint32, syncErr error,
) error {
	updateErr := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		p.txn = txn
		if syncErr != nil {
			insTable, found, err := sqlbase.ResolveInstanceName(ctx, p.txn, dbName, tableName)
			if err != nil {
				return err
			} else if !found {
				return sqlbase.NewUndefinedTableError(tableName)
			}
			insTable.State = sqlbase.ChildDesc_PUBLIC
			// rewrite instance table
			if err := writeInstTableMeta(ctx, p.Txn(), []sqlbase.InstNameSpace{insTable}, true); err != nil {
				return err
			}
		} else {
			// remove the relation between template and instance table
			if err := DropInstanceTable(ctx, p.txn, sqlbase.ID(tableID), dbName, tableName); err != nil {
				return err
			}
			// clean up cache to avoid looking up instance table by using cache,
			// which may return instance table already dropped
			p.execCfg.QueryCache.Clear()
		}
		return nil
	})
	return updateErr
}

// handleCreateInsTable handle result for create instance table.
// if create table success, change table state to public.
// else if create table failed, delete table from system table.
func (p *planner) handleCreateInsTable(
	ctx context.Context, insTable sqlbase.InstNameSpace, syncErr error,
) error {
	updateErr := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		p.txn = txn
		if syncErr != nil {
			// delete instance table
			if err := DropInstanceTable(
				ctx, p.txn, insTable.InstTableID, insTable.DBName, insTable.InstName,
			); err != nil {
				return err
			}
		} else {
			// change table state to public.
			insTable.State = sqlbase.ChildDesc_PUBLIC
			if err := writeInstTableMeta(ctx, p.Txn(), []sqlbase.InstNameSpace{insTable}, true); err != nil {
				return err
			}
		}
		return nil
	})
	return updateErr
}

// handleCreateTSTable processes metadata based on the result of AE execution.
// If create table success, change tableState to PUBLIC.
// If create table fails, rollback the metadata.
func (p *planner) handleCreateTSTable(
	ctx context.Context, tab sqlbase.TableDescriptor, syncErr error,
) error {
	updateErr := p.ExecCfg().DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		p.txn = txn
		tableDesc := sqlbase.NewMutableExistingTableDescriptor(tab)
		// if create table success, chang table state to public.
		// else if create table failed, delete table descriptor.
		if syncErr != nil {
			b := p.txn.NewBatch()
			// remove table from namespace
			if txnErr := sqlbase.RemoveObjectNamespaceEntry(
				ctx, p.txn, tableDesc.GetParentID(), keys.PublicSchemaID, tableDesc.Name, false,
			); txnErr != nil {
				return txnErr
			}
			// remove hash info
			mgr, err := api.GetHashRouterManagerWithTxn(ctx, nil)
			if err != nil {
				return errors.Errorf("get hashrouter manager failed :%v", err)
			}
			if txnErr := mgr.DropTableHashInfo(ctx, txn, uint32(tab.ID)); txnErr != nil {
				return txnErr
			}
			// remove descriptor
			descKey := sqlbase.MakeDescMetadataKey(tableDesc.ID)
			b.Del(descKey)
			if txnErr := p.txn.Run(ctx, b); txnErr != nil {
				return txnErr
			}
		} else {
			tableDesc.State = sqlbase.TableDescriptor_PUBLIC
			if txnErr := p.writeTableDesc(ctx, tableDesc); txnErr != nil {
				return txnErr
			}
		}
		return nil
	})
	return updateErr
}

// OnFailOrCancel is part of the jobs.Resumer interface.
func (r *tsSchemaChangeResumer) OnFailOrCancel(context.Context, interface{}) error { return nil }

func init() {
	createResumerFn := func(job *jobs.Job, settings *cluster.Settings) jobs.Resumer {
		return &tsSchemaChangeResumer{job: job}
	}

	jobs.RegisterConstructor(jobspb.TypeSyncMetaCache, createResumerFn)
}

// makeAndRunDistPlan first check healthy nodes, then builds DISTSQL plan and executes by CGO function
func (sw *TSSchemaChangeWorker) makeAndRunDistPlan(
	ctx context.Context, d jobspb.SyncMetaCacheDetails,
) error {
	var newPlanNode planNode
	var nodeID []roachpb.NodeID
	opType := getDDLOpType(d.Type)
	switch d.Type {
	case dropKwdbTsTable, dropKwdbTsDatabase:
		switch d.Type {
		case dropKwdbTsTable:
			log.Infof(ctx, "%s job start, name: %s, id: %d, jobID: %d",
				opType, d.SNTable.Name, d.SNTable.ID, sw.job.ID())
		case dropKwdbTsDatabase:
			log.Infof(ctx, "%s job start, name: %s, id: %d, jobID: %d",
				opType, d.Database.Name, d.Database.ID, sw.job.ID())
		}
		nodeList, err := api.GetHealthyNodeIDs(ctx)
		if err != nil {
			return err
		}
		for _, td := range d.DropMEInfo {
			log.Infof(ctx, "%s, jobID: %d, waitForOneVersion start", opType, sw.job.ID())
			// Wait for DML execution to complete on this table
			if _, err := sw.p.ExecCfg().LeaseManager.WaitForOneVersion(
				ctx,
				sqlbase.ID(td.TableID),
				base.DefaultRetryOptions(),
			); err != nil {
				return err
			}
			log.Infof(ctx, "%s, jobID: %d, waitForOneVersion finished", opType, sw.job.ID())
			log.Infof(ctx, "%s, jobID: %d, checkReplica start", opType, sw.job.ID())
			if err := sw.checkReplica(ctx, sqlbase.ID(td.TableID)); err != nil {
				return err
			}
			log.Infof(ctx, "%s, jobID: %d, checkReplica finished", opType, sw.job.ID())
		}
		newPlanNode = &tsDDLNode{d: d, nodeID: nodeList}
	case alterKwdbAddTag, alterKwdbAddColumn, alterKwdbDropColumn, alterKwdbDropTag,
		alterKwdbAlterColumnType, alterKwdbAlterTagType, alterKwdbAlterPartitionInterval:
		if d.Type == alterKwdbAlterPartitionInterval {
			log.Infof(ctx, "%s job start, name: %s, id: %d, jobID: %d",
				opType, d.SNTable.Name, d.SNTable.ID, sw.job.ID())
		} else {
			log.Infof(ctx, "%s job start, name: %s, id: %d, column/tag name: %s, jobID: %d",
				opType, d.SNTable.Name, d.SNTable.ID, d.AlterTag.Name, sw.job.ID())
		}
		// Get all healthy nodes.
		nodeList, err := api.GetHealthyNodeIDs(ctx)
		if err != nil {
			return err
		}
		log.Infof(ctx, "%s, jobID: %d, waitForOneVersion start", opType, sw.job.ID())
		if _, err := sw.p.ExecCfg().LeaseManager.WaitForOneVersion(
			ctx,
			d.SNTable.ID,
			base.DefaultRetryOptions(),
		); err != nil {
			return err
		}
		log.Infof(ctx, "%s, jobID: %d, waitForOneVersion finished", opType, sw.job.ID())
		log.Infof(ctx, "%s, jobID: %d, checkReplica start", opType, sw.job.ID())
		if err := sw.checkReplica(ctx, d.SNTable.ID); err != nil {
			return err
		}
		log.Infof(ctx, "%s, jobID: %d, checkReplica finished", opType, sw.job.ID())
		txnID := strconv.AppendInt([]byte{}, *sw.job.ID(), 10)
		miniTxn := tsTxn{txnID: txnID, txnEvent: txnStart}
		newPlanNode = &tsDDLNode{d: d, nodeID: nodeList, tsTxn: miniTxn}
	case createKwdbTsTable:
		log.Infof(ctx, "%s job start, name: %s, id: %d, jobID: %d",
			opType, d.SNTable.Name, d.SNTable.ID, sw.job.ID())
		nodeList, err := api.GetHealthyNodeIDs(ctx)
		if err != nil {
			return err
		}
		newPlanNode = &tsDDLNode{d: d, nodeID: nodeList}
	case dropKwdbInsTable:
		log.Infof(ctx, "%s job start, name: %s, id: %d, jobID: %d",
			opType, d.SNTable.Name, d.SNTable.ID, sw.job.ID())
		for _, dropInfo := range d.DropMEInfo {
			hashRouter, err := api.GetHashRouterWithTable(0, dropInfo.TemplateID, false, sw.p.txn)
			if err != nil {
				return err
			}
			nodeIDs, err := hashRouter.GetLeaseHolderNodeIDs(ctx, false)
			if err != nil {
				return err
			}
			nodeID = append(nodeID, nodeIDs...)
		}
		newPlanNode = &tsDDLNode{d: d, nodeID: nodeID}
	case createKwdbInsTable:
		tsIns := tsInsertNodePool.Get().(*tsInsertNode)
		payInfo := []*sqlbase.SinglePayloadInfo{{
			Payload:       d.CTable.CTable.Payloads[0],
			RowNum:        1,
			PrimaryTagKey: d.CTable.CTable.PrimaryKeys[0],
		}}
		*tsIns = tsInsertNode{
			nodeIDs:             []roachpb.NodeID{roachpb.NodeID(d.CTable.CTable.NodeIDs[0])},
			allNodePayloadInfos: [][]*sqlbase.SinglePayloadInfo{payInfo},
		}
		newPlanNode = tsIns
	case Compress, Retention:
		log.Infof(ctx, "%s job start, jobID: %d", opType, sw.job.ID())
		var desc []sqlbase.TableDescriptor
		var allDesc []sqlbase.DescriptorProto
		nodeList, err := api.GetHealthyNodeIDs(ctx)
		if err != nil {
			return err
		}
		if err = sw.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			allDesc, err = GetAllDescriptors(ctx, txn)
			if err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		for _, table := range allDesc {
			tableDesc, ok := table.(*sqlbase.TableDescriptor)
			if ok && tableDesc.IsTSTable() && tableDesc.State == sqlbase.TableDescriptor_PUBLIC {
				desc = append(desc, *tableDesc)
			}
		}
		if len(desc) == 0 {
			return nil
		}
		newPlanNode = &operateDataNode{d.Type, nodeList, desc}
	default:
		return pgerror.New(pgcode.FeatureNotSupported, "unsupported feature for now")
	}
	log.Infof(ctx, "%s AE execution start, jobID: %d", opType, sw.job.ID())
	return sw.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		_, err := sw.p.makeNewPlanAndRun(ctx, txn, newPlanNode)
		return err
	})
}

func (p *planner) makeNewPlanAndRun(
	ctx context.Context, txn *kv.Txn, newPlanNode planNode,
) (int, error) {
	// Create an internal planner as the planner used to serve the user query
	// would have committed by this point.
	plan := *p
	localPlanner := &plan
	localPlanner.curPlan.plan = newPlanNode
	defer localPlanner.curPlan.close(ctx)
	res := roachpb.BulkOpSummary{}
	rw := newCallbackResultWriter(func(ctx context.Context, row tree.Datums) error {
		var counts roachpb.BulkOpSummary
		if err := protoutil.Unmarshal([]byte(*row[0].(*tree.DBytes)), &counts); err != nil {
			return err
		}
		res.Add(counts)
		return nil
	})
	recv := MakeDistSQLReceiver(
		ctx,
		rw,
		tree.DDL,
		p.execCfg.RangeDescriptorCache,
		p.execCfg.LeaseHolderCache,
		txn,
		func(ts hlc.Timestamp) {
			p.execCfg.Clock.Update(ts)
		},
		// Make a session tracing object on-the-fly. This is OK
		// because it sets "enabled: false" and thus none of the
		// other fields are used.
		&SessionTracing{},
	)
	defer recv.Release()
	rec, err := p.DistSQLPlanner().checkSupportForNode(localPlanner.curPlan.plan)
	var planAndRunErr error
	var rowAffectNum int
	localPlanner.runWithOptions(resolveFlags{skipCache: true}, func() {
		isLocal := err != nil || rec == cannotDistribute
		evalCtx := localPlanner.ExtendedEvalContext()
		planCtx := p.DistSQLPlanner().NewPlanningCtx(ctx, evalCtx, txn)
		planCtx.isLocal = isLocal
		planCtx.planner = localPlanner
		planCtx.stmtType = recv.stmtType
		// Create a physical plan and execute it.
		p.DistSQLPlanner().PlanAndRun(
			ctx,
			evalCtx,
			planCtx,
			txn,
			localPlanner.curPlan.plan,
			recv,
			localPlanner.GetStmt(),
		)
		if planAndRunErr = rw.Err(); planAndRunErr != nil {
			return
		}
		if planAndRunErr = recv.commErr; planAndRunErr != nil {
			return
		}
		if r, ok := recv.resultWriter.(*callbackResultWriter); ok {
			rowAffectNum = r.rowsAffected
		}
	})
	return rowAffectNum, planAndRunErr
}

/*
sendTsTxn makes a new plan to send commit or rollback to wal for ts DDL.
Input:

	ctx:context,
	d(SyncMetaCacheDetails):job detail which contains ddl type and job ID as txn ID.
	event(txnEvent): commit or rollback.

Output:

	error
*/
func (sw *TSSchemaChangeWorker) sendTsTxn(
	ctx context.Context, d jobspb.SyncMetaCacheDetails, event txnEvent,
) error {
	switch d.Type {
	case alterKwdbAddTag, alterKwdbAddColumn, alterKwdbDropColumn, alterKwdbDropTag,
		alterKwdbAlterTagType, alterKwdbAlterColumnType:
		nodeList, err := api.GetHealthyNodeIDs(ctx)
		if err != nil {
			return err
		}
		txnID := strconv.AppendInt([]byte{}, *sw.job.ID(), 10)
		tsTxn := tsTxn{txnID: txnID, txnEvent: event}
		newPlanNode := &tsDDLNode{d: d, nodeID: nodeList, tsTxn: tsTxn}
		return sw.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			_, err := sw.p.makeNewPlanAndRun(ctx, txn, newPlanNode)
			return err
		})
	default:
		return nil
	}
}

func (sw *TSSchemaChangeWorker) checkReplica(ctx context.Context, tableID sqlbase.ID) error {
	// if isComplete is false after check replica status 30 times, return error.
	for r := retry.StartWithCtx(ctx, retry.Options{
		InitialBackoff: 20 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		Multiplier:     2,
		MaxRetries:     30,
	}); r.Next(); {
		isComplete := true
		startKey := sqlbase.MakeTsHashPointKey(tableID, 0)
		endKey := sqlbase.MakeTsHashPointKey(tableID, api.HashParam)
		if sw.p.ExecCfg().StartMode == StartMultiReplica {
			isComplete, _ = sw.db.AdminReplicaStatusConsistent(ctx, startKey, endKey)
		}
		if isComplete {
			return nil
		}
	}
	return pgerror.New(pgcode.Warning, "have tried 30 times, timed out of AdminReplicaVoterStatusConsistent")
}

func getDDLOpType(op int32) string {
	switch op {
	case createKwdbTsTable:
		return "create ts table"
	case dropKwdbTsTable:
		return "drop ts table"
	case dropKwdbTsDatabase:
		return "drop ts database"
	case alterKwdbAddTag:
		return "add tag"
	case alterKwdbDropTag:
		return "drop tag"
	case alterKwdbAlterTagType:
		return "alter tag type"
	case alterKwdbAddColumn:
		return "add column"
	case alterKwdbDropColumn:
		return "drop column"
	case alterKwdbAlterColumnType:
		return "alter column type"
	case alterKwdbAlterPartitionInterval:
		return "alter partition interval"
	case Compress:
		return "compress"
	case Retention:
		return "clean up expired data"
	case alterCompressInterval:
		return "alter compress interval"
	}
	return ""
}