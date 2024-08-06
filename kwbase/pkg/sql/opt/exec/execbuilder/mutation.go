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

package execbuilder

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/execinfrapb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/hashrouter/api"
	"gitee.com/kwbasedb/kwbase/pkg/sql/lex"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/exec"
	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/memo"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
	"gitee.com/kwbasedb/kwbase/pkg/util"
	"gitee.com/kwbasedb/kwbase/pkg/util/duration"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq/oid"
	"github.com/paulsmith/gogeos/geos"
)

func (b *Builder) buildMutationInput(
	mutExpr, inputExpr memo.RelExpr, colList opt.ColList, p *memo.MutationPrivate,
) (execPlan, error) {
	if b.shouldApplyImplicitLockingToMutationInput(mutExpr) {
		// Re-entrance is not possible because mutations are never nested.
		b.forceForUpdateLocking = true
		defer func() { b.forceForUpdateLocking = false }()
	}

	input, err := b.buildRelational(inputExpr)
	if err != nil {
		return execPlan{}, err
	}

	if p.WithID != 0 {
		// The input might have extra columns that are used only by FK checks; make
		// sure we don't project them away.
		cols := inputExpr.Relational().OutputCols.Copy()
		for _, c := range colList {
			cols.Remove(c)
		}
		for c, ok := cols.Next(0); ok; c, ok = cols.Next(c + 1) {
			colList = append(colList, c)
		}
	}

	input, err = b.ensureColumns(input, colList, nil, inputExpr.ProvidedPhysical().Ordering)
	if err != nil {
		return execPlan{}, err
	}

	if p.WithID != 0 {
		label := fmt.Sprintf("buffer %d", p.WithID)
		input.root, err = b.factory.ConstructBuffer(input.root, label)
		if err != nil {
			return execPlan{}, err
		}

		b.addBuiltWithExpr(p.WithID, input.outputCols, input.root)
	}
	return input, nil
}

func (b *Builder) buildInsert(ins *memo.InsertExpr) (execPlan, error) {
	if ep, ok, err := b.tryBuildFastPathInsert(ins); err != nil || ok {
		return ep, err
	}
	// Construct list of columns that only contains columns that need to be
	// inserted (e.g. delete-only mutation columns don't need to be inserted).
	colList := make(opt.ColList, 0, len(ins.InsertCols)+len(ins.CheckCols))
	colList = appendColsWhenPresent(colList, ins.InsertCols)
	colList = appendColsWhenPresent(colList, ins.CheckCols)
	input, err := b.buildMutationInput(ins, ins.Input, colList, &ins.MutationPrivate)
	if err != nil {
		return execPlan{}, err
	}

	// Construct the Insert node.
	tab := b.mem.Metadata().Table(ins.Table)
	insertOrds := ordinalSetFromColList(ins.InsertCols)
	checkOrds := ordinalSetFromColList(ins.CheckCols)
	returnOrds := ordinalSetFromColList(ins.ReturnCols)
	// If we planned FK checks, disable the execution code for FK checks.
	disableExecFKs := !ins.FKFallback
	node, err := b.factory.ConstructInsert(
		input.root,
		tab,
		insertOrds,
		returnOrds,
		checkOrds,
		b.allowAutoCommit && len(ins.Checks) == 0,
		disableExecFKs,
	)
	if err != nil {
		return execPlan{}, err
	}
	// Construct the output column map.
	ep := execPlan{root: node}
	if ins.NeedResults() {
		ep.outputCols = mutationOutputColMap(ins)
	}

	if err := b.buildFKChecks(ins.Checks); err != nil {
		return execPlan{}, err
	}

	return ep, nil
}

// buildTSInsert builds the TSInsertExpr into a tsInsertNode. The main work is as follows:
//   - Check whether the value of input matches the type of column to be inserted.
//   - Encode the input value according to the agreed encoding format.
//     This is done in order to easily and quickly write data to the timing engine.
//   - In particular, if we are inserting an instance table and the instance table
//     does not exist, we need to create the instance table before building node.
func (b *Builder) buildTSInsert(tsInsert *memo.TSInsertExpr) (execPlan, error) {
	// TS table doesn't support explicit txn, so raise error if detected
	if !b.evalCtx.TxnImplicit {
		return execPlan{}, sqlbase.UnsupportedTSExplicitTxnError()
	}
	// create instance table
	if tsInsert.NeedCreateTable && tsInsert.CT != nil {
		if err := preCreateInstTable(b, tsInsert); err != nil {
			return execPlan{}, err
		}
	}
	// prepare metadata used to construct TS insert node.
	md := b.mem.Metadata()
	tab := md.Table(tsInsert.STable)
	dbID := tab.GetParentID()
	tsVersion := tab.GetTSVersion()
	cols := make([]*sqlbase.ColumnDescriptor, tab.ColumnCount())
	for i := 0; i < tab.ColumnCount(); i++ {
		cols[i] = tab.Column(i).(*sqlbase.ColumnDescriptor)
	}
	// builds the payload of each node according to the input value
	payloadNodeMap, err := BuildInputForTSInsert(
		b.evalCtx,
		tsInsert.InputRows,
		cols,
		tsInsert.ColsMap,
		uint32(dbID),
		uint32(tab.ID()),
		tab.GetTableType() == tree.InstanceTable,
		tsVersion,
	)
	if err != nil {
		return execPlan{}, err
	}
	node, err := b.factory.ConstructTSInsert(payloadNodeMap)
	if err != nil {
		return execPlan{}, err
	}

	return execPlan{root: node}, nil
}

// BuildInputForTSInsert groups the input values and assembles them into TsPayload.
//
//		The main work is as follows:
//	  - The columns to be inserted fall into two categories: data columns and tag columns.
//	    Then calculate the size that these columns need to occupy in TsPayload based on type.
//	  - Fill in the parameters needed to build TsPayload.
//	  - Preallocate the memory space needed to build TsPayload. This space is only
//	    an estimated size. It may be necessary to expand the size of P because of the existence
//	    of variable-length types.
//	  - Type check is performed on each line value entered. And the rows with the same primaryTag
//	    value are grouped together.
//	  - Build TsPayload for each group separately. The corresponding node is then calculated by
//	    hashRouter according to primaryTagVal.
func BuildInputForTSInsert(
	evalCtx *tree.EvalContext,
	InputRows opt.RowsValue,
	columns []*sqlbase.ColumnDescriptor,
	colIndexsInMemo map[int]int,
	dbID uint32,
	tabID uint32,
	isInsertInstTable bool,
	tsVersion uint32,
) (map[int]*sqlbase.PayloadForDistTSInsert, error) {
	colIndexs := make(map[int]int, len(colIndexsInMemo))
	for k, v := range colIndexsInMemo {
		colIndexs[k] = v
	}

	var primaryTagCols, allTagCols, dataCols []*sqlbase.ColumnDescriptor
	haveTagCol, haveDataCol := false, false

	// Iterating through the column metadata, primary tag columns/all tag columns/data columns are obtained respectively.
	for i := range columns {
		col := columns[i]
		if col.IsPrimaryTagCol() {
			primaryTagCols = append(primaryTagCols, col)
		}
		if col.IsTagCol() {
			allTagCols = append(allTagCols, col)
		} else {
			dataCols = append(dataCols, col)
		}
		if _, ok := colIndexs[int(col.ID)]; ok {
			if col.IsTagCol() {
				haveTagCol = true
			}
			if !col.IsTagCol() {
				haveDataCol = true
			}
		} else if col.TsCol.ColumnType == sqlbase.ColumnType_TYPE_TAG && isInsertInstTable {
			// Insert instance table may not specify the tag column, so this flag is used to skip non-null checks.
			colIndexs[int(col.ID)] = sqlbase.ColumnValNotExist
		} else {
			// Attempts to insert a NULL value when no value is specified.
			colIndexs[int(col.ID)] = sqlbase.ColumnValIsNull
		}
	}
	if !haveDataCol {
		// only insert tag column, flag = 2.
		dataCols = dataCols[:0]
	}
	if haveDataCol && !haveTagCol {
		// only insert data column, flag = 1.
		allTagCols = allTagCols[:0]
	}

	// Integrate the arguments required by payload
	pArgs, err := BuildPayloadArgs(tsVersion, primaryTagCols, allTagCols, dataCols)
	if err != nil {
		return nil, err
	}
	// Reorder the columns. The order is as follows:
	// [primary tag columns + all tag columns + data columns]
	prettyCols := make([]*sqlbase.ColumnDescriptor, pArgs.PTagNum+pArgs.AllTagNum+pArgs.DataColNum)
	copy(prettyCols[:pArgs.PTagNum], primaryTagCols)
	copy(prettyCols[pArgs.PTagNum:], allTagCols)
	copy(prettyCols[pArgs.PTagNum+pArgs.AllTagNum:], dataCols)

	var inputDatums []tree.Datums
	// partition input data based on primary tag values
	priTagValMap := make(map[string][]int)

	rowNum := len(InputRows)
	rowLen := len(InputRows[0])
	inputDatums = make([]tree.Datums, rowNum)
	// allocate memory for two nested slices, for better performance
	preSlice := make([]tree.Datum, rowNum*rowLen)
	for i := 0; i < rowNum; i++ {
		inputDatums[i], preSlice = preSlice[:rowLen:rowLen], preSlice[rowLen:]
	}
	// Type check for input values.
	var buf strings.Builder
	for i := range InputRows {
		for j, col := range prettyCols {
			valIdx := colIndexs[int(col.ID)]
			if valIdx < 0 {
				if !col.Nullable && valIdx == sqlbase.ColumnValIsNull {
					return nil, sqlbase.NewNonNullViolationError(col.Name)
				}
				continue
			}
			// checks whether the input value is valid for target column.
			inputDatums[i][valIdx], err = TSTypeCheckForInput(evalCtx, &InputRows[i][valIdx], &col.Type, col)
			if err != nil {
				return nil, err
			}
			if j < len(primaryTagCols) {
				buf.WriteString(sqlbase.DatumToString(inputDatums[i][valIdx]))
			}
		}
		// Group rows with the same primary tag value.
		priTagValMap[buf.String()] = append(priTagValMap[buf.String()], i)
		buf.Reset()
	}
	// Gets the Hash ring of the TS table.
	hashRouter, err := api.GetHashRouterWithTable(dbID, tabID, false, evalCtx.Txn)
	if err != nil {
		return nil, err
	}
	// Build payload separately for groups with the same primary tag value.
	payloadNodeMap := make(map[int]*sqlbase.PayloadForDistTSInsert, len(priTagValMap))
	for _, priTagRowIdx := range priTagValMap {
		payload, primaryTagVal, err := BuildPayloadForTsInsert(
			evalCtx,
			evalCtx.Txn,
			inputDatums,
			priTagRowIdx,
			prettyCols,
			colIndexs,
			pArgs,
			dbID,
			tabID,
			hashRouter,
		)
		if err != nil {
			return nil, err
		}
		// Calculate leaseHolder node based on primaryTag value.
		nodeID, err := hashRouter.GetNodeIDByPrimaryTag(evalCtx.Context, primaryTagVal)
		if err != nil {
			return nil, err
		}
		// Make primaryTag key.
		primaryTagKey := sqlbase.MakeTsPrimaryTagKey(sqlbase.ID(tabID), primaryTagVal)
		if val, ok := payloadNodeMap[int(nodeID[0])]; ok {
			val.PerNodePayloads = append(val.PerNodePayloads, &sqlbase.SinglePayloadInfo{
				Payload:       payload,
				RowNum:        uint32(len(priTagRowIdx)),
				PrimaryTagKey: primaryTagKey,
			})
		} else {
			rowVal := sqlbase.PayloadForDistTSInsert{
				NodeID: nodeID[0],
				PerNodePayloads: []*sqlbase.SinglePayloadInfo{{
					Payload:       payload,
					RowNum:        uint32(len(priTagRowIdx)),
					PrimaryTagKey: primaryTagKey,
				}}}
			payloadNodeMap[int(nodeID[0])] = &rowVal
		}
	}
	return payloadNodeMap, nil
}

const (
	// TxnIDOffset offset of txn_id in the payload header
	TxnIDOffset = 0
	// TxnIDSize length of txn_id in the payload header
	TxnIDSize = 16
	// RangeGroupIDOffset offset of range_group_id in the payload header
	RangeGroupIDOffset = 16
	// RangeGroupIDSize length of range_group_id in the payload header
	RangeGroupIDSize = 2
	// PayloadVersionOffset offset of payload_version in the payload header
	PayloadVersionOffset = 18
	// PayloadVersionSize length of payload_version in the payload header
	PayloadVersionSize = 4
	// DBIDOffset offset of db_id in the payload header
	DBIDOffset = 22
	// DBIDSize length of db_id in the payload header
	DBIDSize = 4
	// TableIDOffset offset of table_id in the payload header
	TableIDOffset = 26
	// TableIDSize length of table_id in the payload header
	TableIDSize = 8
	// TSVersionOffset offset of ts_version in the payload header
	TSVersionOffset = 34
	// TSVersionSize length of ts_version in the payload header
	TSVersionSize = 4
	// RowNumOffset offset of row_num in the payload header
	RowNumOffset = 38
	// RowNumSize length of row_num in the payload header
	RowNumSize = 4
	// RowTypeOffset offset of row_type in the payload header
	RowTypeOffset = 42
	// RowTypeSize length of row_type in the payload header
	RowTypeSize = 1
	// HeadSize is the payload fixed header length of insert ts table
	HeadSize = RowTypeOffset + RowTypeSize
	// PTagLenSize length of primary tag
	PTagLenSize = 2
	// AllTagLenSize length of ordinary tag
	AllTagLenSize = 4
	// DataLenSize length of datalen size
	DataLenSize = 4
	// VarDataLenSize length of not fixed datalen
	VarDataLenSize = 2
	// VarColumnSize is the fixed length memory taken by var-length data type
	VarColumnSize = 8
)

// PayloadArgs stores information for three kind of columns
type PayloadArgs struct {
	// input need
	// TSVersion is table version for TS.
	TSVersion uint32
	// PayloadVersion is the payload codec version.
	PayloadVersion uint32
	// PTagNum is primary tag column num.
	// AllTagNum is all tag column num.
	// DataColNum is data column num.
	PTagNum, AllTagNum, DataColNum int
	// RowType identifies the type that the write column contains:
	RowType string
	// PrimaryTagSize represents the fixed length of primary tag column.
	PrimaryTagSize int
	// AllTagSize represents the fixed length of all tag column.
	AllTagSize int
	// DataColSize represents the fixed length of data column.
	DataColSize int
	// PreAllocTagSize represents the pre-allocated size of all tag column.
	PreAllocTagSize int
	// PreAllocColSize represents the pre-allocated size of data column.
	PreAllocColSize int

	// PayloadSize represents the size of payload calculated in advance, not the final size
	PayloadSize int
}

// DeepCopy returns a new PayloadArgs from an exists PayloadArgs
func (p PayloadArgs) DeepCopy() PayloadArgs {
	return PayloadArgs{
		TSVersion:       p.TSVersion,
		PayloadVersion:  p.PayloadVersion,
		PTagNum:         p.PTagNum,
		AllTagNum:       p.AllTagNum,
		DataColNum:      p.DataColNum,
		PrimaryTagSize:  p.PrimaryTagSize,
		AllTagSize:      p.AllTagSize,
		DataColSize:     p.DataColSize,
		PreAllocTagSize: p.PreAllocTagSize,
		PreAllocColSize: p.PreAllocColSize,
		PayloadSize:     p.PayloadSize,
		RowType:         p.RowType,
	}
}

const (
	// OnlyTag TsPayload contains datums consists of tag only data
	OnlyTag = "tag"
	// OnlyData TsPayload contains datums consists of metric data only
	OnlyData = "data"
	// BothTagAndData TsPayload contains datums consists of both tag and metric data
	BothTagAndData = "both"
)

// RowType is bit map of TsPayload which is used to assemble payload
var RowType = map[string]byte{
	OnlyData:       byte(1),
	OnlyTag:        byte(2),
	BothTagAndData: byte(0),
}

// PayloadHeader min params of make payload header all
type PayloadHeader struct {
	TxnID          uuid.UUID
	PayloadVersion uint32
	DBID           uint32
	TBID           uint64
	TSVersion      uint32
	RowNum         uint32
	RowType        string

	groupIDOffset int // offset of groupID start

	otherTagBitmapLen    int
	otherTagLenOffset    int
	otherTagBitmapOffset int

	// compute params
	offset          int
	columnBitmapLen int
}

// TsPayload make payload from PayloadArgs and PayloadHeader
type TsPayload struct {
	args       PayloadArgs
	header     PayloadHeader
	payload    []byte
	primaryKey string
}

// NewTsPayload create a empty struct
func NewTsPayload() *TsPayload {
	return &TsPayload{}
}

// SetHeader copy param from header to TsPayload
func (t *TsPayload) SetHeader(header PayloadHeader) {
	t.header.TxnID = header.TxnID
	t.header.PayloadVersion = header.PayloadVersion
	t.header.DBID = header.DBID
	t.header.TBID = header.TBID
	t.header.TSVersion = header.TSVersion
	t.header.RowNum = header.RowNum
}

// fillHeader fills the header of TsPayload with the obtained parameters.
func (t *TsPayload) fillHeader() {
	/*header part
	  ______________________________________________________________________________________________
	  |    16    |    2    |         4        |   4  |    8    |       4        |   4    |    1    |
	  |----------|---------|------------------|------|---------|----------------|--------|---------|
	  |  txnID   | groupID |  payloadVersion  | dbID |  tbID   |    TSVersion   | rowNum | rowType |
	*/
	t.header.offset = TxnIDOffset
	copy(t.payload[t.header.offset:], t.header.TxnID.GetBytes())
	t.header.offset += TxnIDSize
	t.header.groupIDOffset = t.header.offset
	t.header.offset += RangeGroupIDSize
	// payload version
	binary.LittleEndian.PutUint32(t.payload[t.header.offset:], t.header.PayloadVersion)
	t.header.offset += PayloadVersionSize
	binary.LittleEndian.PutUint32(t.payload[t.header.offset:], t.header.DBID)
	t.header.offset += DBIDSize
	binary.LittleEndian.PutUint64(t.payload[t.header.offset:], t.header.TBID)
	t.header.offset += TableIDSize
	// table version
	binary.LittleEndian.PutUint32(t.payload[t.header.offset:], t.header.TSVersion)
	t.header.offset += TSVersionSize
	binary.LittleEndian.PutUint32(t.payload[t.header.offset:], t.header.RowNum)
	t.header.offset += RowNumSize
	switch t.header.RowType {
	case BothTagAndData:
		t.payload[t.header.offset] = RowType[BothTagAndData]
	case OnlyData:
		t.payload[t.header.offset] = RowType[OnlyData]
	case OnlyTag:
		t.payload[t.header.offset] = RowType[OnlyTag]
	default:
		t.payload[t.header.offset] = RowType[BothTagAndData]
	}
	t.header.offset++

	binary.LittleEndian.PutUint16(t.payload[t.header.offset:], uint16(t.args.PrimaryTagSize))
	t.header.offset += PTagLenSize
}

// SetArgs set payload args to TsPayload
func (t *TsPayload) SetArgs(args PayloadArgs) {
	/*
		args part
		________________________________________________________
		|    2    | 41+ptagSize| 4       |allTagSize|
		|---------|------------|---------|----------|
		| pTagSize| tagLen     |tagBitMap|allTagSize|
	*/
	t.args = args.DeepCopy()
	t.header.otherTagBitmapLen = (t.args.AllTagNum + 7) / 8
	t.header.otherTagLenOffset = HeadSize + PTagLenSize + t.args.PrimaryTagSize
	t.header.otherTagBitmapOffset = t.header.otherTagLenOffset + AllTagLenSize
	t.header.RowType = t.args.RowType
}

// BuildRowsPayloadByDatums encodes the input values according to the agreed format,
// and finally returns tsPayload.
// There are mainly the following parts:
//   - Estimate the memory size required by tp based on the obtained column metadata information
//     and the number of input rows.
//   - Encoding tp header
//   - Because the column metadata has been reordered by primaryTag + allTag + dataColumn.
//     So the data part of tsPayload is also encoded in this order.
//
// Parameters:
//   - InputDatums: Input values that have been checked and converted
//   - rowNum: The number of rows for the input value
//   - prettyCols: Reorder the column metadata.
//   - colIndexs: Mapping between column ids and input values.
//   - tolerant: Whether an error can be tolerated
//
// Returns:
//   - payload: Complete the encoded tsPayload.
//   - primaryTagVal: Encodings that contain only primaryTag values.
//   - importErrorRecord: If tolerant is true, the data and errors are recorded.
func (t *TsPayload) BuildRowsPayloadByDatums(
	InputDatums []tree.Datums,
	rowNum int,
	prettyCols []*sqlbase.ColumnDescriptor,
	colIndexs map[int]int,
	tolerant bool,
	getGroupIDFunc func(primaryTag []byte) (api.EntityRangeGroupID, error),
) ([]byte, []byte, []interface{}, error) {
	// payloadSize, otherTagSize, dataColumnSize
	ComputePayloadSize(&t.args, rowNum)
	t.payload = make([]byte, t.args.PayloadSize)
	t.fillHeader()

	// column data len offset
	dataLenOffset := 0
	// column bitmap length
	t.header.columnBitmapLen = (rowNum + 7) / 8
	var importErrorRecord []interface{}
	// offset for var-length data in tag cols
	independentOffset := t.header.otherTagBitmapOffset + t.args.AllTagSize
	columnBitmapOffset := 0
	var primaryTagVal []byte
	var inputValues tree.Datum
	offset := t.header.offset
	rowIDMapError := make(map[int]error, len(InputDatums))
	for j := range prettyCols {
		var curColLenth int
		column := prettyCols[j]
		IsTagCol := column.IsTagCol()
		IsPrimaryTagCol := column.IsPrimaryTagCol()
		if (int(column.TsCol.VariableLengthType) == sqlbase.StorageTuple) || IsPrimaryTagCol {
			curColLenth = int(column.TsCol.StorageLen)
		} else {
			curColLenth = VarColumnSize
		}
		// other tag data
		if IsTagCol && j == t.args.PTagNum {
			offset += AllTagLenSize + t.header.otherTagBitmapLen
		}

		// first data column
		if !IsTagCol && j == t.args.PTagNum+t.args.AllTagNum {
			// compute data column offset
			dataLenOffset = independentOffset
			columnBitmapOffset = dataLenOffset + DataLenSize
			offset = columnBitmapOffset + t.header.columnBitmapLen
			independentOffset = independentOffset + DataLenSize + t.args.DataColSize
			if independentOffset > len(t.payload) {
				addSize := rowNum * t.args.PreAllocColSize
				// grow payload size
				newPayload := make([]byte, independentOffset+addSize)
				copy(newPayload, t.payload)
				t.payload = newPayload
			}
		}

		colIdx := colIndexs[int(column.ID)]
		for i, datums := range InputDatums {
			if IsTagCol && i != 0 {
				// tag takes only the first row
				break
			}
			if colIdx < 0 {
				if IsTagCol {
					t.payload[t.header.otherTagBitmapOffset+(j-t.args.PTagNum)/8] |= 1 << ((j - t.args.PTagNum) % 8)
				} else {
					t.payload[columnBitmapOffset+i/8] |= 1 << (i % 8)
				}
				offset += curColLenth
				continue
			}
			inputValues = datums[colIdx]
			if inputValues == tree.DNull {
				if IsTagCol {
					t.payload[t.header.otherTagBitmapOffset+(j-t.args.PTagNum)/8] |= 1 << ((j - t.args.PTagNum) % 8)
				} else {
					t.payload[columnBitmapOffset+i/8] |= 1 << (i % 8)
				}
				offset += curColLenth
				continue
			}
			var err error
			if independentOffset, err = t.fillColData(inputValues, column, IsTagCol, IsPrimaryTagCol, offset, independentOffset, columnBitmapOffset); err != nil {
				if tolerant {
					rowIDMapError[i] = errors.Wrap(rowIDMapError[i], err.Error())
					err = nil
					continue
				} else {
					return t.payload, nil, nil, err
				}

			}
			offset += curColLenth
		}
		if j == t.args.PTagNum+t.args.AllTagNum-1 {
			// other tag len
			tagValLen := independentOffset - t.header.otherTagBitmapOffset
			binary.LittleEndian.PutUint32(t.payload[t.header.otherTagLenOffset:], uint32(tagValLen))
		}
		if !IsTagCol {
			// compute next column bitmap offset
			columnBitmapOffset += t.header.columnBitmapLen + curColLenth*rowNum
			offset += t.header.columnBitmapLen
		}
	}
	// var column value length
	colDataLen := independentOffset - dataLenOffset - DataLenSize
	binary.LittleEndian.PutUint32(t.payload[dataLenOffset:], uint32(colDataLen))

	// primary tag value
	primaryTagVal = t.payload[HeadSize+PTagLenSize : HeadSize+PTagLenSize+t.args.PrimaryTagSize]
	for id, err := range rowIDMapError {
		if err != nil {
			importErrorRecord = append(importErrorRecord,
				map[string]error{tree.ConvertDatumsToStr(InputDatums[id], ','): err})
		}
	}
	groupID, err := getGroupIDFunc(primaryTagVal)
	if err != nil {
		return nil, nil, importErrorRecord, err
	}
	binary.LittleEndian.PutUint16(t.payload[t.header.groupIDOffset:], uint16(groupID))
	return t.payload, primaryTagVal, importErrorRecord, nil
}

// fillColData fills the data of TsPayload with the input values.
func (t *TsPayload) fillColData(
	datum tree.Datum,
	column *sqlbase.ColumnDescriptor,
	IsTagCol bool,
	IsPrimaryTagCol bool,
	offset int,
	independentOffset int,
	columnBitmapOffset int,
) (int, error) {
	if datum != nil && reflect.ValueOf(datum).IsNil() {
		return independentOffset, errors.Errorf("unsupported NULL value")
	}
	switch v := datum.(type) {
	case *tree.DInt:
		switch column.Type.Oid() {
		case oid.T_int2:
			binary.LittleEndian.PutUint16(t.payload[offset:], uint16(*v))
		case oid.T_int4:
			binary.LittleEndian.PutUint32(t.payload[offset:], uint32(*v))
		case oid.T_int8, oid.T_timestamp, oid.T_timestamptz:
			binary.LittleEndian.PutUint64(t.payload[offset:], uint64(*v))
		default:
			return independentOffset, errors.Errorf("unsupported int oid")
		}
	case *tree.DFloat:
		switch column.Type.Oid() {
		case oid.T_float4:
			binary.LittleEndian.PutUint32(t.payload[offset:], uint32(int32(math.Float32bits(float32(*v)))))
		case oid.T_float8:
			binary.LittleEndian.PutUint64(t.payload[offset:], uint64(int64(math.Float64bits(float64(*v)))))
		default:
			return independentOffset, errors.Errorf("unsupported float oid")
		}

	case *tree.DBool:
		if *v {
			t.payload[offset] = 1
		} else {
			t.payload[offset] = 0
		}

	case *tree.DTimestamp:
		binary.LittleEndian.PutUint64(t.payload[offset:], uint64(v.UnixMilli()))

	case *tree.DTimestampTZ:
		binary.LittleEndian.PutUint64(t.payload[offset:], uint64(v.UnixMilli()))

	case *tree.DString:
		switch column.Type.Oid() {
		case oid.T_char, oid.T_text, oid.T_bpchar, types.T_geometry:
			copy(t.payload[offset:], *v)
		case types.T_nchar:
			copy(t.payload[offset:], *v)

		case oid.T_varchar, types.T_nvarchar:
			if IsPrimaryTagCol {
				copy(t.payload[offset:], *v)
			} else {
				//copy len
				dataOffset := 0
				if IsTagCol {
					dataOffset = independentOffset - t.header.otherTagBitmapOffset
				} else {
					dataOffset = independentOffset - columnBitmapOffset
				}
				binary.LittleEndian.PutUint32(t.payload[offset:], uint32(dataOffset))
				addSize := len(*v) + VarDataLenSize
				if independentOffset+addSize > len(t.payload) {
					// grow payload size
					newPayload := make([]byte, len(t.payload)+addSize)
					copy(newPayload, t.payload)
					t.payload = newPayload
				}
				// next var column offset
				binary.LittleEndian.PutUint16(t.payload[independentOffset:], uint16(len(*v)))
				copy(t.payload[independentOffset+VarDataLenSize:], *v)
				independentOffset += addSize
			}

		default:
			return independentOffset, errors.Errorf("unsupported int oid %v", column.Type.Oid())
		}

	case *tree.DBytes:
		switch column.Type.Oid() {
		case oid.T_bytea:
			// Special handling: When assembling the payload related to the bytes type,
			// write the actual length of the bytes type data at the beginning of the byte array.
			binary.LittleEndian.PutUint16(t.payload[offset:offset+2], uint16(len(*v)))
			copy(t.payload[offset+2:], *v)

		case types.T_varbytea:
			if IsPrimaryTagCol {
				copy(t.payload[offset:], *v)
			} else {
				dataOffset := 0
				if IsTagCol {
					dataOffset = independentOffset - t.header.otherTagBitmapOffset
				} else {
					dataOffset = independentOffset - columnBitmapOffset
				}
				binary.LittleEndian.PutUint32(t.payload[offset:], uint32(dataOffset))

				addSize := len(*v) + VarDataLenSize
				if independentOffset+addSize > len(t.payload) {
					// grow payload size
					newPayload := make([]byte, len(t.payload)+addSize)
					copy(newPayload, t.payload)
					t.payload = newPayload
				}
				// next var column offset
				binary.LittleEndian.PutUint16(t.payload[independentOffset:], uint16(len(*v)))
				copy(t.payload[independentOffset+VarDataLenSize:], *v)
				independentOffset += addSize
			}
		default:
			return independentOffset, errors.Errorf("unsupported int oid %v", column.Type.Oid())
		}

	default:
		return independentOffset, pgerror.Newf(pgcode.FeatureNotSupported, "unsupported input type %T", datum)
	}
	return independentOffset, nil
}

// BuildPayloadArgs return PayloadArgs
func BuildPayloadArgs(
	tsVersion uint32, primaryTagCols, allTagCols, dataCols []*sqlbase.ColumnDescriptor,
) (PayloadArgs, error) {
	// compute column size for TsPayload.
	pTagSize, _, err := ComputeColumnSize(primaryTagCols)
	if err != nil {
		return PayloadArgs{}, err
	}
	allTagSize, preTagSize, err := ComputeColumnSize(allTagCols)
	if err != nil {
		return PayloadArgs{}, err
	}
	dataSize, preDataSize, err := ComputeColumnSize(dataCols)
	if err != nil {
		return PayloadArgs{}, err
	}
	pTagNum, allTagNum, dataColNum := len(primaryTagCols), len(allTagCols), len(dataCols)
	rowType := BothTagAndData
	if dataColNum == 0 {
		rowType = OnlyTag
	} else if allTagNum == 0 {
		rowType = OnlyData
	}
	return PayloadArgs{
		TSVersion: tsVersion, PayloadVersion: sqlbase.TSInsertPayloadVersion, PTagNum: pTagNum, AllTagNum: allTagNum,
		DataColNum: dataColNum, PrimaryTagSize: pTagSize, AllTagSize: allTagSize, RowType: rowType,
		DataColSize: dataSize, PreAllocTagSize: preTagSize, PreAllocColSize: preDataSize,
	}, nil
}

// BuildPayloadForTsInsert construct tsPayload for build tsInsert.
// The main ones are as follows:
//   - First create a tsPayload and set the required parameter values.
//   - Last call function encodes the data part of tsPayload.
//
// Parameters:
//   - InputDatums: Input values that have been checked and converted
//   - primaryTagRowIdx: The index of the row subscript with the same primaryTag value.
//   - prettyCols: Reorder the column metadata.
//   - colIndexs: Mapping between column ids and input values.
//   - pArgs: Information needed to build tsPayload.
//
// Returns:
//   - payload: Complete the encoded tsPayload.
//   - primaryTagVal: Encodings that contain only primaryTag values.
func BuildPayloadForTsInsert(
	evalCtx *tree.EvalContext,
	txn *kv.Txn,
	InputDatums []tree.Datums,
	primaryTagRowIdx []int,
	prettyCols []*sqlbase.ColumnDescriptor,
	colIndexs map[int]int,
	pArgs PayloadArgs,
	dbID uint32,
	tableID uint32,
	hashRouter api.HashRouter,
) ([]byte, []byte, error) {
	rowNum := len(primaryTagRowIdx)
	tsPayload := NewTsPayload()
	tsPayload.SetArgs(pArgs)
	tsPayload.SetHeader(PayloadHeader{
		TxnID:          txn.ID(),
		PayloadVersion: pArgs.PayloadVersion,
		DBID:           dbID,
		TBID:           uint64(tableID),
		TSVersion:      pArgs.TSVersion,
		RowNum:         uint32(rowNum),
	})
	groupDatums := make([]tree.Datums, rowNum)
	for i := range groupDatums {
		groupDatums[i] = InputDatums[primaryTagRowIdx[i]]
	}

	payload, primaryTagVal, _, err := tsPayload.BuildRowsPayloadByDatums(groupDatums, rowNum, prettyCols, colIndexs,
		false,
		func(primaryTag []byte) (api.EntityRangeGroupID, error) {
			if evalCtx.StartSinglenode {
				return api.EntityRangeGroupID(tableID), nil
			}
			return hashRouter.GetGroupIDByPrimaryTag(evalCtx.Context, primaryTag)
		})
	if err != nil {
		return nil, nil, err
	}
	return payload, primaryTagVal, nil
}

// BuildPreparePayloadForTsInsert construct payload for build tsInsert.
func BuildPreparePayloadForTsInsert(
	evalCtx *tree.EvalContext,
	txn *kv.Txn,
	InputDatums [][][]byte,
	primaryTagRowIdx []int,
	cols []*sqlbase.ColumnDescriptor,
	colIndexs map[int]int,
	pArgs PayloadArgs,
	dbID uint32,
	tableID uint32,
	hashRouter api.HashRouter,
	qargs [][]byte,
	colnum int,
) ([]byte, []byte, error) {
	rowNum := len(primaryTagRowIdx)
	// compute payloadSize, otherTagSize, dataColumnSize
	ComputePayloadSize(&pArgs, rowNum)

	payload := make([]byte, pArgs.PayloadSize)

	var independentOffset int
	var offset int
	// encode payload head
	/*header part
	________________________________________________________________________________________________________
	|    16    |    2    |        4        |        4        |   4  |    8   |   4       |   4    |    1    |
	|----------|---------|-----------------|-----------------|------|--------|-----------|--------|---------|
	|  txnID   | groupID | storage version | payload version | dbID |  tbID  | tsVersion | rowNum | rowType |
	*/
	copy(payload[offset:], txn.ID().GetBytes())
	offset += TxnIDSize
	groupIDOffset := offset
	offset += RangeGroupIDSize
	// payload version
	binary.LittleEndian.PutUint32(payload[offset:], pArgs.PayloadVersion)
	offset += PayloadVersionSize
	binary.LittleEndian.PutUint32(payload[offset:], dbID)
	offset += DBIDSize
	binary.LittleEndian.PutUint64(payload[offset:], uint64(tableID))
	offset += TableIDSize
	// ts version
	binary.LittleEndian.PutUint32(payload[offset:], pArgs.TSVersion)
	offset += TSVersionSize
	binary.LittleEndian.PutUint32(payload[offset:], uint32(rowNum))
	offset += RowNumSize
	if pArgs.DataColNum == 0 {
		// without data column
		payload[offset] = byte(2)
	} else if pArgs.AllTagNum == 0 {
		// only data column
		payload[offset] = byte(1)
	} else {
		// both tag And data
		payload[offset] = byte(0)
	}
	offset++

	// primaryTag len
	binary.LittleEndian.PutUint16(payload[offset:], uint16(pArgs.PrimaryTagSize))
	offset += PTagLenSize

	otherTagBitmapLen := (pArgs.AllTagNum + 7) / 8
	// other tag len offset
	otherTagLenOffset := HeadSize + PTagLenSize + pArgs.PrimaryTagSize
	// other tag bitmap offset
	otherTagBitmapOffset := otherTagLenOffset + AllTagLenSize
	// tag variable length data start position
	independentOffset = otherTagBitmapOffset + pArgs.AllTagSize
	// column data len offset
	dataLenOffset := 0
	// column bitmap offset
	var columnBitmapOffset int
	// column bitmap length
	columnBitmapLen := (rowNum + 7) / 8

	var primaryTagVal []byte
	var inputValues []byte
	for j := range cols {
		var curColLenth int
		//inf := InferredTypes[1]
		column := cols[j]
		IsTagCol := column.IsTagCol()
		IsPrimaryTagCol := column.IsPrimaryTagCol()
		if (int(column.TsCol.VariableLengthType) == sqlbase.StorageTuple) || IsPrimaryTagCol {
			curColLenth = int(column.TsCol.StorageLen)
		} else {
			curColLenth = VarColumnSize
		}

		// other tag data
		if IsTagCol && j == pArgs.PTagNum {
			offset += AllTagLenSize + otherTagBitmapLen
		}

		// first data column
		if !IsTagCol && j == pArgs.PTagNum+pArgs.AllTagNum {
			// compute data column offset
			dataLenOffset = independentOffset
			columnBitmapOffset = dataLenOffset + DataLenSize
			offset = columnBitmapOffset + columnBitmapLen
			independentOffset = independentOffset + DataLenSize + pArgs.DataColSize
			if independentOffset > len(payload) {
				addSize := rowNum * pArgs.PreAllocColSize
				// grow payload size
				newPayload := make([]byte, independentOffset+addSize)
				copy(newPayload, payload)
				payload = newPayload
			}
		}

		colIdx := colIndexs[int(column.ID)]
		for i, rowIdx := range primaryTagRowIdx {
			if IsTagCol && i != 0 {
				// tag takes only the first row
				break
			}
			if colIdx < 0 {
				if IsTagCol {
					payload[otherTagBitmapOffset+(j-pArgs.PTagNum)/8] |= 1 << ((j - pArgs.PTagNum) % 8)
				} else {
					payload[columnBitmapOffset+i/8] |= 1 << (i % 8)
				}
				offset += curColLenth
				continue
			}

			if qargs == nil {
				inputValues = InputDatums[rowIdx][colIdx]
			} else {
				inputValues = qargs[rowIdx*colnum+colIdx]
			}
			if inputValues == nil {
				if IsTagCol {
					payload[otherTagBitmapOffset+(j-pArgs.PTagNum)/8] |= 1 << ((j - pArgs.PTagNum) % 8)
				} else {
					payload[columnBitmapOffset+i/8] |= 1 << (i % 8)
				}
				offset += curColLenth
				continue
			}

			switch column.Type.Family() {
			case types.IntFamily:
				switch column.Type.Oid() {
				case oid.T_int2, oid.T_int4, oid.T_int8, oid.T_timestamp, oid.T_timestamptz:
					copy(payload[offset:], inputValues)
				default:
					return payload, nil, errors.Errorf("unsupported int oid")
				}
			case types.FloatFamily:
				switch column.Type.Oid() {
				case oid.T_float4, oid.T_float8:
					copy(payload[offset:], inputValues)
				default:
					return payload, nil, errors.Errorf("unsupported float oid")
				}
			case types.BoolFamily:
				copy(payload[offset:], inputValues)
			case types.TimestampTZFamily, types.TimestampFamily:
				copy(payload[offset:], inputValues)
			case types.StringFamily:
				switch column.Type.Oid() {
				case oid.T_char, types.T_nchar, oid.T_text, oid.T_bpchar, types.T_geometry:
					copy(payload[offset:], inputValues)
				case oid.T_varchar, types.T_nvarchar:
					if IsPrimaryTagCol {
						copy(payload[offset:], inputValues)
					} else {
						//copy len
						dataOffset := 0
						if IsTagCol {
							dataOffset = independentOffset - otherTagBitmapOffset
						} else {
							dataOffset = independentOffset - columnBitmapOffset
						}
						binary.LittleEndian.PutUint32(payload[offset:], uint32(dataOffset))
						addSize := len(inputValues) + VarDataLenSize
						if independentOffset+addSize > len(payload) {
							// grow payload size
							newPayload := make([]byte, len(payload)+addSize)
							copy(newPayload, payload)
							payload = newPayload
						}
						// next var column offset
						binary.LittleEndian.PutUint16(payload[independentOffset:], uint16(len(inputValues)))
						copy(payload[independentOffset+VarDataLenSize:], inputValues)
						independentOffset += addSize
					}
				default:
					return payload, nil, errors.Errorf("unsupported int oid %v", column.Type.Oid())
				}
			case types.BytesFamily:
				switch column.Type.Oid() {
				case oid.T_bytea:
					// Special handling: When assembling the payload related to the bytes type,
					// write the actual length of the bytes type data at the beginning of the byte array.
					binary.LittleEndian.PutUint16(payload[offset:offset+2], binary.BigEndian.Uint16(inputValues))
					copy(payload[offset+2:], inputValues)

				case types.T_varbytea:
					if IsPrimaryTagCol {
						copy(payload[offset:], inputValues)
					} else {
						dataOffset := 0
						if IsTagCol {
							dataOffset = independentOffset - otherTagBitmapOffset
						} else {
							dataOffset = independentOffset - columnBitmapOffset
						}
						binary.LittleEndian.PutUint32(payload[offset:], uint32(dataOffset))

						addSize := len(inputValues) + VarDataLenSize
						if independentOffset+addSize > len(payload) {
							// grow payload size
							newPayload := make([]byte, len(payload)+addSize)
							copy(newPayload, payload)
							payload = newPayload
						}
						// next var column offset
						binary.LittleEndian.PutUint16(payload[independentOffset:], uint16(len(inputValues)))
						//binary.LittleEndian.PutUint16(payload[independentOffset:], binary.BigEndian.Uint16(inputValues))
						copy(payload[independentOffset+VarDataLenSize:], inputValues)
						independentOffset += addSize
					}
				default:
					return payload, nil, errors.Errorf("unsupported int oid %v", column.Type.Oid())
				}
			default:
				return payload, nil, pgerror.Newf(pgcode.FeatureNotSupported, "unsupported input type %T", inputValues)
			}

			offset += curColLenth
		}
		if j == pArgs.PTagNum+pArgs.AllTagNum-1 {
			// other tag len
			tagValLen := independentOffset - otherTagBitmapOffset
			binary.LittleEndian.PutUint32(payload[otherTagLenOffset:], uint32(tagValLen))
		}

		if !IsTagCol {
			// compute next column bitmap offset
			columnBitmapOffset += columnBitmapLen + curColLenth*rowNum
			offset += columnBitmapLen
		}
	}
	// var column value length
	colDataLen := independentOffset - dataLenOffset - DataLenSize
	binary.LittleEndian.PutUint32(payload[dataLenOffset:], uint32(colDataLen))

	// primary tag value
	primaryTagVal = payload[HeadSize+PTagLenSize : HeadSize+PTagLenSize+pArgs.PrimaryTagSize]
	var groupID api.EntityRangeGroupID
	if evalCtx.StartSinglenode {
		groupID = api.EntityRangeGroupID(tableID)
	} else {
		var err error
		groupID, err = hashRouter.GetGroupIDByPrimaryTag(evalCtx.Context, primaryTagVal)
		if err != nil {
			return nil, nil, err
		}
	}
	binary.LittleEndian.PutUint16(payload[groupIDOffset:], uint16(groupID))
	return payload, primaryTagVal, nil
}

// PgBinaryToTime takes an int64 and interprets it as the Postgres binary format
// for a timestamp. To create a timestamp from this value, it takes the microseconds
// delta and adds it to PGEpochJDate.
func PgBinaryToTime(i int64) time.Time {
	return duration.AddMicros(PGEpochJDate, i)
}

var (
	// PGEpochJDate represents the pg epoch.
	PGEpochJDate = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
)

// BuildInputForTSDelete construct payload for build tsInsert.
func BuildInputForTSDelete(
	evalCtx *tree.EvalContext,
	InputRows opt.RowsValue,
	primaryTagRowIdx []int,
	primaryTagCols []*sqlbase.ColumnDescriptor,
	colIndexs map[int]int,
) ([]byte, bool, error) {
	var isOutOfRange bool
	primaryTagSize, _, err := ComputeColumnSize(primaryTagCols)
	if err != nil {
		return nil, isOutOfRange, err
	}
	// Allocating payload space
	payload := make([]byte, primaryTagSize)
	var putbuf [8]byte
	var offset int
	for j := range primaryTagCols {
		var curColLenth int
		column := primaryTagCols[j]
		curColLenth = int(column.TsCol.StorageLen)
		for i, rowIdx := range primaryTagRowIdx {
			inputRow := InputRows[rowIdx][colIndexs[int(column.ID)]]
			if v, ok := inputRow.(*tree.Placeholder); ok {
				var resValue tree.Expr
				resValue, err = v.Eval(evalCtx)
				if err != nil {
					return nil, isOutOfRange, err
				}
				if _, ok := resValue.(tree.DNullExtern); ok {
					if !column.Nullable {
						return nil, isOutOfRange, sqlbase.NewNonNullViolationError(column.Name)
					}
					break
				}
				if inputRow, err = TSTypeCheckForInput(evalCtx, &resValue, &column.Type, column); err != nil {
					return nil, isOutOfRange, err
				}
			}
			switch v := inputRow.(type) {
			case *tree.DInt:
				width := uint(column.Type.Width() - 1)

				// We're performing bounds checks inline with Go's implementation of min and max ints in Math.go.
				shifted := *v >> width
				if (*v >= 0 && shifted > 0) || (*v < 0 && shifted < -1) {
					isOutOfRange = true
				}
				switch column.Type.Oid() {
				case oid.T_int2:
					binary.LittleEndian.PutUint16(payload[offset+curColLenth*i:], uint16(*v))
				case oid.T_int4:
					binary.LittleEndian.PutUint32(payload[offset+curColLenth*i:], uint32(*v))
				case oid.T_int8, oid.T_timestamp, oid.T_timestamptz:
					binary.LittleEndian.PutUint64(payload[offset+curColLenth*i:], uint64(*v))
				default:
					return nil, isOutOfRange, errors.Errorf("unsupported int oid")
				}
			case *tree.DFloat:
				switch column.Type.Oid() {
				case oid.T_float4:
					binary.LittleEndian.PutUint32(putbuf[:], uint32(int32(math.Float32bits(float32(*v)))))
					copy(payload[offset+curColLenth*i:], putbuf[:4])
				case oid.T_float8:
					binary.LittleEndian.PutUint64(putbuf[:], uint64(int64(math.Float64bits(float64(*v)))))
					copy(payload[offset+curColLenth*i:], putbuf[:8])
				default:
					return nil, isOutOfRange, errors.Errorf("unsupported float oid")
				}

			case *tree.DBool:
				if *v {
					payload[offset+curColLenth*i] = 1
				} else {
					payload[offset+curColLenth*i] = 0
				}

			case *tree.DTimestamp:
				nanosecond := v.Time.Nanosecond()
				second := v.Time.Unix()
				binary.LittleEndian.PutUint64(putbuf[:], uint64(second*1000+int64(nanosecond/1000000))-8*60*60*1000)
				copy(payload[offset+curColLenth*i:], putbuf[:8])

			case *tree.DTimestampTZ:
				nanosecond := v.Time.Nanosecond()
				second := v.Time.Unix()
				binary.LittleEndian.PutUint64(putbuf[:], uint64(second*1000+int64(nanosecond/1000000))-8*60*60*1000)
				copy(payload[offset+curColLenth*i:], putbuf[:8])

			case *tree.DString:
				switch column.Type.Oid() {
				case oid.T_char, oid.T_text, oid.T_bpchar:
					copy(payload[offset+curColLenth*i:], *v)
				case types.T_nchar:
					copy(payload[offset+curColLenth*i:], *v)
				case oid.T_varchar, types.T_nvarchar:
					copy(payload[offset:], *v)
				default:
					return nil, isOutOfRange, errors.Errorf("unsupported int oid %v", column.Type.Oid())
				}

			case *tree.DBytes:
				switch column.Type.Oid() {
				case oid.T_bytea:
					// Special handling: When assembling the payload related to the bytes type,
					// write the actual length of the bytes type data at the beginning of the byte array.
					binary.LittleEndian.PutUint16(payload[offset+(curColLenth)*i:offset+2+(curColLenth)*i], uint16(len(*v)))
					copy(payload[offset+2+(curColLenth)*i:], *v)
				case types.T_varbytea:
					copy(payload[offset+curColLenth*i:], *v)
				default:
					return nil, isOutOfRange, errors.Errorf("unsupported int oid %v", column.Type.Oid())
				}

			// decimal means out of range when primary tag is type int
			case *tree.DDecimal:
				isOutOfRange = true

			default:
				return nil, isOutOfRange, pgerror.Newf(pgcode.FeatureNotSupported, "unsupported input type %s", column.Type.String())
			}
		}
		offset += curColLenth
	}
	// primary tag value
	return payload, isOutOfRange, nil
}

// ComputePayloadSize calculates the fixed-length part size for payload
func ComputePayloadSize(pArgs *PayloadArgs, rowCount int) {
	if pArgs.AllTagSize != 0 {
		// other tags bitmap size
		pArgs.AllTagSize += (pArgs.AllTagNum + 7) / 8
	}
	if pArgs.DataColSize != 0 {
		pArgs.DataColSize = pArgs.DataColSize*rowCount + ((rowCount+7)/8)*pArgs.DataColNum
	}
	pArgs.PayloadSize = HeadSize + PTagLenSize + pArgs.PrimaryTagSize + AllTagLenSize + pArgs.AllTagSize + DataLenSize + pArgs.DataColSize
	pArgs.PayloadSize += pArgs.PreAllocTagSize + rowCount*pArgs.PreAllocColSize
	return
}

// PreComputePayloadSize computes how much memory need make for rowCount
func PreComputePayloadSize(pArgs *PayloadArgs, rowCount int) int {
	var AllTagSize, DataColSize, PayloadSize int
	if pArgs.AllTagSize != 0 {
		// other tags bitmap size
		AllTagSize = pArgs.AllTagSize + (pArgs.AllTagNum+7)/8
	}
	if pArgs.DataColSize != 0 {
		DataColSize = pArgs.DataColSize*rowCount + ((rowCount+7)/8)*pArgs.DataColNum
	}
	PayloadSize = HeadSize + PTagLenSize + pArgs.PrimaryTagSize + AllTagLenSize + AllTagSize + DataLenSize + DataColSize
	PayloadSize += pArgs.PreAllocTagSize + rowCount*pArgs.PreAllocColSize
	return PayloadSize
}

// ComputeColumnSize computes colSize
func ComputeColumnSize(cols []*sqlbase.ColumnDescriptor) (int, int, error) {
	colSize := 0
	preAllocSize := 0
	for i := range cols {
		col := cols[i]
		switch col.Type.Oid() {
		case oid.T_int2:
			colSize += 2
		case oid.T_int4, oid.T_float4:
			colSize += 4
		case oid.T_int8, oid.T_float8, oid.T_timestamp, oid.T_timestamptz:
			if !col.IsTagCol() && i == 0 {
				// The first timestamp column in the data column needs to reserve 16 bytes for LSN
				colSize += sqlbase.FirstTsDataColSize
			} else {
				colSize += 8
			}
		case oid.T_bool:
			colSize++
		case oid.T_char, types.T_nchar, oid.T_text, oid.T_bpchar, oid.T_bytea, types.T_geometry:
			colSize += int(col.TsCol.StorageLen)
		case oid.T_varchar, types.T_nvarchar, types.T_varbytea:
			if col.TsCol.VariableLengthType == sqlbase.StorageTuple || col.IsPrimaryTagCol() {
				colSize += int(col.TsCol.StorageLen)
			} else {
				// pre allocate paylaod space for var-length colums
				// here we use some fixed-rule to preallocate more space to improve efficiency
				// StorageLen = userWidth+1
				// varDataLen = StorageLen+2
				if col.TsCol.StorageLen < 68 {
					// 100%
					preAllocSize += int(col.TsCol.StorageLen)
				} else if col.TsCol.StorageLen < 260 {
					// 60%
					preAllocSize += int(col.TsCol.StorageLen/5) * 3
				} else {
					// 30%
					preAllocSize += int(col.TsCol.StorageLen/10) * 3
				}
				colSize += VarColumnSize
			}
		default:
			return 0, 0, pgerror.Newf(pgcode.DatatypeMismatch, "unsupported input type oid %d", col.Type.Oid())
		}
	}
	return colSize, preAllocSize, nil
}

// TSTypeCheckForInput checks whether the input value is valid based on the time-series table type，
// mostly data type and value validation
// The following cases correspond to two situations:
//   - For Parse values: input values are mostly of the form NumVal and StrVal,
//     with some DBool, FuncExpr, and DNullExtern.
//   - For Prepare DML or JDBC DML, the input values are basically Placeholder and Datum.
func TSTypeCheckForInput(
	evalCtx *tree.EvalContext,
	inputExpr *tree.Expr,
	colType *types.T,
	column *sqlbase.ColumnDescriptor,
) (tree.Datum, error) {
	var actualExpr tree.Expr
	actualExpr = *inputExpr
	if place, ok := (*inputExpr).(*tree.Placeholder); ok {
		actualExpr = tree.Expr(evalCtx.Placeholders.Values[place.Idx])
	}
	switch v := actualExpr.(type) {
	case *tree.NumVal:
		return v.TSTypeCheck(colType)

	case *tree.StrVal:
		return (*v).TSTypeCheck(colType, evalCtx)

	case *tree.DBool:
		switch colType.Family() {
		case types.BoolFamily:
			return v, nil
		case types.IntFamily:
			// Convert a bool value to a numeric value
			if v == tree.DBoolTrue {
				return tree.NewDInt(tree.DInt(1)), nil
			}
			return tree.NewDInt(tree.DInt(0)), nil
		default:
			return nil, tree.NewDatatypeMismatchError(v.String(), colType.SQLString())
		}

	case *tree.FuncExpr:
		if v.Func.FunctionName() != "now" {
			return nil, pgerror.Newf(pgcode.Syntax, "unsupported function input \"%s\"", v.Func.FunctionName())
		}
		evalDatum, err := v.Eval(evalCtx)
		if err != nil {
			return nil, err
		}
		switch colType.Oid() {
		case oid.T_timestamp:
			dVal := tree.DInt(evalDatum.(*tree.DTimestamp).UnixMilli())
			return &dVal, nil
		case oid.T_timestamptz:
			dVal := tree.DInt(evalDatum.(*tree.DTimestampTZ).UnixMilli())
			return &dVal, nil
		default:
			return nil, tree.NewDatatypeMismatchError(v.String(), colType.SQLString())
		}
	case tree.DNullExtern:
		if !column.Nullable {
			return nil, sqlbase.NewNonNullViolationError(column.Name)
		}
		return v, nil
	case *tree.DInt:
		switch colType.Oid() {
		case oid.T_int2, oid.T_int4, oid.T_int8:
			// Width is defined in bits.
			width := uint(colType.Width() - 1)
			// We're performing bounds checks inline with Go's implementation of min and max ints in Math.go.
			shifted := *v >> width
			if (*v >= 0 && shifted > 0) || (*v < 0 && shifted < -1) {
				return nil, pgerror.Newf(pgcode.NumericValueOutOfRange,
					"integer out of range for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return v, nil
		case oid.T_timestamp, oid.T_timestamptz:
			if *v < tree.TsMinTimestamp || *v > tree.TsMaxTimestamp {
				return nil, pgerror.Newf(pgcode.StringDataLengthMismatch,
					"value '%s' out of range for type %s", sqlbase.DatumToString(v), colType.SQLString())
			}
			// always send timestamp in DInt format to assemble payload
			return v, nil
		case oid.T_bool:
			if *v == 0 {
				return tree.DBoolFalse, nil
			}
			return tree.DBoolTrue, nil
		case oid.T_bpchar, oid.T_varchar, oid.T_text:
			if colType.Width() > 0 && len(v.String()) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return tree.NewDString(v.String()), nil
		case types.T_nchar, types.T_nvarchar:
			if colType.Width() > 0 && utf8.RuneCountInString(v.String()) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return tree.NewDString(v.String()), nil
		case oid.T_varbytea:
			if colType.Width() > 0 && len(v.String()) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return tree.NewDBytes(tree.DBytes(v.String())), nil

		default:
			return nil, tree.NewDatatypeMismatchError(sqlbase.DatumToString(v), colType.SQLString())
		}
	case *tree.DFloat:
		switch colType.Oid() {
		case oid.T_float4:
			// when checking float value is overflow or not, use abs() value.
			if *v != 0 && (math.Abs(float64(*v)) < math.SmallestNonzeroFloat32 || math.Abs(float64(*v)) > math.MaxFloat32) {
				return nil, pgerror.Newf(pgcode.NumericValueOutOfRange,
					"float \"%s\" out of range for type %s", v.String(), colType.Name())
			}
			return v, nil
		case oid.T_float8:
			if *v != 0 && (math.Abs(float64(*v)) < math.SmallestNonzeroFloat64 || math.Abs(float64(*v)) > math.MaxFloat64) {
				return nil, pgerror.Newf(pgcode.NumericValueOutOfRange,
					"float \"%s\" out of range for type %s", v.String(), colType.Name())
			}
			return v, nil
		default:
			return nil, tree.NewDatatypeMismatchError(sqlbase.DatumToString(v), colType.SQLString())
		}
	case *tree.DString:
		switch colType.Oid() {
		case oid.T_bpchar, oid.T_varchar, oid.T_text:
			// string(n)/char(n)/varchar(n) Calculates the length in bytes
			if colType.Width() > 0 && len(string(*v)) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return v, nil
		case types.T_nchar, types.T_nvarchar:
			// nchar(n)/nvarchar(n) Calculates the length by character
			if colType.Width() > 0 && utf8.RuneCountInString(string(*v)) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return v, nil
		case oid.T_bytea, oid.T_varbytea:
			dVal, err := tree.ParseDByte(string(*v))
			if err != nil {
				return nil, tree.NewDatatypeMismatchError(string(*v), colType.SQLString())
			}
			if colType.Width() > 0 && len(string(*dVal)) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return dVal, nil
		case oid.T_timestamptz:
			dVal, err := tree.ParseTimestampTZForTS(evalCtx, string(*v))
			if err != nil {
				return nil, tree.NewDatatypeMismatchError(string(*v), colType.SQLString())
			}
			// Check the maximum and minimum value of the timestamp
			if *dVal < tree.TsMinTimestamp || *dVal > tree.TsMaxTimestamp {
				return nil, pgerror.Newf(pgcode.StringDataLengthMismatch,
					"value '%s' out of range for type %s", string(*v), colType.SQLString())
			}
			return dVal, nil
		case oid.T_timestamp:
			dVal, err := tree.ParseTimestampForTS(evalCtx, string(*v))
			if err != nil {
				return nil, tree.NewDatatypeMismatchError(string(*v), colType.SQLString())
			}
			// Check the maximum and minimum value of the timestamp
			if *dVal < tree.TsMinTimestamp || *dVal > tree.TsMaxTimestamp {
				return nil, pgerror.Newf(pgcode.StringDataLengthMismatch,
					"value '%s' out of range for type %s", string(*v), colType.SQLString())
			}
			return dVal, nil
		case types.T_geometry:
			_, err := geos.FromWKT(string(*v))
			if err != nil {
				if strings.Contains(err.Error(), "load error") {
					return nil, err
				}
				return nil, pgerror.Newf(pgcode.DataException, "value '%s' is invalid for type %s", string(*v), colType.SQLString())
			}
			// string(n)/char(n)/varchar(n) Calculates the length in bytes
			if len(string(*v)) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value '%s' too long for type %s", string(*v), colType.SQLString())
			}
			return v, nil
		case oid.T_bool:
			dVal, err := tree.ParseDBool(string(*v))
			if err != nil {
				return nil, tree.NewDatatypeMismatchError(string(*v), colType.SQLString())
			}
			return dVal, nil

		default:
			return nil, tree.NewDatatypeMismatchError(sqlbase.DatumToString(v), colType.SQLString())
		}

	case *tree.DBytes:
		switch colType.Oid() {
		case oid.T_bytea, oid.T_varbytea:
			if colType.Width() > 0 && len(string(*v)) > int(colType.Width()) {
				return nil, pgerror.Newf(pgcode.StringDataRightTruncation,
					"value too long for type %s (column %q)",
					colType.SQLString(), column.Name)
			}
			return v, nil
		default:
			return nil, tree.NewDatatypeMismatchError(sqlbase.DatumToString(v), colType.SQLString())
		}
	case *tree.DTimestampTZ:
		if colType.Oid() == oid.T_timestamptz {
			dVal := tree.DInt(v.UnixMilli())
			// Check the maximum and minimum value of the timestamp
			if dVal < tree.TsMinTimestamp || dVal > tree.TsMaxTimestamp {
				return nil, pgerror.Newf(pgcode.StringDataLengthMismatch,
					"value '%s' out of range for type %s", sqlbase.DatumToString(v), colType.SQLString())
			}
			// always send timestamp in DInt format to assemble payload
			return &dVal, nil
		}
		return nil, tree.NewDatatypeMismatchError(sqlbase.DatumToString(v), colType.SQLString())

	case *tree.DTimestamp:
		if colType.Oid() == oid.T_timestamp {
			dVal := tree.DInt(v.UnixMilli())
			// Check the maximum and minimum value of the timestamp
			if dVal < tree.TsMinTimestamp || dVal > tree.TsMaxTimestamp {
				return nil, pgerror.Newf(pgcode.StringDataLengthMismatch,
					"value '%s' out of range for type %s", sqlbase.DatumToString(v), colType.SQLString())
			}
			// always send timestamp in DInt format to assemble payload
			return &dVal, nil
		}
		return nil, tree.NewDatatypeMismatchError(sqlbase.DatumToString(v), colType.SQLString())

	case *tree.UnresolvedName:
		return nil, pgerror.Newf(pgcode.Syntax, "unsupported input type relation \"%s\"", v.String())
	case *tree.BinaryExpr:
		return nil, pgerror.Newf(pgcode.Syntax, "unsupported input type BinaryOperator")

	default:
		return nil, pgerror.Newf(pgcode.Syntax, "unsupported input type %T", v)
	}
	return nil, pgerror.Newf(pgcode.Syntax, "unexpected value")
}

// preCreateInstTable creates the instance table first, and then insert the instance table
// after the instance table is successfully created.
func preCreateInstTable(b *Builder, tsInsert *memo.TSInsertExpr) error {
	var creatTable tree.CreateTable
	creatTable = *tsInsert.CT
	for i, tag := range creatTable.Tags {
		if ph, ok := tag.TagVal.(*tree.Placeholder); ok {
			tagval := b.evalCtx.Placeholders.Values[ph.Idx]
			creatTable.Tags[i].TagVal = tagval
		}
	}
	formatter := tree.NewFmtCtx(tree.FmtSimple)
	creatTable.Format(formatter)
	sql := formatter.CloseAndGetString()
	log.Infof(b.evalCtx.Ctx(), "ddl: %s", sql)
	if offset := strings.Index(sql, "ACTIVETIME"); offset != -1 {
		sql = sql[:offset]
	}
	// Create instance table using InternalExecutor
	_, err := b.evalCtx.InternalExecutor.Query(b.evalCtx.Ctx(), creatTable.StatOp(), nil, sql)
	if err != nil {
		log.Errorf(b.evalCtx.Ctx(), "create table failed: %s", err.Error())
		return err
	}
	return nil
}

// tryBuildFastPathInsert attempts to construct an insert using the fast path,
// checking all required conditions. See exec.Factory.ConstructInsertFastPath.
func (b *Builder) tryBuildFastPathInsert(ins *memo.InsertExpr) (_ execPlan, ok bool, _ error) {
	// If FKFallback is set, the optimizer-driven FK checks are disabled. We must
	// use the legacy path.
	if !b.allowInsertFastPath || ins.FKFallback {
		return execPlan{}, false, nil
	}

	// Conditions from ConstructFastPathInsert:
	//
	//  - there are no other mutations in the statement, and the output of the
	//    insert is not processed through side-effecting expressions (i.e. we can
	//    auto-commit);
	if !b.allowAutoCommit {
		return execPlan{}, false, nil
	}

	//  - the input is Values with at most InsertFastPathMaxRows, and there are no
	//    subqueries;
	values, ok := ins.Input.(*memo.ValuesExpr)
	if !ok || values.ChildCount() > exec.InsertFastPathMaxRows || values.Relational().HasSubquery {
		return execPlan{}, false, nil
	}

	md := b.mem.Metadata()
	tab := md.Table(ins.Table)

	//  - there are no self-referencing foreign keys;
	//  - all FK checks can be performed using direct lookups into unique indexes.
	fkChecks := make([]exec.InsertFastPathFKCheck, len(ins.Checks))
	for i := range ins.Checks {
		c := &ins.Checks[i]
		if md.Table(c.ReferencedTable).ID() == md.Table(ins.Table).ID() {
			// Self-referencing FK.
			return execPlan{}, false, nil
		}
		fk := tab.OutboundForeignKey(c.FKOrdinal)
		lookupJoin, isLookupJoin := c.Check.(*memo.LookupJoinExpr)
		if !isLookupJoin || lookupJoin.JoinType != opt.AntiJoinOp {
			// Not a lookup anti-join.
			return execPlan{}, false, nil
		}
		if len(lookupJoin.On) > 0 ||
			len(lookupJoin.KeyCols) != fk.ColumnCount() {
			return execPlan{}, false, nil
		}
		inputExpr := lookupJoin.Input
		// Ignore any select (used to deal with NULLs).
		if sel, isSelect := inputExpr.(*memo.SelectExpr); isSelect {
			inputExpr = sel.Input
		}
		withScan, isWithScan := inputExpr.(*memo.WithScanExpr)
		if !isWithScan {
			return execPlan{}, false, nil
		}
		if withScan.With != ins.WithID {
			return execPlan{}, false, nil
		}

		out := &fkChecks[i]
		out.InsertCols = make([]exec.ColumnOrdinal, len(lookupJoin.KeyCols))
		findCol := func(cols opt.ColList, col opt.ColumnID) int {
			res, ok := cols.Find(col)
			if !ok {
				panic(errors.AssertionFailedf("cannot find column %d", col))
			}
			return res
		}
		for i, keyCol := range lookupJoin.KeyCols {
			// The keyCol comes from the WithScan operator. We must find the matching
			// column in the mutation input.
			withColOrd := findCol(withScan.OutCols, keyCol)
			inputCol := withScan.InCols[withColOrd]
			out.InsertCols[i] = exec.ColumnOrdinal(findCol(ins.InsertCols, inputCol))
		}

		out.ReferencedTable = md.Table(lookupJoin.Table)
		out.ReferencedIndex = out.ReferencedTable.Index(lookupJoin.Index)
		out.MatchMethod = fk.MatchMethod()
		out.MkErr = func(values tree.Datums) error {
			if len(values) != len(out.InsertCols) {
				return errors.AssertionFailedf("invalid FK violation values")
			}
			// This is a little tricky. The column ordering might not match between
			// the FK reference and the index we're looking up. We have to reshuffle
			// the values to fix that.
			fkVals := make(tree.Datums, len(values))
			for i, ordinal := range out.InsertCols {
				for j := range out.InsertCols {
					if fk.OriginColumnOrdinal(tab, j) == int(ordinal) {
						fkVals[j] = values[i]
						break
					}
				}
			}
			for i := range fkVals {
				if fkVals[i] == nil {
					return errors.AssertionFailedf("invalid column mapping")
				}
			}
			return mkFKCheckErr(md, c, fkVals)
		}
	}

	colList := make(opt.ColList, 0, len(ins.InsertCols)+len(ins.CheckCols))
	colList = appendColsWhenPresent(colList, ins.InsertCols)
	colList = appendColsWhenPresent(colList, ins.CheckCols)
	if !colList.Equals(values.Cols) {
		// We have a Values input, but the columns are not in the right order. For
		// example:
		//   INSERT INTO ab (SELECT y, x FROM (VALUES (1, 10)) AS v (x, y))
		//
		// TODO(radu): we could rearrange the columns of the rows below, or add
		// a normalization rule that adds a Project to rearrange the Values node
		// columns.
		return execPlan{}, false, nil
	}

	rows, err := b.buildValuesRows(values)
	if err != nil {
		return execPlan{}, false, err
	}

	// Construct the InsertFastPath node.
	insertOrds := ordinalSetFromColList(ins.InsertCols)
	checkOrds := ordinalSetFromColList(ins.CheckCols)
	returnOrds := ordinalSetFromColList(ins.ReturnCols)
	node, err := b.factory.ConstructInsertFastPath(
		rows,
		tab,
		insertOrds,
		returnOrds,
		checkOrds,
		fkChecks,
	)
	if err != nil {
		return execPlan{}, false, err
	}
	// Construct the output column map.
	ep := execPlan{root: node}
	if ins.NeedResults() {
		ep.outputCols = mutationOutputColMap(ins)
	}
	return ep, true, nil
}

func (b *Builder) buildUpdate(upd *memo.UpdateExpr) (execPlan, error) {
	// Currently, the execution engine requires one input column for each fetch
	// and update expression, so use ensureColumns to map and reorder columns so
	// that they correspond to target table columns. For example:
	//
	//   UPDATE xyz SET x=1, y=1
	//
	// Here, the input has just one column (because the constant is shared), and
	// so must be mapped to two separate update columns.
	//
	// TODO(andyk): Using ensureColumns here can result in an extra Render.
	// Upgrade execution engine to not require this.
	cnt := len(upd.FetchCols) + len(upd.UpdateCols) + len(upd.PassthroughCols) + len(upd.CheckCols)
	colList := make(opt.ColList, 0, cnt)
	colList = appendColsWhenPresent(colList, upd.FetchCols)
	colList = appendColsWhenPresent(colList, upd.UpdateCols)
	// The RETURNING clause of the Update can refer to the columns
	// in any of the FROM tables. As a result, the Update may need
	// to passthrough those columns so the projection above can use
	// them.
	if upd.NeedResults() {
		colList = appendColsWhenPresent(colList, upd.PassthroughCols)
	}
	colList = appendColsWhenPresent(colList, upd.CheckCols)

	input, err := b.buildMutationInput(upd, upd.Input, colList, &upd.MutationPrivate)
	if err != nil {
		return execPlan{}, err
	}

	// Construct the Update node.
	md := b.mem.Metadata()
	tab := md.Table(upd.Table)
	fetchColOrds := ordinalSetFromColList(upd.FetchCols)
	updateColOrds := ordinalSetFromColList(upd.UpdateCols)
	returnColOrds := ordinalSetFromColList(upd.ReturnCols)
	checkOrds := ordinalSetFromColList(upd.CheckCols)

	// Construct the result columns for the passthrough set.
	var passthroughCols sqlbase.ResultColumns
	if upd.NeedResults() {
		for _, passthroughCol := range upd.PassthroughCols {
			colMeta := b.mem.Metadata().ColumnMeta(passthroughCol)
			passthroughCols = append(passthroughCols, sqlbase.ResultColumn{Name: colMeta.Alias, Typ: colMeta.Type})
		}
	}

	disableExecFKs := !upd.FKFallback
	node, err := b.factory.ConstructUpdate(
		input.root,
		tab,
		fetchColOrds,
		updateColOrds,
		returnColOrds,
		checkOrds,
		passthroughCols,
		b.allowAutoCommit && len(upd.Checks) == 0,
		disableExecFKs,
	)
	if err != nil {
		return execPlan{}, err
	}

	if err := b.buildFKChecks(upd.Checks); err != nil {
		return execPlan{}, err
	}

	// Construct the output column map.
	ep := execPlan{root: node}
	if upd.NeedResults() {
		ep.outputCols = mutationOutputColMap(upd)
	}
	return ep, nil
}

func (b *Builder) buildUpsert(ups *memo.UpsertExpr) (execPlan, error) {
	// Currently, the execution engine requires one input column for each insert,
	// fetch, and update expression, so use ensureColumns to map and reorder
	// columns so that they correspond to target table columns. For example:
	//
	//   INSERT INTO xyz (x, y) VALUES (1, 1)
	//   ON CONFLICT (x) DO UPDATE SET x=2, y=2
	//
	// Here, both insert values and update values come from the same input column
	// (because the constants are shared), and so must be mapped to separate
	// output columns.
	//
	// If CanaryCol = 0, then this is the "blind upsert" case, which uses a KV
	// "Put" to insert new rows or blindly overwrite existing rows. Existing rows
	// do not need to be fetched or separately updated (i.e. ups.FetchCols and
	// ups.UpdateCols are both empty).
	//
	// TODO(andyk): Using ensureColumns here can result in an extra Render.
	// Upgrade execution engine to not require this.
	cnt := len(ups.InsertCols) + len(ups.FetchCols) + len(ups.UpdateCols) + len(ups.CheckCols) + 1
	colList := make(opt.ColList, 0, cnt)
	colList = appendColsWhenPresent(colList, ups.InsertCols)
	colList = appendColsWhenPresent(colList, ups.FetchCols)
	colList = appendColsWhenPresent(colList, ups.UpdateCols)
	if ups.CanaryCol != 0 {
		colList = append(colList, ups.CanaryCol)
	}
	colList = appendColsWhenPresent(colList, ups.CheckCols)

	input, err := b.buildMutationInput(ups, ups.Input, colList, &ups.MutationPrivate)
	if err != nil {
		return execPlan{}, err
	}

	// Construct the Upsert node.
	md := b.mem.Metadata()
	tab := md.Table(ups.Table)
	canaryCol := exec.ColumnOrdinal(-1)
	if ups.CanaryCol != 0 {
		canaryCol = input.getColumnOrdinal(ups.CanaryCol)
	}
	insertColOrds := ordinalSetFromColList(ups.InsertCols)
	fetchColOrds := ordinalSetFromColList(ups.FetchCols)
	updateColOrds := ordinalSetFromColList(ups.UpdateCols)
	returnColOrds := ordinalSetFromColList(ups.ReturnCols)
	checkOrds := ordinalSetFromColList(ups.CheckCols)
	disableExecFKs := !ups.FKFallback
	node, err := b.factory.ConstructUpsert(
		input.root,
		tab,
		canaryCol,
		insertColOrds,
		fetchColOrds,
		updateColOrds,
		returnColOrds,
		checkOrds,
		b.allowAutoCommit && len(ups.Checks) == 0,
		disableExecFKs,
	)
	if err != nil {
		return execPlan{}, err
	}

	if err := b.buildFKChecks(ups.Checks); err != nil {
		return execPlan{}, err
	}

	// If UPSERT returns rows, they contain all non-mutation columns from the
	// table, in the same order they're defined in the table. Each output column
	// value is taken from an insert, fetch, or update column, depending on the
	// result of the UPSERT operation for that row.
	ep := execPlan{root: node}
	if ups.NeedResults() {
		ep.outputCols = mutationOutputColMap(ups)
	}
	return ep, nil
}

func (b *Builder) buildDelete(del *memo.DeleteExpr) (execPlan, error) {
	// Check for the fast-path delete case that can use a range delete.
	if b.canUseDeleteRange(del) {
		return b.buildDeleteRange(del)
	}

	// Ensure that order of input columns matches order of target table columns.
	//
	// TODO(andyk): Using ensureColumns here can result in an extra Render.
	// Upgrade execution engine to not require this.
	colList := make(opt.ColList, 0, len(del.FetchCols))
	colList = appendColsWhenPresent(colList, del.FetchCols)

	input, err := b.buildMutationInput(del, del.Input, colList, &del.MutationPrivate)
	if err != nil {
		return execPlan{}, err
	}

	// Construct the Delete node.
	md := b.mem.Metadata()
	tab := md.Table(del.Table)
	fetchColOrds := ordinalSetFromColList(del.FetchCols)
	returnColOrds := ordinalSetFromColList(del.ReturnCols)
	disableExecFKs := !del.FKFallback
	node, err := b.factory.ConstructDelete(
		input.root,
		tab,
		fetchColOrds,
		returnColOrds,
		b.allowAutoCommit && len(del.Checks) == 0,
		disableExecFKs,
	)
	if err != nil {
		return execPlan{}, err
	}

	if err := b.buildFKChecks(del.Checks); err != nil {
		return execPlan{}, err
	}

	// Construct the output column map.
	ep := execPlan{root: node}
	if del.NeedResults() {
		ep.outputCols = mutationOutputColMap(del)
	}

	return ep, nil
}

// buildTSDelete builds time series delete node.
func (b *Builder) buildTSDelete(tsDelete *memo.TSDeleteExpr) (execPlan, error) {
	// TS table doesn't support explicit txn, so raise error if detected
	if !b.evalCtx.TxnImplicit {
		return execPlan{}, sqlbase.UnsupportedTSExplicitTxnError()
	}
	// prepare metadata used to construct TS delete node.
	md := b.mem.Metadata()
	tab := md.Table(tsDelete.STable)
	dbID := tab.GetParentID()

	// build table's hash hook
	hashRouter, err := api.GetHashRouterWithTable(uint32(dbID), uint32(tab.ID()), false, b.evalCtx.Txn)
	if err != nil {
		return execPlan{}, err
	}

	var spans []execinfrapb.Span
	for _, span := range tsDelete.Spans {
		spans = append(spans, execinfrapb.Span{
			StartTs: span.Start,
			EndTs:   span.End,
		})
	}

	if tsDelete.InputRows == nil && tab.GetTableType() == tree.TimeseriesTable {
		node, err := b.factory.ConstructTSDelete(
			[]roachpb.NodeID{b.evalCtx.NodeID},
			uint64(tab.ID()),
			uint64(0), //group id
			spans,
			uint8(tsDelete.DeleteType),
			[][]byte{}, // primary tag key
			[][]byte{}, // primary tag value
			false)
		if err != nil {
			return execPlan{}, err
		}

		return execPlan{root: node}, nil
	}

	cols := make([]*sqlbase.ColumnDescriptor, tab.ColumnCount())
	var primaryTagCols []*sqlbase.ColumnDescriptor

	// get table's primary tag metadata and it's filter expr
	childColIndexs := make(map[int]int, 1)
	colIndexs := make(map[int]int, len(cols))
	for i := 0; i < len(cols); i++ {
		cols[i] = tab.Column(i).(*sqlbase.ColumnDescriptor)
		if cols[i].IsPrimaryTagCol() {
			primaryTagCols = append(primaryTagCols, cols[i])
			childColIndexs[int(cols[i].ID)] = 0
		}
		if ord, ok := tsDelete.ColsMap[int(cols[i].ID)]; ok {
			// get value expr from colMap
			colIndexs[int(cols[i].ID)] = ord
		}
	}

	var primaryTagVal []byte
	var isOutOfRange bool
	// time series table support delete entity and delete data
	if tab.GetTableType() == tree.TimeseriesTable {
		// build time series table's primary tags values through expr
		primaryTagVal, isOutOfRange, err = BuildInputForTSDelete(
			b.evalCtx,
			tsDelete.InputRows,
			[]int{0},
			primaryTagCols,
			colIndexs,
		)
		if err != nil {
			return execPlan{}, err
		}
		// instance table only support delete data
	} else if tab.GetTableType() == tree.InstanceTable {
		// build instance table's primary tags values through it's table name
		var inputRow tree.Exprs
		name := tab.Name()
		inputRow = append(inputRow, tree.NewDString(name.String()))
		var inputRows opt.RowsValue
		inputRows = append(inputRows, inputRow)
		primaryTagVal, _, err = BuildInputForTSDelete(
			b.evalCtx,
			inputRows,
			[]int{0},
			primaryTagCols,
			childColIndexs,
		)
		if err != nil {
			return execPlan{}, err
		}
	}

	primaryTagKey := sqlbase.MakeTsPrimaryTagKey(sqlbase.ID(tab.ID()), primaryTagVal)
	primaryTagVals := [][]byte{primaryTagVal}

	nodeID, err := hashRouter.GetNodeIDByPrimaryTag(b.evalCtx.Context, primaryTagVal)
	if err != nil {
		return execPlan{}, err
	}

	groupID, err := hashRouter.GetGroupIDByPrimaryTag(b.evalCtx.Context, primaryTagVal)

	node, err := b.factory.ConstructTSDelete(
		nodeID,
		uint64(tab.ID()),
		uint64(groupID),
		spans,
		uint8(tsDelete.DeleteType),
		[][]byte{primaryTagKey},
		primaryTagVals,
		isOutOfRange)
	if err != nil {
		return execPlan{}, err
	}

	return execPlan{root: node}, nil
}

// buildTSUpdate builds time series update node.
func (b *Builder) buildTSUpdate(tsUpdate *memo.TSUpdateExpr) (execPlan, error) {
	// TS table doesn't support explicit txn, so raise error if detected
	if !b.evalCtx.TxnImplicit {
		return execPlan{}, sqlbase.UnsupportedTSExplicitTxnError()
	}

	// if primary tag values in filter don't exist，return update 0
	if tsUpdate.PTagValueNotExist {
		node, err := b.factory.ConstructTSTagUpdate(
			[]roachpb.NodeID{},
			uint64(0),
			uint64(0),
			[][]byte{},
			[][]byte{},
			tsUpdate.PTagValueNotExist,
		)
		if err != nil {
			return execPlan{}, err
		}
		return execPlan{root: node}, nil
	}

	// prepare metadata used to construct TS delete node.
	md := b.mem.Metadata()
	tab := md.Table(tsUpdate.ID)
	dbID := tab.GetParentID()

	// build table's hash hook
	hashRouter, err := api.GetHashRouterWithTable(uint32(dbID), uint32(tab.ID()), false, b.evalCtx.Txn)
	if err != nil {
		return execPlan{}, err
	}

	cols := make([]*sqlbase.ColumnDescriptor, tab.ColumnCount())

	// get table's primary tag metadata and it's filter expr
	var primaryTagCols, otherTagCols []*sqlbase.ColumnDescriptor
	colIndexs := make(map[int]int, len(cols))
	valIndexs := make(map[int]*sqlbase.ColumnDescriptor, len(cols))
	for i := 0; i < len(cols); i++ {
		cols[i] = tab.Column(i).(*sqlbase.ColumnDescriptor)
		if cols[i].IsTagCol() {
			otherTagCols = append(otherTagCols, cols[i])
			if cols[i].IsPrimaryTagCol() {
				primaryTagCols = append(primaryTagCols, cols[i])
			}
		}
		colID := int(cols[i].ID)
		if ord, ok := tsUpdate.ColsMap[colID]; ok {
			// get value expr from colMap
			colIndexs[colID] = ord
			valIndexs[ord] = cols[i]
		}
	}

	var prettyCols []*sqlbase.ColumnDescriptor
	prettyCols = append(prettyCols, primaryTagCols...)
	prettyCols = append(prettyCols, otherTagCols...)

	pTagSize, _, err := ComputeColumnSize(primaryTagCols)
	allTagSize, _, err := ComputeColumnSize(otherTagCols)

	pArgs := PayloadArgs{
		PTagNum: len(primaryTagCols), AllTagNum: len(otherTagCols), DataColNum: 0,
		PrimaryTagSize: pTagSize, AllTagSize: allTagSize, DataColSize: 0,
	}
	inputDatums := make([]tree.Datums, 1)
	inputDatums[0] = make([]tree.Datum, len(tsUpdate.UpdateRows))
	for i, value := range tsUpdate.UpdateRows {
		if _, ok := value.(*tree.Placeholder); ok {
			exp := tree.Expr(value)
			inputDatums[0][i], err = TSTypeCheckForInput(b.evalCtx, &exp, &valIndexs[i].Type, valIndexs[i])
			if err != nil {
				return execPlan{}, err
			}
		} else {
			inputDatums[0][i] = value
		}
	}

	var primaryTagVal []byte
	// build time series table's primary tags values through expr
	payload, primaryTagVal, err := BuildPayloadForTsInsert(
		b.evalCtx,
		b.evalCtx.Txn,
		inputDatums,
		[]int{0},
		prettyCols,
		colIndexs,
		pArgs,
		uint32(dbID),
		uint32(tab.ID()),
		hashRouter,
	)
	if err != nil {
		return execPlan{}, err
	}

	primaryTagKey := sqlbase.MakeTsPrimaryTagKey(sqlbase.ID(tab.ID()), primaryTagVal)
	payloads := [][]byte{payload}

	nodeID, err := hashRouter.GetNodeIDByPrimaryTag(b.evalCtx.Context, primaryTagVal)
	if err != nil {
		return execPlan{}, err
	}

	groupID, err := hashRouter.GetGroupIDByPrimaryTag(b.evalCtx.Context, primaryTagVal)

	node, err := b.factory.ConstructTSTagUpdate(
		nodeID,
		uint64(tab.ID()),
		uint64(groupID),
		[][]byte{primaryTagKey},
		payloads,
		tsUpdate.PTagValueNotExist,
	)
	if err != nil {
		return execPlan{}, err
	}

	return execPlan{root: node}, nil
}

// canUseDeleteRange checks whether a logical Delete operator can be implemented
// by a fast delete range execution operator. This logic should be kept in sync
// with canDeleteFast.
func (b *Builder) canUseDeleteRange(del *memo.DeleteExpr) bool {
	// If rows need to be returned from the Delete operator (i.e. RETURNING
	// clause), no fast path is possible, because row values must be fetched.
	if del.NeedResults() {
		return false
	}

	tab := b.mem.Metadata().Table(del.Table)
	if tab.DeletableIndexCount() > 1 {
		// Any secondary index prevents fast path, because separate delete batches
		// must be formulated to delete rows from them.
		return false
	}
	if tab.IsInterleaved() {
		// There is a separate fast path for interleaved tables in sql/delete.go.
		return false
	}
	if tab.InboundForeignKeyCount() > 0 {
		// If the table is referenced by other tables' foreign keys, no fast path
		// is possible, because the integrity of those references must be checked.
		return false
	}

	// Check for simple Scan input operator without a limit; anything else is not
	// supported by a range delete.
	if scan, ok := del.Input.(*memo.ScanExpr); !ok || scan.HardLimit != 0 {
		return false
	}

	return true
}

// buildDeleteRange constructs a DeleteRange operator that deletes contiguous
// rows in the primary index. canUseDeleteRange should have already been called.
func (b *Builder) buildDeleteRange(del *memo.DeleteExpr) (execPlan, error) {
	// canUseDeleteRange has already validated that input is a Scan operator.
	scan := del.Input.(*memo.ScanExpr)
	tab := b.mem.Metadata().Table(scan.Table)
	needed, _ := b.getColumns(scan.Cols, scan.Table)
	// Calculate the maximum number of keys that the scan could return by
	// multiplying the number of possible result rows by the number of column
	// families of the table. The execbuilder needs this information to determine
	// whether or not allowAutoCommit can be enabled.
	maxKeys := int(b.indexConstraintMaxResults(scan)) * tab.FamilyCount()
	root, err := b.factory.ConstructDeleteRange(
		tab,
		needed,
		scan.Constraint,
		maxKeys,
		b.allowAutoCommit && len(del.Checks) == 0,
	)
	if err != nil {
		return execPlan{}, err
	}
	return execPlan{root: root}, nil
}

// appendColsWhenPresent appends non-zero column IDs from the src list into the
// dst list, and returns the possibly grown list.
func appendColsWhenPresent(dst, src opt.ColList) opt.ColList {
	for _, col := range src {
		if col != 0 {
			dst = append(dst, col)
		}
	}
	return dst
}

// ordinalSetFromColList returns the set of ordinal positions of each non-zero
// column ID in the given list. This is used with mutation operators, which
// maintain lists that correspond to the target table, with zero column IDs
// indicating columns that are not involved in the mutation.
func ordinalSetFromColList(colList opt.ColList) exec.ColumnOrdinalSet {
	var res util.FastIntSet
	if colList == nil {
		return res
	}
	for i, col := range colList {
		if col != 0 {
			res.Add(i)
		}
	}
	return res
}

// mutationOutputColMap constructs a ColMap for the execPlan that maps from the
// opt.ColumnID of each output column to the ordinal position of that column in
// the result.
func mutationOutputColMap(mutation memo.RelExpr) opt.ColMap {
	private := mutation.Private().(*memo.MutationPrivate)
	tab := mutation.Memo().Metadata().Table(private.Table)
	outCols := mutation.Relational().OutputCols

	var colMap opt.ColMap
	ord := 0
	for i, n := 0, tab.DeletableColumnCount(); i < n; i++ {
		colID := private.Table.ColumnID(i)
		if outCols.Contains(colID) {
			colMap.Set(int(colID), ord)
			ord++
		}
	}

	// The output columns of the mutation will also include all
	// columns it allowed to pass through.
	for _, colID := range private.PassthroughCols {
		if colID != 0 {
			colMap.Set(int(colID), ord)
			ord++
		}
	}

	return colMap
}

func (b *Builder) buildFKChecks(checks memo.FKChecksExpr) error {
	md := b.mem.Metadata()
	for i := range checks {
		c := &checks[i]
		// Construct the query that returns FK violations.
		query, err := b.buildRelational(c.Check)
		if err != nil {
			return err
		}
		// Wrap the query in an error node.
		mkErr := func(row tree.Datums) error {
			keyVals := make(tree.Datums, len(c.KeyCols))
			for i, col := range c.KeyCols {
				keyVals[i] = row[query.getColumnOrdinal(col)]
			}
			return mkFKCheckErr(md, c, keyVals)
		}
		node, err := b.factory.ConstructErrorIfRows(query.root, mkErr)
		if err != nil {
			return err
		}
		b.postqueries = append(b.postqueries, node)
	}
	return nil
}

// mkFKCheckErr generates a user-friendly error describing a foreign key
// violation. The keyVals are the values that correspond to the
// cat.ForeignKeyConstraint columns.
func mkFKCheckErr(md *opt.Metadata, c *memo.FKChecksItem, keyVals tree.Datums) error {
	origin := md.TableMeta(c.OriginTable)
	referenced := md.TableMeta(c.ReferencedTable)

	var msg, details bytes.Buffer
	if c.FKOutbound {
		// Generate an error of the form:
		//   ERROR:  insert on table "child" violates foreign key constraint "foo"
		//   DETAIL: Key (child_p)=(2) is not present in table "parent".
		fk := origin.Table.OutboundForeignKey(c.FKOrdinal)
		fmt.Fprintf(&msg, "%s on table ", c.OpName)
		lex.EncodeEscapedSQLIdent(&msg, string(origin.Alias.TableName))
		msg.WriteString(" violates foreign key constraint ")
		lex.EncodeEscapedSQLIdent(&msg, fk.Name())

		details.WriteString("Key (")
		for i := 0; i < fk.ColumnCount(); i++ {
			if i > 0 {
				details.WriteString(", ")
			}
			col := origin.Table.Column(fk.OriginColumnOrdinal(origin.Table, i))
			details.WriteString(string(col.ColName()))
		}
		details.WriteString(")=(")
		sawNull := false
		for i, d := range keyVals {
			if i > 0 {
				details.WriteString(", ")
			}
			if d == tree.DNull {
				// If we see a NULL, this must be a MATCH FULL failure (otherwise the
				// row would have been filtered out).
				sawNull = true
				break
			}
			details.WriteString(d.String())
		}
		if sawNull {
			details.Reset()
			details.WriteString("MATCH FULL does not allow mixing of null and nonnull key values.")
		} else {
			details.WriteString(") is not present in table ")
			lex.EncodeEscapedSQLIdent(&details, string(referenced.Alias.TableName))
			details.WriteByte('.')
		}
	} else {
		// Generate an error of the form:
		//   ERROR:  delete on table "parent" violates foreign key constraint
		//           "child_child_p_fkey" on table "child"
		//   DETAIL: Key (p)=(1) is still referenced from table "child".
		fk := referenced.Table.InboundForeignKey(c.FKOrdinal)
		fmt.Fprintf(&msg, "%s on table ", c.OpName)
		lex.EncodeEscapedSQLIdent(&msg, string(referenced.Alias.TableName))
		msg.WriteString(" violates foreign key constraint ")
		lex.EncodeEscapedSQLIdent(&msg, fk.Name())
		msg.WriteString(" on table ")
		lex.EncodeEscapedSQLIdent(&msg, string(origin.Alias.TableName))

		details.WriteString("Key (")
		for i := 0; i < fk.ColumnCount(); i++ {
			if i > 0 {
				details.WriteString(", ")
			}
			col := referenced.Table.Column(fk.ReferencedColumnOrdinal(referenced.Table, i))
			details.WriteString(string(col.ColName()))
		}
		details.WriteString(")=(")
		for i, d := range keyVals {
			if i > 0 {
				details.WriteString(", ")
			}
			details.WriteString(d.String())
		}
		details.WriteString(") is still referenced from table ")
		lex.EncodeEscapedSQLIdent(&details, string(origin.Alias.TableName))
		details.WriteByte('.')
	}

	return errors.WithDetail(
		pgerror.New(pgcode.ForeignKeyViolation, msg.String()),
		details.String(),
	)
}

// canAutoCommit determines if it is safe to auto commit the mutation contained
// in the expression.
//
// Mutations can commit the transaction as part of the same KV request,
// potentially taking advantage of the 1PC optimization. This is not ok to do in
// general; a sufficient set of conditions is:
//  1. There is a single mutation in the query.
//  2. The mutation is the root operator, or it is directly under a Project
//     with no side-effecting expressions. An example of why we can't allow
//     side-effecting expressions: if the projection encounters a
//     division-by-zero error, the mutation shouldn't have been committed.
//
// An extra condition relates to how the FK checks are run. If they run before
// the mutation (via the insert fast path), auto commit is possible. If they run
// after the mutation (the general path), auto commit is not possible. It is up
// to the builder logic for each mutation to handle this.
//
// Note that there are other necessary conditions related to execution
// (specifically, that the transaction is implicit); it is up to the exec
// factory to take that into account as well.
func (b *Builder) canAutoCommit(rel memo.RelExpr) bool {
	if !rel.Relational().CanMutate {
		// No mutations in the expression.
		return false
	}

	switch rel.Op() {
	case opt.InsertOp, opt.UpsertOp, opt.UpdateOp, opt.DeleteOp:
		// Check that there aren't any more mutations in the input.
		// TODO(radu): this can go away when all mutations are under top-level
		// With ops.
		return !rel.Child(0).(memo.RelExpr).Relational().CanMutate

	case opt.ProjectOp:
		// Allow Project on top, as long as the expressions are not side-effecting.
		//
		// TODO(radu): for now, we only allow passthrough projections because not all
		// builtins that can error out are marked as side-effecting.
		proj := rel.(*memo.ProjectExpr)
		if len(proj.Projections) != 0 {
			return false
		}
		return b.canAutoCommit(proj.Input)

	default:
		return false
	}
}

// forUpdateLocking is the row-level locking mode used by mutations during their
// initial row scan, when such locking is deemed desirable. The locking mode is
// equivalent that used by a SELECT ... FOR UPDATE statement.
var forUpdateLocking = &tree.LockingItem{Strength: tree.ForUpdate}

// shouldApplyImplicitLockingToMutationInput determines whether or not the
// builder should apply a FOR UPDATE row-level locking mode to the initial row
// scan of a mutation expression.
func (b *Builder) shouldApplyImplicitLockingToMutationInput(mutExpr memo.RelExpr) bool {
	switch t := mutExpr.(type) {
	case *memo.InsertExpr:
		// Unlike with the other three mutation expressions, it never makes
		// sense to apply implicit row-level locking to the input of an INSERT
		// expression because any contention results in unique constraint
		// violations.
		return false

	case *memo.UpdateExpr:
		return b.shouldApplyImplicitLockingToUpdateInput(t)

	case *memo.UpsertExpr:
		return b.shouldApplyImplicitLockingToUpsertInput(t)

	case *memo.DeleteExpr:
		return b.shouldApplyImplicitLockingToDeleteInput(t)

	default:
		panic(errors.AssertionFailedf("unexpected mutation expression %T", t))
	}
}

// shouldApplyImplicitLockingToUpdateInput determines whether or not the builder
// should apply a FOR UPDATE row-level locking mode to the initial row scan of
// an UPDATE statement.
//
// Conceptually, if we picture an UPDATE statement as the composition of a
// SELECT statement and an INSERT statement (with loosened semantics around
// existing rows) then this method determines whether the builder should perform
// the following transformation:
//
//	UPDATE t = SELECT FROM t + INSERT INTO t
//	=>
//	UPDATE t = SELECT FROM t FOR UPDATE + INSERT INTO t
//
// The transformation is conditional on the UPDATE expression tree matching a
// pattern. Specifically, the FOR UPDATE locking mode is only used during the
// initial row scan when all row filters have been pushed into the ScanExpr. If
// the statement includes any filters that cannot be pushed into the scan then
// no row-level locking mode is applied. The rationale here is that FOR UPDATE
// locking is not necessary for correctness due to serializable isolation, so it
// is strictly a performance optimization for contended writes. Therefore, it is
// not worth risking the transformation being a pessimization, so it is only
// applied when doing so does not risk creating artificial contention.
func (b *Builder) shouldApplyImplicitLockingToUpdateInput(upd *memo.UpdateExpr) bool {
	if !b.evalCtx.SessionData.ImplicitSelectForUpdate {
		return false
	}

	// Try to match the Update's input expression against the pattern:
	//
	//   [Project] [IndexJoin] Scan
	//
	input := upd.Input
	if proj, ok := input.(*memo.ProjectExpr); ok {
		input = proj.Input
	}
	if idxJoin, ok := input.(*memo.IndexJoinExpr); ok {
		input = idxJoin.Input
	}
	_, ok := input.(*memo.ScanExpr)
	return ok
}

// tryApplyImplicitLockingToUpsertInput determines whether or not the builder
// should apply a FOR UPDATE row-level locking mode to the initial row scan of
// an UPSERT statement.
//
// TODO(nvanbenschoten): implement this method to match on appropriate Upsert
// expression trees and apply a row-level locking mode.
func (b *Builder) shouldApplyImplicitLockingToUpsertInput(ups *memo.UpsertExpr) bool {
	return false
}

// tryApplyImplicitLockingToDeleteInput determines whether or not the builder
// should apply a FOR UPDATE row-level locking mode to the initial row scan of
// an DELETE statement.
//
// TODO(nvanbenschoten): implement this method to match on appropriate Delete
// expression trees and apply a row-level locking mode.
func (b *Builder) shouldApplyImplicitLockingToDeleteInput(del *memo.DeleteExpr) bool {
	return false
}
