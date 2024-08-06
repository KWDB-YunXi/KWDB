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

package serverpb

// Add adds values from ots to ts.
func (ts *TableStatsResponse) Add(ots *TableStatsResponse) {
	ts.RangeCount += ots.RangeCount
	ts.ReplicaCount += ots.ReplicaCount
	ts.ApproximateDiskBytes += ots.ApproximateDiskBytes
	ts.Stats.Add(ots.Stats)

	// The stats in TableStatsResponse were generated by getting separate stats
	// for each node, then aggregating them into TableStatsResponse.
	// So resulting NodeCount should be the same, unless ots contains nodeData
	// in MissingNodes that isn't already tracked in ts.MissingNodes.
	// Note: when comparing missingNode objects, there's a chance that the nodeId
	// could be the same, but that the error messages differ. Keeping the first
	// and dropping subsequent ones seems reasonable to do, and is what is done
	// here.
	missingNodeIds := make(map[string]struct{})
	for _, nodeData := range ts.MissingNodes {
		missingNodeIds[nodeData.NodeID] = struct{}{}
	}
	for _, nodeData := range ots.MissingNodes {
		if _, found := missingNodeIds[nodeData.NodeID]; !found {
			ts.MissingNodes = append(ts.MissingNodes, nodeData)
			ts.NodeCount--
		}
	}
}
