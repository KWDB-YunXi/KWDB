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

// This file contains tests for pgwire that need to be in the sql package.

package sql

import (
	"context"
	"database/sql/driver"
	"net/url"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/security"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/lib/pq"
	"github.com/pkg/errors"
)

// Test that abruptly closing a pgwire connection releases all leases held by
// that session.
func TestPGWireConnectionCloseReleasesLeases(t *testing.T) {
	defer leaktest.AfterTest(t)()
	s, _, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	ctx := context.TODO()
	defer s.Stopper().Stop(ctx)
	url, cleanupConn := sqlutils.PGUrl(t, s.ServingSQLAddr(), "SetupServer", url.User(security.RootUser))
	defer cleanupConn()
	conn, err := pq.Open(url.String())
	if err != nil {
		t.Fatal(err)
	}
	ex := conn.(driver.ExecerContext)
	if _, err := ex.ExecContext(ctx, "CREATE DATABASE test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ExecContext(ctx, "CREATE TABLE test.t (i INT PRIMARY KEY)", nil); err != nil {
		t.Fatal(err)
	}
	// Start a txn so leases are accumulated by queries.
	if _, err := ex.ExecContext(ctx, "BEGIN", nil); err != nil {
		t.Fatal(err)
	}
	// Get a table lease.
	if _, err := ex.ExecContext(ctx, "SELECT * FROM test.t", nil); err != nil {
		t.Fatal(err)
	}
	// Abruptly close the connection.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Verify that there are no leases held.
	tableDesc := sqlbase.GetTableDescriptor(kvDB, "test", "t")
	lm := s.LeaseManager().(*LeaseManager)
	// Looking for a table state validates that there used to be a lease on the
	// table.
	ts := lm.findTableState(tableDesc.ID, false /* create */)
	if ts == nil {
		t.Fatal("table state not found")
	}
	ts.mu.Lock()
	leases := ts.mu.active.data
	ts.mu.Unlock()
	if len(leases) != 1 {
		t.Fatalf("expected one lease, found: %d", len(leases))
	}
	// Wait for the lease to be released.
	testutils.SucceedsSoon(t, func() error {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		tv := ts.mu.active.data[0]
		tv.mu.Lock()
		defer tv.mu.Unlock()
		refcount := tv.mu.refcount
		if refcount != 0 {
			return errors.Errorf(
				"expected lease to be unused, found refcount: %d", refcount)
		}
		return nil
	})
}
