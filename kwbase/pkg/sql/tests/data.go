// Copyright 2017 The Cockroach Authors.
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

package tests

import (
	"bytes"
	"context"
	gosql "database/sql"
	"fmt"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
)

// CheckKeyCount checks that the number of keys in the provided span matches
// numKeys.
func CheckKeyCount(t *testing.T, kvDB *kv.DB, span roachpb.Span, numKeys int) {
	t.Helper()
	if kvs, err := kvDB.Scan(context.TODO(), span.Key, span.EndKey, 0); err != nil {
		t.Fatal(err)
	} else if l := numKeys; len(kvs) != l {
		t.Fatalf("expected %d key value pairs, but got %d", l, len(kvs))
	}
}

// CreateKVTable creates a basic table named t.<name> that stores key/value
// pairs with numRows of arbitrary data.
func CreateKVTable(sqlDB *gosql.DB, name string, numRows int) error {
	// Fix the column families so the key counts don't change if the family
	// heuristics are updated.
	schema := fmt.Sprintf(`
		CREATE DATABASE IF NOT EXISTS t;
		CREATE TABLE t.%s (k INT8 PRIMARY KEY, v INT8, FAMILY (k), FAMILY (v));
		CREATE INDEX foo on t.%s (v);`, name, name)

	if _, err := sqlDB.Exec(schema); err != nil {
		return err
	}

	// Bulk insert.
	var insert bytes.Buffer
	if _, err := insert.WriteString(
		fmt.Sprintf(`INSERT INTO t.%s VALUES (%d, %d)`, name, 0, numRows-1)); err != nil {
		return err
	}
	for i := 1; i < numRows; i++ {
		if _, err := insert.WriteString(fmt.Sprintf(` ,(%d, %d)`, i, numRows-i)); err != nil {
			return err
		}
	}
	_, err := sqlDB.Exec(insert.String())
	return err
}

// CreateKVInterleavedTable is like CreateKVTable, but it interleaves table
// t.intlv inside of t.kv and adds rows to both.
func CreateKVInterleavedTable(t *testing.T, sqlDB *gosql.DB, numRows int) {
	// Fix the column families so the key counts don't change if the family
	// heuristics are updated.
	if _, err := sqlDB.Exec(`
CREATE DATABASE t;
SET DATABASE=t;
CREATE TABLE kv (k INT8 PRIMARY KEY, v INT8);
CREATE TABLE intlv (k INT8, m INT8, n INT8, PRIMARY KEY (k, m)) INTERLEAVE IN PARENT kv (k);
CREATE INDEX intlv_idx ON intlv (k, n) INTERLEAVE IN PARENT kv (k);
`); err != nil {
		t.Fatal(err)
	}

	var insert bytes.Buffer
	if _, err := insert.WriteString(fmt.Sprintf(`INSERT INTO t.kv VALUES (%d, %d)`, 0, numRows-1)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < numRows; i++ {
		if _, err := insert.WriteString(fmt.Sprintf(` ,(%d, %d)`, i, numRows-i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.Exec(insert.String()); err != nil {
		t.Fatal(err)
	}
	insert.Reset()
	if _, err := insert.WriteString(fmt.Sprintf(`INSERT INTO t.intlv VALUES (%d, %d, %d)`, 0, numRows-1, numRows-1)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < numRows; i++ {
		if _, err := insert.WriteString(fmt.Sprintf(` ,(%d, %d, %d)`, i, numRows-i, numRows-i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.Exec(insert.String()); err != nil {
		t.Fatal(err)
	}
}
