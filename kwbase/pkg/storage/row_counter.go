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

package storage

import (
	"bytes"

	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/util/encoding"
)

// RowCounter is a helper that counts how many distinct rows appear in the KVs
// that is is shown via `Count`. Note: the `DataSize` field of the BulkOpSummary
// is *not* populated by this and should be set separately.
type RowCounter struct {
	roachpb.BulkOpSummary
	prev roachpb.Key
}

// Count examines each key passed to it and increments the running count when it
// sees a key that belongs to a new row.
func (r *RowCounter) Count(key roachpb.Key) error {
	// EnsureSafeSplitKey is usually used to avoid splitting a row across ranges,
	// by returning the row's key prefix.
	// We reuse it here to count "rows" by counting when it changes.
	// Non-SQL keys are returned unchanged or may error -- we ignore them, since
	// non-SQL keys are obviously thus not SQL rows.
	//
	// TODO(ajwerner): provide a separate mechanism to determine whether the key
	// is a valid SQL key which explicitly indicates whether the key is valid as
	// a split key independent of an error. See #43423.
	row, err := keys.EnsureSafeSplitKey(key)
	if err != nil || len(key) == len(row) {
		// TODO(ajwerner): Determine which errors should be ignored and only
		// ignore those.
		return nil //nolint:returnerrcheck
	}

	// no change key prefix => no new row.
	if bytes.Equal(row, r.prev) {
		return nil
	}

	r.prev = append(r.prev[:0], row...)

	rest, tableID, err := keys.DecodeTablePrefix(row)
	if err != nil {
		return err
	}

	_, indexID, err := encoding.DecodeUvarintAscending(rest)
	if err != nil {
		return err
	}

	if r.EntryCounts == nil {
		r.EntryCounts = make(map[uint64]int64)
	}
	r.EntryCounts[roachpb.BulkOpSummaryID(tableID, indexID)]++

	if indexID == 1 {
		r.DeprecatedRows++
	} else {
		r.DeprecatedIndexEntries++
	}

	return nil
}
