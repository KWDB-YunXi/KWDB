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

#include "ee_tag_scan_op.h"

#include <cmath>
#include <memory>
#include <string>
#include <vector>

#include "cm_func.h"
#include "ee_row_batch.h"
#include "ee_flow_param.h"
#include "ee_global.h"
#include "ee_handler.h"
#include "ee_pb_plan.pb.h"
#include "ee_table.h"
#include "lg_api.h"
#include "ee_kwthd.h"

namespace kwdbts {

TagScanOperator::TagScanOperator(TSTagReaderSpec* spec, TSPostProcessSpec* post,
                                 TABLE* table, int32_t processor_id)
    : BaseOperator(table, processor_id),
      spec_(spec),
      post_(post),
      schema_id_(0),
      param_(post, table) {
  if (spec) {
    table->SetAccessMode(spec->accessmode());
    object_id_ = spec->tableid();
    table->SetTableVersion(spec->tableversion());
  }
}

TagScanOperator::~TagScanOperator() = default;

EEIteratorErrCode TagScanOperator::PreInit(kwdbContext_p ctx) {
  EnterFunc();
  std::unique_lock l(tag_lock_);
  if (is_pre_init_) {
    Return(pre_init_code_);
  }
  EEIteratorErrCode ret = EEIteratorErrCode::EE_ERROR;
  do {
    // resolve tag
    param_.ResolveScanTags(ctx);
    // post->filter;
    ret = param_.ResolveFilter(ctx, &filter_, true);
    if (EEIteratorErrCode::EE_OK != ret) {
      LOG_ERROR("ReaderPostResolve::ResolveFilter() failed");
      break;
    }
    if (object_id_ > 0) {
      // renders num
      param_.RenderSize(ctx, &num_);
      ret = param_.ResolveRender(ctx, &renders_, num_);
      if (ret != EEIteratorErrCode::EE_OK) {
        LOG_ERROR("ResolveRender() failed");
        break;
      }
      // resolve render output type
      if (table_->GetAccessMode() == TSTableReadMode::onlyTag) {
        param_.ResolveOutputType(ctx, renders_, num_);
      }
      // Output Fields
      ret = param_.ResolveOutputFields(ctx, renders_, num_, output_fields_);
      if (EEIteratorErrCode::EE_OK != ret) {
        LOG_ERROR("ResolveOutputFields() failed");
        break;
      }
    }
  } while (0);
  is_pre_init_ = true;
  pre_init_code_ = ret;
  Return(ret);
}

EEIteratorErrCode TagScanOperator::Init(kwdbContext_p ctx) {
  EnterFunc();
  std::unique_lock l(tag_lock_);
  // Involving parallelism, ensuring that it is only called once
  if (is_init_) {
    Return(init_code_);
  }
  is_init_ = true;
  init_code_ = EEIteratorErrCode::EE_ERROR;
  handler_ = new Handler(table_);
  init_code_ = handler_->PreInit(ctx);
  if (init_code_ == EEIteratorErrCode::EE_ERROR) {
    Return(init_code_);
  }

  k_uint32 access_mode = table_->GetAccessMode();
  switch (access_mode) {
    case TSTableReadMode::tagIndex:
    case TSTableReadMode::tagIndexTable: {
      break;
    }
    case TSTableReadMode::tableTableMeta:
    case TSTableReadMode::metaTable:
    case TSTableReadMode::onlyTag: {
      handler_->SetReadMode((TSTableReadMode) access_mode);
      init_code_ = handler_->NewTagIterator(ctx);
      if (init_code_ != EE_OK) {
        Return(init_code_);
      }
      break;
    }
    default: {
      LOG_ERROR("access mode unknow, %d", access_mode);
      break;
    }
  }
  Return(init_code_);
}

EEIteratorErrCode TagScanOperator::Next(kwdbContext_p ctx) {
EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_END_OF_RECORD;
  k_uint32 access_mode = table_->GetAccessMode();
  do {
    tagdata_handle_ = std::make_shared<TagRowBatch>();
    tagdata_handle_->Init(table_);
    handler_->SetTagRowBatch(tagdata_handle_);
    if (access_mode < TSTableReadMode::tableTableMeta) {
      if (!tag_index_once_) {
        break;
      }
      tag_index_once_ = false;
      code = handler_->GetEntityIdList(ctx, spec_, filter_);
      if (code != EE_OK && code != EE_END_OF_RECORD) {
        break;
      }
    } else {
      code = handler_->TagNext(ctx, filter_);
      if (code != EE_OK && code != EE_END_OF_RECORD) {
        break;
      }
    }
    total_read_row_ += tagdata_handle_->count_;
  } while (0);

  Return(code);
}

EEIteratorErrCode TagScanOperator::Next(kwdbContext_p ctx, DataChunkPtr& chunk) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_END_OF_RECORD;

  k_uint32 access_mode = table_->GetAccessMode();
  auto start = std::chrono::high_resolution_clock::now();
  do {
    tagdata_handle_ = std::make_shared<TagRowBatch>();
    tagdata_handle_->Init(table_);
    handler_->SetTagRowBatch(tagdata_handle_);
    if (access_mode < TSTableReadMode::tableTableMeta) {
      if (!tag_index_once_) {
        break;
      }
      tag_index_once_ = false;
      code = handler_->GetEntityIdList(ctx, spec_, filter_);
      if (code != EE_OK && code != EE_END_OF_RECORD) {
        break;
      }
    } else {
      code = handler_->TagNext(ctx, filter_);
      if (code != EE_OK && code != EE_END_OF_RECORD) {
        break;
      }
    }
    total_read_row_ += tagdata_handle_->count_;
    current_thd->SetRowBatch(tagdata_handle_);

    // reset
    tagdata_handle_->ResetLine();
    if (tagdata_handle_->Count() > 0) {
      // init DataChunk
      if (nullptr == chunk) {
        // init column
        std::vector<ColumnInfo> col_info;
        for (int i = 0; i < GetRenderSize(); i++) {
          Field* field = GetRender(i);
          col_info.emplace_back(field->get_storage_length(), field->get_storage_type(), field->get_return_type());
        }

        chunk = std::make_unique<DataChunk>(col_info, tagdata_handle_->Count());
        if (chunk->Initialize() < 0) {
          chunk = nullptr;
          EEPgErrorInfo::SetPgErrorInfo(ERRCODE_OUT_OF_MEMORY, "Insufficient memory");
          Return(EEIteratorErrCode::EE_ERROR);
        }
      }

      KStatus status = chunk->AddRowBatchData(ctx, tagdata_handle_.get(), renders_);
      if (status != KStatus::SUCCESS) {
        Return(EEIteratorErrCode::EE_ERROR);
      }
    }
  } while (0);
  auto *fetchers = static_cast<VecTsFetcher *>(ctx->fetcher);
  if (fetchers != nullptr && fetchers->collected) {
    auto end = std::chrono::high_resolution_clock::now();
    std::chrono::duration<int64_t, std::nano> duration = end - start;
    if (nullptr != chunk) {
      int64_t bytes_read = int64_t(chunk->Capacity()) * int64_t(chunk->RowSize());
      chunk->GetFvec().AddAnalyse(ctx, this->processor_id_,
                        duration.count(), int64_t(tagdata_handle_->count_), bytes_read, 0, 0);
    }
  }

  Return(code);
}

RowBatchPtr TagScanOperator::GetRowBatch(kwdbContext_p ctx) {
  EnterFunc();

  Return(tagdata_handle_);
}

EEIteratorErrCode TagScanOperator::Reset(kwdbContext_p ctx) {
  EnterFunc();

  if (handler_) {
    SafeDeletePointer(handler_);
  }
  examined_rows_ = 0;
  total_read_row_ = 0;
  data_ = nullptr;
  count_ = 0;
  tag_index_once_ = false;
  is_init_ = false;
  tag_index_once_ = true;

  Return(EEIteratorErrCode::EE_OK)
}

KStatus TagScanOperator::Close(kwdbContext_p ctx) {
  EnterFunc();
  Reset(ctx);

  Return(KStatus::SUCCESS);
}

KStatus TagScanOperator::GetEntities(kwdbContext_p ctx,
                                     std::vector<EntityResultIndex> *entities,
                                     k_uint32 *start_tag_index,
                                     TagRowBatchPtr *row_batch_ptr) {
  EnterFunc();
  std::unique_lock l(tag_lock_);
  if (*row_batch_ptr == nullptr) {
    *row_batch_ptr = tagdata_handle_;
  }
  if (is_first_entity_ || (*row_batch_ptr != nullptr &&
                           (row_batch_ptr->get()->isAllDistributed()))) {
    if (is_first_entity_ || *row_batch_ptr == tagdata_handle_ || tagdata_handle_->isAllDistributed()) {
      is_first_entity_ = false;
      EEIteratorErrCode code = Next(ctx);
      if (code != EE_OK) {
        Return(FAIL);
      }
    } else if (tagdata_handle_.get()->Count() == 0) {
      Return(FAIL);
    }

    // construct ts_iterator
    *row_batch_ptr = tagdata_handle_;
  }
  KStatus ret = row_batch_ptr->get()->GetEntities(entities, start_tag_index);
  Return(ret);
}

}  // namespace kwdbts