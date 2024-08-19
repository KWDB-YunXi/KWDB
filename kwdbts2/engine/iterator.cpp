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

#include <utility>
#include "engine.h"
#include "iterator.h"
#include "perf_stat.h"

namespace kwdbts {

Batch* CreateAggBatch(void* mem, std::shared_ptr<MMapSegmentTable> segment_table) {
  if (mem) {
    return new AggBatch(mem, 1, segment_table);
  } else {
    return new AggBatch(nullptr, 0, segment_table);
  }
}

Batch* CreateAggBatch(std::shared_ptr<void> mem, std::shared_ptr<MMapSegmentTable> segment_table) {
  if (mem) {
    return new AggBatch(mem, 1, segment_table);
  } else {
    return new AggBatch(nullptr, 0, segment_table);
  }
}

// Agreement between storage layer and execution layer:
// 1. The SUM aggregation result of integer type returns the int64 type uniformly without overflow;
//    In case of overflow, return double type
// 2. The return type for floating-point numbers is double
// This function is used for type conversion of SUM aggregation results.
bool ChangeSumType(DATATYPE type, void* base, void** new_base) {
  if (type != DATATYPE::INT8 && type != DATATYPE::INT16 && type != DATATYPE::INT32 && type != DATATYPE::FLOAT) {
    *new_base = base;
    return false;
  }
  void* sum_base = malloc(8);
  memset(sum_base, 0, 8);
  switch (type) {
    case DATATYPE::INT8:
      *(static_cast<int64_t*>(sum_base)) = *(static_cast<int8_t*>(base));
      break;
    case DATATYPE::INT16:
      *(static_cast<int64_t*>(sum_base)) = *(static_cast<int16_t*>(base));
      break;
    case DATATYPE::INT32:
      *(static_cast<int64_t*>(sum_base)) = *(static_cast<int32_t*>(base));
      break;
    case DATATYPE::FLOAT:
      *(static_cast<double*>(sum_base)) = *(static_cast<float*>(base));
  }
  *new_base = sum_base;
  return true;
}

TsIterator::TsIterator(std::shared_ptr<TsEntityGroup> entity_group, uint64_t entity_group_id, uint32_t subgroup_id,
                       vector<uint32_t>& entity_ids, std::vector<KwTsSpan>& ts_spans,
                       std::vector<uint32_t>& kw_scan_cols, std::vector<uint32_t>& ts_scan_cols)
                       : entity_group_id_(entity_group_id),
                         subgroup_id_(subgroup_id),
                         entity_ids_(entity_ids),
                         ts_spans_(ts_spans),
                         kw_scan_cols_(kw_scan_cols),
                         ts_scan_cols_(ts_scan_cols),
                         entity_group_(entity_group) {
  entity_group_->RdDropLock();
}

TsIterator::~TsIterator() {
  for (auto& bt : p_bts_) {
    if (bt != nullptr) {
      entity_group_->GetSubEntityGroupManager()->ReleasePartitionTable(bt);
    }
  }
  if (segment_iter_ != nullptr) {
    delete segment_iter_;
    segment_iter_ = nullptr;
  }
  entity_group_->DropUnlock();
}

void TsIterator::fetchBlockItems(k_uint32 entity_id) {
  p_bts_[cur_p_bts_idx_]->GetAllBlockItems(entity_id, block_item_queue_, is_reversed_);
}

KStatus TsIterator::Init(std::vector<MMapPartitionTable*>& p_bts, bool is_reversed) {
  p_bts_ = std::move(p_bts);
  is_reversed_ = is_reversed;
  if (!p_bts_.empty() && is_reversed_) {
    reverse(p_bts_.begin(), p_bts_.end());
  }
  if (!p_bts_.empty()) {
    attrs_ = (*p_bts_.begin())->hierarchyInfo();
  }
  if (cur_entity_idx_ < entity_ids_.size()) {
    cur_entity_id_ = entity_ids_[cur_entity_idx_];
  }
  if (cur_p_bts_idx_ < p_bts_.size() && cur_entity_id_ != 0) {
    fetchBlockItems(cur_entity_id_);
  }
  return SUCCESS;
}

// return -1 means all partition tables scan over.
int TsIterator::nextBlockItem(k_uint32 entity_id) {
  cur_block_item_ = nullptr;
  // block_item_queue_ saves the BlockItem object pointer of the partition table for the current query
  // Calling the Next function once can retrieve data within a maximum of one BlockItem.
  // If a BlockItem query is completed, the next BlockItem object needs to be obtained:
  // 1. If there are still BlockItems that have not been queried in block_item_queue_, obtain them in block_item_queue_
  // 2. If there are no more BlockItems that have not been queried in block_item_queue_,
  //    switch the partition table to retrieve all block_item_queue_ in the next partition table, and then proceed to step 1
  // 3. If all partition tables have been queried, return -1
  while (true) {
    if (block_item_queue_.empty()) {
      cur_p_bts_idx_++;
      if (segment_iter_ != nullptr) {
        delete segment_iter_;
        segment_iter_ = nullptr;
      }
      if (cur_p_bts_idx_ >= p_bts_.size()) {
        return -1;
      }
      fetchBlockItems(entity_id);
      continue;
    }
    cur_block_item_ = block_item_queue_.front();
    block_item_queue_.pop_front();

    if (!cur_block_item_) {
      LOG_WARN("BlockItem[] error: No space has been allocated");
      continue;
    }
    return 0;
  }
}

KStatus TsRawDataIterator::Next(ResultSet* res, k_uint32* count, bool* is_finished, timestamp64 ts) {
  KWDB_DURATION(StStatistics::Get().it_next);
  *count = 0;
  while (true) {
    // If cur_block_item_ is a null pointer and attempts to call nextBlockItem to retrieve a new BlockItem for querying:
    // 1. nextBlockItem ended normally, query cur_block_item_
    // 2. If nextBlockItem returns -1, it indicates that all data on the current entity has been queried and
    //    needs to be switched to the next entity before attempting to retrieve cur_block_item_
    // 3. If all entities have been queried, the current query process ends and returns directly
    if (!cur_block_item_) {
      if (nextBlockItem(cur_entity_id_) < 0) {
        if (++cur_entity_idx_ >= entity_ids_.size()) {
          *is_finished = true;
          break;
        }
        cur_entity_id_ = entity_ids_[cur_entity_idx_];
        cur_p_bts_idx_ = -1;
      }
      continue;
    }
    MMapPartitionTable* cur_pt = p_bts_[cur_p_bts_idx_];
    if (ts != INVALID_TS) {
      if (!is_reversed_ && cur_pt->minTimestamp() * 1000 > ts) {
        // 此时确定没有比ts更小的数据存在，直接返回-1，查询结束
        return SUCCESS;
      } else if (is_reversed_ && cur_pt->maxTimestamp() * 1000 < ts) {
        // 此时确定没有比ts更大的数据存在，直接返回-1，查询结束
        return SUCCESS;
      }
    }
    uint32_t first_row = 1;
    MetricRowID first_real_row = cur_block_item_->getRowID(first_row);

    std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(cur_block_item_->block_id);
    if (segment_tbl == nullptr) {
      LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                cur_block_item_->block_id, cur_pt->GetPath().c_str());
      return FAIL;
    }
    if (nullptr == segment_iter_ || segment_iter_->segment_id() != segment_tbl->segment_id()) {
      if (segment_iter_ != nullptr) {
        delete segment_iter_;
        segment_iter_ = nullptr;
      }
      segment_iter_ = new MMapSegmentTableIterator(segment_tbl, ts_spans_, kw_scan_cols_, ts_scan_cols_, attrs_);
    }

    bool has_data = false;
    // Sequential read optimization, if the maximum and minimum timestamps of a BlockItem are within the ts_span range,
    // there is no need to determine the timestamps for each row data.
    if (cur_block_item_->is_agg_res_available && cur_block_item_->publish_row_count > 0 && cur_blockdata_offset_ == 1
        && isTimestampWithinSpans(ts_spans_,
               KTimestamp(segment_tbl->columnAggAddr(cur_block_item_->block_id, 0, Sumfunctype::MIN)),
               KTimestamp(segment_tbl->columnAggAddr(cur_block_item_->block_id, 0, Sumfunctype::MAX)))) {
      k_uint32 cur_row = 1;
      while (cur_row <= cur_block_item_->publish_row_count) {
        bool is_deleted;
        if (cur_block_item_->isDeleted(cur_row, &is_deleted) < 0) {
          return KStatus::FAIL;
        }
        if (is_deleted) {
          break;
        }
        ++cur_row;
      }
      if (cur_row > cur_block_item_->publish_row_count) {
        cur_blockdata_offset_ = cur_row;
        *count = cur_block_item_->publish_row_count;
        first_row = 1;
        first_real_row = cur_block_item_->getRowID(first_row);
        has_data = true;
      }
    }
    // If the data has been queried through the sequential reading optimization process, assemble Batch and return it;
    // Otherwise, traverse the data within the current BlockItem one by one,
    // and return the maximum number of consecutive data that meets the condition.
    if (has_data) {
      segment_iter_->GetBatch(cur_block_item_, first_row, res, *count);
    } else {
      segment_iter_->Next(cur_block_item_, &cur_blockdata_offset_, res, count);
    }
    // If the data query within the current BlockItem is completed, switch to the next block.
    if (cur_blockdata_offset_ > cur_block_item_->publish_row_count) {
      cur_blockdata_offset_ = 1;
      nextBlockItem(cur_entity_id_);
    }
    if (*count > 0) {
      KWDB_STAT_ADD(StStatistics::Get().it_num, *count);
      res->entity_index = {entity_group_id_, cur_entity_id_, subgroup_id_};
      return SUCCESS;
    }
  }
  KWDB_STAT_ADD(StStatistics::Get().it_num, *count);
  return SUCCESS;
}

// Used to update the value of the first/first_row member variable during the traversal process
void TsAggIterator::updateFirstCols(timestamp64 ts, MetricRowID row_id) {
  MMapPartitionTable* cur_pt = p_bts_[cur_first_idx_];
  std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(row_id.block_id);
  if (segment_tbl == nullptr) {
    LOG_ERROR("Can not find segment use block [%d], in path [%s]", cur_block_item_->block_id, cur_pt->GetPath().c_str());
    return;
  }

  for (auto& it : first_pairs_) {
    timestamp64 first_ts = it.second.second.first;
    // If the timestamp corresponding to the data in this row is less than the first value of the record and
    // is non-empty, update it.
    if ((first_ts == INVALID_TS || first_ts > ts) && !segment_tbl->isNullValue(row_id, it.first)) {
      it.second = {cur_first_idx_, {ts, row_id}};
    }
  }
  timestamp64 first_row_ts = first_row_pair_.second.first;
  // If the timestamp corresponding to the data in this row is less than the first record, update it.
  if ((first_row_ts == INVALID_TS || first_row_ts > ts)) {
    first_row_pair_ = {cur_first_idx_, {ts, row_id}};
  }
}

// Used to update the value of the last/last_row member variable during the traversal process
void TsAggIterator::updateLastCols(timestamp64 ts, MetricRowID row_id) {
  MMapPartitionTable* cur_pt = p_bts_[cur_last_idx_];
  std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(row_id.block_id);
  if (segment_tbl == nullptr) {
    LOG_ERROR("Can not find segment use block [%d], in path [%s]", cur_block_item_->block_id, cur_pt->GetPath().c_str());
    return;
  }

  for (auto& it : last_pairs_) {
    timestamp64 last_ts = it.second.second.first;
    // If the timestamp corresponding to the data in this row is greater than the last value of the record and
    // is non-empty, update it.
    if ((last_ts == INVALID_TS || last_ts < ts) && !segment_tbl->isNullValue(row_id, it.first)) {
      it.second = {cur_last_idx_, {ts, row_id}};
    }
  }
  timestamp64 last_row_ts = last_row_pair_.second.first;
  // If the timestamp corresponding to the data in this row is greater than the last record, update it.
  if ((last_row_ts == INVALID_TS || last_row_ts < ts)) {
    last_row_pair_ = {cur_last_idx_, {ts, row_id}};
  }
}

// Used to update the value of the last/last_row member variable during the traversal process
void TsAggIterator::updateFirstLastCols(timestamp64 ts, MetricRowID row_id) {
  MMapPartitionTable* cur_pt = p_bts_[cur_p_bts_idx_];
  std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(row_id.block_id);
  if (segment_tbl == nullptr) {
    LOG_ERROR("Can not find segment use block [%d], in path [%s]", cur_block_item_->block_id, cur_pt->GetPath().c_str());
    return;
  }

  for (auto& it : first_pairs_) {
    timestamp64 first_ts = it.second.second.first;
    // If the timestamp corresponding to the data in this row is less than the first value of the record and is
    // non-empty, update it.
    if ((first_ts == INVALID_TS || first_ts > ts) && !segment_tbl->isNullValue(row_id, it.first)) {
      it.second = {cur_p_bts_idx_, {ts, row_id}};
    }
  }
  for (auto& it : last_pairs_) {
    timestamp64 last_ts = it.second.second.first;
    // If the timestamp corresponding to the data in this row is greater than the last value of the record and
    // is non-empty, update it.
    if ((last_ts == INVALID_TS || last_ts < ts) && !segment_tbl->isNullValue(row_id, it.first)) {
      it.second = {cur_p_bts_idx_, {ts, row_id}};
    }
  }
  timestamp64 first_row_ts = first_row_pair_.second.first;
  // If the timestamp corresponding to the data in this row is less than the first record, update it.
  if ((first_row_ts == INVALID_TS || first_row_ts > ts)) {
    first_row_pair_ = {cur_p_bts_idx_, {ts, row_id}};
  }
  timestamp64 last_row_ts = last_row_pair_.second.first;
  // If the timestamp corresponding to the data in this row is greater than the last record, update it.
  if ((last_row_ts == INVALID_TS || last_row_ts < ts)) {
    last_row_pair_ = {cur_p_bts_idx_, {ts, row_id}};
  }
}

// first/first_row aggregate type query optimization function:
// Starting from the first data entry of the first BlockItem in the smallest partition table, if the timestamp of the
// data being queried is equal to min_ts recorded in the EntityItem,
// it can be confirmed that the data record with the smallest timestamp has been queried.
// Due to the fact that most temporal data is written in sequence, this approach is likely to quickly find the data
// with the smallest timestamp and avoid subsequent invalid data traversal, accelerating the query time
// for first/first_row types.
KStatus TsAggIterator::findFirstData(ResultSet* res, k_uint32* count, timestamp64 ts) {
  KWDB_DURATION(StStatistics::Get().agg_first);
  *count = 0;
  if (hasFoundFirstAggData() || cur_first_idx_ >= p_bts_.size()) {
    return KStatus::SUCCESS;
  }
  for ( ; cur_first_idx_ < p_bts_.size(); ++cur_first_idx_) {
    MMapPartitionTable* cur_pt = p_bts_[cur_first_idx_];
    if (ts != INVALID_TS) {
      if (!is_reversed_ && cur_pt->minTimestamp() * 1000 > ts) {
        // 此时确定没有比ts更小的数据存在，跳出循环
        break;
      } else if (is_reversed_ && cur_pt->maxTimestamp() * 1000 < ts) {
        // 此时确定没有比ts更大的数据存在，直接返回-1，查询结束
        break;
      }
    }
    block_item_queue_.clear();
    cur_pt->GetAllBlockItems(cur_entity_id_, block_item_queue_);
    auto entity_item = cur_pt->getEntityItem(cur_entity_id_);
    // Obtain the minimum timestamp for the current query entity.
    // Once a record's timestamp is traversed to be equal to it,
    // it indicates that the query result has been found and no additional data needs to be traversed.
    timestamp64 min_entity_ts = entity_item->min_ts;
    while (!block_item_queue_.empty()) {
      BlockItem* block_item = block_item_queue_.front();
      block_item_queue_.pop_front();
      if (!block_item || !block_item->publish_row_count) {
        continue;
      }

      std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(block_item->block_id);
      if (segment_tbl == nullptr) {
        LOG_ERROR("Can not find segment use block [%d], in path [%s]", block_item->block_id, cur_pt->GetPath().c_str());
        return KStatus::FAIL;
      }
      // If there is no first_row type query, it can be skipped directly when all data in the current block is null.
      if (no_first_row_type_ &&
          segment_tbl->isAllNullValue(block_item->block_id, block_item->publish_row_count, ts_scan_cols_)) {
        continue;
      }
      timestamp64 min_ts = segment_tbl->getBlockMinTs(block_item->block_id);
      timestamp64 max_ts = segment_tbl->getBlockMaxTs(block_item->block_id);
      // If the time range of the BlockItem is not within the ts_span range, continue traversing the next BlockItem.
      if (!isTimestampInSpans(ts_spans_, min_ts, max_ts)) {
        continue;
      }
      bool has_found = false;
      // Traverse all data of this BlockItem
      uint32_t cur_row_offset = 1;
      while (cur_row_offset <= block_item->publish_row_count) {
        bool is_deleted;
        if (block_item->isDeleted(cur_row_offset, &is_deleted) < 0) {
          return KStatus::FAIL;
        }
        // If the data in the cur_row_offset row is not within the ts_span range or has been deleted,
        // continue to verify the data in the next row.
        MetricRowID real_row = block_item->getRowID(cur_row_offset);
        timestamp64 cur_ts = KTimestamp(segment_tbl->columnAddr(real_row, 0));
        if (is_deleted || !checkIfTsInSpan(cur_ts)) {
          ++cur_row_offset;
          continue;
        }
        // Update variables that record the query results of first/first_row
        updateFirstCols(cur_ts, real_row);
        // If all queried columns and their corresponding query types already have results,
        // and the timestamp of the updated data is equal to the minimum timestamp of the entity, then the query can end.
        if (hasFoundFirstAggData() && cur_ts == min_entity_ts) {
          has_found = true;
          break;
        }
        ++cur_row_offset;
      }
      if (has_found) {
        break;
      }
    }
    if (hasFoundFirstAggData()) {
      break;
    }
  }

  // The data traversal is completed, and the variable of the first/first_row query result updated by the
  // updateFirstCols function is encapsulated as Batch and added to res to be returned to the execution layer.
  for (k_uint32 i = 0; i < scan_agg_types_.size(); ++i) {
    k_int32 ts_col = -1;
    if (i < ts_scan_cols_.size()) {
      ts_col = ts_scan_cols_[i];
    }
    if (ts_col < 0) {
      continue;
    }
    switch (scan_agg_types_[i]) {
      case FIRST: {
        Batch* b;
        k_int32 pt_idx = first_pairs_[ts_col].first;
        // Read the first_pairs_ result recorded during the traversal process.
        // If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = first_pairs_[ts_col].second.second;
          timestamp64 first_ts = first_pairs_[ts_col].second.first;
          int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
          if (err_code < 0) {
            LOG_ERROR("getActualColBatch failed.");
            return FAIL;
          }
        }
        res->push_back(i, b);
        break;
      }
      case FIRSTTS: {
        Batch* b;
        k_int32 pt_idx = first_pairs_[ts_col].first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = first_pairs_[ts_col].second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return KStatus::FAIL;;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      case FIRST_ROW: {
        Batch* b;
        k_int32 pt_idx = first_row_pair_.first;
        // Read the first_row_pairs_ result recorded during the traversal process.
        // If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = first_row_pair_.second.second;
          timestamp64 first_row_ts = first_row_pair_.second.first;
          MMapPartitionTable* cur_pt = p_bts_[pt_idx];
          std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, cur_pt->GetPath().c_str());
            return KStatus::FAIL;;
          }
          if (segment_tbl->isNullValue(real_row, ts_col)) {
            std::shared_ptr<void> first_row_data(nullptr);
            b = new AggBatch(first_row_data, 1, nullptr);
          } else {
            int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
            if (err_code < 0) {
              LOG_ERROR("getActualColBatch failed.");
              return FAIL;
            }
          }
        }
        res->push_back(i, b);
        break;
      }
      case FIRSTROWTS: {
        Batch* b;
        k_int32 pt_idx = first_row_pair_.first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = first_row_pair_.second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return KStatus::FAIL;;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      default:
        break;
    }
  }
  res->entity_index = {entity_group_id_, cur_entity_id_, subgroup_id_};
  if (isAllAggResNull(res)) {
    *count = 0;
    res->clear();
  } else {
    *count = 1;
  }
  return SUCCESS;
}

// last/last_row aggregate type query optimization function:
// Starting from the last data entry of the last BlockItem in the largest partition table,
// if the timestamp of the data being queried is equal to the max_ts recorded in the EntityItem,
// it can be confirmed that the data record with the largest timestamp has been queried.
// Due to the fact that most temporal data is written in sequence,
// this approach significantly improves query speed compared to traversing from the head.
KStatus TsAggIterator::findLastData(ResultSet* res, k_uint32* count, timestamp64 ts) {
  KWDB_DURATION(StStatistics::Get().agg_last);
  *count = 0;
  if (hasFoundLastAggData() || cur_last_idx_ < 0) {
    return KStatus::SUCCESS;
  }
  for ( ; cur_last_idx_ >= 0; --cur_last_idx_) {
    MMapPartitionTable* cur_pt = p_bts_[cur_last_idx_];
    if (ts != INVALID_TS) {
      if (!is_reversed_ && cur_pt->minTimestamp() * 1000 > ts) {
        // 此时确定没有比ts更小的数据存在，跳出循环
        break;
      } else if (is_reversed_ && cur_pt->maxTimestamp() * 1000 < ts) {
        // 此时确定没有比ts更大的数据存在，直接返回-1，查询结束
        break;
      }
    }
    block_item_queue_.clear();
    cur_pt->GetAllBlockItems(cur_entity_id_, block_item_queue_);
    auto entity_item = cur_pt->getEntityItem(cur_entity_id_);
    // Obtain the maximum timestamp of the current query entity.
    // Once a record's timestamp is traversed to be equal to it,
    // it indicates that the query result has been found and no additional data needs to be traversed.
    timestamp64 max_entity_ts = entity_item->max_ts;
    while (!block_item_queue_.empty()) {
      BlockItem* block_item = block_item_queue_.back();
      block_item_queue_.pop_back();
      if (!block_item || !block_item->publish_row_count) {
        continue;
      }

      std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(block_item->block_id);
      if (segment_tbl == nullptr) {
        LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                  block_item->block_id, cur_pt->GetPath().c_str());
        return KStatus::FAIL;
      }
      // If there is no last row type query, it can be skipped directly when the data in the current block is all null.
      if (no_last_row_type_ &&
          segment_tbl->isAllNullValue(block_item->block_id, block_item->publish_row_count, ts_scan_cols_)) {
        continue;
      }
      timestamp64 min_ts = segment_tbl->getBlockMinTs(block_item->block_id);
      timestamp64 max_ts = segment_tbl->getBlockMaxTs(block_item->block_id);
      // If the time range of the BlockItem is not within the ts_span range, continue traversing the next BlockItem.
      if (!isTimestampInSpans(ts_spans_, min_ts, max_ts)) {
        continue;
      }
      bool has_found = false;
      // Traverse all data of this BlockItem
      uint32_t cur_row_offset = block_item->publish_row_count;
      while (cur_row_offset > 0) {
        bool is_deleted;
        if (block_item->isDeleted(cur_row_offset, &is_deleted) < 0) {
          return KStatus::FAIL;
        }
        // If the data in the cur_row_offset_ row is not within the ts_span range or has been deleted,
        // continue to verify the data in the next row.
        MetricRowID real_row = block_item->getRowID(cur_row_offset);
        timestamp64 cur_ts = KTimestamp(segment_tbl->columnAddr(real_row, 0));
        if (is_deleted || !checkIfTsInSpan(cur_ts)) {
          --cur_row_offset;
          continue;
        }
        // Update variables that record the query results of last/last_row
        updateLastCols(cur_ts, real_row);
        // If all queried columns and their corresponding query types already have query results,
        // and the timestamp of the updated data is equal to the maximum timestamp of the entity, then the query can end.
        if (hasFoundLastAggData() && cur_ts == max_entity_ts) {
          has_found = true;
          break;
        }
        --cur_row_offset;
      }
      if (has_found) {
        break;
      }
    }
    if (hasFoundLastAggData()) {
      break;
    }
  }
  // The data traversal is completed, and the variables of the last/last_row query result updated by the
  // updateLastCols function are encapsulated as Batch and added to res to be returned to the execution layer.
  for (k_uint32 i = 0; i < scan_agg_types_.size(); ++i) {
    k_int32 ts_col = -1;
    if (i < ts_scan_cols_.size()) {
      ts_col = ts_scan_cols_[i];
    }
    if (ts_col < 0) {
      continue;
    }
    switch (scan_agg_types_[i]) {
      case LAST: {
        Batch* b;
        k_int32 pt_idx = last_pairs_[ts_col].first;
        //  Read the last_pairs_ result recorded during the traversal process.
        //  If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = last_pairs_[ts_col].second.second;
          timestamp64 last_ts = last_pairs_[ts_col].second.first;
          int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
          if (err_code < 0) {
            LOG_ERROR("getActualColBatch failed.");
            return FAIL;
          }
        }
        res->push_back(i, b);
        break;
      }
      case LASTTS: {
        Batch* b;
        k_int32 pt_idx = last_pairs_[ts_col].first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = last_pairs_[ts_col].second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return FAIL;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      case LAST_ROW: {
        Batch* b;
        k_int32 pt_idx = last_row_pair_.first;
        //  Read the last_row_pairs_ result recorded during the traversal process.
        //  If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = last_row_pair_.second.second;
          timestamp64 last_row_ts = last_row_pair_.second.first;
          MMapPartitionTable* cur_pt = p_bts_[pt_idx];
          std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, cur_pt->GetPath().c_str());
            return FAIL;
          }
          if (segment_tbl->isNullValue(real_row, ts_col)) {
            std::shared_ptr<void> last_row_data(nullptr);
            b = new AggBatch(last_row_data, 1, nullptr);
          } else {
            int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
            if (err_code < 0) {
              LOG_ERROR("getActualColBatch failed.");
              return FAIL;
            }
          }
        }
        res->push_back(i, b);
        break;
      }
      case LASTROWTS: {
        Batch* b;
        k_int32 pt_idx = last_row_pair_.first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = last_row_pair_.second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return FAIL;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      default:
        break;
    }
  }
  res->entity_index = {entity_group_id_, cur_entity_id_, subgroup_id_};
  if (isAllAggResNull(res)) {
    *count = 0;
    res->clear();
  } else {
    *count = 1;
  }
  return SUCCESS;
}

KStatus TsAggIterator::findFirstLastData(ResultSet* res, k_uint32* count, timestamp64 ts) {
  KWDB_DURATION(StStatistics::Get().agg_first);
  *count = 0;
  k_uint32 count1, count2;
  if (findFirstData(res, &count1, ts) != KStatus::SUCCESS || findLastData(res, &count2, ts) != KStatus::SUCCESS) {
    return KStatus::FAIL;
  }
  *count = (count1 != 0 || count2 != 0) ? 1 : 0;
  return SUCCESS;
}

KStatus TsAggIterator::traverseAllBlocks(ResultSet* res, k_uint32* count, timestamp64 ts) {
  KWDB_DURATION(StStatistics::Get().agg_blocks);
  *count = 0;
  while (true) {
    if (!cur_block_item_) {
      if ( nextBlockItem(cur_entity_id_) < 0 ) {
        break;
      }
      continue;
    }
    BlockItem* cur_block = cur_block_item_;
    MMapPartitionTable* cur_pt = p_bts_[cur_p_bts_idx_];
    if (ts != INVALID_TS) {
      if (!is_reversed_ && cur_pt->minTimestamp() * 1000 > ts) {
        // 此时确定没有比ts更小的数据存在，直接返回-1，查询结束
        return SUCCESS;
      } else if (is_reversed_ && cur_pt->maxTimestamp() * 1000 < ts) {
        // 此时确定没有比ts更大的数据存在，直接返回-1，查询结束
        return SUCCESS;
      }
    }
    uint32_t first_row = 1;
    MetricRowID first_real_row = cur_block->getRowID(first_row);
    std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(cur_block->block_id);
    if (segment_tbl == nullptr) {
      LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                cur_block->block_id, cur_pt->GetPath().c_str());
      return KStatus::FAIL;
    }
    bool has_data = false;
    // Sequential read optimization, if the maximum and minimum timestamps of a BlockItem are within the ts_span range,
    // there is no need to determine the timestamps for each BlockItem.
    if (no_first_last_type_ && cur_block->is_agg_res_available
        && cur_block->publish_row_count > 0 && cur_blockdata_offset_ == 1
        && checkIfTsInSpan(KTimestamp(segment_tbl->columnAggAddr(first_real_row.block_id, 0, Sumfunctype::MAX)))
        && checkIfTsInSpan(KTimestamp(segment_tbl->columnAggAddr(first_real_row.block_id, 0, Sumfunctype::MIN)))) {
      k_uint32 cur_row = 1;
      while (cur_row <= cur_block->publish_row_count) {
        bool is_deleted;
        if (cur_block->isDeleted(cur_row, &is_deleted) < 0) {
          return KStatus::FAIL;
        }
        if (is_deleted) {
          break;
        }
        ++cur_row;
      }
      if (cur_row > cur_block->publish_row_count) {
        cur_blockdata_offset_ = cur_row;
        *count = cur_block->publish_row_count;
        first_row = 1;
        has_data = true;
      }
    }
    // If it is not achieved sequential reading optimization process,
    // the data under the BlockItem will be traversed one by one,
    // and the maximum number of consecutive data that meets the query conditions will be obtained.
    // The aggregation result of this continuous data will be further obtained in the future.
    while (cur_blockdata_offset_ <= cur_block->publish_row_count) {
      bool is_deleted;
      if (cur_block->isDeleted(cur_blockdata_offset_, &is_deleted) < 0) {
        return KStatus::FAIL;
      }
      // If the data in the cur_blockdata_offset_ row is not within the ts_span range or has been deleted,
      // continue to verify the data in the next row.
      MetricRowID real_row = cur_block->getRowID(cur_blockdata_offset_);
      timestamp64 cur_ts = KTimestamp(segment_tbl->columnAddr(real_row, 0));
      if (is_deleted || !checkIfTsInSpan(cur_ts)) {
        ++cur_blockdata_offset_;
        if (has_data) {
          break;
        }
        continue;
      }

      if (!has_data) {
        has_data = true;
        first_real_row = real_row;
        first_row = cur_blockdata_offset_;
      }
      // Continuously updating member variables that record first/last/first_row/last_row results during data traversal.
      updateFirstLastCols(cur_ts, real_row);
      ++(*count);
      ++cur_blockdata_offset_;
    }
    // If qualified data is obtained, further obtain the aggregation result of this continuous data
    // and package Batch to be added to res to return.
    // 1. If the data obtained is for the entire BlockItem and the aggregation results stored in the BlockItem are
    //    available and the query column has not undergone type conversion, then the aggregation results stored in
    //    the BlockItem can be directly obtained.
    //    The query for SUM type belongs to a special case:
    //    (1) If type overflow is identified, it needs to be recalculated.
    //    (2) If there is no type overflow, the read aggregation result needs to be converted to a type.
    // 2. If the above situation is not met, it is necessary to calculate the aggregation result
    //    and use the AggCalculator/VarColAggCalculator class to calculate the aggregation result.
    //    There is also a special case where the column being queried has undergone column type conversion,
    //    and the obtained continuous data needs to be first converted to the column type of the query
    //    through getActualColMem.
    if (has_data) {
      // Add all queried column data to the res result.
      for (k_uint32 i = 0; i < kw_scan_cols_.size(); ++i) {
        k_int32 ts_col = -1;
        if (i < ts_scan_cols_.size()) {
          ts_col = ts_scan_cols_[i];
        }
        if (ts_col < 0 ||
            !segment_tbl->hasValue(first_real_row, *count, ts_col) ||
            !colTypeHasAggResult((DATATYPE)attrs_[ts_col].type, scan_agg_types_[i])) {
          continue;
        }
        AttributeInfo actual_col = segment_tbl->GetActualCol(ts_col);
        Batch* b;
        if (*count < cur_block->publish_row_count || !cur_block->is_agg_res_available ||
            actual_col.type != attrs_[ts_col].type || actual_col.size != attrs_[ts_col].size) {
          switch (scan_agg_types_[i]) {
            case Sumfunctype::MAX: {
              void* mem = segment_tbl->columnAddr(first_real_row, ts_col);
              void* bitmap = segment_tbl->columnNullBitmapAddr(first_real_row.block_id, ts_col);
              if (actual_col.type != attrs_[ts_col].type || actual_col.size != attrs_[ts_col].size) {
                std::shared_ptr<void> new_mem;
                std::vector<std::shared_ptr<void>> new_var_mem;
                int error_code = getActualColMem(segment_tbl, first_row, ts_col, *count, &new_mem, new_var_mem);
                if (error_code < 0) {
                  return KStatus::FAIL;
                }
                if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
                  AggCalculator agg_cal(new_mem.get(), bitmap, first_row,
                                        DATATYPE(attrs_[ts_col].type), attrs_[ts_col].size, *count);
                  b = CreateAggBatch(agg_cal.GetMax(nullptr, true), nullptr);
                } else {
                  std::shared_ptr<void> max_base = nullptr;
                  for (auto var_mem : new_var_mem) {
                    VarColAggCalculator agg_cal(var_mem, 1);
                    max_base = agg_cal.GetMax(max_base);
                  }
                  b = CreateAggBatch(max_base, nullptr);
                }
              } else {
                if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
                  AggCalculator agg_cal(mem, bitmap, first_row,
                                        DATATYPE(attrs_[ts_col].type), attrs_[ts_col].size, *count);
                  b = CreateAggBatch(agg_cal.GetMax(), nullptr);
                } else {
                  // Skip the null first and last rows because varColumnAddr does not support obtaining data
                  // from empty first and last rows
                  MetricRowID start_row, end_row;
                  for (start_row = first_real_row; start_row < first_real_row + *count - 1; ++start_row) {
                    if (!segment_tbl->isNullValue(start_row, ts_col)) {
                      break;
                    }
                  }
                  for (end_row = first_real_row + *count - 1; end_row > first_real_row ; --end_row) {
                    if (!segment_tbl->isNullValue(end_row, ts_col)) {
                      break;
                    }
                  }
                  std::shared_ptr<void> var_mem =
                      segment_tbl->varColumnAddr(start_row, end_row, ts_col);
                  VarColAggCalculator agg_cal(mem, var_mem, bitmap, first_row, attrs_[ts_col].size, *count);
                  b = CreateAggBatch(agg_cal.GetMax(), nullptr);
                }
              }
              break;
            }
            case Sumfunctype::MIN: {
              void* mem = segment_tbl->columnAddr(first_real_row, ts_col);
              void* bitmap = segment_tbl->columnNullBitmapAddr(first_real_row.block_id, ts_col);
              if (actual_col.type != attrs_[ts_col].type || actual_col.size != attrs_[ts_col].size) {
                std::shared_ptr<void> new_mem;
                std::vector<std::shared_ptr<void>> new_var_mem;
                int error_code = getActualColMem(segment_tbl, first_row, ts_col, *count, &new_mem, new_var_mem);
                if (error_code < 0) {
                  return KStatus::FAIL;
                }
                if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
                  AggCalculator agg_cal(new_mem.get(), bitmap, first_row,
                                        DATATYPE(attrs_[ts_col].type), attrs_[ts_col].size, *count);
                  b = CreateAggBatch(agg_cal.GetMin(nullptr, true), nullptr);
                } else {
                  std::shared_ptr<void> min_base = nullptr;
                  for (auto var_mem : new_var_mem) {
                    VarColAggCalculator agg_cal(var_mem, 1);
                    min_base = agg_cal.GetMin(min_base);
                  }
                  b = CreateAggBatch(min_base, nullptr);
                }
              } else {
                if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
                  AggCalculator agg_cal(mem, bitmap, first_row,
                                        DATATYPE(attrs_[ts_col].type), attrs_[ts_col].size, *count);
                  b = CreateAggBatch(agg_cal.GetMin(), nullptr);
                } else {
                  // Skip the null first and last rows because varColumnAddr does not support obtaining data
                  // from empty first and last rows
                  MetricRowID start_row, end_row;
                  for (start_row = first_real_row; start_row < first_real_row + *count - 1; ++start_row) {
                    if (!segment_tbl->isNullValue(start_row, ts_col)) {
                      break;
                    }
                  }
                  for (end_row = first_real_row + *count - 1; end_row > first_real_row ; --end_row) {
                    if (!segment_tbl->isNullValue(end_row, ts_col)) {
                      break;
                    }
                  }
                  std::shared_ptr<void> var_mem =
                      segment_tbl->varColumnAddr(start_row, end_row, ts_col);
                  VarColAggCalculator agg_cal(mem, var_mem, bitmap, first_row, attrs_[ts_col].size, *count);
                  b = CreateAggBatch(agg_cal.GetMin(), nullptr);
                }
              }
              break;
            }
            case Sumfunctype::SUM: {
              AggCalculator agg_cal(segment_tbl->columnAddr(first_real_row, ts_col),
                                    segment_tbl->columnNullBitmapAddr(first_real_row.block_id, ts_col), first_row,
                                    DATATYPE(actual_col.type), actual_col.size, *count);
              void* sum;
              bool is_overflow = agg_cal.GetSum(&sum);
              b = CreateAggBatch(sum, nullptr);
              b->is_new = true;
              b->is_overflow = is_overflow;
              break;
            }
            case Sumfunctype::COUNT: {
              uint16_t notnull_count = 0;
              for (uint32_t j = 0; j < *count; ++j) {
                if (!segment_tbl->isNullValue(first_real_row + j, ts_col)) {
                  ++notnull_count;
                }
              }
              b = new AggBatch(malloc(BLOCK_AGG_COUNT_SIZE), 1, nullptr);
              *static_cast<uint16_t*>(b->mem) = notnull_count;
              b->is_new = true;
              break;
            }
            default:
              break;
          }
        } else {
          if (scan_agg_types_[i] == SUM && cur_block->is_overflow) {
            // If a type overflow is identified, the SUM result needs to be recalculated and cannot be read directly.
            AggCalculator agg_cal(segment_tbl->columnAddr(first_real_row, ts_col),
                                  segment_tbl->columnNullBitmapAddr(first_real_row.block_id, ts_col), first_row,
                                  DATATYPE(actual_col.type), actual_col.size, *count);
            void* sum;
            bool is_overflow = agg_cal.GetSum(&sum);
            b = CreateAggBatch(sum, nullptr);
            b->is_new = true;
            b->is_overflow = is_overflow;
          } else if (scan_agg_types_[i] == SUM) {
            // Convert the obtained SUM result to the type agreed upon with the execution layer.
            void* new_sum_base;
            void* sum_base = segment_tbl->columnAggAddr(first_real_row.block_id, ts_col, scan_agg_types_[i]);
            bool is_new = ChangeSumType(DATATYPE(attrs_[ts_col].type), sum_base, &new_sum_base);
            b = CreateAggBatch(new_sum_base, nullptr);
            b->is_new = is_new;
          } else {
            if ((attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) || scan_agg_types_[i] == COUNT) {
              b = CreateAggBatch(segment_tbl->columnAggAddr(first_real_row.block_id, ts_col,
                                                            scan_agg_types_[i]), segment_tbl);
            } else {
              std::shared_ptr<void> var_mem = segment_tbl->varColumnAggAddr(first_real_row, ts_col, scan_agg_types_[i]);
              b = CreateAggBatch(var_mem, segment_tbl);
            }
          }
        }
        res->push_back(i, b);
      }
      if (cur_blockdata_offset_ > cur_block->publish_row_count) {
        cur_blockdata_offset_ = 1;
        nextBlockItem(cur_entity_id_);
      }
      return SUCCESS;
    }
    if (cur_blockdata_offset_ > cur_block->publish_row_count) {
      cur_blockdata_offset_ = 1;
      nextBlockItem(cur_entity_id_);
    }
  }
  return SUCCESS;
}

int TsAggIterator::getActualColAggBatch(MMapPartitionTable* p_bt, MetricRowID real_row, uint32_t ts_col, Batch** b) {
  std::shared_ptr<MMapSegmentTable> segment_tbl = p_bt->getSegmentTable(real_row.block_id);
  if (segment_tbl == nullptr) {
    LOG_ERROR("Can not find segment use block [%d], in path [%s]", real_row.block_id, p_bt->GetPath().c_str());
    return KStatus::FAIL;
  }

  int32_t actual_col_type = segment_tbl->GetActualColType(ts_col);
  bool is_var_type = actual_col_type == VARSTRING || actual_col_type == VARBINARY;
  // Encapsulation Batch Result:
  // 1. If a column type conversion occurs, it is necessary to convert the data in the real_row
  //    and write the original data into the newly applied space
  // 2. If no column type conversion occurs, directly read the original data stored in the file
  if (actual_col_type != attrs_[ts_col].type) {
    void* old_mem = nullptr;
    std::shared_ptr<void> old_var_mem = nullptr;
    if (!is_var_type) {
      old_mem = segment_tbl->columnAddr(real_row, ts_col);
    } else {
      old_var_mem = segment_tbl->varColumnAddr(real_row, ts_col);
    }
    // table altered. column type changes.
    std::shared_ptr<void> new_mem;
    int err_code = p_bt->ConvertDataTypeToMem(static_cast<DATATYPE>(actual_col_type),
                                              static_cast<DATATYPE>(attrs_[ts_col].type),
                                              attrs_[ts_col].size, old_mem, old_var_mem, &new_mem);
    if (err_code < 0) {
      LOG_ERROR("failed ConvertDataType from %u to %u", actual_col_type, attrs_[ts_col].type);
      return FAIL;
    }
    *b = new AggBatch(new_mem, 1, segment_tbl);
  } else {
    if (!is_var_type) {
      *b = new AggBatch(segment_tbl->columnAddr(real_row, ts_col), 1, segment_tbl);
    } else {
      *b = new AggBatch(segment_tbl->varColumnAddr(real_row, ts_col), 1, segment_tbl);
    }
  }
  return 0;
}

// Convert the obtained continuous data into a query type and write it into the new application space.
int TsAggIterator::getActualColMem(std::shared_ptr<MMapSegmentTable> segment_tbl, size_t start_row,
                                   uint32_t ts_col, k_uint32 count, std::shared_ptr<void>* mem,
                                   std::vector<std::shared_ptr<void>>& var_mem) {
  ErrorInfo err_info;
  auto schema_info = segment_tbl->getSchemaInfo();
  void* bitmap = segment_tbl->getBlockHeader(cur_block_item_->block_id, ts_col);
  // There are two situations to handle:
  // 1. convert to fixed length types,  which can be further divided into:
  //    (1) other types to fixed length types
  //    (2) conversion between the same fixed length type but different lengths
  // 2. convert to variable length types
  if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
    if (schema_info[ts_col].type != attrs_[ts_col].type) {
      // Conversion from other types to fixed length types.
      char* value = static_cast<char*>(malloc(attrs_[ts_col].size * count));
      memset(value, 0, attrs_[ts_col].size * count);
      KStatus s = ConvertToFixedLen(segment_tbl, value, cur_block_item_,
                                    (DATATYPE)(schema_info[ts_col].type), (DATATYPE)(attrs_[ts_col].type),
                                    attrs_[ts_col].size, start_row, count, ts_col, bitmap);
      if (s != KStatus::SUCCESS) {
        free(value);
        return -1;
      }
      std::shared_ptr<void> ptr(value, free);
      *mem = ptr;
    } else if (schema_info[ts_col].size != attrs_[ts_col].size) {
      // Conversion between same fixed length type, but different lengths.
      char* value = static_cast<char*>(malloc(attrs_[ts_col].size * count));
      memset(value, 0, attrs_[ts_col].size * count);
      for (k_uint32 idx = start_row - 1; idx < count; ++idx) {
        memcpy(value + idx * attrs_[ts_col].size,
               segment_tbl->columnAddrByBlk(cur_block_item_->block_id, idx, ts_col), schema_info[ts_col].size);
      }
      std::shared_ptr<void> ptr(value, free);
      *mem = ptr;
    }
  } else {
    auto b = new VarColumnBatch(count, bitmap, start_row, segment_tbl);
    for (k_uint32 j = 0; j < count; ++j) {
      std::shared_ptr<void> data = nullptr;
      bool is_null;
      if (b->isNull(j, &is_null) != KStatus::SUCCESS) {
        delete b;
        b = nullptr;
        return -1;
      }
      if (is_null) {
        continue;
      }
      // Convert other types to variable length data types.
      data = ConvertVarLen(segment_tbl, cur_block_item_, static_cast<DATATYPE>(schema_info[ts_col].type),
                           static_cast<DATATYPE>(attrs_[ts_col].type), start_row + j - 1, ts_col);
      var_mem.push_back(data);
    }
    delete b;
  }
  return KStatus::SUCCESS;
}

KStatus TsAggIterator::Init(std::vector<MMapPartitionTable*>& p_bts, bool is_reversed) {
  TsIterator::Init(p_bts, is_reversed);
  only_first_type_ = onlyHasFirstAggType();
  only_last_type_ = onlyHasLastAggType();
  only_first_last_type_ = onlyHasFirstLastAggType();
  return SUCCESS;
}

KStatus TsAggIterator::Next(ResultSet* res, k_uint32* count, bool* is_finished, timestamp64 ts) {
  KWDB_DURATION(StStatistics::Get().agg_next);
  *count = 0;
  if (cur_entity_idx_ >= entity_ids_.size()) {
    *is_finished = true;
    return KStatus::SUCCESS;
  }
  if (p_bts_.empty()) {
    reset();
    return KStatus::SUCCESS;
  }
  cur_entity_id_ = entity_ids_[cur_entity_idx_];

  KStatus s;
  // If only queries related to first/last aggregation types are involved, the optimization process can be followed.
  if (only_first_type_ || only_last_type_ || only_first_last_type_) {
    if (only_first_type_) {
      s = findFirstData(res, count, ts);
    } else if (only_last_type_) {
      s = findLastData(res, count, ts);
    } else if (only_first_last_type_) {
      s = findFirstLastData(res, count, ts);
    }
    reset();
    return s;
  }

  ResultSet result{(k_uint32) kw_scan_cols_.size()};
  // Continuously calling the traceAllBlocks function to obtain
  // the intermediate aggregation result of all data for the current query entity.
  // When the count is 0, it indicates that the query is complete.
  // Further integration and calculation of the results in the variable result are needed in the future.
  do {
    s = traverseAllBlocks(&result, count, ts);
    if (s != KStatus::SUCCESS) {
      return KStatus::FAIL;
    }
  } while (*count != 0);

  if (result.empty() && no_first_last_type_) {
    reset();
    return KStatus::SUCCESS;
  }

  // By calling the AggCalculator/VarColAggCalculator function,
  // integrate the intermediate results to obtain the final aggregated query result of an entity.
  for (k_uint32 i = 0; i < kw_scan_cols_.size(); ++i) {
    k_int32 ts_col = -1;
    if (i < ts_scan_cols_.size()) {
      ts_col = ts_scan_cols_[i];
    }
    if (ts_col < 0) {
      LOG_ERROR("TsAggIterator::Next : no column : %d", kw_scan_cols_[i]);
      continue;
    }
    switch (scan_agg_types_[i]) {
      case Sumfunctype::MAX: {
        KWDB_DURATION(StStatistics::Get().agg_max);
        if (result.data[i].empty()) {
          Batch* b = CreateAggBatch(nullptr, nullptr);
          res->push_back(i, b);
        } else {
          if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
            bool need_to_new = false;
            void* max_base = nullptr;
            for (auto it : result.data[i]) {
              if (it->is_new) need_to_new = true;
              AggCalculator agg_cal(it->mem, DATATYPE(attrs_[ts_col].type), attrs_[ts_col].size, 1);
              max_base = agg_cal.GetMax(max_base);
            }
            if (need_to_new) {
              void* new_max_base = malloc(attrs_[ts_col].size);
              memcpy(new_max_base, max_base, attrs_[ts_col].size);
              max_base = new_max_base;
            }
            Batch* b = new AggBatch(max_base, 1, nullptr);
            b->is_new = need_to_new;
            res->push_back(i, b);
          } else {
            std::shared_ptr<void> max_base = nullptr;
            for (auto it : result.data[i]) {
              VarColAggCalculator agg_cal((reinterpret_cast<const AggBatch*>(it))->var_mem_, 1);
              max_base = agg_cal.GetMax(max_base);
            }
            Batch* b = new AggBatch(max_base, 1, nullptr);
            res->push_back(i, b);
          }
        }
        break;
      }
      case Sumfunctype::MIN: {
        KWDB_DURATION(StStatistics::Get().agg_min);
        if (result.data[i].empty()) {
          Batch* b = CreateAggBatch(nullptr, nullptr);
          res->push_back(i, b);
        } else {
          if (attrs_[ts_col].type != VARSTRING && attrs_[ts_col].type != VARBINARY) {
            bool need_to_new = false;
            void* min_base = nullptr;
            for (auto it : result.data[i]) {
              if (it->is_new) need_to_new = true;
              AggCalculator agg_cal(it->mem, DATATYPE(attrs_[ts_col].type), attrs_[ts_col].size, 1);
              min_base = agg_cal.GetMin(min_base);
            }
            if (need_to_new) {
              void* new_min_base = malloc(attrs_[ts_col].size);
              memcpy(new_min_base, min_base, attrs_[ts_col].size);
              min_base = new_min_base;
            }
            Batch* b = new AggBatch(min_base, 1, nullptr);
            b->is_new = need_to_new;
            res->push_back(i, b);
          } else {
            std::shared_ptr<void> min_base = nullptr;
            for (auto it : result.data[i]) {
              VarColAggCalculator agg_cal(reinterpret_cast<const AggBatch*>(it)->var_mem_, 1);
              min_base = agg_cal.GetMin(min_base);
            }
            Batch* b = new AggBatch(min_base, 1, nullptr);
            res->push_back(i, b);
          }
        }
        break;
      }
      case Sumfunctype::SUM: {
        KWDB_DURATION(StStatistics::Get().agg_sum);
        void *sum_base = nullptr;
        bool is_overflow = false;
        for (auto it : result.data[i]) {
          AggCalculator agg_cal(it->mem, getSumType(DATATYPE(attrs_[ts_col].type)),
                                getSumSize(DATATYPE(attrs_[ts_col].type)), 1, it->is_overflow);
          if (agg_cal.GetSum(&sum_base, sum_base, is_overflow)) {
            is_overflow = true;
          }
        }
        Batch* b = CreateAggBatch(sum_base, nullptr);
        b->is_new = true;
        b->is_overflow = is_overflow;
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::COUNT: {
        KWDB_DURATION(StStatistics::Get().agg_count);
        k_uint64 total_count = 0;
        for (auto it : result.data[i]) {
          total_count += *static_cast<k_uint16*>(it->mem);
        }
        auto* b = new AggBatch(malloc(sizeof(k_uint64)), 1, nullptr);
        b->is_new = true;
        *static_cast<k_uint64*>(b->mem) = total_count;
        b->is_new = true;
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::FIRST: {
        KWDB_DURATION(StStatistics::Get().agg_first);
        Batch* b;
        k_int32 pt_idx = first_pairs_[ts_col].first;
        // Read the first_pairs_ result recorded during the traversal process.
        // If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = first_pairs_[ts_col].second.second;
          timestamp64 first_ts = first_pairs_[ts_col].second.first;
          int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
          if (err_code < 0) {
            LOG_ERROR("getActualColBatch failed.");
            return FAIL;
          }
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::LAST: {
        KWDB_DURATION(StStatistics::Get().agg_last);
        Batch* b;
        k_int32 pt_idx = last_pairs_[ts_col].first;
        // Read the last_pairs_ result recorded during the traversal process.
        // If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = last_pairs_[ts_col].second.second;
          timestamp64 last_ts = last_pairs_[ts_col].second.first;
          int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
          if (err_code < 0) {
            LOG_ERROR("getActualColBatch failed.");
            return KStatus::FAIL;
          }
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::FIRST_ROW: {
        Batch* b;
        k_int32 pt_idx = first_row_pair_.first;
        // Read the first_row_pair_ result recorded during the traversal process.
        // If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = first_row_pair_.second.second;
          timestamp64 first_row_ts = first_row_pair_.second.first;
          MMapPartitionTable* cur_pt = p_bts_[pt_idx];
          std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, cur_pt->GetPath().c_str());
            return KStatus::FAIL;
          }
          if (segment_tbl->isNullValue(real_row, ts_col)) {
            std::shared_ptr<void> first_row_data(nullptr);
            b = new AggBatch(first_row_data, 1, nullptr);
          } else {
            int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
            if (err_code < 0) {
              LOG_ERROR("getActualColBatch failed.");
              return FAIL;
            }
          }
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::LAST_ROW: {
        KWDB_DURATION(StStatistics::Get().agg_lastrow);
        Batch* b;
        k_int32 pt_idx = last_row_pair_.first;
        // Read the last_row_pair_ result recorded during the traversal process.
        // If not found, return nullptr. Otherwise, obtain the data address based on the partition table index and row id.
        if (pt_idx < 0) {
          b = CreateAggBatch(nullptr, nullptr);
        } else {
          MetricRowID real_row = last_row_pair_.second.second;
          timestamp64 last_row_ts = last_row_pair_.second.first;
          MMapPartitionTable* cur_pt = p_bts_[pt_idx];

          std::shared_ptr<MMapSegmentTable> segment_tbl = cur_pt->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, cur_pt->GetPath().c_str());
            return KStatus::FAIL;
          }
          if (segment_tbl->isNullValue(real_row, ts_col)) {
            std::shared_ptr<void> last_row_data(nullptr);
            b = new AggBatch(last_row_data, 1, nullptr);
          } else {
            int err_code = getActualColAggBatch(p_bts_[pt_idx], real_row, ts_col, &b);
            if (err_code < 0) {
              LOG_ERROR("getActualColBatch failed.");
              return FAIL;
            }
          }
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::FIRSTTS: {
        KWDB_DURATION(StStatistics::Get().agg_firstts);
        Batch* b;
        k_int32 pt_idx = first_pairs_[ts_col].first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = first_pairs_[ts_col].second.second;

          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return KStatus::FAIL;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::LASTTS: {
        KWDB_DURATION(StStatistics::Get().agg_lastts);
        Batch* b;
        k_int32 pt_idx = last_pairs_[ts_col].first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = last_pairs_[ts_col].second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return KStatus::FAIL;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::FIRSTROWTS: {
        Batch* b;
        k_int32 pt_idx = first_row_pair_.first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = first_row_pair_.second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return KStatus::FAIL;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      case Sumfunctype::LASTROWTS: {
        Batch* b;
        k_int32 pt_idx = last_row_pair_.first;
        if (pt_idx < 0) {
          b = new AggBatch(nullptr, 0, nullptr);
        } else {
          MetricRowID real_row = last_row_pair_.second.second;
          std::shared_ptr<MMapSegmentTable> segment_tbl = p_bts_[pt_idx]->getSegmentTable(real_row.block_id);
          if (segment_tbl == nullptr) {
            LOG_ERROR("Can not find segment use block [%d], in path [%s]",
                      real_row.block_id, p_bts_[pt_idx]->GetPath().c_str());
            return KStatus::FAIL;
          }
          b = new AggBatch(segment_tbl->columnAddr(real_row, 0), 1, segment_tbl);
        }
        res->push_back(i, b);
        break;
      }
      default:
        break;
    }
  }
  res->entity_index = {entity_group_id_, cur_entity_id_, subgroup_id_};
  if (isAllAggResNull(res)) {
    *count = 0;
    res->clear();
  } else {
    *count = 1;
  }
  // An entity query has been completed and requires resetting the state variables in the iterator.
  reset();
  result.clear();
  return SUCCESS;
}

KStatus TsTableIterator::Next(ResultSet* res, k_uint32* count, timestamp64 ts) {
  *count = 0;
  MUTEX_LOCK(&latch_);
  Defer defer{[&]() { MUTEX_UNLOCK(&latch_); }};

  KStatus s;
  bool is_finished;
  do {
    is_finished = false;
    if (current_iter_ >= iterators_.size()) {
      break;
    }

    s = iterators_[current_iter_]->Next(res, count, &is_finished, ts);
    if (s == FAIL) {
      return s;
    }
    // When is_finished is true, it indicates that a TsIterator iterator query has ended and continues to read the next one.
    if (is_finished) current_iter_++;
  } while (is_finished);

  return KStatus::SUCCESS;
}

}  // namespace kwdbts