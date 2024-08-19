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

#include "ee_distinct_op.h"
#include "cm_func.h"
#include "lg_api.h"
#include "ee_common.h"
#include "ee_kwthd.h"

namespace kwdbts {

DistinctOperator::DistinctOperator(BaseOperator* input, DistinctSpec* spec,
                                   TSPostProcessSpec* post, TABLE* table, int32_t processor_id)
    : BaseOperator(table, processor_id),
      spec_{spec},
      post_{post},
      param_(input, spec, post, table),
      input_(input),
      offset_(post->offset()),
      limit_(post->limit()),
      input_fields_{input->OutputFields()} {}

DistinctOperator::DistinctOperator(const DistinctOperator& other, BaseOperator* input, int32_t processor_id)
    : BaseOperator(other.table_, processor_id),
      spec_(other.spec_),
      post_(other.post_),
      param_(input, other.spec_, other.post_, other.table_),
      input_(input),
      offset_(other.offset_),
      limit_(other.limit_),
      input_fields_{input->OutputFields()} {
  is_clone_ = true;
}

DistinctOperator::~DistinctOperator() {
  //  delete input
  if (is_clone_) {
    delete input_;
  }
}

EEIteratorErrCode DistinctOperator::PreInit(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;
  do {
    // init subquery iterator
    code = input_->PreInit(ctx);
    if (EEIteratorErrCode::EE_OK != code) {
      break;
    }
    // resolve renders num
    param_.RenderSize(ctx, &num_);
    // resolve render
    code = param_.ResolveRender(ctx, &renders_, num_);
    if (EEIteratorErrCode::EE_OK != code) {
      LOG_ERROR("ResolveRender() error\n");
      break;
    }

    // dispose Output Fields
    code = param_.ResolveOutputFields(ctx, renders_, num_, output_fields_);
    if (EEIteratorErrCode::EE_OK != code) {
      LOG_ERROR("ResolveOutputFields() failed\n");
      break;
    }

    // dispose Distinct col
    KStatus ret = ResolveDistinctCols(ctx);
    if (ret != KStatus::SUCCESS) {
      code = EEIteratorErrCode::EE_ERROR;
      break;
    }
  } while (0);
  Return(code);
}

EEIteratorErrCode DistinctOperator::Init(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;

  code = input_->Init(ctx);
  if (EEIteratorErrCode::EE_OK != code) {
    Return(code);
  }

  Return(code);
}

EEIteratorErrCode DistinctOperator::Next(kwdbContext_p ctx, DataChunkPtr& chunk) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;

  int64_t read_row_num = 0;
  auto start = std::chrono::high_resolution_clock::now();
  do {
    // read a batch of data
    DataChunkPtr data_chunk = nullptr;
    code = input_->Next(ctx, data_chunk);
    if (code != EEIteratorErrCode::EE_OK) {
      break;
    }

    // data is null
    if (data_chunk == nullptr || data_chunk->Count() == 0) {
      continue;
    }

    read_row_num += data_chunk->Count();
    // result set
    if (nullptr == chunk) {
      std::vector<ColumnInfo> col_info;
      col_info.reserve(output_fields_.size());
      for (auto field : output_fields_) {
        col_info.emplace_back(field->get_storage_length(), field->get_storage_type(), field->get_return_type());
      }

      chunk = std::make_unique<DataChunk>(col_info, data_chunk->Count());
      if (chunk->Initialize() < 0) {
        chunk = nullptr;
        EEPgErrorInfo::SetPgErrorInfo(ERRCODE_OUT_OF_MEMORY, "Insufficient memory");
        Return(EEIteratorErrCode::EE_ERROR);
      }
    }

    current_thd->SetDataChunk(data_chunk.get());

    // Distinct
    k_uint32 i = 0;
    data_chunk->ResetLine();
    while (i < data_chunk->Count()) {
      k_int32 row = data_chunk->NextLine();
      if (row < 0) {
        break;
      }

      // distinct col
      CombinedGroupKey distinct_keys;
      encodeDistinctCols(data_chunk, row, distinct_keys);

      // not find
      if (seen.find(distinct_keys) == seen.end()) {
        // limit
        if (limit_ && examined_rows_ >= limit_) {
          code = EEIteratorErrCode::EE_END_OF_RECORD;
          break;
        }

        // offset
        if (cur_offset_ > 0) {
          --cur_offset_;
          continue;
        }

        // insert data
        FieldsToChunk(GetRender(), GetRenderSize(), i, chunk);
        chunk->AddCount();


        // rows++
        ++examined_rows_;
        ++i;

        // update distinct info
        seen.insert(distinct_keys);
      }
    }
  } while (0);
  auto *fetchers = static_cast<VecTsFetcher *>(ctx->fetcher);
  if (fetchers != nullptr && fetchers->collected) {
    auto end = std::chrono::high_resolution_clock::now();
    std::chrono::duration<int64_t, std::nano> duration = end - start;
    if (nullptr != chunk) {
      chunk->GetFvec().AddAnalyse(ctx, this->processor_id_,
                        duration.count(), read_row_num, 0, 1, 0);
    }
  }

  Return(code);
}

KStatus DistinctOperator::Close(kwdbContext_p ctx) {
  EnterFunc();
  KStatus ret = input_->Close(ctx);
  Reset(ctx);

  Return(ret);
}

EEIteratorErrCode DistinctOperator::Reset(kwdbContext_p ctx) {
  EnterFunc();
  input_->Reset(ctx);

  Return(EEIteratorErrCode::EE_OK);
}

BaseOperator* DistinctOperator::Clone() {
  BaseOperator* input = input_->Clone();
  if (input == nullptr) {
    input = input_;
  }
  BaseOperator* iter = NewIterator<DistinctOperator>(*this, input, this->processor_id_);
  return iter;
}

KStatus DistinctOperator::ResolveDistinctCols(kwdbContext_p ctx) {
  EnterFunc();

  k_int32 count = spec_->distinct_columns_size();
  for (k_int32 i = 0; i < count; ++i) {
    distinct_cols_.push_back(spec_->distinct_columns(i));
  }

  Return(KStatus::SUCCESS);
}

void DistinctOperator::encodeDistinctCols(DataChunkPtr& chunk, k_uint32 line, CombinedGroupKey& distinct_keys) {
  distinct_keys.Reserve(distinct_cols_.size());
  for (const auto& col : distinct_cols_) {
    DatumPtr ptr = chunk->GetData(line, col);
    bool is_null = chunk->IsNull(line, col);

    roachpb::DataType col_type = input_fields_[col]->get_storage_type();
    if (is_null) {
      distinct_keys.AddGroupKey(std::monostate(), col_type);
      continue;
    }
    switch (col_type) {
      case roachpb::DataType::BOOL: {
        k_bool val = *reinterpret_cast<k_bool*>(ptr);
        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::SMALLINT: {
        k_int16 val = *reinterpret_cast<k_int16*>(ptr);
        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::INT: {
        k_int32 val = *reinterpret_cast<k_int32*>(ptr);
        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::TIMESTAMP:
      case roachpb::DataType::TIMESTAMPTZ:
      case roachpb::DataType::DATE:
      case roachpb::DataType::BIGINT: {
        k_int64 val = *reinterpret_cast<k_int64*>(ptr);
        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::FLOAT: {
        k_float32 val = *reinterpret_cast<k_float32*>(ptr);
        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::DOUBLE: {
        k_double64 val = *reinterpret_cast<k_double64*>(ptr);
        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::CHAR:
      case roachpb::DataType::VARCHAR:
      case roachpb::DataType::NCHAR:
      case roachpb::DataType::NVARCHAR:
      case roachpb::DataType::BINARY:
      case roachpb::DataType::VARBINARY: {
        k_uint16 len;
        std::memcpy(&len, ptr, sizeof(k_uint16));
        std::string val = std::string{ptr + sizeof(k_uint16), len};

        distinct_keys.AddGroupKey(val, col_type);
        break;
      }
      case roachpb::DataType::DECIMAL: {
        k_bool is_double = *reinterpret_cast<k_bool*>(ptr);
        if (is_double) {
          k_double64 val = *reinterpret_cast<k_double64*>(ptr + sizeof(k_bool));
          distinct_keys.AddGroupKey(val, roachpb::DataType::DOUBLE);
        } else {
          k_int64 val = *reinterpret_cast<k_int64*>(ptr + sizeof(k_bool));
          distinct_keys.AddGroupKey(val, roachpb::DataType::BIGINT);
        }
        break;
      }
      default:
        break;
    }
  }
}

}  // namespace kwdbts