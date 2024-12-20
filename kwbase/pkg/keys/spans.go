// Copyright 2015 The Cockroach Authors.
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

package keys

import "gitee.com/kwbasedb/kwbase/pkg/roachpb"

//go:generate go run gen_cpp_keys.go

var (
	// Meta1Span holds all first level addressing records.
	Meta1Span = roachpb.Span{Key: roachpb.KeyMin, EndKey: Meta2Prefix}

	// Meta2MaxSpan begins at key Meta2KeyMax with the last entry in the second
	// level addressing keyspace. The rest of the span is always empty. We cannot
	// split at this starting key or between it and MetaMax.
	// - The first condition is explained and enforced in libroach/mvcc.cc.
	// - The second condition is necessary because a split between Meta2KeyMax
	//   and MetaMax would conflict with the use of Meta1KeyMax to hold the
	//   descriptor for the range that spans the meta2/userspace boundary (see
	//   case 3a in rangeAddressing). If splits were allowed in this span then
	//   the descriptor for the ranges ending in this span would be stored AFTER
	//   Meta1KeyMax, which would allow meta1 to get out of order.
	Meta2MaxSpan = roachpb.Span{Key: Meta2KeyMax, EndKey: MetaMax}

	// NodeLivenessSpan holds the liveness records for nodes in the cluster.
	NodeLivenessSpan = roachpb.Span{Key: NodeLivenessPrefix, EndKey: NodeLivenessKeyMax}

	// SystemConfigSpan is the range of system objects which will be gossiped.
	SystemConfigSpan = roachpb.Span{Key: SystemConfigSplitKey, EndKey: SystemConfigTableDataMax}

	// NoSplitSpans describes the ranges that should never be split.
	// Meta1Span: needed to find other ranges.
	// Meta2MaxSpan: between meta and system ranges.
	// NodeLivenessSpan: liveness information on nodes in the cluster.
	// SystemConfigSpan: system objects which will be gossiped.
	NoSplitSpans = []roachpb.Span{Meta1Span, Meta2MaxSpan, NodeLivenessSpan, SystemConfigSpan}
)

// Silence unused warnings. These variables are actually used by gen_cpp_keys.go.
var _ = NoSplitSpans
