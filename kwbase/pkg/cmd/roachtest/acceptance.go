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

package main

import (
	"context"
	"time"
)

func registerAcceptance(r *testRegistry) {
	testCases := []struct {
		name       string
		fn         func(ctx context.Context, t *test, c *cluster)
		skip       string
		minVersion string
		timeout    time.Duration
	}{
		// Sorted. Please keep it that way.
		{name: "bank/cluster-recovery", fn: runBankClusterRecovery},
		{name: "bank/node-restart", fn: runBankNodeRestart},
		{
			name: "bank/zerosum-splits", fn: runBankNodeZeroSum,
			skip: "https://gitee.com/kwbasedb/kwbase/issues/33683 (runs into " +
				" various errors during its rebalances, see IsExpectedRelocateError)",
		},
		// {"bank/zerosum-restart", runBankZeroSumRestart},
		{name: "build-info", fn: runBuildInfo},
		{name: "build-analyze", fn: runBuildAnalyze},
		{name: "cli/node-status", fn: runCLINodeStatus},
		{name: "cluster-init", fn: runClusterInit},
		{name: "event-log", fn: runEventLog},
		{name: "gossip/peerings", fn: runGossipPeerings},
		{name: "gossip/restart", fn: runGossipRestart},
		{name: "gossip/restart-node-one", fn: runGossipRestartNodeOne},
		{name: "gossip/locality-address", fn: runCheckLocalityIPAddress},
		{name: "rapid-restart", fn: runRapidRestart},
		{
			name: "many-splits", fn: runManySplits,
			minVersion: "v19.2.0", // SQL syntax unsupported on 19.1.x
		},
		{name: "status-server", fn: runStatusServer},
		{
			name: "version-upgrade",
			fn: func(ctx context.Context, t *test, c *cluster) {
				runVersionUpgrade(ctx, t, c, r.buildVersion)
			},
			// This test doesn't like running on old versions because it upgrades to
			// the latest released version and then it tries to "head", where head is
			// the kwbase binary built from the branch on which the test is
			// running. If that branch corresponds to an older release, then upgrading
			// to head after 19.2 fails.
			minVersion: "v19.2.0",
			timeout:    30 * time.Minute,
		},
	}
	tags := []string{"default", "quick"}
	const numNodes = 4
	specTemplate := testSpec{
		// NB: teamcity-post-failures.py relies on the acceptance tests
		// being named acceptance/<testname> and will avoid posting a
		// blank issue for the "acceptance" parent test. Make sure to
		// teach that script (if it's still used at that point) should
		// this naming scheme ever change (or issues such as #33519)
		// will be posted.
		Name:    "acceptance",
		Owner:   OwnerKV,
		Timeout: 10 * time.Minute,
		Tags:    tags,
		Cluster: makeClusterSpec(numNodes),
	}

	for _, tc := range testCases {
		tc := tc // copy for closure
		spec := specTemplate
		spec.Skip = tc.skip
		spec.Name = specTemplate.Name + "/" + tc.name
		spec.MinVersion = tc.minVersion
		if tc.timeout != 0 {
			spec.Timeout = tc.timeout
		}
		spec.Run = func(ctx context.Context, t *test, c *cluster) {
			tc.fn(ctx, t, c)
		}
		r.Add(spec)
	}
}
