// Copyright 2016 The Cockroach Authors.
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

package sqlmigrations

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/gossip"
	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/sql"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sqlmigrations/leasemanager"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

var (
	noopMigration1 = migrationDescriptor{
		name:   "noop 1",
		workFn: func(_ context.Context, _ runner) error { return nil },
	}
	noopMigration2 = migrationDescriptor{
		name:   "noop 2",
		workFn: func(_ context.Context, _ runner) error { return nil },
	}
	errorMigration = migrationDescriptor{
		name:   "error",
		workFn: func(_ context.Context, _ runner) error { return errors.New("errorMigration") },
	}
	panicMigration = migrationDescriptor{
		name:   "panic",
		workFn: func(_ context.Context, _ runner) error { panic("panicMigration") },
	}
)

type fakeLeaseManager struct {
	extendErr          error
	releaseErr         error
	leaseTimeRemaining time.Duration
}

func (f *fakeLeaseManager) AcquireLease(
	ctx context.Context, key roachpb.Key,
) (*leasemanager.Lease, error) {
	return &leasemanager.Lease{}, nil
}

func (f *fakeLeaseManager) ExtendLease(ctx context.Context, l *leasemanager.Lease) error {
	return f.extendErr
}

func (f *fakeLeaseManager) ReleaseLease(ctx context.Context, l *leasemanager.Lease) error {
	return f.releaseErr
}

func (f *fakeLeaseManager) TimeRemaining(l *leasemanager.Lease) time.Duration {
	// Default to a reasonable amount of time left if the field wasn't set.
	if f.leaseTimeRemaining == 0 {
		return leaseRefreshInterval * 2
	}
	return f.leaseTimeRemaining
}

type fakeDB struct {
	kvs     map[string][]byte
	scanErr error
	putErr  error
}

func (f *fakeDB) Scan(
	ctx context.Context, begin, end interface{}, maxRows int64,
) ([]kv.KeyValue, error) {
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	if !bytes.Equal(begin.(roachpb.Key), keys.MigrationPrefix) {
		return nil, errors.Errorf("expected begin key %q, got %q", keys.MigrationPrefix, begin)
	}
	if !bytes.Equal(end.(roachpb.Key), keys.MigrationKeyMax) {
		return nil, errors.Errorf("expected end key %q, got %q", keys.MigrationKeyMax, end)
	}
	var results []kv.KeyValue
	for k, v := range f.kvs {
		results = append(results, kv.KeyValue{
			Key:   []byte(k),
			Value: &roachpb.Value{RawBytes: v},
		})
	}
	return results, nil
}

func (f *fakeDB) Get(ctx context.Context, key interface{}) (kv.KeyValue, error) {
	return kv.KeyValue{}, errors.New("unimplemented")
}

func (f *fakeDB) Put(ctx context.Context, key, value interface{}) error {
	if f.putErr != nil {
		return f.putErr
	}
	if f.kvs != nil {
		f.kvs[string(key.(roachpb.Key))] = []byte(value.(string))
	}
	return nil
}

func (f *fakeDB) Txn(context.Context, func(context.Context, *kv.Txn) error) error {
	return errors.New("unimplemented")
}

func TestEnsureMigrations(t *testing.T) {
	defer leaktest.AfterTest(t)()
	db := &fakeDB{}
	mgr := Manager{
		stopper:      stop.NewStopper(),
		leaseManager: &fakeLeaseManager{},
		db:           db,
	}
	defer mgr.stopper.Stop(context.TODO())

	fnGotCalled := false
	fnGotCalledDescriptor := migrationDescriptor{
		name: "got-called-verifier",
		workFn: func(context.Context, runner) error {
			fnGotCalled = true
			return nil
		},
	}
	testCases := []struct {
		preCompleted []migrationDescriptor
		migrations   []migrationDescriptor
		expectedErr  string
	}{
		{
			nil,
			nil,
			"",
		},
		{
			nil,
			[]migrationDescriptor{noopMigration1},
			"",
		},
		{
			[]migrationDescriptor{noopMigration1},
			[]migrationDescriptor{noopMigration1},
			"",
		},
		{
			[]migrationDescriptor{},
			[]migrationDescriptor{noopMigration1, noopMigration2},
			"",
		},
		{
			[]migrationDescriptor{noopMigration1},
			[]migrationDescriptor{noopMigration1, noopMigration2},
			"",
		},
		{
			[]migrationDescriptor{noopMigration1, noopMigration2, panicMigration},
			[]migrationDescriptor{noopMigration1, noopMigration2, panicMigration},
			"",
		},
		{
			[]migrationDescriptor{noopMigration1, noopMigration2},
			[]migrationDescriptor{noopMigration1, noopMigration2, fnGotCalledDescriptor},
			"",
		},
		{
			[]migrationDescriptor{noopMigration1, noopMigration2},
			[]migrationDescriptor{noopMigration1, noopMigration2, errorMigration},
			fmt.Sprintf("failed to run migration %q", errorMigration.name),
		},
	}

	defer func(prev []migrationDescriptor) { backwardCompatibleMigrations = prev }(backwardCompatibleMigrations)

	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			db.kvs = make(map[string][]byte)
			for _, name := range tc.preCompleted {
				db.kvs[string(migrationKey(name))] = []byte{}
			}
			backwardCompatibleMigrations = tc.migrations

			err := mgr.EnsureMigrations(context.Background(), roachpb.Version{} /* bootstrapVersion */)
			if !testutils.IsError(err, tc.expectedErr) {
				t.Errorf("expected error %q, got error %v", tc.expectedErr, err)
			}
			if err != nil {
				return
			}

			for _, migration := range tc.migrations {
				if _, ok := db.kvs[string(migrationKey(migration))]; !ok {
					t.Errorf("expected key %s to be written, but it wasn't", migrationKey(migration))
				}
			}
			if len(db.kvs) != len(tc.migrations) {
				t.Errorf("expected %d key to be written, but %d were",
					len(tc.migrations), len(db.kvs))
			}
		})
	}
	if !fnGotCalled {
		t.Errorf("expected fnGotCalledDescriptor to be run by the migration coordinator, but it wasn't")
	}
}

func TestSkipMigrationsIncludedInBootstrap(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	db := &fakeDB{}
	mgr := Manager{
		stopper:      stop.NewStopper(),
		leaseManager: &fakeLeaseManager{},
		db:           db,
	}
	defer mgr.stopper.Stop(ctx)
	defer func(prev []migrationDescriptor) {
		backwardCompatibleMigrations = prev
	}(backwardCompatibleMigrations)

	v := roachpb.MustParseVersion("19.1")
	fnGotCalled := false
	backwardCompatibleMigrations = []migrationDescriptor{{
		name:                "got-called-verifier",
		includedInBootstrap: v,
		workFn: func(context.Context, runner) error {
			fnGotCalled = true
			return nil
		},
	}}
	// If the cluster has been bootstrapped at an old version, the migration should run.
	require.NoError(t, mgr.EnsureMigrations(ctx, roachpb.Version{} /* bootstrapVersion */))
	require.True(t, fnGotCalled)
	fnGotCalled = false
	// If the cluster has been bootstrapped at a new version, the migration should
	// not run.
	require.NoError(t, mgr.EnsureMigrations(ctx, v /* bootstrapVersion */))
	require.False(t, fnGotCalled)
}

func TestDBErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	db := &fakeDB{}
	mgr := Manager{
		stopper:      stop.NewStopper(),
		leaseManager: &fakeLeaseManager{},
		db:           db,
	}
	defer mgr.stopper.Stop(context.TODO())

	migration := noopMigration1
	defer func(prev []migrationDescriptor) { backwardCompatibleMigrations = prev }(backwardCompatibleMigrations)
	backwardCompatibleMigrations = []migrationDescriptor{migration}
	testCases := []struct {
		scanErr     error
		putErr      error
		expectedErr string
	}{
		{
			nil,
			nil,
			"",
		},
		{
			fmt.Errorf("context deadline exceeded"),
			nil,
			"failed to get list of completed migrations.*context deadline exceeded",
		},
		{
			nil,
			fmt.Errorf("context deadline exceeded"),
			"failed to persist record of completing migration.*context deadline exceeded",
		},
	}
	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			db.scanErr = tc.scanErr
			db.putErr = tc.putErr
			db.kvs = make(map[string][]byte)
			err := mgr.EnsureMigrations(context.Background(), roachpb.Version{} /* bootstrapVersion */)
			if !testutils.IsError(err, tc.expectedErr) {
				t.Errorf("expected error %q, got error %v", tc.expectedErr, err)
			}
			if err != nil {
				return
			}
			if _, ok := db.kvs[string(migrationKey(migration))]; !ok {
				t.Errorf("expected key %s to be written, but it wasn't", migrationKey(migration))
			}
			if len(db.kvs) != len(backwardCompatibleMigrations) {
				t.Errorf("expected %d key to be written, but %d were",
					len(backwardCompatibleMigrations), len(db.kvs))
			}
		})
	}
}

// ExtendLease and ReleaseLease errors should not, by themselves, cause the
// migration process to fail. Not being able to acquire the lease should, but
// we don't test that here due to the added code that would be needed to change
// its retry settings to allow for testing it in a reasonable amount of time.
func TestLeaseErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	db := &fakeDB{kvs: make(map[string][]byte)}
	mgr := Manager{
		stopper: stop.NewStopper(),
		leaseManager: &fakeLeaseManager{
			extendErr:  fmt.Errorf("context deadline exceeded"),
			releaseErr: fmt.Errorf("context deadline exceeded"),
		},
		db: db,
	}
	defer mgr.stopper.Stop(context.TODO())

	migration := noopMigration1
	defer func(prev []migrationDescriptor) { backwardCompatibleMigrations = prev }(backwardCompatibleMigrations)
	backwardCompatibleMigrations = []migrationDescriptor{migration}
	if err := mgr.EnsureMigrations(context.Background(), roachpb.Version{} /* bootstrapVersion */); err != nil {
		t.Error(err)
	}
	if _, ok := db.kvs[string(migrationKey(migration))]; !ok {
		t.Errorf("expected key %s to be written, but it wasn't", migrationKey(migration))
	}
	if len(db.kvs) != len(backwardCompatibleMigrations) {
		t.Errorf("expected %d key to be written, but %d were",
			len(backwardCompatibleMigrations), len(db.kvs))
	}
}

// The lease not having enough time left on it to finish migrations should
// cause the process to exit via a call to log.Fatal.
func TestLeaseExpiration(t *testing.T) {
	defer leaktest.AfterTest(t)()
	db := &fakeDB{kvs: make(map[string][]byte)}
	mgr := Manager{
		stopper:      stop.NewStopper(),
		leaseManager: &fakeLeaseManager{leaseTimeRemaining: time.Nanosecond},
		db:           db,
	}
	defer mgr.stopper.Stop(context.TODO())

	oldLeaseRefreshInterval := leaseRefreshInterval
	leaseRefreshInterval = time.Microsecond
	defer func() { leaseRefreshInterval = oldLeaseRefreshInterval }()

	exitCalled := make(chan bool)
	log.SetExitFunc(true /* hideStack */, func(int) { exitCalled <- true })
	defer log.ResetExitFunc()
	// Disable stack traces to make the test output in teamcity less deceiving.
	defer log.DisableTracebacks()()

	waitForExitMigration := migrationDescriptor{
		name: "wait for exit to be called",
		workFn: func(context.Context, runner) error {
			select {
			case <-exitCalled:
				return nil
			case <-time.After(10 * time.Second):
				return fmt.Errorf("timed out waiting for exit to be called")
			}
		},
	}
	defer func(prev []migrationDescriptor) { backwardCompatibleMigrations = prev }(backwardCompatibleMigrations)
	backwardCompatibleMigrations = []migrationDescriptor{waitForExitMigration}
	if err := mgr.EnsureMigrations(context.Background(), roachpb.Version{} /* bootstrapVersion */); err != nil {
		t.Error(err)
	}
}

// migrationTest assists in testing the effects of a migration. It provides
// methods to edit the list of migrations run at server startup, start a test
// server, and run an individual migration. The test server's KV and SQL
// interfaces are intended to be accessed directly to verify the effect of the
// migration. Any mutations to the list of migrations run at server startup are
// reverted at the end of the test.
type migrationTest struct {
	oldMigrations []migrationDescriptor
	server        serverutils.TestServerInterface
	sqlDB         *sqlutils.SQLRunner
	kvDB          *kv.DB
	memMetrics    *sql.MemoryMetrics
}

// makeMigrationTest creates a new migrationTest.
//
// The caller is responsible for calling the test's close method at the end of
// the test.
func makeMigrationTest(ctx context.Context, t testing.TB) migrationTest {
	t.Helper()

	oldMigrations := append([]migrationDescriptor(nil), backwardCompatibleMigrations...)
	memMetrics := sql.MakeMemMetrics("migration-test-internal", time.Minute)
	return migrationTest{
		oldMigrations: oldMigrations,
		memMetrics:    &memMetrics,
	}
}

// pop removes the migration whose name starts with namePrefix from the list of
// migrations run at server startup. It fails the test if the number of
// migrations that match namePrefix is not exactly one.
//
// You must not call pop after calling start.
func (mt *migrationTest) pop(t testing.TB, namePrefix string) migrationDescriptor {
	t.Helper()

	if mt.server != nil {
		t.Fatal("migrationTest.pop must be called before mt.start")
	}

	var migration migrationDescriptor
	var newMigrations []migrationDescriptor
	for _, m := range backwardCompatibleMigrations {
		if strings.HasPrefix(m.name, namePrefix) {
			migration = m
			continue
		}
		newMigrations = append(newMigrations, m)
	}
	if n := len(backwardCompatibleMigrations) - len(newMigrations); n != 1 {
		t.Fatalf("expected prefix %q to match exactly one migration, but matched %d", namePrefix, n)
	}
	backwardCompatibleMigrations = newMigrations
	return migration
}

// start starts a test server with the given serverArgs.
func (mt *migrationTest) start(t testing.TB, serverArgs base.TestServerArgs) {
	server, sqlDB, kvDB := serverutils.StartServer(t, serverArgs)
	mt.server = server
	mt.sqlDB = sqlutils.MakeSQLRunner(sqlDB)
	mt.kvDB = kvDB
}

// runMigration triggers a manual run of migration. It does not mark migration
// as completed, so subsequent calls will cause migration to be re-executed.
// This is useful for verifying idempotency.
//
// You must call start before calling runMigration.
func (mt *migrationTest) runMigration(ctx context.Context, m migrationDescriptor) error {
	if m.workFn == nil {
		// Migration has been baked in. Ignore it.
		return nil
	}
	return m.workFn(ctx, runner{
		settings:    mt.server.ClusterSettings(),
		db:          mt.kvDB,
		sqlExecutor: mt.server.InternalExecutor().(*sql.InternalExecutor),
	})
}

// close stops the test server and restores package-global state.
func (mt *migrationTest) close(ctx context.Context) {
	if mt.server != nil {
		mt.server.Stopper().Stop(ctx)
	}
	backwardCompatibleMigrations = mt.oldMigrations
}

func TestCreateSystemTable(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	table := sqlbase.NamespaceTable
	table.ID = keys.MaxReservedDescID

	prevPrivileges, ok := sqlbase.SystemAllowedPrivileges[table.ID]
	defer func() {
		if ok {
			// Restore value of privileges.
			sqlbase.SystemAllowedPrivileges[table.ID] = prevPrivileges
		} else {
			delete(sqlbase.SystemAllowedPrivileges, table.ID)
		}
	}()
	sqlbase.SystemAllowedPrivileges[table.ID] = sqlbase.SystemAllowedPrivileges[keys.NamespaceTableID]

	table.Name = "dummy"
	nameKey := sqlbase.NewPublicTableKey(table.ParentID, table.Name).Key()
	descKey := sqlbase.MakeDescMetadataKey(table.ID)
	descVal := sqlbase.WrapDescriptor(&table)

	mt := makeMigrationTest(ctx, t)
	defer mt.close(ctx)

	mt.start(t, base.TestServerArgs{})

	// Verify that the keys were not written.
	if kv, err := mt.kvDB.Get(ctx, nameKey); err != nil {
		t.Error(err)
	} else if kv.Exists() {
		t.Errorf("expected %q not to exist, got %v", nameKey, kv)
	}
	if kv, err := mt.kvDB.Get(ctx, descKey); err != nil {
		t.Error(err)
	} else if kv.Exists() {
		t.Errorf("expected %q not to exist, got %v", descKey, kv)
	}

	migration := migrationDescriptor{
		name: "add system.dummy table",
		workFn: func(ctx context.Context, r runner) error {
			return createSystemTable(ctx, r, table)
		},
	}
	if err := mt.runMigration(ctx, migration); err != nil {
		t.Fatal(err)
	}

	// Verify that the appropriate keys were written.
	if kv, err := mt.kvDB.Get(ctx, nameKey); err != nil {
		t.Error(err)
	} else if !kv.Exists() {
		t.Errorf("expected %q to exist, got that it doesn't exist", nameKey)
	}
	var descriptor sqlbase.Descriptor
	if err := mt.kvDB.GetProto(ctx, descKey, &descriptor); err != nil {
		t.Error(err)
	} else if !proto.Equal(descVal, &descriptor) {
		t.Errorf("expected %v for key %q, got %v", descVal, descKey, descriptor)
	}

	// Verify the idempotency of the migration.
	if err := mt.runMigration(ctx, migration); err != nil {
		t.Fatal(err)
	}
}

func TestAdminUserExists(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	mt := makeMigrationTest(ctx, t)
	defer mt.close(ctx)

	migration := mt.pop(t, "add system.users isRole column and create admin role")
	mt.start(t, base.TestServerArgs{})

	// Create a user named "admin". We have to do a manual insert as "CREATE USER"
	// knows about "isRole", but the migration hasn't run yet.
	mt.sqlDB.Exec(t, `INSERT INTO system.users (username, "hashedPassword") VALUES ($1, '')`,
		sqlbase.AdminRole)

	// The revised migration in v2.1 upserts the admin user, so this should succeed.
	if err := mt.runMigration(ctx, migration); err != nil {
		t.Errorf("expected success, got %q", err)
	}
}

func TestPublicRoleExists(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	mt := makeMigrationTest(ctx, t)
	defer mt.close(ctx)

	migration := mt.pop(t, "disallow public user or role name")
	mt.start(t, base.TestServerArgs{})

	// Create a user (we check for user or role) named "public".
	// We have to do a manual insert as "CREATE USER" knows to disallow "public".
	mt.sqlDB.Exec(t, `INSERT INTO system.users (username, "hashedPassword", "isRole") VALUES ($1, '', false)`,
		sqlbase.PublicRole)

	e := `found a user named public which is now a reserved name.`
	// The revised migration in v2.1 upserts the admin user, so this should succeed.
	if err := mt.runMigration(ctx, migration); !testutils.IsError(err, e) {
		t.Errorf("expected error %q got %q", e, err)
	}

	// Now try with a role instead of a user.
	mt.sqlDB.Exec(t, `DELETE FROM system.users WHERE username = $1`, sqlbase.PublicRole)
	mt.sqlDB.Exec(t, `INSERT INTO system.users (username, "hashedPassword", "isRole") VALUES ($1, '', true)`,
		sqlbase.PublicRole)

	e = `found a role named public which is now a reserved name.`
	// The revised migration in v2.1 upserts the admin user, so this should succeed.
	if err := mt.runMigration(ctx, migration); !testutils.IsError(err, e) {
		t.Errorf("expected error %q got %q", e, err)
	}

	// Drop it and try again.
	mt.sqlDB.Exec(t, `DELETE FROM system.users WHERE username = $1`, sqlbase.PublicRole)
	if err := mt.runMigration(ctx, migration); err != nil {
		t.Errorf("expected success, got %q", err)
	}
}

func TestReplayMigrations(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	mt := makeMigrationTest(ctx, t)
	defer mt.close(ctx)

	mt.start(t, base.TestServerArgs{})

	// Test all migrations again. Starting the server did the first round.
	for _, m := range backwardCompatibleMigrations {
		if err := mt.runMigration(ctx, m); err != nil {
			t.Error(err)
		}
	}
}

func TestExpectedInitialRangeCount(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	s, sqlDB, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(ctx)

	testutils.SucceedsSoon(t, func() error {
		lastMigration := backwardCompatibleMigrations[len(backwardCompatibleMigrations)-1]
		if _, err := kvDB.Get(ctx, migrationKey(lastMigration)); err != nil {
			return errors.New("last migration has not completed")
		}

		sysCfg := s.GossipI().(*gossip.Gossip).GetSystemConfig()
		if sysCfg == nil {
			return errors.New("gossipped system config not available")
		}

		rows, err := sqlDB.Query(`SELECT range_id, start_key, end_key FROM kwdb_internal.ranges`)
		if err != nil {
			return err
		}
		defer rows.Close()
		nranges := 0
		for rows.Next() {
			var rangeID int
			var startKey, endKey []byte
			if err := rows.Scan(&rangeID, &startKey, &endKey); err != nil {
				return err
			}
			if sysCfg.NeedsSplit(startKey, endKey) {
				return fmt.Errorf("range %d needs split", rangeID)
			}
			nranges++
		}
		if rows.Err() != nil {
			return err
		}

		expectedRanges, err := s.ExpectedInitialRangeCount()
		if err != nil {
			return err
		}
		if expectedRanges != nranges {
			return fmt.Errorf("expected %d ranges but got %d", expectedRanges, nranges)
		}

		return nil
	})
}

func TestUpdateSystemLocationData(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	mt := makeMigrationTest(ctx, t)
	defer mt.close(ctx)

	migration := mt.pop(t, "update system.locations with default location data")
	mt.start(t, base.TestServerArgs{})

	// Check that we don't have any data in the system.locations table without the migration.
	var count int
	mt.sqlDB.QueryRow(t, `SELECT count(*) FROM system.locations`).Scan(&count)
	if count != 0 {
		t.Fatalf("Exected to find 0 rows in system.locations. Found  %d instead", count)
	}

	// Run the migration to insert locations.
	if err := mt.runMigration(ctx, migration); err != nil {
		t.Errorf("expected success, got %q", err)
	}

	// Check that we have all of the expected locations.
	mt.sqlDB.QueryRow(t, `SELECT count(*) FROM system.locations`).Scan(&count)
	if count != len(roachpb.DefaultLocationInformation) {
		t.Fatalf("Exected to find 0 rows in system.locations. Found  %d instead", count)
	}
}

func TestMigrateNamespaceTableDescriptors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()

	mt := makeMigrationTest(ctx, t)
	defer mt.close(ctx)

	migration := mt.pop(t, "create new system.namespace table")
	mt.start(t, base.TestServerArgs{})

	// Since we're already on 20.1, mimic the beginning state by deleting the
	// new namespace descriptor.
	key := sqlbase.MakeDescMetadataKey(keys.NamespaceTableID)
	require.NoError(t, mt.kvDB.Del(ctx, key))

	deprecatedKey := sqlbase.MakeDescMetadataKey(keys.DeprecatedNamespaceTableID)
	desc := &sqlbase.Descriptor{}
	require.NoError(t, mt.kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		_, err := txn.GetProtoTs(ctx, deprecatedKey, desc)
		return err
	}))

	// Run the migration.
	require.NoError(t, mt.runMigration(ctx, migration))

	require.NoError(t, mt.kvDB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
		// Check that the persisted descriptors now match our in-memory versions,
		// ignoring create and modification times.
		{
			ts, err := txn.GetProtoTs(ctx, key, desc)
			require.NoError(t, err)
			table := desc.Table(ts)
			table.CreateAsOfTime = sqlbase.NamespaceTable.CreateAsOfTime
			table.ModificationTime = sqlbase.NamespaceTable.ModificationTime
			require.True(t, table.Equal(sqlbase.NamespaceTable))
		}
		{
			ts, err := txn.GetProtoTs(ctx, deprecatedKey, desc)
			require.NoError(t, err)
			table := desc.Table(ts)
			table.CreateAsOfTime = sqlbase.DeprecatedNamespaceTable.CreateAsOfTime
			table.ModificationTime = sqlbase.DeprecatedNamespaceTable.ModificationTime
			require.True(t, table.Equal(sqlbase.DeprecatedNamespaceTable))
		}
		return nil
	}))
}
