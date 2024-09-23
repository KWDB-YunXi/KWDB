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

#include "ee_storage_handler.h"

#include "cm_func.h"
#include "ee_field.h"
#include "ee_global.h"
#include "ee_kwthd_context.h"
#include "ee_scan_row_batch.h"
#include "ee_table.h"
#include "ee_tag_scan_op.h"
#include "engine.h"
#include "iterator.h"
#include "lg_api.h"
#include "tag_iterator.h"
#include "ts_table.h"

namespace kwdbts {

StorageHandler::~StorageHandler() {
  LOG_DEBUG("StorageHandler end. total read rows: %lu, entity num: %lu.", total_read_rows_, entities_.size());
  table_ = nullptr;
  Close();
}

EEIteratorErrCode StorageHandler::Init(kwdbContext_p ctx) {
  EnterFunc();
  KStatus ret = KStatus::FAIL;
  TSEngine *ts_engine = static_cast<TSEngine *>(ctx->ts_engine);
  if (ts_engine)
    ret = ts_engine->GetTsTable(ctx, table_->object_id_, ts_table_);
  Return(ret == KStatus::SUCCESS ? EEIteratorErrCode::EE_OK
                                 : EEIteratorErrCode::EE_ERROR);
}

void StorageHandler::SetSpans(std::vector<KwTsSpan> *ts_spans) {
  ts_spans_ = ts_spans;
}

EEIteratorErrCode StorageHandler::TsNext(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_OK;

  while (true) {
    ScanRowBatch* row_batch =
        static_cast<ScanRowBatch *>(current_thd->GetRowBatch());
    if (nullptr == ts_iterator) {
      code = NewTsIterator(ctx);
      if (code != EEIteratorErrCode::EE_OK) {
        break;
      }
      code = GetNextTagData(ctx, row_batch);
      if (code != EEIteratorErrCode::EE_OK) {
        Return(code);
      }
    }

    row_batch->Reset();
    KStatus ret = ts_iterator->Next(&row_batch->res_, &row_batch->count_, row_batch->ts_);
    // LOG_DEBUG("TsTableIterator::Next() count:%d", row_batch->count_);
    if (KStatus::FAIL == ret) {
      LOG_ERROR("TsTableIterator::Next() Failed\n");
      code = EEIteratorErrCode::EE_ERROR;
      break;
    }
    total_read_rows_ += row_batch->count_;

    if (0 == row_batch->count_) {
      // ret = row_batch->tag_rowbatch_->NextLine(&(current_tag_index_));
      // if (KStatus::FAIL == ret) {
      current_line_++;
      if (current_line_ >= entities_.size()) {
        code = NewTsIterator(ctx);
        if (code != EEIteratorErrCode::EE_OK) {
          Return(code);
        }
      }
      code = GetNextTagData(ctx, row_batch);
      if (code != EEIteratorErrCode::EE_OK) {
        Return(code);
      }
    } else {
      while (!entities_[current_line_].equalsWithoutMem(
             row_batch->res_.entity_index)) {
        // ret = data_handle->tag_rowbatch_->NextLine(&(current_tag_index_));
        // if (KStatus::FAIL == ret) {
        current_line_++;
        if (current_line_ >= entities_.size()) {
          code = NewTsIterator(ctx);
          if (code != EEIteratorErrCode::EE_OK) {
            Return(code);
          }
        }
        code = GetNextTagData(ctx, row_batch);
        if (code != EEIteratorErrCode::EE_OK) {
          Return(code);
        }
      }
      break;
    }
  }

  Return(code);
}

EEIteratorErrCode StorageHandler::TagNext(kwdbContext_p ctx, Field *tag_filter) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;
  KWThdContext *thd = current_thd;
  RowBatch* ptr = thd->GetRowBatch();
  thd->SetRowBatch(tag_rowbatch_.get());
  while (true) {
    tag_rowbatch_->Reset();
    KStatus ret = tag_iterator->Next(&(tag_rowbatch_->entity_indexs_),
                                     &(tag_rowbatch_->res_),
                                     &(tag_rowbatch_->count_));
    if (KStatus::FAIL == ret) {
      break;
    }

    code = EEIteratorErrCode::EE_OK;
    // LOG_DEBUG("Handler::TagNext count:%d", tag_rowbatch_->count_);
    if (0 == tag_rowbatch_->count_) {
      code = EEIteratorErrCode::EE_END_OF_RECORD;
      break;
    }

    if (nullptr == tag_filter) {
      break;
    }
    tagFilter(ctx, tag_filter);
    if (tag_rowbatch_->effect_count_ > 0) {
      break;
    }
  }
  tag_rowbatch_->SetPipeEntityNum(ctx, current_thd->GetDegree());
  thd->SetRowBatch(ptr);
  Return(code);
}

KStatus StorageHandler::Close() {
  KStatus ret = KStatus::SUCCESS;
  SafeDeletePointer(ts_iterator);

  if (nullptr != tag_iterator) {
    tag_iterator->Close();
    SafeDeletePointer(tag_iterator);
  }

  return ret;
}
EEIteratorErrCode StorageHandler::GetNextTagData(kwdbContext_p ctx, ScanRowBatch *row_batch) {
  KStatus ret = FAIL;
  EEIteratorErrCode code = EEIteratorErrCode::EE_OK;

  ret = row_batch->tag_rowbatch_->GetTagData(&(row_batch->tagdata_),
                                                 &(row_batch->tag_bitmap_),
                                                 entities_[current_line_].index);
  if (KStatus::FAIL == ret) {
    code = EE_END_OF_RECORD;
  }
  return code;
}

EEIteratorErrCode StorageHandler::NewTsIterator(kwdbContext_p ctx) {
  EnterFunc();
  KStatus ret = FAIL;
  EEIteratorErrCode code = EEIteratorErrCode::EE_OK;

  ScanRowBatch* data_handle =
      static_cast<ScanRowBatch *>(current_thd->GetRowBatch());

  do {
    entities_.clear();
    ret = tag_scan_->GetEntities(ctx, &entities_,
                                 &(data_handle->tag_rowbatch_));
    if (KStatus::FAIL == ret) {
      code = EE_END_OF_RECORD;
      break;
    }
    current_line_ = 0;
    data_handle->SetTagToColOffset(table_->GetMinTagId());
    if (ts_iterator) {
      SafeDeletePointer(ts_iterator);
    }
    std::vector<KwTsSpan> ts_spans;
    if (ctx->is_single_node) {
      ts_spans = *ts_spans_;
    } else {
      auto it = table_->hash_points_spans_.find(entities_[0].hash_point);
      if (it != table_->hash_points_spans_.end()) {
        for (auto const &ts_span : it->second) {
          ts_spans.push_back(ts_span);
          // LOG_DEBUG(
          //     "TSTable::GetIterator() entityID is %d, hashPoint is %d , ts_span.begin is %ld, "
          //     "ts_span.end is %ld  \n",
          //     entities[0].entityId, entities[0].hash_point, ts_span.begin, ts_span.end);
        }
      }
    }
    // LOG_DEBUG("TSTable::GetIterator() entity_size %ld", sizeof(entities_));

    // LOG_DEBUG("TSTable::GetIterator() ts_span_size:%ld", sizeof(ts_spans));
    ret = ts_table_->GetIterator(ctx, entities_, ts_spans, table_->scan_cols_,
                                 table_->scan_real_agg_types_, table_->table_version_,
                                 &ts_iterator, table_->scan_real_last_ts_points_, table_->is_reverse_, false);
    if (KStatus::FAIL == ret) {
      code = EEIteratorErrCode::EE_ERROR;
      LOG_ERROR("TsTable::GetIterator() error\n");
      break;
    }
  } while (0);
  Return(code);
}

EEIteratorErrCode StorageHandler::NewTagIterator(kwdbContext_p ctx) {
  EnterFunc();
  KStatus ret = FAIL;
  if (ctx->is_single_node) {
      if (read_mode_ == TSTableReadMode::metaTable) {
        MetaIterator *meta = nullptr;
        ret = ts_table_->GetMetaIterator(ctx, &meta, table_->table_version_);
        tag_iterator = meta;
      } else {
        TagIterator *tag = nullptr;
        ret = ts_table_->GetTagIterator(ctx, table_->scan_tags_, &tag, table_->table_version_);
        tag_iterator = tag;
      }
  } else {
    TagIterator *tag = nullptr;
    ret = ts_table_->GetTagIterator(ctx, table_->scan_tags_, table_->hash_points_, &tag, table_->table_version_);
    tag_iterator = tag;
  }

  Return(ret == KStatus::FAIL ? EEIteratorErrCode::EE_ERROR
                              : EEIteratorErrCode::EE_OK);
}

EEIteratorErrCode StorageHandler::GetEntityIdList(kwdbContext_p ctx,
                                           TSTagReaderSpec *spec,
                                           Field *tag_filter) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;
  KWThdContext *thd = current_thd;
  RowBatch* old_ptr = thd->GetRowBatch();
  thd->SetRowBatch(tag_rowbatch_.get());

  std::vector<void*> primary_tags;
  do {
    k_int32 sz = spec->primarytags_size();
    if (sz <= 0) {
      break;
    }
    size_t malloc_size = 0;
    for (int i = 0; i < sz; ++i) {
      k_uint32 tag_id = spec->mutable_primarytags(i)->colid();
      malloc_size += table_->fields_[tag_id]->get_storage_length();
    }
    KStatus ret = GeneratePrimaryTags(spec, malloc_size, sz, &primary_tags);
    if (ret != SUCCESS) {
      break;
    }
    ret = ts_table_->GetEntityIdList(
        ctx, primary_tags, table_->scan_tags_, &tag_rowbatch_->entity_indexs_,
        &tag_rowbatch_->res_, &tag_rowbatch_->count_);
    if (ret != SUCCESS) {
      break;
    }
    if (tag_filter) {
      tagFilter(ctx, tag_filter);
      if (0 == tag_rowbatch_->effect_count_) {
        code = EEIteratorErrCode::EE_END_OF_RECORD;
        break;
      }
    } else if (0 == tag_rowbatch_->count_) {
      code = EEIteratorErrCode::EE_END_OF_RECORD;
      break;
    }
    tag_rowbatch_->SetPipeEntityNum(ctx, current_thd->GetDegree());
    code = EEIteratorErrCode::EE_OK;
  } while (0);
  for (auto& it : primary_tags) {
    SafeFreePointer(it);
  }
  thd->SetRowBatch(old_ptr);
  Return(code);
}

KStatus StorageHandler::GeneratePrimaryTags(TSTagReaderSpec *spec, size_t malloc_size,
                                     kwdbts::k_int32 sz,
                                     std::vector<void *> *primary_tags) {
  char *ptr = nullptr;
  k_int32 ns = spec->mutable_primarytags(0)->tagvalues_size();
  for (k_int32 i = 0; i < ns; ++i) {
    void *buffer = malloc(malloc_size);
    if (buffer == nullptr) {
      return FAIL;
    }

    memset(buffer, 0, malloc_size);
    ptr = static_cast<char *>(buffer);
    for (k_int32 j = 0; j < sz; ++j) {
      TSTagReaderSpec_TagValueArray *tagInfo = spec->mutable_primarytags(j);
      k_uint32 tag_id = tagInfo->colid();
      const std::string &str = tagInfo->tagvalues(i);
      roachpb::DataType d_type = table_->fields_[tag_id]->get_storage_type();
      k_int32 len = table_->fields_[tag_id]->get_storage_length();
      switch (d_type) {
        case roachpb::DataType::BOOL: {
          k_bool val = 0;
          if (str == "true" || str == "TRUE") {
            val = 1;
          } else if (str == "false" || str == "FALSE") {
            val = 0;
          } else {
            val = std::stoi(str);
          }
          memcpy(ptr, &val, len);
        } break;
        case roachpb::DataType::SMALLINT: {
          k_int32 val = std::stoi(str);
          if (!CHECK_VALID_SMALLINT(val)) {
            return FAIL;
          }
          memcpy(ptr, &val, len);
        } break;
        case roachpb::DataType::INT: {
          k_int64 val = std::stoll(str);
          if (!CHECK_VALID_INT(val)) {
            return FAIL;
          }
          memcpy(ptr, &val, sizeof(k_int32));
        } break;
        case roachpb::DataType::TIMESTAMP: {
          k_uint64 val = std::stoll(str);
          memcpy(ptr, &val, sizeof(KTimestamp));
        } break;
        case roachpb::DataType::TIMESTAMPTZ: {
          k_uint64 val = std::stoll(str);
          memcpy(ptr, &val, sizeof(KTimestampTz));
        }
        case roachpb::DataType::DATE: {
          k_uint64 val = std::stoll(str);
          memcpy(ptr, &val, sizeof(KDate));
        }
        case roachpb::DataType::BIGINT: {
          k_int64 val = std::stoll(str);
          memcpy(ptr, &val, sizeof(k_int64));
        } break;
        case roachpb::DataType::FLOAT: {
          k_float32 val = std::stof(str);
          memcpy(ptr, &val, sizeof(k_float32));
        } break;
        case roachpb::DataType::DOUBLE: {
          k_double64 val = std::stod(str);
          memcpy(ptr, &val, sizeof(k_double64));
        } break;
        case roachpb::DataType::CHAR:
        case roachpb::DataType::NCHAR:
        case roachpb::DataType::VARCHAR:
        case roachpb::DataType::NVARCHAR:
          memcpy(ptr, str.c_str(), str.length());
          break;
        case roachpb::DataType::BINARY:
        case roachpb::DataType::VARBINARY: {
          k_uint32 buf_len = str.length() - 1;
          if (buf_len > 2 * len + 3) {
            buf_len = 2 * len + 3;
          }
          k_int32 n = 2;
          for (k_uint32 i = 3; i < buf_len; i = i + 2) {
            if (str[i] >= 'a' && str[i] >= 'f') {
              ptr[n] = str[i] - 'a' + 10;
            } else {
              ptr[n] = str[i] - '0';
            }
            if (str[i + 1] >= 'a' && str[i + 1] >= 'f') {
              ptr[n] = ptr[n] << 4 | (str[i + 1] - 'a' + 10);
            } else {
              ptr[n] = ptr[n] << 4 | (str[i + 1] - '0');
            }
            n++;
          }
          *(static_cast<k_int16 *>(static_cast<void *>(ptr))) = n - 2;
          break;
        }
        default: {
          free(buffer);
          LOG_ERROR("unsupported data type:%d", d_type);
          return FAIL;
        }
      }
      ptr += table_->fields_[tag_id]->get_storage_length();
    }
    primary_tags->push_back(buffer);
  }
  return SUCCESS;
}

void StorageHandler::tagFilter(kwdbContext_p ctx, Field *tag_filter) {
  EnterFunc();

  for (k_uint32 i = 0; i < tag_rowbatch_->count_; ++i) {
    if (0 == tag_filter->ValInt()) {
      tag_rowbatch_->NextLine();
      continue;
    }

    tag_rowbatch_->AddSelection();
    tag_rowbatch_->NextLine();
  }
  tag_rowbatch_->isFilter_ = true;
  tag_rowbatch_->ResetLine();

  ReturnVoid();
}

bool StorageHandler::isDisorderedMetrics() {
  if (ts_iterator == nullptr) {
    return false;
  } else {
    return ts_iterator->IsDisordered();
  }
}

}  // namespace kwdbts
