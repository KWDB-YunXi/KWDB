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

package heapprofiler

import (
	"context"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/envutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
)

// resetHighWaterMarkInterval specifies how often the high-water mark value will
// be reset. Immediately after it is reset, a new profile will be taken.
//
// If the value is 0, the collection of profiles gets disabled.
var resetHighWaterMarkInterval = func() time.Duration {
	dur := envutil.EnvOrDefaultDuration("KWBASE_MEMPROF_INTERVAL", time.Hour)
	if dur <= 0 {
		// Instruction to disable.
		return 0
	}
	return dur
}()

// timestampFormat is chosen to mimix that used by the log
// package. This is not a hard requirement thought; the profiles are
// stored in a separate directory.
const timestampFormat = "2006-01-02T15_04_05.000"

type testingKnobs struct {
	dontWriteProfiles    bool
	maybeTakeProfileHook func(willTakeProfile bool)
	now                  func() time.Time
}

type profiler struct {
	store *profileStore

	// lastProfileTime marks the time when we took the last profile.
	lastProfileTime time.Time
	// highwaterMarkBytes represents the maximum heap size that we've seen since
	// resetting the filed (which happens periodically).
	highwaterMarkBytes int64

	knobs testingKnobs
}

func (o *profiler) now() time.Time {
	if o.knobs.now != nil {
		return o.knobs.now()
	}
	return timeutil.Now()
}

func (o *profiler) maybeTakeProfile(
	ctx context.Context,
	thresholdValue int64,
	takeProfileFn func(ctx context.Context, path string) bool,
) {
	if resetHighWaterMarkInterval == 0 {
		// Instruction to disable.
		return
	}

	now := o.now()
	// If it's been too long since we took a profile, make sure we'll take one now.
	if now.Sub(o.lastProfileTime) >= resetHighWaterMarkInterval {
		o.highwaterMarkBytes = 0
	}

	takeProfile := thresholdValue > o.highwaterMarkBytes
	if hook := o.knobs.maybeTakeProfileHook; hook != nil {
		hook(takeProfile)
	}
	if !takeProfile {
		return
	}

	o.highwaterMarkBytes = thresholdValue
	o.lastProfileTime = now

	if o.knobs.dontWriteProfiles {
		return
	}
	success := takeProfileFn(ctx, o.store.makeNewFileName(now, thresholdValue))
	if success {
		// We only remove old files if the current dump was
		// successful. Otherwise, the GC may remove "interesting" files
		// from a previous crash.
		o.store.gcProfiles(ctx, now)
	}
}
