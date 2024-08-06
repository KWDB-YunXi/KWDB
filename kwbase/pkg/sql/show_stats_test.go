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

package sql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/sqlutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
)

func TestShowStatisticsJSON(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop(context.TODO())

	r := sqlutils.MakeSQLRunner(db)
	r.Exec(t, `SET CLUSTER SETTING sql.stats.automatic_collection.enabled = false`)

	r.Exec(t, `
		CREATE TABLE t (
		  i INT,
			f FLOAT,
			d DECIMAL,
			u UUID,
			s STRING,
			t TIMESTAMP,
			INDEX (i),
			INDEX (f),
			INDEX (d),
			INDEX (u),
			INDEX (s),
			INDEX (t)
		)`)

	r.Exec(t, `
		INSERT INTO t VALUES
		  (1, 1.0, 1.012034314, '00000000-0000-0000-0000-000000000000', 'string', '2020-01-01'),
		  (-1, -0, -0.00000000000, gen_random_uuid(), 'string with space', now()),
		  (10, 1.1, 100.1, gen_random_uuid(), 'spaces ''quotes'' "double quotes"', now())`)

	r.Exec(t, `CREATE STATISTICS foo FROM t`)

	row := r.QueryRow(t, `SHOW STATISTICS USING JSON FOR TABLE t`)
	var stats string
	row.Scan(&stats)

	// TODO(radu): we should add support for placeholders for the statistics.
	r.Exec(t, fmt.Sprintf(
		`ALTER TABLE t INJECT STATISTICS '%s'`, strings.Replace(stats, "'", "''", -1),
	))

	row = r.QueryRow(t, `SHOW STATISTICS USING JSON FOR TABLE t`)
	var stats2 string
	row.Scan(&stats2)
	if stats != stats2 {
		t.Errorf("after injecting back the same stats, got different stats:\n%s\nvs.\n%s", stats, stats2)
	}
}
