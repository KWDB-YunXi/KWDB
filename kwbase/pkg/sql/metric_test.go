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

package sql_test

import (
	"bytes"
	"context"
	"regexp"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/storagebase"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/sql"
	"gitee.com/kwbasedb/kwbase/pkg/sql/tests"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
)

type queryCounter struct {
	query                           string
	expectError                     bool
	txnBeginCount                   int64
	selectCount                     int64
	selectExecutedCount             int64
	distSQLSelectCount              int64
	fallbackCount                   int64
	updateCount                     int64
	insertCount                     int64
	deleteCount                     int64
	ddlCount                        int64
	miscCount                       int64
	miscExecutedCount               int64
	failureCount                    int64
	txnCommitCount                  int64
	txnRollbackCount                int64
	txnAbortCount                   int64
	savepointCount                  int64
	restartSavepointCount           int64
	releaseRestartSavepointCount    int64
	rollbackToRestartSavepointCount int64
}

func TestQueryCounts(t *testing.T) {
	defer leaktest.AfterTest(t)()

	params, _ := tests.CreateTestServerParams()
	params.Knobs = base.TestingKnobs{
		SQLLeaseManager: &sql.LeaseManagerTestingKnobs{
			// Disable SELECT called for delete orphaned leases to keep
			// query stats stable.
			DisableDeleteOrphanedLeases: true,
		},
	}
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())

	var testcases = []queryCounter{
		// The counts are deltas for each query.
		{query: "SET DISTSQL = 'off'", miscCount: 1, miscExecutedCount: 1},
		{query: "BEGIN; END", txnBeginCount: 1, txnCommitCount: 1},
		{query: "SELECT 1", selectCount: 1, selectExecutedCount: 1, txnCommitCount: 1},
		{query: "CREATE DATABASE mt", ddlCount: 1},
		{query: "CREATE TABLE mt.n (num INTEGER PRIMARY KEY)", ddlCount: 1},
		{query: "INSERT INTO mt.n VALUES (3)", insertCount: 1},
		// Test failure (uniqueness violation).
		{query: "INSERT INTO mt.n VALUES (3)", failureCount: 1, insertCount: 1, expectError: true},
		// Test failure (planning error).
		{
			query:        "INSERT INTO nonexistent VALUES (3)",
			failureCount: 1, insertCount: 1, expectError: true,
		},
		{query: "UPDATE mt.n SET num = num + 1", updateCount: 1},
		{query: "DELETE FROM mt.n", deleteCount: 1},
		{query: "ALTER TABLE mt.n ADD COLUMN num2 INTEGER", ddlCount: 1},
		{query: "EXPLAIN SELECT * FROM mt.n", miscCount: 1, miscExecutedCount: 1},
		{
			query:         "BEGIN; UPDATE mt.n SET num = num + 1; END",
			txnBeginCount: 1, updateCount: 1, txnCommitCount: 1,
		},
		{
			query:       "SELECT * FROM mt.n; SELECT * FROM mt.n; SELECT * FROM mt.n",
			selectCount: 3, selectExecutedCount: 3,
		},
		{query: "SET DISTSQL = 'on'", miscCount: 1, miscExecutedCount: 1},
		{
			query:       "SELECT * FROM mt.n",
			selectCount: 1, selectExecutedCount: 1, distSQLSelectCount: 1,
		},
		{query: "SET DISTSQL = 'off'", miscCount: 1, miscExecutedCount: 1},
		{query: "DROP TABLE mt.n", ddlCount: 1},
		{query: "SET database = system", miscCount: 1, miscExecutedCount: 1},
		{query: "SELECT 3", selectCount: 1, selectExecutedCount: 1},
		{query: "CREATE TABLE mt.n (num INTEGER PRIMARY KEY)", ddlCount: 1},
		{query: "UPDATE mt.n SET num = num + 1", updateCount: 1},
	}

	accum := initializeQueryCounter(s)

	for _, tc := range testcases {
		t.Run(tc.query, func(t *testing.T) {
			if _, err := sqlDB.Exec(tc.query); err != nil && !tc.expectError {
				t.Fatalf("unexpected error executing '%s': %s'", tc.query, err)
			}

			// Force metric snapshot refresh.
			if err := s.WriteSummaries(); err != nil {
				t.Fatal(err)
			}

			var err error
			if accum.txnBeginCount, err = checkCounterDelta(s, sql.MetaTxnBeginStarted, accum.txnBeginCount, tc.txnBeginCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.distSQLSelectCount, err = checkCounterDelta(s, sql.MetaDistSQLSelect, accum.distSQLSelectCount, tc.distSQLSelectCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.txnRollbackCount, err = checkCounterDelta(s, sql.MetaTxnRollbackStarted, accum.txnRollbackCount, tc.txnRollbackCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.txnAbortCount, err = checkCounterDelta(s, sql.MetaTxnAbort, accum.txnAbortCount, 0); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.selectCount, err = checkCounterDelta(s, sql.MetaSelectStarted, accum.selectCount, tc.selectCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.selectExecutedCount, err = checkCounterDelta(s, sql.MetaSelectExecuted, accum.selectExecutedCount, tc.selectExecutedCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.updateCount, err = checkCounterDelta(s, sql.MetaUpdateStarted, accum.updateCount, tc.updateCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.insertCount, err = checkCounterDelta(s, sql.MetaInsertStarted, accum.insertCount, tc.insertCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.deleteCount, err = checkCounterDelta(s, sql.MetaDeleteStarted, accum.deleteCount, tc.deleteCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.ddlCount, err = checkCounterDelta(s, sql.MetaDdlStarted, accum.ddlCount, tc.ddlCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.miscCount, err = checkCounterDelta(s, sql.MetaMiscStarted, accum.miscCount, tc.miscCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.miscExecutedCount, err = checkCounterDelta(s, sql.MetaMiscExecuted, accum.miscExecutedCount, tc.miscExecutedCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.failureCount, err = checkCounterDelta(s, sql.MetaFailure, accum.failureCount, tc.failureCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
			if accum.fallbackCount, err = checkCounterDelta(s, sql.MetaSQLOptFallback, accum.fallbackCount, tc.fallbackCount); err != nil {
				t.Errorf("%q: %s", tc.query, err)
			}
		})
	}
}

func TestAbortCountConflictingWrites(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testutils.RunTrueAndFalse(t, "retry loop", func(t *testing.T, retry bool) {
		params, cmdFilters := tests.CreateTestServerParams()
		s, sqlDB, _ := serverutils.StartServer(t, params)
		defer s.Stopper().Stop(context.TODO())

		accum := initializeQueryCounter(s)

		if _, err := sqlDB.Exec("CREATE DATABASE db"); err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.Exec("CREATE TABLE db.t (k TEXT PRIMARY KEY, v TEXT)"); err != nil {
			t.Fatal(err)
		}

		// Inject errors on the INSERT below.
		restarted := false
		cmdFilters.AppendFilter(func(args storagebase.FilterArgs) *roachpb.Error {
			switch req := args.Req.(type) {
			// SQL INSERT generates ConditionalPuts for unique indexes (such as the PK).
			case *roachpb.ConditionalPutRequest:
				if bytes.Contains(req.Value.RawBytes, []byte("marker")) && !restarted {
					restarted = true
					return roachpb.NewErrorWithTxn(
						roachpb.NewTransactionAbortedError(
							roachpb.ABORT_REASON_ABORTED_RECORD_FOUND), args.Hdr.Txn)
				}
			}
			return nil
		}, false)

		txn, err := sqlDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if retry {
			if _, err := txn.Exec("SAVEPOINT kwbase_restart"); err != nil {
				t.Fatal(err)
			}
		}
		// Run a batch of statements to move the txn out of the AutoRetry state,
		// otherwise the INSERT below would be automatically retried.
		if _, err := txn.Exec("SELECT 1"); err != nil {
			t.Fatal(err)
		}

		_, err = txn.Exec("INSERT INTO db.t VALUES ('key', 'marker')")
		expErr := "TransactionAbortedError(ABORT_REASON_ABORTED_RECORD_FOUND)"
		if !testutils.IsError(err, regexp.QuoteMeta(expErr)) {
			t.Fatalf("expected %s, got: %v", expErr, err)
		}

		var expRestart, expRollback, expCommit, expAbort int64
		if retry {
			if _, err := txn.Exec("ROLLBACK TO SAVEPOINT kwbase_restart"); err != nil {
				t.Fatal(err)
			}
			if _, err := txn.Exec("RELEASE SAVEPOINT kwbase_restart"); err != nil {
				t.Fatal(err)
			}
			if err = txn.Commit(); err != nil {
				t.Fatal(err)
			}

			expRestart = 1
			expCommit = 1
		} else {
			if err = txn.Rollback(); err != nil {
				t.Fatal(err)
			}

			expRollback = 1
			expAbort = 1
		}

		if _, err := checkCounterDelta(s, sql.MetaTxnBeginStarted, accum.txnBeginCount, 1); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaInsertStarted, accum.insertCount, 1); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaRestartSavepointStarted, accum.restartSavepointCount, expRestart); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaRollbackToRestartSavepointStarted, accum.rollbackToRestartSavepointCount, expRestart); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaReleaseRestartSavepointStarted, accum.releaseRestartSavepointCount, expRestart); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaTxnRollbackStarted, accum.txnRollbackCount, expRollback); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaTxnCommitStarted, accum.txnCommitCount, expCommit); err != nil {
			t.Error(err)
		}
		if _, err := checkCounterDelta(s, sql.MetaTxnAbort, accum.txnAbortCount, expAbort); err != nil {
			t.Error(err)
		}
	})
}

// TestErrorDuringTransaction tests that the transaction abort count goes up when a query
// results in an error during a txn.
func TestAbortCountErrorDuringTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)()
	params, _ := tests.CreateTestServerParams()
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())

	accum := initializeQueryCounter(s)

	txn, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := txn.Query("SELECT * FROM i_do.not_exist"); err == nil {
		t.Fatal("Expected an error but didn't get one")
	}

	if _, err := checkCounterDelta(s, sql.MetaTxnBeginStarted, accum.txnBeginCount, 1); err != nil {
		t.Error(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaSelectStarted, accum.selectCount, 1); err != nil {
		t.Error(err)
	}

	if err := txn.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, err := checkCounterDelta(s, sql.MetaTxnAbort, accum.txnAbortCount, 1); err != nil {
		t.Error(err)
	}
}

func TestSavepointMetrics(t *testing.T) {
	defer leaktest.AfterTest(t)()

	params, _ := tests.CreateTestServerParams()
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.TODO())

	accum := initializeQueryCounter(s)

	// Normal-case use of all three savepoint statements.
	txn, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Exec("SAVEPOINT kwbase_restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Exec("ROLLBACK TRANSACTION TO SAVEPOINT kwbase_restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Exec("RELEASE SAVEPOINT kwbase_restart"); err != nil {
		t.Fatal(err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := checkCounterDelta(s, sql.MetaRestartSavepointStarted, accum.restartSavepointCount, 1); err != nil {
		t.Error(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaRestartSavepointStarted, accum.releaseRestartSavepointCount, 1); err != nil {
		t.Error(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaRestartSavepointStarted, accum.rollbackToRestartSavepointCount, 1); err != nil {
		t.Error(err)
	}

	// Unsupported savepoints go in a different counter.
	txn, err = sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Exec("SAVEPOINT blah"); err != nil {
		t.Fatal(err)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaSavepointStarted, accum.savepointCount, 1); err != nil {
		t.Error(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaTxnRollbackStarted, accum.txnRollbackCount, 1); err != nil {
		t.Error(err)
	}

	// Custom restart savepoint names are recognized.
	txn, err = sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Exec("SET force_savepoint_restart = true"); err != nil {
		t.Fatal(err)
	}
	if _, err := txn.Exec("SAVEPOINT blah"); err != nil {
		t.Fatal(err)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaRestartSavepointStarted, accum.restartSavepointCount, 2); err != nil {
		t.Error(err)
	}
	if _, err := checkCounterDelta(s, sql.MetaTxnRollbackStarted, accum.txnRollbackCount, 2); err != nil {
		t.Error(err)
	}
}
