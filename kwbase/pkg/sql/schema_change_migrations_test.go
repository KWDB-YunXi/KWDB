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

package sql_test

import (
	"context"
	gosql "database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/jobs"
	"gitee.com/kwbasedb/kwbase/pkg/jobs/jobspb"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/security"
	"gitee.com/kwbasedb/kwbase/pkg/sql"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/tests"
	"gitee.com/kwbasedb/kwbase/pkg/sqlmigrations"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/jobutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/ctxgroup"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/syncutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

type BlockState int

// These are the states that we want to block the 19.2 style schema change and
// ensure that it can be migrated properly when it is in that state.
const (
	BeforeBackfill BlockState = iota
	AfterBackfill
	AfterReversingMutations // Only used if the job was canceled.
	WaitingForGC            // Only applies to DROP INDEX, DROP TABLE, TRUNCATE TABLE.
	AfterTableGC            // Only applies to DROP DATABASE (after GCing 1 of 2 tables).
)

type SchemaChangeType int

const (
	AddColumn SchemaChangeType = iota
	DropColumn
	CreateIndex
	DropIndex
	AddConstraint
	DropConstraint
	CreateTable
	DropTable
	TruncateTable
	DropDatabase
)

const setup = `
CREATE DATABASE t;
CREATE TABLE t.test (k INT PRIMARY KEY, v INT, INDEX k_idx (k), CONSTRAINT k_cons CHECK (k > 0));
INSERT INTO t.test VALUES (1, 2);
`

// runsBackfill is a set of schema change types that run a backfill.
var runsBackfill = map[SchemaChangeType]bool{
	AddColumn:   true,
	DropColumn:  true,
	CreateIndex: true,
	DropIndex:   true,
}

func isDeletingTable(schemaChangeType SchemaChangeType) bool {
	return schemaChangeType == TruncateTable || schemaChangeType == DropTable
}

func checkBlockedSchemaChange(
	t *testing.T, runner *sqlutils.SQLRunner, testCase migrationTestCase,
) {
	if testCase.blockState == WaitingForGC || testCase.blockState == AfterTableGC {
		// Earlier we turned the 20.1 GC job into a 19.2 schema change job. Delete
		// the original schema change job which is now succeeded, to avoid having
		// special cases later, since we rely heavily on the index of the job row in
		// the jobs table when verifying a job.
		//
		// First, though, we have to actually wait for the original job to become
		// Succeeded.
		runner.CheckQueryResultsRetry(t,
			"SELECT count(*) FROM [SHOW JOBS] WHERE job_type = 'SCHEMA CHANGE' AND status = 'succeeded'",
			[][]string{{"1"}},
		)
		rows := runner.QueryStr(
			t,
			"SELECT * FROM [SHOW JOBS] WHERE job_type = 'SCHEMA CHANGE' AND status = 'succeeded'",
		)
		jobID, _ := strconv.Atoi(rows[0][0])
		runner.Exec(t, "DELETE FROM system.jobs WHERE id = $1", jobID)
	}

	oldVersion := jobutils.GetJobFormatVersion(t, runner)
	require.Equal(t, jobspb.BaseFormatVersion, oldVersion)
	expStatus := jobs.StatusRunning
	if testCase.shouldCancel {
		expStatus = jobs.StatusReverting
	}
	if err := jobutils.VerifySystemJob(t, runner, 0, jobspb.TypeSchemaChange, expStatus, jobs.Record{
		Description:   testCase.schemaChange.query,
		Username:      security.RootUser,
		DescriptorIDs: getTableIDsUnderTest(testCase.schemaChange.kind),
	}); err != nil {
		t.Fatal(err)
	}

	if !hadJobInOldVersion(testCase.schemaChange.kind) {
		// Delete the job if it didn't have a schema change before.
		rows := runner.QueryStr(t, "SELECT * FROM [SHOW JOBS] WHERE job_type = 'SCHEMA CHANGE'")
		for _, job := range rows {
			jobID, _ := strconv.Atoi(job[0])
			runner.Exec(t, "DELETE FROM system.jobs WHERE id = $1", jobID)
		}
	}
}

type schemaChangeRequest struct {
	kind  SchemaChangeType
	query string
}

type migrationTestCase struct {
	blockState   BlockState
	shouldCancel bool
	schemaChange schemaChangeRequest
}

// testSchemaChangeMigrations tests that a schema change can be migrated after
// being blocked in a certain state.
//
// 1. Create a 20.1 schema change.
// 2. Block the schema change at a certain point in its execution.
// 3. Mutate the job descriptor and table descriptor such that it appears as a
// 19.2 format job. These jobs will not be resumed anymore as 20.1 will refuse
// to run 19.2 jobs.
// 4. Verify that the job has been marked as a 19.2 job and is blocked.
// 5. Run the migration and wait for the migration to complete.
// 6. Ensure that the schema change completes.
func testSchemaChangeMigrations(t *testing.T, testCase migrationTestCase) {
	ctx := context.Background()
	shouldSignalMigration := int32(0)
	blockFnErrChan := make(chan error, 1)
	revMigrationDoneCh, signalRevMigrationDone := makeSignal()
	migrationDoneCh, signalMigrationDone := makeCondSignal(&shouldSignalMigration)
	g := ctxgroup.WithContext(context.Background())
	runner, sqlDB, tc := setupServerAndStartSchemaChange(
		t,
		g,
		blockFnErrChan,
		testCase,
		signalRevMigrationDone,
		signalMigrationDone,
	)
	defer tc.Stopper().Stop(context.TODO())
	defer disableGCTTLStrictEnforcement(t, sqlDB)()

	log.Info(ctx, "waiting for all schema changes to block")
	<-revMigrationDoneCh
	log.Info(ctx, "all schema changes have blocked")

	close(blockFnErrChan)
	for err := range blockFnErrChan {
		if err != nil {
			t.Fatalf("%+v", err)
		}
	}

	checkBlockedSchemaChange(t, runner, testCase)

	// Start the migrations.
	log.Info(ctx, "starting job migration")
	atomic.StoreInt32(&shouldSignalMigration, 1)
	migMgr := tc.Server(0).MigrationManager().(*sqlmigrations.Manager)
	if err := migMgr.StartSchemaChangeJobMigration(ctx); err != nil {
		t.Fatal(err)
	}

	log.Info(ctx, "waiting for migration to complete")
	<-migrationDoneCh

	// The original schema change, which has had its job modified, should return a
	// result at this point now that all relevant jobs are in a terminal state (or
	// no longer exist?). But the result doesn't matter.
	_ = g.Wait()

	if testCase.schemaChange.kind == DropDatabase {
		// TODO(lucy): Another hardcoded table ID to hopefully someday get rid of.
		// This is for the other table in the database that hasn't been dropped
		// already by the GC job. We set the GC TTL for it now to get the entire
		// drop database GC job to finish.
		if _, err := addImmediateGCZoneConfig(sqlDB, sqlbase.ID(79)); err != nil {
			t.Fatal(err)
		}
	}
	// TODO(pbardea): SHOW JOBS WHEN COMPLETE SELECT does not work on some schema
	// changes when canceling jobs, but querying until there are no jobs works.
	//runner.Exec(t, "SHOW JOBS WHEN COMPLETE SELECT job_id FROM [SHOW JOBS] WHERE (job_type = 'SCHEMA CHANGE' OR job_type = 'SCHEMA CHANGE GC')")
	// Wait until there are no more running schema changes.
	log.Info(ctx, "waiting for new schema change jobs to complete")
	runner.CheckQueryResultsRetry(t, "SELECT * FROM [SHOW JOBS] WHERE (job_type = 'SCHEMA CHANGE' OR job_type = 'SCHEMA CHANGE GC') AND NOT (status = 'succeeded' OR status = 'canceled')", [][]string{})
	log.Info(ctx, "done running new schema change jobs")

	verifySchemaChangeJobRan(t, runner, testCase)
}

func makeCondSignal(shouldSignal *int32) (chan struct{}, func()) {
	signalCh := make(chan struct{})
	signalFn := func() {
		if atomic.LoadInt32(shouldSignal) == 1 {
			signalCh <- struct{}{}
		}
	}
	return signalCh, signalFn
}

func makeSignal() (chan struct{}, func()) {
	alwaysSignal := int32(1)
	return makeCondSignal(&alwaysSignal)
}

func setupServerAndStartSchemaChange(
	t *testing.T,
	g ctxgroup.Group,
	errCh chan error,
	testCase migrationTestCase,
	revMigrationDone, signalMigrationDone func(),
) (*sqlutils.SQLRunner, *gosql.DB, serverutils.TestClusterInterface) {
	clusterSize := 3
	params, _ := tests.CreateTestServerParams()

	var runner *sqlutils.SQLRunner
	var kvDB *kv.DB
	var registry *jobs.Registry

	blockSchemaChanges := false

	migrateJob := func(jobID int64) {
		if testCase.blockState == WaitingForGC || testCase.blockState == AfterTableGC {
			if err := migrateGCJobToOldFormat(kvDB, registry, jobID, testCase.schemaChange.kind); err != nil {
				errCh <- err
			}
		} else {
			if err := migrateJobToOldFormat(kvDB, registry, jobID, testCase.schemaChange.kind); err != nil {
				errCh <- err
			}
		}
	}
	cancelJob := func(jobID int64) {
		runner.Exec(t, `CANCEL JOB (
					SELECT job_id FROM [SHOW JOBS]
					WHERE
						job_id = $1
				)`, jobID)
	}

	setupTestingKnobs(t, testCase, &params, &blockSchemaChanges, revMigrationDone, signalMigrationDone, migrateJob, cancelJob)

	tc := serverutils.StartTestCluster(t, clusterSize,
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs:      params,
		})
	sqlDB := tc.ServerConn(0)
	kvDB = tc.Server(0).DB()
	runner = sqlutils.MakeSQLRunner(sqlDB)
	registry = tc.Server(0).JobRegistry().(*jobs.Registry)

	if _, err := sqlDB.Exec(setup); err != nil {
		t.Fatal(err)
	}
	// TODO (lucy): The rest of this test should also be using this value instead
	// of the hardcoded ID.
	var testTableID sqlbase.ID
	require.NoError(t, sqlDB.QueryRow(`SELECT 't.test'::regclass::int`).Scan(&testTableID))

	runner.CheckQueryResultsRetry(t, "SELECT count(*) FROM [SHOW JOBS] WHERE job_type = 'SCHEMA CHANGE' AND NOT (status = 'succeeded' OR status = 'canceled')", [][]string{{"0"}})
	blockSchemaChanges = true

	g.GoCtx(func(ctx context.Context) error {
		// If we're dropping the database, also add another table to it, so that the
		// migration runs when t.test has been GC'ed already but the other table
		// hasn't.
		if testCase.schemaChange.kind == DropDatabase {
			if _, err := sqlDB.ExecContext(ctx, `CREATE TABLE t.long_gc_ttl()`); err != nil {
				return err
			}
		}
		if _, err := sqlDB.ExecContext(ctx, testCase.schemaChange.query); err != nil {
			return err
		}
		return nil
	})
	if _, err := addImmediateGCZoneConfig(sqlDB, testTableID); err != nil {
		t.Fatal(err)
	}
	return runner, sqlDB, tc
}

// migrateJobToOldFormat updates the state of a job and table descriptor from
// it's 20.1 to its 19.2 representation. There is a separate implementation for
// GC jobs.
func migrateJobToOldFormat(
	kvDB *kv.DB, registry *jobs.Registry, jobID int64, schemaChangeType SchemaChangeType,
) error {
	ctx := context.Background()

	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")
	if schemaChangeType == CreateTable {
		tableDesc = sqlbase.GetTableDescriptor(kvDB, "t", "new_table")
	}

	if err := kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		job, err := registry.LoadJobWithTxn(ctx, jobID, txn)
		if err != nil {
			return err
		}
		return job.WithTxn(txn).Update(ctx, func(txn *kv.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			details := job.Details().(jobspb.SchemaChangeDetails)
			// Explicitly zero out these fields as they will be set to their 0 value
			// on 19.2 nodes.
			details.TableID = 0
			details.MutationID = 0
			details.FormatVersion = jobspb.BaseFormatVersion
			if isDeletingTable(schemaChangeType) {
				details.DroppedTables = []jobspb.DroppedTableDetails{
					{
						Name:   tableDesc.Name,
						ID:     tableDesc.ID,
						Status: jobspb.Status_DRAINING_NAMES,
					},
				}
			}

			progress := job.Progress()
			// TODO(pbardea): Probably want to change this to check on block state
			// being draining names.
			if isDeletingTable(schemaChangeType) {
				progress.RunningStatus = string(sql.RunningStatusDrainingNames)
			}

			md.Payload.Lease = nil
			md.Payload.Details = jobspb.WrapPayloadDetails(details)
			md.Progress = &progress
			ju.UpdatePayload(md.Payload)
			ju.UpdateProgress(md.Progress)
			return nil
		})
	}); err != nil {
		return err
	}

	// Update the table descriptor.
	tableDesc.Lease = &sqlbase.TableDescriptor_SchemaChangeLease{
		ExpirationTime: timeutil.Now().UnixNano(),
		NodeID:         roachpb.NodeID(0),
	}
	if schemaChangeType == TruncateTable {
		tableDesc.DropJobID = jobID
		// TODO(pbardea): When is drop time populated?
	}

	// Write the table descriptor back.
	return kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		if err := txn.SetSystemConfigTrigger(); err != nil {
			return err
		}
		return kvDB.Put(ctx, sqlbase.MakeDescMetadataKey(tableDesc.GetID()), sqlbase.WrapDescriptor(tableDesc))
	})
}

// migrateGCJobToOldFormat converts a GC job created in 20.1 into a 19.2-style
// schema change job that is waiting for GC. This involves changing the type of
// the job details and progress.
//
// We could have gone back and set the original schema change job to Running,
// but then we'd have to update that job from inside the GC job testing knob
// function, which seems risky since we have no way of controlling that schema
// change job once it's eligible to be adopted.
func migrateGCJobToOldFormat(
	kvDB *kv.DB, registry *jobs.Registry, jobID int64, schemaChangeType SchemaChangeType,
) error {
	ctx := context.Background()

	if err := kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		job, err := registry.LoadJobWithTxn(ctx, jobID, txn)
		if err != nil {
			return err
		}
		return job.WithTxn(txn).Update(ctx, func(txn *kv.Txn, md jobs.JobMetadata, ju *jobs.JobUpdater) error {
			// Replace the details with an entirely new SchemaChangeDetails.
			details := jobspb.SchemaChangeDetails{
				FormatVersion: jobspb.BaseFormatVersion,
			}
			if isDeletingTable(schemaChangeType) {
				details.DroppedTables = []jobspb.DroppedTableDetails{
					{
						// TODO (lucy): Stop hard-coding these if possible. We can't get
						// these values from the table descriptor if we're dropping the
						// table, since at this point the table descriptor would have been
						// deleted.
						Name:   "test",
						ID:     79,
						Status: jobspb.Status_WAIT_FOR_GC_INTERVAL,
					},
				}
			} else if schemaChangeType == DropDatabase {
				details.DroppedTables = []jobspb.DroppedTableDetails{
					{
						Name:   "test",
						ID:     79,
						Status: jobspb.Status_DONE,
					},
					{
						Name:   "long_ttl_table",
						ID:     80,
						Status: jobspb.Status_WAIT_FOR_GC_INTERVAL,
					},
				}
			}

			progress := jobspb.Progress{
				Details:       jobspb.WrapProgressDetails(jobspb.SchemaChangeProgress{}),
				RunningStatus: string(sql.RunningStatusWaitingGC),
			}

			md.Payload.Lease = nil
			md.Payload.Description = strings.TrimPrefix(md.Payload.Description, "GC for ")
			md.Payload.Details = jobspb.WrapPayloadDetails(details)
			md.Progress = &progress
			ju.UpdatePayload(md.Payload)
			ju.UpdateProgress(md.Progress)
			return nil
		})
	}); err != nil {
		return err
	}

	switch schemaChangeType {
	case DropTable, DropDatabase:
		// There's no table descriptor to update, so we're done.
		return nil

	case DropIndex:
		tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")
		if l := len(tableDesc.GCMutations); l != 1 {
			return errors.AssertionFailedf("expected exactly 1 GCMutation, found %d", l)
		}

		// Update the table descriptor.
		tableDesc.Lease = &sqlbase.TableDescriptor_SchemaChangeLease{
			ExpirationTime: timeutil.Now().UnixNano(),
			NodeID:         roachpb.NodeID(0),
		}

		tableDesc.GCMutations[0].JobID = jobID
		tableDesc.GCMutations[0].DropTime = timeutil.Now().UnixNano()

		// Write the table descriptor back.
		return kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			if err := txn.SetSystemConfigTrigger(); err != nil {
				return err
			}
			return kvDB.Put(ctx, sqlbase.MakeDescMetadataKey(tableDesc.GetID()), sqlbase.WrapDescriptor(tableDesc))
		})
	default:
		return errors.Errorf("invalid schema change type: %d", schemaChangeType)
	}
}

// Set up server testing args such that knobs are set to block and abandon any
// given schema change at a certain point. The "blocked" channel will be
// signaled when the schema change gets abandoned.
// The runner should only be used inside callback closures.
func setupTestingKnobs(
	t *testing.T,
	testCase migrationTestCase,
	args *base.TestServerArgs,
	blockSchemaChanges *bool,
	revMigrationDone, signalMigrationDone func(),
	migrateJob, cancelJob func(int64),
) {
	numJobs := 1
	if testCase.schemaChange.kind == CreateTable {
		numJobs = 2
	}
	var (
		mu                   syncutil.Mutex
		migratedCount        int
		doneReverseMigration bool
		ranCancelCommand     bool
		hasCanceled          bool
	)

	blockFn := func(jobID int64) error {
		mu.Lock()
		defer mu.Unlock()
		if !(*blockSchemaChanges) {
			return nil
		}

		// In the case we're canceling the job, this blockFn should only be called
		// after the OnFailOrCancel hook is called. At this point we know that the
		// job is actually canceled.
		hasCanceled = true

		if doneReverseMigration {
			// Already migrated all the jobs that we want to migrate to 19.2.
			// New jobs created after we migrated the original batch should be allowed
			// to continue.
			return nil
		} else {
			migrateJob(jobID)
			migratedCount++
		}

		if migratedCount == numJobs {
			doneReverseMigration = true
			revMigrationDone()
		}

		// Return a retryable error so that the job doesn't make any progress past
		// this point. It should not get adopted since it has been marked as a 19.2
		// job.
		return jobs.NewRetryJobError("stop this job until cluster upgrade")
	}

	cancelFn := func(jobID int64) error {
		mu.Lock()
		defer mu.Unlock()
		if hasCanceled {
			// The job has already been successfully canceled.
			return nil
		}

		if !ranCancelCommand {
			cancelJob(jobID)
			ranCancelCommand = true
		}

		// Don't allow the job to progress further than this knob until it has
		// actually been canceled	.
		return jobs.NewRetryJobError("retry until canceled")
	}

	knobs := &sql.SchemaChangerTestingKnobs{}
	gcKnobs := &sql.GCJobTestingKnobs{}

	shouldCancel := testCase.shouldCancel
	if shouldCancel {
		if runsBackfill[testCase.schemaChange.kind] {
			knobs.RunAfterBackfill = cancelFn
		} else {
			knobs.RunBeforeResume = cancelFn
		}
	}

	switch testCase.blockState {
	case BeforeBackfill:
		if shouldCancel {
			knobs.RunBeforeOnFailOrCancel = blockFn
		} else {
			knobs.RunBeforeResume = blockFn
		}
	case AfterBackfill:
		if shouldCancel {
			// This is a special case where (1) RunAfterBackfill within Resume() needs
			// to call cancelFn() to cancel the job, (2) RunBeforeOnFailOrCancel needs
			// to set hasCanceled, and (3) RunAfterBackfill, running for the 2nd time
			// within OnFailOrCancel(), needs to read the value of hasCanceled (which
			// is true) and run BlockFn().
			knobs.RunBeforeOnFailOrCancel = func(jobID int64) error {
				mu.Lock()
				defer mu.Unlock()
				hasCanceled = true
				return nil
			}
			knobs.RunAfterBackfill = func(jobID int64) error {
				mu.Lock()
				hasCanceled := hasCanceled
				mu.Unlock()
				if hasCanceled {
					return blockFn(jobID)
				} else {
					return cancelFn(jobID)
				}
			}
		} else {
			knobs.RunAfterBackfill = blockFn
		}
	case AfterReversingMutations:
		if !shouldCancel {
			t.Fatal("can only block after reversing mutations if the job is expected to be canceled")
		}
		knobs.RunAfterBackfill = cancelFn
		knobs.RunAfterMutationReversal = blockFn
	case WaitingForGC:
		if shouldCancel {
			t.Fatal("cannot block on waiting for GC if the job should also be canceled")
		}
		gcKnobs.RunBeforeResume = blockFn
	case AfterTableGC:
		if shouldCancel {
			t.Fatal("cannot block after table GC if the job should also be canceled")
		}
		gcKnobs.RunAfterGC = blockFn
	}

	args.Knobs.SQLSchemaChanger = knobs
	args.Knobs.SQLMigrationManager = &sqlmigrations.MigrationManagerTestingKnobs{
		AfterJobMigration:     signalMigrationDone,
		AlwaysRunJobMigration: true,
	}
	args.Knobs.GCJob = gcKnobs
}

func getTestName(schemaChange SchemaChangeType, blockState BlockState, shouldCancel bool) string {
	stateNames := map[BlockState]string{
		BeforeBackfill:          "before-backfill",
		AfterBackfill:           "after-backfill",
		AfterReversingMutations: "after-reversing-mutations",
		WaitingForGC:            "waiting-for-gc",
		AfterTableGC:            "after-table-gc",
	}
	schemaChangeName := map[SchemaChangeType]string{
		AddColumn:      "add-column",
		DropColumn:     "drop-column",
		CreateIndex:    "add-index",
		DropIndex:      "drop-index",
		AddConstraint:  "add-constraint",
		DropConstraint: "drop-constraint",
		CreateTable:    "create-table",
		TruncateTable:  "truncate-table",
		DropTable:      "drop-table",
		DropDatabase:   "drop-database",
	}

	testName := fmt.Sprintf("%s-blocked-at-%s", schemaChangeName[schemaChange], stateNames[blockState])
	if shouldCancel {
		testName += "-canceled"
	}
	return testName
}

func verifySchemaChangeJobRan(
	t *testing.T, runner *sqlutils.SQLRunner, testCase migrationTestCase,
) {
	expStatus := jobs.StatusSucceeded
	description := testCase.schemaChange.query
	if testCase.shouldCancel {
		expStatus = jobs.StatusCanceled
	}
	if testCase.schemaChange.kind == CreateTable {
		description = "adding table 80"
	} else {
		if err := jobutils.VerifySystemJob(t, runner, 0, jobspb.TypeSchemaChange, expStatus, jobs.Record{
			Description:   description,
			Username:      security.RootUser,
			DescriptorIDs: getTableIDsUnderTest(testCase.schemaChange.kind),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Verify that the GC job exists and is in the correct state, if applicable.
	if testCase.blockState == WaitingForGC || testCase.blockState == AfterTableGC {
		descriptorIDs := getTableIDsUnderTest(testCase.schemaChange.kind)
		if testCase.blockState == AfterTableGC {
			// The database had two tables, but one was GC'ed before the new job was
			// reconstituted, so the job only knows about one table.
			descriptorIDs = sqlbase.IDs{80}
		}

		if err := jobutils.VerifySystemJob(t, runner, 0, jobspb.TypeSchemaChangeGC, jobs.StatusSucceeded, jobs.Record{
			Description:   "GC for " + description,
			Username:      security.RootUser,
			DescriptorIDs: descriptorIDs,
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		// For non-GC jobs, verify that the schema change job format version was
		// updated.
		newVersion := jobutils.GetJobFormatVersion(t, runner)
		require.Equal(t, jobspb.JobResumerFormatVersion, newVersion)
	}

	var expected [][]string
	didCancel := testCase.shouldCancel
	switch testCase.schemaChange.kind {
	case AddColumn:
		if didCancel {
			expected = [][]string{{"1", "2"}}
		} else {
			expected = [][]string{{"1", "2", "NULL"}}
		}
		rows := runner.QueryStr(t, "SELECT * FROM t.test")
		require.Equal(t, expected, rows)
	case DropColumn:
		if didCancel {
			expected = [][]string{{"1", "NULL"}}
		} else {
			expected = [][]string{{"1"}}
		}
		rows := runner.QueryStr(t, "SELECT * FROM t.test")
		require.Equal(t, expected, rows)
	case CreateIndex:
		if didCancel {
			expected = [][]string{{"primary"}, {"k_idx"}}
		} else {
			expected = [][]string{{"primary"}, {"k_idx"}, {"v_idx"}}
		}
		rows := runner.QueryStr(t, "SELECT DISTINCT index_name FROM [SHOW INDEXES FROM t.test]")
		require.Equal(t, expected, rows)
	case DropIndex:
		if didCancel {
			expected = [][]string{{"primary"}, {"k_idx"}}
		} else {
			expected = [][]string{{"primary"}}
		}
		rows := runner.QueryStr(t, "SELECT DISTINCT index_name FROM [SHOW INDEXES FROM t.test]")
		require.Equal(t, expected, rows)
	case AddConstraint:
		if didCancel {
			expected = [][]string{{"k_cons"}, {"primary"}}
		} else {
			expected = [][]string{{"k_cons"}, {"primary"}, {"v_unq"}}
		}
		rows := runner.QueryStr(t, "SELECT constraint_name FROM [SHOW CONSTRAINTS FROM t.test] ORDER BY constraint_name")
		require.Equal(t, expected, rows)
	case DropConstraint:
		if didCancel {
			expected = [][]string{{"k_cons"}, {"primary"}}
		} else {
			expected = [][]string{{"primary"}}
		}
		rows := runner.QueryStr(t, "SELECT constraint_name FROM [SHOW CONSTRAINTS FROM t.test] ORDER BY constraint_name")
		require.Equal(t, expected, rows)
	case CreateTable:
		if didCancel {
			t.Fatal("cannot cancel create table")
		} else {
			expected = [][]string{{"new_table"}, {"test"}}
		}
		rows := runner.QueryStr(t, "SHOW TABLES FROM t")
		require.Equal(t, expected, rows)
	case TruncateTable:
		if didCancel {
			expected = [][]string{{"0"}}
		} else {
			expected = [][]string{{"0"}}
		}
		rows := runner.QueryStr(t, "SELECT count(*) FROM t.test")
		require.Equal(t, expected, rows)
	case DropTable:
		// Canceling after the backfill has no effect.
		expected = [][]string{}
		rows := runner.QueryStr(t, "SHOW TABLES FROM t")
		require.Equal(t, expected, rows)
	case DropDatabase:
		expected = [][]string{}
		rows := runner.QueryStr(t, "SELECT * FROM system.namespace WHERE name = 't' OR name = 'test' OR name = 'long_ttl_table'")
		require.Equal(t, expected, rows)
	}
}

func getTableIDsUnderTest(schemaChangeType SchemaChangeType) []sqlbase.ID {
	if schemaChangeType == CreateTable {
		return []sqlbase.ID{80}
	} else if schemaChangeType == DropDatabase {
		return []sqlbase.ID{79, 80}
	} else {
		return []sqlbase.ID{79}
	}
}

// Helpers used to determine valid test cases.

// canBlockIfCanceled returns if a certain state (where we want to block the
// schema change) will be reached given if the job was canceled or not.
func canBlockIfCanceled(blockState BlockState, shouldCancel bool) bool {
	// States that are only valid when the job is canceled.
	if blockState == WaitingForGC || blockState == AfterTableGC {
		return !shouldCancel
	}
	if blockState == AfterReversingMutations {
		return shouldCancel
	}
	return true
}

// Ensures that the given schema change actually passes through the state where
// we're proposing to block.
func validBlockStateForSchemaChange(blockState BlockState, schemaChangeType SchemaChangeType) bool {
	if schemaChangeType == DropDatabase {
		return blockState == AfterTableGC
	}
	switch blockState {
	case AfterBackfill:
		return runsBackfill[schemaChangeType]
	case WaitingForGC:
		return schemaChangeType == DropIndex || schemaChangeType == DropTable
	case AfterTableGC:
		return schemaChangeType == DropDatabase
	}
	return true
}

// hasJobInOldVersion returns if a given schema change had a job in 19.2.
// Therefore these jobs could not be canceled in 19.2
func hadJobInOldVersion(schemaChangeType SchemaChangeType) bool {
	return schemaChangeType != CreateTable
}

// TestGCJobCreated tests that a table descriptor in the DROP state with no
// running job has a GC job created for it.
func TestGCJobCreated(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setTestJobsAdoptInterval()()
	params, _ := tests.CreateTestServerParams()
	params.Knobs.SQLMigrationManager = &sqlmigrations.MigrationManagerTestingKnobs{
		AlwaysRunJobMigration: true,
	}
	s, sqlDB, kvDB := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())
	ctx := context.Background()
	sqlRunner := sqlutils.MakeSQLRunner(sqlDB)

	// Create a table and then force it to be in the DROP state.
	if _, err := sqlDB.Exec(`CREATE DATABASE t; CREATE TABLE t.test();`); err != nil {
		t.Fatal(err)
	}
	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")
	tableDesc.State = sqlbase.TableDescriptor_DROP
	tableDesc.Version++
	tableDesc.DropTime = 1
	if err := kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		if err := txn.SetSystemConfigTrigger(); err != nil {
			return err
		}
		if err := sqlbase.RemoveObjectNamespaceEntry(ctx, txn, tableDesc.ID, tableDesc.ParentID, tableDesc.Name, false /* kvTrace */); err != nil {
			return err
		}
		return kvDB.Put(ctx, sqlbase.MakeDescMetadataKey(tableDesc.GetID()), sqlbase.WrapDescriptor(tableDesc))
	}); err != nil {
		t.Fatal(err)
	}

	// Run the migration.
	migMgr := s.MigrationManager().(*sqlmigrations.Manager)
	if err := migMgr.StartSchemaChangeJobMigration(ctx); err != nil {
		t.Fatal(err)
	}

	// Check that a GC job was created and completed successfully.
	sqlRunner.CheckQueryResultsRetry(t,
		"SELECT count(*) FROM [SHOW JOBS] WHERE job_type = 'SCHEMA CHANGE GC' AND status = 'succeeded'",
		[][]string{{"1"}},
	)
}

// TestMissingMutation tests that a malformed table descriptor with a
// MutationJob but no Mutation for the given job causes the job to fail with an
// error. Regression test for #48786.
func TestMissingMutation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setTestJobsAdoptInterval()()
	schemaChangeBlocked, descriptorUpdated := make(chan struct{}), make(chan struct{})
	migratedJob := false
	var schemaChangeJobID int64
	params, _ := tests.CreateTestServerParams()
	params.Knobs.SQLMigrationManager = &sqlmigrations.MigrationManagerTestingKnobs{
		AlwaysRunJobMigration: true,
	}
	params.Knobs.SQLSchemaChanger = &sql.SchemaChangerTestingKnobs{
		RunBeforeResume: func(jobID int64) error {
			if !migratedJob {
				migratedJob = true
				schemaChangeJobID = jobID
				close(schemaChangeBlocked)
			}

			<-descriptorUpdated
			return jobs.NewRetryJobError("stop this job until cluster upgrade")
		},
	}
	s, sqlDB, kvDB := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())
	registry := s.JobRegistry().(*jobs.Registry)

	_, err := sqlDB.Exec(`CREATE DATABASE t; CREATE TABLE t.test(k INT PRIMARY KEY, v INT);`)
	require.NoError(t, err)

	g := ctxgroup.WithContext(context.Background())
	// Start a schema change on the table in a separate goroutine.
	g.GoCtx(func(ctx context.Context) error {
		if _, err := sqlDB.ExecContext(ctx, `ALTER TABLE t.test ADD COLUMN a INT;`); err != nil {
			return err
		}
		return nil
	})

	<-schemaChangeBlocked

	// Rewrite the job to be a 19.2-style job.
	require.NoError(t, migrateJobToOldFormat(kvDB, registry, schemaChangeJobID, AddColumn))

	ctx := context.Background()
	// To get the table descriptor into the (invalid) state we're trying to test,
	// clear the mutations on the table descriptor.
	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")
	tableDesc.Mutations = nil
	require.NoError(
		t, kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			if err := txn.SetSystemConfigTrigger(); err != nil {
				return err
			}
			return kvDB.Put(ctx, sqlbase.MakeDescMetadataKey(
				tableDesc.GetID()), sqlbase.WrapDescriptor(tableDesc),
			)
		}),
	)

	// Run the migration.
	migMgr := s.MigrationManager().(*sqlmigrations.Manager)
	require.NoError(t, migMgr.StartSchemaChangeJobMigration(ctx))

	close(descriptorUpdated)

	err = g.Wait()
	require.Regexp(t, fmt.Sprintf("mutation %d not found for MutationJob %d", 1, schemaChangeJobID), err)
}

// TestMissingTable tests that a missing table descriptor for the referenced
// table causes a (single-table) schema change job to be marked as failed.
func TestMissingTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer setTestJobsAdoptInterval()()
	schemaChangeBlocked, descriptorDeleted := make(chan struct{}), make(chan struct{})
	migratedJob := false
	var schemaChangeJobID int64
	params, _ := tests.CreateTestServerParams()
	params.Knobs.SQLMigrationManager = &sqlmigrations.MigrationManagerTestingKnobs{
		AlwaysRunJobMigration: true,
	}
	params.Knobs.SQLSchemaChanger = &sql.SchemaChangerTestingKnobs{
		RunBeforeResume: func(jobID int64) error {
			if !migratedJob {
				migratedJob = true
				schemaChangeJobID = jobID
				close(schemaChangeBlocked)
			}

			<-descriptorDeleted
			return jobs.NewRetryJobError("stop this job until cluster upgrade")
		},
	}
	s, sqlDB, kvDB := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())
	registry := s.JobRegistry().(*jobs.Registry)

	_, err := sqlDB.Exec(`CREATE DATABASE t; CREATE TABLE t.test(k INT PRIMARY KEY, v INT);`)
	require.NoError(t, err)

	g := ctxgroup.WithContext(context.Background())
	// Start a schema change on the table in a separate goroutine.
	g.GoCtx(func(ctx context.Context) error {
		if _, err := sqlDB.ExecContext(ctx, `ALTER TABLE t.test ADD COLUMN a INT;`); err != nil {
			return err
		}
		return nil
	})

	<-schemaChangeBlocked

	// Rewrite the job to be a 19.2-style job.
	require.NoError(t, migrateJobToOldFormat(kvDB, registry, schemaChangeJobID, AddColumn))

	ctx := context.Background()
	// Delete the table descriptor.
	tableDesc := sqlbase.GetTableDescriptor(kvDB, "t", "test")
	tableDesc.Mutations = nil
	require.NoError(
		t, kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			if err := txn.SetSystemConfigTrigger(); err != nil {
				return err
			}
			return kvDB.Del(ctx, sqlbase.MakeDescMetadataKey(tableDesc.GetID()))
		}),
	)

	// Run the migration.
	migMgr := s.MigrationManager().(*sqlmigrations.Manager)
	require.NoError(t, migMgr.StartSchemaChangeJobMigration(ctx))

	close(descriptorDeleted)

	err = g.Wait()
	require.Regexp(t, "descriptor not found", err)
}
