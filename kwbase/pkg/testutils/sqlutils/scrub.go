// Copyright 2017 The Cockroach Authors.
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

package sqlutils

import (
	gosql "database/sql"
	"fmt"
	"time"

	"github.com/pkg/errors"
)

// ScrubResult is the go struct for the row results for an
// EXPERIMENTAL SCRUB query.
type ScrubResult struct {
	ErrorType  string
	Database   string
	Table      string
	PrimaryKey string
	Timestamp  time.Time
	Repaired   bool
	Details    string
}

// GetScrubResultRows will scan and unmarshal ScrubResults from a Rows
// iterator. The Rows iterate must from an EXPERIMENTAL SCRUB query.
func GetScrubResultRows(rows *gosql.Rows) (results []ScrubResult, err error) {
	defer rows.Close()

	var unused *string
	for rows.Next() {
		result := ScrubResult{}
		if err := rows.Scan(
			// TODO(joey): In the future, SCRUB will run as a job during execution.
			&unused, /* job_uuid */
			&result.ErrorType,
			&result.Database,
			&result.Table,
			&result.PrimaryKey,
			&result.Timestamp,
			&result.Repaired,
			&result.Details,
		); err != nil {
			return nil, err
		}
		results = append(results, result)
	}

	if rows.Err() != nil {
		return nil, err
	}

	return results, nil
}

// RunScrub will run execute an exhaustive scrub check for a table.
func RunScrub(sqlDB *gosql.DB, database string, table string) error {
	return RunScrubWithOptions(sqlDB, database, table, "")
}

// RunScrubWithOptions will run a SCRUB check for a table with the specified options string.
func RunScrubWithOptions(sqlDB *gosql.DB, database string, table string, options string) error {
	rows, err := sqlDB.Query(fmt.Sprintf(`EXPERIMENTAL SCRUB TABLE %s.%s %s`,
		database, table, options))
	if err != nil {
		return err
	}

	results, err := GetScrubResultRows(rows)
	if err != nil {
		return err
	}

	if len(results) > 0 {
		return errors.Errorf("expected no scrub results instead got: %#v", results)
	}
	return nil
}
