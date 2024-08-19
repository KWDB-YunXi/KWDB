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
// Created by liguoliang on 2022/07/18.

#include "ee_table.h"

#include "ee_field.h"
#include "ee_handler.h"
#include "ee_kwthd.h"
#include "er_api.h"
#include "lg_api.h"

namespace kwdbts {

TABLE::~TABLE() {
  if (fields_) {
    for (k_uint32 i = 0; i < field_num_; ++i) {
      SafeDeletePointer(fields_[i]);
    }

    free(fields_);
  }

  fields_ = nullptr;
  field_num_ = 0;
}


Field *TABLE::GetFieldWithColNum(k_uint32 num) {
  Field *table_field = nullptr;
  if (num < field_num_) {
    table_field = fields_[num];
  }

  return table_field;
}

KStatus TABLE::Init(kwdbContext_p ctx, const TSTagReaderSpec *spec) {
  EnterFunc();
  if (KTRUE == init_) {
    Return(KStatus::SUCCESS);
  }

  KStatus ret = KStatus::SUCCESS;
  do {
    // field num
    field_num_ = spec->colmetas_size();
    fields_ = static_cast<Field **>(malloc(field_num_ * sizeof(Field *)));
    if (nullptr == fields_) {
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_OUT_OF_MEMORY, "Insufficient memory");
      LOG_ERROR("malloc error\n");
      break;
    }
    memset(fields_, 0, field_num_ * sizeof(Field *));
    // resolve col
    for (k_int32 i = 0; i < field_num_; ++i) {
      Field *field = nullptr;
      const TSCol &col = spec->colmetas(i);
      ret = InitField(ctx, col, i, &field);
      if (ret != SUCCESS) break;
      fields_[i] = field;
    }

    if (KStatus::FAIL == ret) {
      break;
    }
  } while (0);

  Return(ret);
}

KStatus TABLE::InitField(kwdbContext_p ctx, const TSCol &col, k_uint32 index,
                         Field **field) {
  EnterFunc();
  KStatus ret = SUCCESS;
  roachpb::DataType sql_type = col.storage_type();
  switch (sql_type) {
    case roachpb::DataType::TIMESTAMPTZ: {
      *field = new FieldTimestampTZ();
      break;
    }
    case roachpb::DataType::TIMESTAMP: {
      *field = new FieldTimestamp();
      break;
    }
    case roachpb::DataType::SMALLINT: {
      *field = new FieldShort();
      break;
    }
    case roachpb::DataType::INT: {
      *field = new FieldInt();
      break;
    }
    case roachpb::DataType::BIGINT: {
      *field = new FieldLonglong();
      break;
    }
    case roachpb::DataType::FLOAT: {
      *field = new FieldFloat();
      break;
    }
    case roachpb::DataType::DOUBLE: {
      *field = new FieldDouble();
      break;
    }
    case roachpb::DataType::BOOL: {
      *field = new FieldBool();
      break;
    }
    case roachpb::DataType::CHAR: {
      *field = new FieldChar();
      break;
    }
    case roachpb::DataType::BINARY: {
      *field = new FieldBlob();
      break;
    }
    case roachpb::DataType::NCHAR: {
      *field = new FieldNchar();
      break;
    }
    case roachpb::DataType::VARCHAR: {
      *field = new FieldVarchar();
      break;
    }
    case roachpb::DataType::NVARCHAR: {
      *field = new FieldNvarchar();
      break;
    }
    case roachpb::DataType::VARBINARY: {
      *field = new FieldVarBlob();
      break;
    }
    default: {
      LOG_ERROR("unknow column type %d\n", sql_type);
      break;
    }
  }

  if (nullptr == *field) {
    EEPgErrorInfo::SetPgErrorInfo(ERRCODE_OUT_OF_MEMORY, "Insufficient memory");
    LOG_ERROR("malloc error\n");
    ret = KStatus::FAIL;
    Return(ret);
  }

  (*field)->table_ = this;
  (*field)->set_num(index);
  (*field)->set_sql_type(sql_type);
  (*field)->set_storage_type(sql_type);
  (*field)->setNullable(col.nullable());
  if (col.has_storage_len()) {
    if (0 == index) {
      (*field)->set_storage_length(col.storage_len() - sizeof(k_int64));
    } else {
      (*field)->set_storage_length(col.storage_len());
    }
  }
  if (col.has_col_offset()) (*field)->set_column_offset(col.col_offset());
  if (col.has_variable_length_type())
    (*field)->set_variable_length_type(col.variable_length_type());
  if (col.has_column_type()) {
    (*field)->set_column_type(col.column_type());
    if (col.column_type() != roachpb::KWDBKTSColumn::TYPE_DATA) {
      if (min_tag_id_ == 0) {
        min_tag_id_ = index;
      }
      tag_num_++;
    }
  }
  Return(ret);
}

void *TABLE::GetData(int col, k_uint64 offset, roachpb::KWDBKTSColumn::ColumnType ctype, roachpb::DataType dt) {
  auto data_handle = current_thd->GetRowBatchOriginalPtr();
  return data_handle->GetData(col, offset, ctype, dt);
}

k_uint16 TABLE::GetDataLen(int col, k_uint64 offset, roachpb::KWDBKTSColumn::ColumnType ctype) {
  auto data_handle = current_thd->GetRowBatchOriginalPtr();
  return data_handle->GetDataLen(col, offset, ctype);
}

bool TABLE::is_nullable(int col, roachpb::KWDBKTSColumn::ColumnType ctype) {
//  if (roachpb::KWDBKTSColumn::TYPE_PTAG == ctype) {
//    return false;
//  }
  auto data_handle = current_thd->GetRowBatchOriginalPtr();

  return data_handle->IsNull(col, ctype);
}

void *TABLE::GetData2(int col, k_uint64 offset, roachpb::KWDBKTSColumn::ColumnType ctype, roachpb::DataType dt) {
  RowContainer* data_handle = current_thd->GetDataChunk();
  return data_handle->GetData(col);
}

bool TABLE::is_nullable2(int col, roachpb::KWDBKTSColumn::ColumnType ctype) {
  RowContainer* data_handle = current_thd->GetDataChunk();
  return data_handle->IsNull(col);
}

k_bool TABLE::IsOverflow(k_uint32 col, roachpb::KWDBKTSColumn::ColumnType ctype) {
  auto data_handle = current_thd->GetRowBatchOriginalPtr();
  return data_handle->IsOverflow(col, ctype);
}

}  // namespace kwdbts