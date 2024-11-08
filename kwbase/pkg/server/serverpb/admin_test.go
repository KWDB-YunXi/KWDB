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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTableStatsResponseAdd verifies that TableStatsResponse.Add()
// correctly represents the result of combining stats from two spans.
// Specifically, most TableStatsResponse's stats are a straight-forward sum,
// but NodeCount should decrement as more missing nodes are added.
func TestTableStatsResponseAdd(t *testing.T) {

	// Initial object: no missing nodes.
	underTest := TableStatsResponse{
		RangeCount:           4,
		ReplicaCount:         4,
		ApproximateDiskBytes: 1000,
		NodeCount:            8,
	}

	// Add stats: no missing nodes, so NodeCount should stay the same.
	underTest.Add(&TableStatsResponse{
		RangeCount:           1,
		ReplicaCount:         2,
		ApproximateDiskBytes: 2345,
		NodeCount:            8,
	})
	assert.Equal(t, int64(5), underTest.RangeCount)
	assert.Equal(t, int64(6), underTest.ReplicaCount)
	assert.Equal(t, uint64(3345), underTest.ApproximateDiskBytes)
	assert.Equal(t, int64(8), underTest.NodeCount)

	// Add more stats: this time "node1" is missing. NodeCount should decrement.
	underTest.Add(&TableStatsResponse{
		RangeCount:           0,
		ReplicaCount:         0,
		ApproximateDiskBytes: 0,
		NodeCount:            7,
		MissingNodes: []TableStatsResponse_MissingNode{
			{
				NodeID:       "node1",
				ErrorMessage: "error msg",
			},
		},
	})
	assert.Equal(t, int64(5), underTest.RangeCount)
	assert.Equal(t, int64(6), underTest.ReplicaCount)
	assert.Equal(t, uint64(3345), underTest.ApproximateDiskBytes)
	assert.Equal(t, int64(7), underTest.NodeCount)
	assert.Equal(t, []TableStatsResponse_MissingNode{
		{
			NodeID:       "node1",
			ErrorMessage: "error msg",
		},
	}, underTest.MissingNodes)

	// Add more stats: "node1" is missing again. NodeCount shouldn't decrement.
	underTest.Add(&TableStatsResponse{
		RangeCount:           0,
		ReplicaCount:         0,
		ApproximateDiskBytes: 0,
		NodeCount:            7,
		MissingNodes: []TableStatsResponse_MissingNode{
			{
				NodeID:       "node1",
				ErrorMessage: "different error msg",
			},
		},
	})
	assert.Equal(t, int64(5), underTest.RangeCount)
	assert.Equal(t, int64(6), underTest.ReplicaCount)
	assert.Equal(t, uint64(3345), underTest.ApproximateDiskBytes)
	assert.Equal(t, int64(7), underTest.NodeCount)

	// Add more stats: new node is missing ("node2"). NodeCount should decrement.
	underTest.Add(&TableStatsResponse{
		RangeCount:           0,
		ReplicaCount:         0,
		ApproximateDiskBytes: 0,
		NodeCount:            7,
		MissingNodes: []TableStatsResponse_MissingNode{
			{
				NodeID:       "node2",
				ErrorMessage: "totally new error msg",
			},
		},
	})
	assert.Equal(t, int64(5), underTest.RangeCount)
	assert.Equal(t, int64(6), underTest.ReplicaCount)
	assert.Equal(t, uint64(3345), underTest.ApproximateDiskBytes)
	assert.Equal(t, int64(6), underTest.NodeCount)
	assert.Equal(t, []TableStatsResponse_MissingNode{
		{
			NodeID:       "node1",
			ErrorMessage: "error msg",
		},
		{
			NodeID:       "node2",
			ErrorMessage: "totally new error msg",
		},
	}, underTest.MissingNodes)

}
