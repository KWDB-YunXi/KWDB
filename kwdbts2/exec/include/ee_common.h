// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

#pragma once

#include "me_metadata.pb.h"
#include "ee_field.h"
#include "ee_field_agg.h"
#include "ee_base_op.h"

namespace kwdbts {

static const k_uint16 STRING_WIDE = sizeof(k_uint16);
static const k_uint16 BOOL_WIDE = sizeof(k_bool);

bool IsStringType(roachpb::DataType storage_type);

bool IsFirstLastAggFunc(kwdbts::TSAggregatorSpec_Func func);

// create agg field by input field and agg type
KStatus CreateAggField(k_int32 i, Field* input_field, BaseOperator *agg_op, FieldAggNum **func_field);

// convert field to chunk
void FieldsToChunk(Field **fields, k_uint32 field_num, k_uint32 row, DataChunkPtr& chunk);

}       // namespace kwdbts
