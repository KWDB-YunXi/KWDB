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

#include "ee_common.h"
#include "lg_api.h"

namespace kwdbts {

bool IsStringType(roachpb::DataType storage_type) {
    return storage_type == roachpb::DataType::CHAR || storage_type == roachpb::DataType::VARCHAR ||
           storage_type == roachpb::DataType::NCHAR || storage_type == roachpb::DataType::NVARCHAR ||
           storage_type == roachpb::DataType::BINARY || storage_type == roachpb::DataType::VARBINARY;
}

bool IsFirstLastAggFunc(kwdbts::TSAggregatorSpec_Func func) {
  return func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_LAST ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_LAST_ROW ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_LASTTS ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_LASTROWTS ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_FIRST ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_FIRST_ROW ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_FIRSTTS ||
         func == TSAggregatorSpec_Func::TSAggregatorSpec_Func_FIRSTROWTS;
}

KStatus CreateAggField(k_int32 i, Field* input_field, BaseOperator *agg_op, FieldAggNum **func_field) {
    switch (input_field->get_storage_type()) {
        case roachpb::DataType::BOOL:
            *func_field = new FieldAggBool(i, roachpb::DataType::BOOL, sizeof(k_bool), agg_op);
            break;
        case roachpb::DataType::SMALLINT:
            *func_field = new FieldAggShort(i, roachpb::DataType::SMALLINT, sizeof(k_int16), agg_op);
            break;
        case roachpb::DataType::INT:
            *func_field = new FieldAggInt(i, roachpb::DataType::INT, sizeof(k_int32), agg_op);
            break;
        case roachpb::DataType::TIMESTAMP:
        case roachpb::DataType::TIMESTAMPTZ:
        case roachpb::DataType::DATE:
        case roachpb::DataType::BIGINT:
            *func_field = new FieldAggLonglong(i, roachpb::DataType::BIGINT, sizeof(k_int64), agg_op);
            break;
        case roachpb::DataType::FLOAT:
            *func_field = new FieldAggFloat(i, roachpb::DataType::FLOAT, sizeof(k_float32), agg_op);
            break;
        case roachpb::DataType::DOUBLE:
            *func_field = new FieldAggDouble(i, roachpb::DataType::DOUBLE, sizeof(k_double64), agg_op);
            break;
        case roachpb::DataType::CHAR:
        case roachpb::DataType::VARCHAR:
        case roachpb::DataType::NCHAR:
        case roachpb::DataType::NVARCHAR:
        case roachpb::DataType::BINARY:
        case roachpb::DataType::VARBINARY:
            *func_field = new FieldAggString(i,
                input_field->get_storage_type(), input_field->get_storage_length(), agg_op);
            break;
        case roachpb::DataType::DECIMAL:
            *func_field = new FieldAggDecimal(i,
                roachpb::DataType::DECIMAL, sizeof(k_double64), agg_op);
            break;
        default:
            LOG_ERROR("unsupported data type for max/min aggregation\n");
            return KStatus::FAIL;
            break;
      }

    return KStatus::SUCCESS;
}

void FieldsToChunk(Field **fields, k_uint32 field_num, k_uint32 row, DataChunkPtr& chunk) {
  for (int col = 0; col < field_num; col++) {
    Field* field = fields[col];

    if (field->is_nullable()) {
      chunk->SetNull(row, col);
      continue;
    }

    k_uint32 len = field->get_storage_length();
    switch (field->get_storage_type()) {
      case roachpb::DataType::BOOL: {
        bool val = field->ValInt() > 0 ? 1 : 0;
        chunk->InsertData(row, col, reinterpret_cast<char*>(&val), len);
        break;
      }
      case roachpb::DataType::TIMESTAMP:
      case roachpb::DataType::TIMESTAMPTZ:
      case roachpb::DataType::DATE:
      case roachpb::DataType::BIGINT: {
        k_int64 val = field->ValInt();
        chunk->InsertData(row, col, reinterpret_cast<char*>(&val), len);
        break;
      }
      case roachpb::DataType::INT: {
        k_int32 val = field->ValInt();
        chunk->InsertData(row, col, reinterpret_cast<char*>(&val), len);
        break;
      }
      case roachpb::DataType::SMALLINT: {
        k_int16 val = field->ValInt();
        chunk->InsertData(row, col, reinterpret_cast<char*>(&val), len);
        break;
      }
      case roachpb::DataType::FLOAT: {
        k_float32 val = field->ValReal();
        chunk->InsertData(row, col, reinterpret_cast<char*>(&val), len);
        break;
      }
      case roachpb::DataType::DOUBLE: {
        k_double64 val = field->ValReal();
        chunk->InsertData(row, col, reinterpret_cast<char*>(&val), len);
        break;
      }
      case roachpb::DataType::CHAR:
      case roachpb::DataType::NCHAR:
      case roachpb::DataType::NVARCHAR:
      case roachpb::DataType::VARCHAR:
      case roachpb::DataType::BINARY:
      case roachpb::DataType::VARBINARY: {
        kwdbts::String s = field->ValStr();
        char* mem = const_cast<char*>(s.c_str());
        chunk->InsertData(row, col, mem, s.length());
        break;
      }
      case roachpb::DataType::DECIMAL: {
        if (field->get_field_type() == Field::Type::FIELD_AGG ||
            field->get_field_type() == Field::Type::FIELD_ITEM) {
          DatumPtr src = field->get_ptr();
          chunk->InsertData(row, col, src, len + BOOL_WIDE);
        } else {
          k_bool overflow = field->is_over_flow();
          if (field->get_sql_type() == roachpb::DataType::DOUBLE ||
              field->get_sql_type() == roachpb::DataType::FLOAT || overflow) {
            k_double64 val = field->ValReal();
            chunk->InsertDecimal(row, col, reinterpret_cast<char*>(&val), true);
          } else {
            k_int64 val = field->ValInt();
            chunk->InsertDecimal(row, col, reinterpret_cast<char*>(&val), false);
          }
        }
        break;
      }
      default:
        break;
    }
  }
}

}  // namespace kwdbts