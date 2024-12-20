// Copyright 2018 The Cockroach Authors.
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

package compactor

import (
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/settings"
	"github.com/pkg/errors"
)

func validateFraction(v float64) error {
	if v >= 0 && v <= 1 { // handles +-Inf, Nan
		return nil
	}
	return errors.Errorf("value %v not between zero and one", v)
}

var enabled = settings.RegisterBoolSetting(
	"compactor.enabled",
	"when false, the system will reclaim space occupied by deleted data less aggressively",
	true,
)

// minInterval indicates the minimum period of
// time to wait before any compaction activity is considered, after
// suggestions are made. The intent is to allow sufficient time for
// all ranges to be cleared when a big table is dropped, so the
// compactor can determine contiguous stretches and efficient delete
// sstable files.
var minInterval = settings.RegisterDurationSetting(
	"compactor.min_interval",
	"minimum time interval to wait before compacting",
	15*time.Second,
)

// thresholdBytes is the threshold in bytes of suggested
// reclamation, after which the compactor will begin processing
// (taking compactor min interval into account). Note that we want
// to target roughly the target size of an L6 SSTable (128MB) but
// these are logical bytes (as in, from MVCCStats) which can't be
// translated into SSTable-bytes. As a result, we conservatively set
// a higher threshold.
var thresholdBytes = settings.RegisterByteSizeSetting(
	"compactor.threshold_bytes",
	"minimum expected logical space reclamation required before considering an aggregated suggestion",
	256<<20, // more than 256MiB will trigger
)

// thresholdBytesUsedFraction is the fraction of total logical
// bytes used which are up for suggested reclamation, after which
// the compactor will begin processing (taking compactor min
// interval into account). Note that this threshold handles the case
// where a table is dropped which is a significant fraction of the
// total space in the database, but does not exceed the absolute
// defaultThresholdBytes threshold.
var thresholdBytesUsedFraction = settings.RegisterValidatedFloatSetting(
	"compactor.threshold_used_fraction",
	"consider suggestions for at least the given percentage of the used logical space (zero to disable)",
	0.10, // more than 10% of space will trigger
	validateFraction,
)

// thresholdBytesAvailableFraction is the fraction of remaining
// available space on a disk, which, if exceeded by the size of a suggested
// compaction, should trigger the processing of said compaction. This
// threshold is meant to make compaction more aggressive when a store is
// nearly full, since reclaiming space is much more important in such
// scenarios.	ThresholdBytesAvailableFraction() float64
var thresholdBytesAvailableFraction = settings.RegisterValidatedFloatSetting(
	"compactor.threshold_available_fraction",
	"consider suggestions for at least the given percentage of the available logical space (zero to disable)",
	0.10, // more than 10% of space will trigger
	validateFraction,
)

// maxSuggestedCompactionRecordAge is the maximum age of a
// suggested compaction record. If not processed within this time
// interval since the compaction was suggested, it will be deleted.
var maxSuggestedCompactionRecordAge = settings.RegisterNonNegativeDurationSetting(
	"compactor.max_record_age",
	"discard suggestions not processed within this duration",
	24*time.Hour,
)
