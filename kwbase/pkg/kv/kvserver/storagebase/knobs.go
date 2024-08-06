// Copyright 2018 The Cockroach Authors.
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

// Package workload provides an abstraction for generators of sql query loads
// (and requisite initial data) as well as tools for working with these
// generators.

package storagebase

// BatchEvalTestingKnobs contains testing helpers that are used during batch evaluation.
type BatchEvalTestingKnobs struct {
	// TestingEvalFilter is called before evaluating each command.
	TestingEvalFilter ReplicaCommandFilter

	// TestingPostEvalFilter is called after evaluating each command.
	TestingPostEvalFilter ReplicaCommandFilter

	// NumKeysEvaluatedForRangeIntentResolution is set by the stores to the
	// number of keys evaluated for range intent resolution.
	NumKeysEvaluatedForRangeIntentResolution *int64

	// RecoverIndeterminateCommitsOnFailedPushes will propagate indeterminate
	// commit errors to trigger transaction recovery even if the push that
	// discovered the indeterminate commit was going to fail. This increases
	// the chance that conflicting transactions will prevent parallel commit
	// attempts from succeeding.
	RecoverIndeterminateCommitsOnFailedPushes bool
}

// IntentResolverTestingKnobs contains testing helpers that are used during
// intent resolution.
type IntentResolverTestingKnobs struct {
	// DisableAsyncIntentResolution disables the async intent resolution
	// path (but leaves synchronous resolution). This can avoid some
	// edge cases in tests that start and stop servers.
	DisableAsyncIntentResolution bool

	// ForceSyncIntentResolution forces all asynchronous intent resolution to be
	// performed synchronously. It is equivalent to setting IntentResolverTaskLimit
	// to -1.
	ForceSyncIntentResolution bool

	// MaxGCBatchSize overrides the maximum number of transaction record gc
	// requests which can be sent in a single batch.
	MaxGCBatchSize int

	// MaxIntentResolutionBatchSize overrides the maximum number of intent
	// resolution requests which can be sent in a single batch.
	MaxIntentResolutionBatchSize int
}
