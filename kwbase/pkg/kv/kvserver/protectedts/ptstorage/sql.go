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

package ptstorage

const (

	// currentMetaCTE is used by all queries which access the meta row.
	// The query returns a default row if there currently is no meta row.
	// At the time of writing, there will never be a physical row in the meta
	// table with version zero.
	currentMetaCTE = `
SELECT
    version, num_records, num_spans, total_bytes
FROM
    system.protected_ts_meta
UNION ALL
    SELECT 0 AS version, 0 AS num_records, 0 AS num_spans, 0 AS total_bytes
ORDER BY
    version DESC
LIMIT
    1
`

	protectQuery = `
WITH
    current_meta AS (` + currentMetaCTE + `),
    checks AS (` + protectChecksCTE + `),
    updated_meta AS (` + protectUpsertMetaCTE + `),
    new_record AS (` + protectInsertRecordCTE + `)
SELECT
    failed,
    num_spans AS prev_spans,
    total_bytes AS prev_total_bytes,
    version AS prev_version
FROM
    checks, current_meta;`

	protectChecksCTE = `
SELECT
    new_version, 
    new_num_records,
    new_num_spans, 
    new_total_bytes,
    (
       ($1 > 0 AND new_num_spans > $1)
       OR ($2 > 0 AND new_total_bytes > $2)
       OR EXISTS(SELECT * FROM system.protected_ts_records WHERE id = $4)
    ) AS failed
FROM (
    SELECT
        version + 1 AS new_version,
        num_records + 1 AS new_num_records, 
        num_spans + $3 AS new_num_spans, 
        total_bytes + length($9) + length($6) + length($7) AS new_total_bytes
    FROM
        current_meta
)
`

	updateTimestampQuery = `
WITH
    current_meta AS (` + currentMetaCTE + `),
    updated_meta AS (` + updateTimestampUpsertMetaCTE + `),
    updated_record AS (` + updateTimestampUpsertRecordCTE + `)
SELECT
    id
FROM
    updated_record;`

	updateTimestampUpsertMetaCTE = `
UPSERT
INTO
    system.protected_ts_meta (version, num_records, num_spans, total_bytes)
(
    SELECT
        version + 1,
        num_records,
        num_spans,
        total_bytes
    FROM
        current_meta
)
RETURNING
    NULL
`

	updateTimestampUpsertRecordCTE = `
UPDATE
    system.protected_ts_records
SET
    ts = $2
WHERE
    id = $1
RETURNING
    id
`

	protectUpsertMetaCTE = `
UPSERT
INTO
    system.protected_ts_meta
(version, num_records, num_spans, total_bytes)
(
    SELECT
        new_version, new_num_records, new_num_spans, new_total_bytes
    FROM
        checks
    WHERE
        NOT failed
)
RETURNING
    version, num_records, num_spans, total_bytes
`

	protectInsertRecordCTE = `
INSERT
INTO
    system.protected_ts_records (id, ts, meta_type, meta, num_spans, spans)
(
    SELECT
        $4, $5, $6, $7, $8, $9
    WHERE
        NOT EXISTS(SELECT * FROM checks WHERE failed)
)
RETURNING
    id
`

	getRecordsQueryBase = `
SELECT
    id, ts, meta_type, meta, spans, verified
FROM
    system.protected_ts_records`

	getRecordsQuery = getRecordsQueryBase + ";"
	getRecordQuery  = getRecordsQueryBase + `
WHERE
    id = $1;`

	markVerifiedQuery = `
UPDATE
    system.protected_ts_records
SET
    verified = true
WHERE
    id = $1
RETURNING
    true
`

	releaseQuery = `
WITH
    current_meta AS (` + currentMetaCTE + `),
    record AS (` + releaseSelectRecordCTE + `),
    updated_meta AS (` + releaseUpsertMetaCTE + `)
DELETE FROM
    system.protected_ts_records AS r
WHERE
    EXISTS(SELECT NULL FROM record WHERE r.id = record.id)
RETURNING
    NULL;`

	// Collect the number of spans for the record identified by $1.
	releaseSelectRecordCTE = `
SELECT
    id,
    num_spans AS record_spans,
    length(spans) + length(meta_type) + length(meta) AS record_bytes
FROM
    system.protected_ts_records
WHERE
    id = $1
`

	// Updates the meta row if there was a record.
	releaseUpsertMetaCTE = `
UPSERT
INTO
    system.protected_ts_meta (version, num_records, num_spans, total_bytes)
(
    SELECT
        version, num_records, num_spans, total_bytes
    FROM
        (
            SELECT
                version + 1 AS version,
                num_records - 1 AS num_records,
                num_spans - record_spans AS num_spans,
                total_bytes - record_bytes AS total_bytes
            FROM
                current_meta RIGHT JOIN record ON true
        )
)
RETURNING
    1
`

	getMetadataQuery = `
WITH
    current_meta AS (` + currentMetaCTE + `)
SELECT
    version, num_records, num_spans, total_bytes
FROM
    current_meta;`
)
