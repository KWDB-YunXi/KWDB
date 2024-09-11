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
#pragma once

#include <memory>
#include <queue>
#include <utility>
#include <vector>

#include "cm_kwdb_context.h"
#include "ee_row_batch.h"
#include "ee_global.h"
#include "er_api.h"
#include "ee_data_chunk.h"

namespace kwdbts {

struct GroupByColumnInfo {
  k_uint32 col_index;
  char* data_ptr;
  k_uint32 len;
};

class TABLE;

class Field;

/**
 * @brief   The base class of the operator module
 *
 * @author  liguoliang
 */
class BaseOperator {
 public:
  BaseOperator() {}

  explicit BaseOperator(TABLE* table, int32_t processor_id) : table_(table), processor_id_(processor_id) {}

  virtual ~BaseOperator() {
    if (num_ > 0 && renders_) {
      free(renders_);
    }

    for (auto field : output_fields_) {
      SafeDeletePointer(field);
    }

    num_ = 0;
    renders_ = nullptr;
  }

  BaseOperator(const BaseOperator&) = delete;

  BaseOperator& operator=(const BaseOperator&) = delete;

  BaseOperator(BaseOperator&&) = delete;

  BaseOperator& operator=(BaseOperator&&) = delete;

  virtual EEIteratorErrCode Init(kwdbContext_p ctx) = 0;

  virtual EEIteratorErrCode Start(kwdbContext_p ctx) = 0;

  // get next batch data
  virtual EEIteratorErrCode Next(kwdbContext_p ctx) {
    return EEIteratorErrCode::EE_END_OF_RECORD;
  }

  virtual EEIteratorErrCode Next(kwdbContext_p ctx, DataChunkPtr& chunk) {
    return EEIteratorErrCode::EE_END_OF_RECORD;
  }

  virtual EEIteratorErrCode Reset(kwdbContext_p ctx) {
    return EEIteratorErrCode::EE_OK;
  }

  virtual RowBatchPtr GetRowBatch(kwdbContext_p ctx) { return nullptr; }

  virtual KStatus Close(kwdbContext_p ctx) = 0;

  virtual Field** GetRender() { return renders_; }

  virtual Field* GetRender(int i) {
    if (i < num_)
      return renders_[i];
    else
      return nullptr;
  }

  // render size
  virtual k_uint32 GetRenderSize() { return num_; }

  // table object
  virtual TABLE* table() { return table_; }

  // output fields
  virtual std::vector<Field*>& OutputFields() { return output_fields_; }

  virtual BaseOperator* Clone() { return nullptr; }

  virtual k_uint32 GetTotalReadRow() { return 0; }

  static const k_uint64 DEFAULT_MAX_MEM_BUFFER_SIZE = 268435456;  // 256M

 protected:
  inline void constructDataChunk() {
    current_data_chunk_ = std::make_unique<DataChunk>(output_col_info_);
    if (current_data_chunk_->Initialize() != true) {
      current_data_chunk_ = nullptr;
      return;
    }
  }

  inline void constructDataChunk(k_uint32 capacity) {
    current_data_chunk_ = std::make_unique<DataChunk>(output_col_info_, capacity);
    if (current_data_chunk_->Initialize() != true) {
      current_data_chunk_ = nullptr;
      return;
    }
  }

  Field** renders_{nullptr};  // the operator projection column of this layer
  k_uint32 num_{0};           // count of projected column
  TABLE* table_{nullptr};     // table object
  k_int32 processor_id_{0};   // operator ID
  // output columns of the current layer，input columns of Parent
  // operator(FieldNum)
  std::vector<Field*> output_fields_;
  std::vector<ColumnInfo> output_col_info_;

  DataChunkPtr current_data_chunk_;
  std::queue<DataChunkPtr> output_queue_;
  DataChunkPtr temporary_data_chunk_;
  k_bool is_done_{false};

  bool is_clone_{false};
};

template<class RealNode, class... Args>
BaseOperator* NewIterator(Args&& ... args) {
  return new RealNode(std::forward<Args>(args)...);
}

class GroupByMetadata {
 public:
  GroupByMetadata() : capacity_(DEFAULT_CAPACITY) {
    initialize();
  }

  ~GroupByMetadata() {
    delete[] bitmap_;
  }

  void reset(k_uint32 capacity) {
    if (capacity_ < capacity) {
      delete[] bitmap_;
      capacity_ = capacity;
      initialize();
    } else {
      k_uint32 len = (capacity_ + 7) / 8;
      std::memset(bitmap_, 0, len);
    }
  }

  void setNewGroup(k_uint32 line) {
    bitmap_[line / 8] |= (1 << 7) >> (line % 8);
  }

  bool isNewGroup(k_uint32 line) {
    return (bitmap_[line / 8] & ((1 << 7) >> (line % 8))) != 0;
  }

 private:
  void initialize() {
    k_uint32 len = (capacity_ + 7) / 8;
    bitmap_ = new char[len];
    std::memset(bitmap_, 0, len);
  }

  static const k_uint32 DEFAULT_CAPACITY = 1000;
  k_uint32 capacity_;
  char* bitmap_{nullptr};
};

}  // namespace kwdbts
