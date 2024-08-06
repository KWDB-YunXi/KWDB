// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package tree

// ReplicationInformation represents a ALTER REPLICATE TO statement.
type ReplicationInformation struct {
	Targets TargetList
	To      PartitionedBackup
	Options KVOptions
	IsTs    bool
}

// Format implements the NodeFormatter interface.
func (node *ReplicationInformation) Format(ctx *FmtCtx) {
	ctx.WriteString("ALTER ")
	ctx.FormatNode(&node.Targets)
	if node.To != nil {
		ctx.WriteString("REPLICATE TO ")
		ctx.FormatNode(&node.To)
	}
}
