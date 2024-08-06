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

#include <dirent.h>
#include <iostream>
#if defined(__GNUC__) && (__GNUC__ < 8)
  #include <experimental/filesystem>
  namespace fs = std::experimental::filesystem;
#else
  #include <filesystem>
  namespace fs = std::filesystem;
#endif
#include "engine.h"
#include "mmap/MMapMetricsTable.h"
#include "BigObjectApplication.h"
#include "mmap/MMapTagColumnTable.h"
#include "mmap/MMapTagColumnTableAux.h"
#include "ts_snapshot.h"
#include "ts_table.h"
#include "perf_stat.h"
#include "sys_utils.h"
#include "lt_cond.h"
#include "ee_global.h"
#include "pgcode.h"

namespace kwdbts {

string IdToEntityBigTableUrl(const KTableKey& table_id) {
  return nameToEntityBigTableURL(std::to_string(table_id), s_bt() + "_1");
}

TsEntityGroup::TsEntityGroup(kwdbContext_p ctx, MMapMetricsTable*& root_table, const string& db_path,
                             const KTableKey& table_id, const RangeGroup& range, const string& tbl_sub_path) :
    root_bt_(root_table), db_path_(db_path), table_id_(table_id), range_(range), tbl_sub_path_(tbl_sub_path) {
  cur_subgroup_id_ = 1;
  mutex_ = new KLatch(LATCH_ID_TSENTITY_GROUP_MUTEX);
  drop_mutex_ = new KRWLatch(RWLATCH_ID_TS_ENTITY_GROUP_DROP_RWLOCK);
}

TsEntityGroup::~TsEntityGroup() {
  if (ebt_manager_ != nullptr) {
    delete ebt_manager_;
    ebt_manager_ = nullptr;
  }
  if (tag_bt_) {
    delete tag_bt_;
  }
  if (mutex_) {
    delete mutex_;
    mutex_ = nullptr;
  }
  if (drop_mutex_) {
    delete drop_mutex_;
    drop_mutex_ = nullptr;
  }
}

KStatus TsEntityGroup::Create(kwdbContext_p ctx, vector<TagInfo>& tag_schema) {
  if (ebt_manager_ != nullptr) {
    LOG_ERROR("TsTableRange already OpenInit.")
    return KStatus::FAIL;
  }
  ErrorInfo err_info;
  // TODO(jiadx): create individual Tag table under the range directory
  tag_bt_ = CreateTagTable(tag_schema, db_path_, tbl_sub_path_, table_id_, range_.range_group_id, TAG_TABLE, err_info);
  if (tag_bt_ == nullptr) {
    LOG_ERROR("TsTableRange create error : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }

  ebt_manager_ = new SubEntityGroupManager(root_bt_);
  ebt_manager_->OpenInit(db_path_, tbl_sub_path_, table_id_, err_info);
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::OpenInit(kwdbContext_p ctx) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  if (ebt_manager_ != nullptr) {
    LOG_ERROR("TsTableRange already OpenInit.")
    return KStatus::FAIL;
  }

  ErrorInfo err_info;
  // Open Tag table under the range directory
  tag_bt_ = OpenTagTable(db_path_, tbl_sub_path_, table_id_, err_info);
  if (err_info.errcode < 0 || tag_bt_ == nullptr) {
    LOG_ERROR("TsTableRange OpenInit error :%s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }

  ebt_manager_ = new SubEntityGroupManager(root_bt_);
  ebt_manager_->OpenInit(db_path_, tbl_sub_path_, table_id_, err_info);
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::PutEntity(kwdbContext_p ctx, TSSlice& payload, uint64_t mtr_id) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  KStatus status = SUCCESS;
  ErrorInfo err_info;
  Payload pd(root_bt_->getActualSchemaInfo(), payload);
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    return KStatus::FAIL;
  }
  // 1. query tag table
  TSSlice tmp_slice = pd.GetPrimaryTag();
  uint32_t entity_id, subgroup_id;
  if (tag_bt_->getEntityIdGroupId(tmp_slice.data, tmp_slice.len, entity_id, subgroup_id) == 0) {
    // update
    if (tag_bt_->UpdateTagRecord(pd, subgroup_id, entity_id, err_info) < 0) {
      LOG_ERROR("Update tag record failed. error: %s ", err_info.errmsg.c_str());
      releaseTagTable();
      return KStatus::FAIL;
    }
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::PutData(kwdbContext_p ctx, TSSlice* payloads, int length, uint64_t mtr_id,
                               DedupResult* dedup_result, DedupRule dedup_rule) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  for (int i = 0; i < length; i++) {
    // Based on the number of payloads, call PutData repeatedly to write data
    KStatus s = PutData(ctx, payloads[i], 0, dedup_result, dedup_rule);
    if (s == FAIL) return s;
  }
  return SUCCESS;
}

KStatus TsEntityGroup::PutData(kwdbContext_p ctx, TSSlice payload) {
  DedupResult dedup_result{0, 0, 0, TSSlice {nullptr, 0}};
  return PutData(ctx, payload, 0, &dedup_result);
}

KStatus TsEntityGroup::PutData(kwdbContext_p ctx, TSSlice payload, TS_LSN mini_trans_id,
                               DedupResult* dedup_result, DedupRule dedup_rule) {
  // If wal is not enabled, this function will be used.
  KStatus status = SUCCESS;
  uint32_t group_id, entity_id;
  ErrorInfo err_info;
  group_id = 1;
  entity_id = 1;
  // Create a Payload object pd to prepare for subsequent writing.
  // The payload object includes both the tag and the payload data.
  // The data to be written can be retrieved using GetColumnAddr() and GetVarColumnAddr() methods.
  Payload pd(root_bt_->getActualSchemaInfo(), payload);
  pd.dedup_rule_ = dedup_rule;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    return KStatus::FAIL;
  }
  // check if lsn is set, in wal-off mode, lsn will not set, we should set lsn to 1 for marking lsn exist.
  TS_LSN pl_lsn;
  if (pd.GetLsn(pl_lsn)) {
    if (pl_lsn == 0) {
      pd.SetLsn(1);
    }
  }

  // Query or assign EntityGroupID and EntityID based on the provided payload.
  // Initially, attempt to retrieve the IDs directly from the tag table.
  // If it does not exist, allocate and insert it into the tag table.
  if (KStatus::SUCCESS != allocateEntityGroupId(ctx, pd, &entity_id, &group_id)) {
    LOG_ERROR("allocateEntityGroupId failed, entity id: %d, group id: %d.", entity_id, group_id);
    releaseTagTable();
    return KStatus::FAIL;
  }
  if (pd.GetFlag() == Payload::TAG_ONLY) {
    // Only when a tag is present, do not write data
    releaseTagTable();
    return KStatus::SUCCESS;
  }
  releaseTagTable();
  status = putDataColumnar(ctx, group_id, entity_id, pd, dedup_result);
  return status;
}

KStatus TsEntityGroup::putTagData(kwdbContext_p ctx, int32_t groupid, int32_t entity_id, Payload& payload) {
  KWDB_DURATION(StStatistics::Get().put_tag);
  ErrorInfo err_info;
  // 1. Write tag data
  uint8_t payload_data_flag = payload.GetFlag();
  if (payload_data_flag == Payload::DATA_AND_TAG || payload_data_flag == Payload::TAG_ONLY) {
    // tag
    err_info.errcode = tag_bt_->insert(entity_id, groupid, payload.GetTagAddr());
  }
  if (err_info.errcode < 0) {
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::allocateEntityGroupId(kwdbContext_p ctx, Payload& payload, uint32_t* entity_id, uint32_t* group_id) {
  KWDB_DURATION(StStatistics::Get().alloc_entity);
  ErrorInfo err_info;
  // Attempting to retrieve the group ID and entity ID from the tag table
  // 1.If found, assign a value and return
  // 2.If not found, attempt to lock and query again to prevent concurrent scenarios where the first query fails and
  // the entity is created successfully.
  // 3.If the second search fails, it indicates that no other thread has created the entity. Therefore, use the
  // consistent hashID algorithm to allocate a new ID and write it to the tag table.
  TSSlice tmp_slice = payload.GetPrimaryTag();
  uint32_t entityid, groupid;
  if (tag_bt_->getEntityIdGroupId(tmp_slice.data, tmp_slice.len, entityid, groupid) == 0) {
    *entity_id = entityid;
    *group_id = groupid;
    return KStatus::SUCCESS;
  }
  {
    // Locking, concurrency control, and the putTagData operation must not be executed repeatedly
    MUTEX_LOCK(mutex_);
    if (tag_bt_->getEntityIdGroupId(tmp_slice.data, tmp_slice.len, entityid, groupid) == 0) {
      *entity_id = entityid;
      *group_id = groupid;
      MUTEX_UNLOCK(mutex_);
      return KStatus::SUCCESS;
    }
    // not found
    std::string tmp_str = std::to_string(table_id_);
    uint64_t tag_hash = TsTable::GetConsistentHashId(tmp_str.data(), tmp_str.size());
    std::string primary_tags;
    err_info.errcode = ebt_manager_->AllocateEntity(primary_tags, tag_hash, &groupid, &entityid);
    if (err_info.errcode < 0) {
      MUTEX_UNLOCK(mutex_);
      return KStatus::FAIL;
    }
    // insert tag table
    if (KStatus::SUCCESS != putTagData(ctx, groupid, entityid, payload)) {
      MUTEX_UNLOCK(mutex_);
      return KStatus::FAIL;
    }
    MUTEX_UNLOCK(mutex_);
  }
  *entity_id = entityid;
  *group_id = groupid;
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::putDataColumnar(kwdbContext_p ctx, int32_t group_id, int32_t entity_id,
                                       Payload& payload, DedupResult* dedup_result) {
  KWDB_DURATION(StStatistics::Get().put_data);
  KWDB_STAT_ADD(StStatistics::Get().payload_row, payload.GetRowCount());
  ErrorInfo err_info;

  // only used for import data, other scenario will not use discard mode.
  // When importing data, it is necessary to allocate space for bitmap to record discarded rows.
  if (payload.dedup_rule_ == DedupRule::DISCARD
      && dedup_result->payload_num == 1
      && dedup_result->discard_bitmap.data == nullptr) {
    dedup_result->discard_bitmap.len = (payload.GetRowCount() + 7) / 8;
    dedup_result->discard_bitmap.data = reinterpret_cast<char*>(malloc(dedup_result->discard_bitmap.len));
    memset(dedup_result->discard_bitmap.data, 0, dedup_result->discard_bitmap.len);
  }

  // Write data column by column, employing a columnar storage format. Each data column is accompanied by an
  // independent mmap file,
  // necessitating the updating of multiple column files during data writes.
  // After parsing the INSERT statement, the execution module constructs a payload that is stored in a columnar
  // format in memory, which is identical to the physical storage structure.
  // To enhance data writing performance, batch data writing can utilize memory copying methods directly,
  // based on the data column.
  timestamp64 last_p_time = INVALID_TS;
  MMapPartitionTable* p_bt = nullptr;
  int batch_start = payload.GetStartRowId();
  timestamp64 p_time = INVALID_TS;

  bool all_success = true;
  std::vector<BlockSpan> alloc_spans;
  TsSubEntityGroup* sub_group = ebt_manager_->GetSubGroup(group_id, err_info, false);
  if (err_info.errcode < 0) {
    LOG_ERROR("push_back_payload error: there is no subgroup: %d", group_id);
    return KStatus::FAIL;
  }

  unordered_map<MMapPartitionTable*, PutAfterProcessInfo*> after_process_info;
  bool payload_dup = false;
  std::unordered_set<timestamp64> dup_set;

  // Examine the timestamp of the first piece of data within the payload to verify the partition being written to
  KTimestamp first_ts_ms = payload.GetTimestamp(payload.GetStartRowId());
  KTimestamp first_ts = first_ts_ms / 1000;
  timestamp64 first_max_ts;

  last_p_time = sub_group->PartitionTime(first_ts, first_max_ts);
  k_int32 row_id = 0;

  // Based on the obtained partition timestamp, determine whether the current payload needs to switch partitions.
  // If necessary, first call push_back_payload to write the current partition data.
  // Then continue to call payloadNextSlice until the traversal of the Payload data is complete.
  while (payloadNextSlice(sub_group, payload, last_p_time, batch_start, &row_id, &p_time)) {
    p_bt = ebt_manager_->GetPartitionTable(last_p_time, group_id, err_info, true);
    if (err_info.errcode < 0) {
      LOG_ERROR("GetPartitionTable error: %s", err_info.errmsg.c_str());
      all_success = false;
      break;
    }
    if (!p_bt->isValid()) {
      LOG_WARN("Partition is invalid.");
      err_info.setError(DEDUPREJECT, "Partition is invalid.");
      continue;
    }
    last_p_time = p_time;

    // Update the minimum and maximum timestamps, and record the maximum and minimum partition times.
    if (root_bt_->minTimestamp() == 0 || root_bt_->minTimestamp() > p_time) {
      root_bt_->minTimestamp() = p_time;
    }
    if (root_bt_->maxTimestamp() < p_time) {
      root_bt_->maxTimestamp() = p_time;
    }
    std::vector<BlockSpan> cur_alloc_spans;
    std::vector<MetricRowID> to_deleted_real_rows;

    // push_back_payload is used to allocate space, write data to the partition table, and perform data deduplication
    // and aggregation during this process.
    // The returned cur_alloc_spans contains records of all allocated blocks
    // and the number of rows written to each block.
    // If there is a failure, putAfterProcess will remove the spatial marker for cur_alloc_spans.
    // All rows that need to be deleted (deduplicated) are recorded in the returned to_deleted_real_rows,
    // and recordPutAfterProInfo will process them.
    err_info.errcode = p_bt->push_back_payload(ctx, entity_id, &payload, batch_start, row_id - batch_start,
                                               &cur_alloc_spans, &to_deleted_real_rows, err_info, dedup_result);

    // After handling a partition, record the information to be processed subsequently
    recordPutAfterProInfo(after_process_info, p_bt, cur_alloc_spans, to_deleted_real_rows);

    if (err_info.errcode < 0) {
      LOG_ERROR("push_back_payload error: %s", err_info.errmsg.c_str());
      all_success = false;
      break;
    }
    batch_start = row_id;
  }

  // Writing completed, processing after_process_info recorded earlier
  putAfterProcess(after_process_info, entity_id, all_success);

  if (!all_success) {
    if (err_info.errcode == DEDUPREJECT) {
      return KStatus::SUCCESS;
    }
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

void TsEntityGroup::recordPutAfterProInfo(unordered_map<MMapPartitionTable*, PutAfterProcessInfo*>& after_process_info,
                                          MMapPartitionTable* p_bt, std::vector<BlockSpan>& cur_alloc_spans,
                                          std::vector<MetricRowID>& to_deleted_real_rows) {
  if (after_process_info.find(p_bt) != after_process_info.end()) {
    ebt_manager_->ReleasePartitionTable(p_bt);
  } else {
    after_process_info[p_bt] = new PutAfterProcessInfo();
  }

  // Update the partition data
  after_process_info[p_bt]->spans.insert(after_process_info[p_bt]->spans.end(),
                                         cur_alloc_spans.begin(), cur_alloc_spans.end());
  after_process_info[p_bt]->del_real_rows.insert(after_process_info[p_bt]->del_real_rows.end(),
                                                 to_deleted_real_rows.begin(), to_deleted_real_rows.end());
}

void TsEntityGroup::putAfterProcess(unordered_map<MMapPartitionTable*, PutAfterProcessInfo*>& after_process_info,
                                    uint32_t entity_id, bool all_success) {
  for (auto iter : after_process_info) {
    iter.first->publish_payload_space((iter.second)->spans, (iter.second)->del_real_rows, entity_id, all_success);
    ebt_manager_->ReleasePartitionTable(iter.first);
    delete iter.second;
  }

  after_process_info.clear();
}

KStatus TsEntityGroup::DeleteRangeData(kwdbContext_p ctx, const HashIdSpan& hash_span, TS_LSN lsn,
                                       const std::vector<KwTsSpan>& ts_spans, vector<DelRowSpans>* del_rows,
                                       uint64_t* count, uint64_t mtr_id, bool evaluate_del) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  // Based on the hash ID range, find all the rows of tag data that need to be deleted.
  vector<string> primary_tags;
  tag_bt_->startRead();
  for (size_t row = 1; row <= tag_bt_->size(); row++) {
    if (!tag_bt_->isValidRow(row)) {
      continue;
    }
    string primary_tag(reinterpret_cast<char*>(tag_bt_->record(row)), tag_bt_->primaryTagSize());
    uint64_t hash_id = TsTable::GetConsistentHashId(primary_tag.data(), primary_tag.size());
    if (hash_id >= hash_span.begin && hash_id <= hash_span.end) {
      primary_tags.emplace_back(primary_tag);
    }
  }
  tag_bt_->stopRead();
  *count = 0;
  for (const auto& p_tags : primary_tags) {
    // Delete the data corresponding to the tag within the time range
    uint64_t entity_del_count = 0;
    DelRowSpans del_row_spans{p_tags};
    KStatus status = DeleteData(ctx, p_tags, lsn, ts_spans, &del_row_spans.spans, &entity_del_count, mtr_id, evaluate_del);
    if (status == KStatus::FAIL) {
      LOG_ERROR("DeleteRangeData failed, delete entity by primary key %s failed", p_tags.c_str());
      return KStatus::FAIL;
    }
    // Update count
    *count += entity_del_count;
    if (!del_row_spans.spans.empty() && evaluate_del) {
      del_rows->push_back(del_row_spans);
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::DeleteData(kwdbContext_p ctx, const string& primary_tag, TS_LSN lsn,
                                  const std::vector<KwTsSpan>& ts_spans, vector<DelRowSpan>* rows,
                                  uint64_t* count, uint64_t mtr_id, bool evaluate_del) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  *count = 0;
  uint32_t entity_id = 0;
  uint32_t subgroup_id = 0;
  // Get subgroup id and entity id based on the primary tag.
  int ret = tag_bt_->getEntityIdGroupId(const_cast<char*>(primary_tag.c_str()),
                                        primary_tag.size(), entity_id, subgroup_id);
  if (ret < 0) {
    LOG_WARN("entity not exists, primary_tag: %s", primary_tag.c_str());
    return KStatus::SUCCESS;
  }
  timestamp64 min_ts = INT64_MAX, max_ts = INT64_MIN;
  for (auto span : ts_spans) {
    min_ts = (min_ts > span.begin) ? span.begin : min_ts;
    max_ts = (max_ts < span.end) ? span.end : max_ts;
  }
  ErrorInfo err_info;
  vector<MMapPartitionTable*> p_tables = ebt_manager_->GetPartitionTables({min_ts/1000, max_ts/1000}, subgroup_id, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("GetPartitionTable error : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  bool del_failed = false;
  for (MMapPartitionTable* p_bt : p_tables) {
    if (del_failed) {
      ebt_manager_->ReleasePartitionTable(p_bt);
      continue;
    }
    vector<DelRowSpan> delete_rows;
    uint64_t cnt = 0;
    int res = 0;
    if (evaluate_del) {
      res = p_bt->DeleteData(entity_id, 0, ts_spans, &delete_rows, &cnt, err_info, evaluate_del);
    } else {
      res = p_bt->DeleteData(entity_id, 0, ts_spans, nullptr, &cnt, err_info, evaluate_del);
    }
    if (res < 0) {
      ebt_manager_->ReleasePartitionTable(p_bt);
      del_failed = true;
    }
    if (rows && evaluate_del) {
      for (auto& row_span : delete_rows) {
        row_span.partition_ts = p_bt->minTimestamp();
        rows->emplace_back(row_span);
      }
    }
    (*count) += cnt;
    ebt_manager_->ReleasePartitionTable(p_bt);
  }
  if (del_failed) {
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::DeleteExpiredData(kwdbContext_p ctx, int64_t end_ts) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  int ret = ebt_manager_->DeleteExpiredData(end_ts, err_info);
  if (ret < 0) {
    LOG_ERROR("SubGroup delete expired data failed, err: %s.", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::DeleteEntity(kwdbContext_p ctx, const string& primary_tag, uint64_t* del_cnt, uint64_t mtr_id) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  uint32_t sub_group_id, entity_id;
  int ret = tag_bt_->getEntityIdGroupId(primary_tag.data(), primary_tag.size(), entity_id, sub_group_id);
  if (ret < 0) {
    return KStatus::FAIL;
  }
  ErrorInfo err_info;
  // Delete tag and its index
  tag_bt_->DeleteTagRecord(primary_tag.data(), primary_tag.size(), err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("delete_tag_record error, error msg: %s", err_info.errmsg.c_str())
    return KStatus::FAIL;
  }
  // Delete entities
  TsSubEntityGroup* p_bt = ebt_manager_->GetSubGroup(sub_group_id, err_info, false);
  if (p_bt) {
    p_bt->DeleteEntity(entity_id, 0, del_cnt, err_info);
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::DeleteEntities(kwdbContext_p ctx, const std::vector<std::string>& primary_tags,
                                      uint64_t* count, uint64_t mtr_id) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  *count = 0;
  for (const auto& p_tags : primary_tags) {
    uint64_t num = 0;
    KStatus status = DeleteEntity(ctx, p_tags, &num, mtr_id);
    if (status == KStatus::FAIL) {
      LOG_ERROR("Failed to delete entity by primary key %s", p_tags.c_str())
      return KStatus::FAIL;
    }
    *count += num;
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::DeleteRangeEntities(kwdbContext_p ctx, const HashIdSpan& hash_span,
                                           uint64_t* count, uint64_t mtr_id) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  // 1. First, query the primary tags of each record in the tag_bt.
  // 2. Compute the hash ID of the primary tags and check if it falls within the range HashIdSpan.
  //    If it does, proceed with the following steps.
  // 3. Invoke the `DeleteEntities` based on the primary tags to delete the entities.
  // 4. Remove the corresponding data and indexes from tag_bt.
  vector<string> primary_tags;
  tag_bt_->startRead();
  for (size_t row = 1; row <= tag_bt_->size(); row++) {
    if (!tag_bt_->isValidRow(row)) {
      continue;
    }
    string primary_tag(reinterpret_cast<char*>(tag_bt_->record(row)), tag_bt_->primaryTagSize());
    uint64_t hash_id = TsTable::GetConsistentHashId(primary_tag.data(), primary_tag.size());
    if (hash_id >= hash_span.begin && hash_id <= hash_span.end) {
      primary_tags.emplace_back(primary_tag);
    }
  }
  tag_bt_->stopRead();
  if (DeleteEntities(ctx, primary_tags, count, mtr_id) == KStatus::FAIL) {
    LOG_ERROR("delete entities error")
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::GetEntityIdList(kwdbContext_p ctx, const std::vector<void*>& primary_tags,
                                       const std::vector<uint32_t>& scan_tags,
                                       std::vector<EntityResultIndex>* entity_id_list, ResultSet* res, uint32_t* count) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    return KStatus::FAIL;
  }
  if (tag_bt_->GetEntityIdList(primary_tags, scan_tags, entity_id_list, res, count) < 0) {
    LOG_ERROR("GetEntityIdList error ");
    status = KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::Drop(kwdbContext_p ctx, bool is_force) {
  RW_LATCH_X_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  ebt_manager_->DropAll(is_force, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("TsTable::Drop error : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }

  // tag table
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable fail. error: %s ", err_info.errmsg.c_str());
    return KStatus::SUCCESS;
  }
  DropTagTable(tag_bt_, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("dropTagTable : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  delete tag_bt_;
  tag_bt_ = nullptr;
  // delete directory of entity_group
  string group_path = db_path_ + tbl_sub_path_;
  LOG_INFO("remove group %s", group_path.c_str());
  fs::remove_all(group_path.c_str());
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::Compress(kwdbContext_p ctx, const KTimestamp& ts) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  ebt_manager_->Compress(ts, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("TsEntityGroup::Compress error : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::GetIterator(kwdbContext_p ctx, SubGroupID sub_group_id, vector<uint32_t> entity_ids,
                                   std::vector<KwTsSpan> ts_spans, std::vector<k_uint32> scan_cols,
                                   std::vector<k_uint32> ts_scan_cols,
                                   std::vector<Sumfunctype> scan_agg_types, TsIterator** iter,
                                   std::shared_ptr<TsEntityGroup> entity_group, bool reverse) {
  // TODO(liuwei) update to use read_lsn to fetch Metrics data optimistically.
  // if the read_lsn is 0, ignore the read lsn checking and return all data (it's no WAL support case).
  // TS_LSN read_lsn = GetOptimisticReadLsn();
  TsIterator* ts_iter = nullptr;
  if (scan_agg_types.empty()) {
    ts_iter = new TsRawDataIterator(entity_group, range_.range_group_id, sub_group_id,
                                    entity_ids, ts_spans, scan_cols, ts_scan_cols);
  } else {
    ts_iter = new TsAggIterator(entity_group, range_.range_group_id, sub_group_id, entity_ids,
                                ts_spans, scan_cols, ts_scan_cols, scan_agg_types);
  }

  ErrorInfo err_info;
  timestamp64 min_ts = INT64_MAX, max_ts = INT64_MIN;
  getMaxAndMinTs(ts_spans, &min_ts, &max_ts);
  KwTsSpan p_span{min_ts / 1000, max_ts / 1000};
  std::vector<MMapPartitionTable*> p_bts = ebt_manager_->GetPartitionTables(p_span, sub_group_id, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("GetPartitionTables error : %s", err_info.errmsg.c_str());
    delete ts_iter;
    ts_iter = nullptr;
    return KStatus::FAIL;
  }
  for (auto it = p_bts.begin(); it != p_bts.end(); ) {
    bool has_data = false;

    if (!(*it)->DeleteFlag()) {
      for (auto id : entity_ids) {
        EntityItem* entity_item = (*it)->getEntityItem(id);
        if (entity_item->row_written > 0) {
          has_data = true;
          break;
        }
      }
    }

    if (has_data) {
      ++it;
    } else {
      ebt_manager_->ReleasePartitionTable(*it);
      it = p_bts.erase(it);
    }
  }
  ts_iter->Init(p_bts, reverse);
  *iter = ts_iter;
  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::GetTagIterator(kwdbContext_p ctx, std::vector<k_uint32>& scan_tags, EntityGroupTagIterator** iter) {
  ErrorInfo err_info;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable failed. error: %s ", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  EntityGroupTagIterator* entity_group_iter = new EntityGroupTagIterator(tag_bt_, scan_tags);
  *iter = entity_group_iter;
  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::GetMetaIterator(kwdbContext_p ctx, EntityGroupMetaIterator** iter) {
  EntityGroupMetaIterator* meta_iter = new EntityGroupMetaIterator(range_.range_group_id, ebt_manager_);
  if (KStatus::SUCCESS != meta_iter->Init()) {
    delete meta_iter;
    meta_iter = nullptr;
    *iter = nullptr;
    return KStatus::FAIL;
  }
  *iter = meta_iter;
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::TSxClean(kwdbContext_p ctx) {
  CleanTagFiles(db_path_ + tbl_sub_path_, table_id_, -1);
  return KStatus::SUCCESS;
}

KStatus TsEntityGroup::AlterTagInfo(kwdbContext_p ctx, TagInfo& new_tag_schema, ErrorInfo& err_info) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  // 1.get bigtable
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    return KStatus::FAIL;
  }
  // 2. construct old_tag_schema
  TagInfo old_tag_schema;
  bool is_found = false;
  for (const auto it : tag_bt_->getSchemaInfo()) {
    if (it->attributeInfo().m_id == new_tag_schema.m_id) {
      old_tag_schema = it->attributeInfo();
      is_found = true;
      break;
    }
  }
  if (!is_found) {
    LOG_ERROR("tag id: %u was no found", new_tag_schema.m_id);
    releaseTagTable();
    return KStatus::FAIL;
  }
  // 3. alter type
  if (tag_bt_->AlterTagType(old_tag_schema, new_tag_schema, err_info) < 0) {
    LOG_ERROR("AlterTagType failed. error: %s ", err_info.errmsg.c_str());
    releaseTagTable();
    return KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::AddTagInfo(kwdbContext_p ctx, TagInfo& new_tag_schema, ErrorInfo& err_info) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    releaseTagTable();
    return KStatus::FAIL;
  }
  if (tag_bt_->AddTagColumn(new_tag_schema, err_info) < 0) {
    LOG_ERROR("AddTagColumn failed. error: %s ", err_info.errmsg.c_str());
    releaseTagTable();
    return KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::DropTagInfo(kwdbContext_p ctx, TagInfo& tag_schema, ErrorInfo& err_info) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    return KStatus::FAIL;
  }
  if (tag_bt_->DropTagColumn(tag_schema, err_info) < 0) {
    LOG_ERROR("DropTagColumn failed. error: %s ", err_info.errmsg.c_str());
    releaseTagTable();
    return KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::UndoAddTagInfo(kwdbContext_p ctx, TagInfo& tag_schema) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    return KStatus::FAIL;
  }
  TagInfo old{};
  if (tag_bt_->AlterTableForUndo(0, 0, old, tag_schema, 3) < 0) {
    LOG_ERROR("AlterTableForUndo failed. error: %s ", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::UndoDropTagInfo(kwdbContext_p ctx, TagInfo& tag_schema) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    return KStatus::FAIL;
  }
  TagInfo info{};
  if (tag_bt_->AlterTableForUndo(0, 0, tag_schema, info, 4) < 0) {
    LOG_ERROR("AlterTableForUndo failed. error: %s ", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::UndoAlterTagInfo(kwdbContext_p ctx, TagInfo& origin_tag_schema) {
  ErrorInfo err_info;
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ");
    return KStatus::FAIL;
  }
  TagInfo info{};
  info.m_id = origin_tag_schema.m_id;
  if (tag_bt_->AlterTableForUndo(0, 0, origin_tag_schema, info, 2) < 0) {
    LOG_ERROR("AlterTableForUndo failed. error: %s ", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::RedoAddTagInfo(kwdbContext_p ctx, TagInfo& tag_schema) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error ")
    return status;
  }
  TagInfo old{};
  if (tag_bt_->AlterTableForRedo(0, 0, old, tag_schema, 3) < 0) {
    LOG_ERROR("AlterTableForUndo failed. error: %s ", err_info.errmsg.c_str())
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::RedoDropTagInfo(kwdbContext_p ctx, TagInfo& tag_schema) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  ErrorInfo err_info;
  KStatus status = KStatus::SUCCESS;
  if (getTagTable(err_info) != KStatus::SUCCESS) {
    LOG_ERROR("getTagTable error")
    return status;
  }
  TagInfo info{};
  if (tag_bt_->AlterTableForRedo(0, 0, tag_schema, info, 4) < 0) {
    LOG_ERROR("AlterTableForUndo failed. error: %s ", err_info.errmsg.c_str())
  }
  releaseTagTable();
  return status;
}

KStatus TsEntityGroup::AlterTable(kwdbContext_p ctx, AlterType alter_type, AttributeInfo& attr_info) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  auto ebt_manager = GetSubEntityGroupManager();
  auto max_subgroup_id = ebt_manager->GetMaxSubGroupId();
  ErrorInfo err_info;
  timestamp64 elapsed_sec;

  timestamp64 min_ts = root_bt_->minTimestamp();
  timestamp64 max_ts = root_bt_->maxTimestamp();
  if (min_ts == 0) {
    return KStatus::SUCCESS;
  }

  if (ebt_manager_->AlterSubGroupColumn(attr_info, err_info) < 0) {
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::CheckAlterValid(kwdbContext_p ctx, const std::map<uint32_t, std::vector<uint32_t>>& entities,
                               int col_index, DATATYPE new_type, bool& is_valid, ErrorInfo& err_info) {
  RW_LATCH_S_LOCK(drop_mutex_);
  Defer drop_defer{[&]() { RW_LATCH_UNLOCK(drop_mutex_); }};
  KwTsSpan span{INT64_MIN, INT64_MAX};
  // Iteratively check whether the column types of all subgroups' entity ids in the current entity group can be modified
  for (auto& subgroup_entities : entities) {
    std::vector<MMapPartitionTable*> p_bts = ebt_manager_->GetPartitionTables(span, subgroup_entities.first, err_info);
    Defer defer{[&]() { for (auto& p_bt : p_bts) { ebt_manager_->ReleasePartitionTable(p_bt); } }};
    for (auto& p_bt : p_bts) {
      if (p_bt->CheckAlterValid(subgroup_entities.second, col_index, new_type, is_valid, err_info) < 0) {
        return KStatus::FAIL;
      }
    }
  }
  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::GetColAttributeInfo(kwdbContext_p ctx, const roachpb::KWDBKTSColumn& col,
                                   struct AttributeInfo& attr_info, bool first_col) {
  switch (col.storage_type()) {
    case roachpb::TIMESTAMP:
    case roachpb::TIMESTAMPTZ:
    case roachpb::DATE:
      if (first_col) {
        attr_info.type = DATATYPE::TIMESTAMP64_LSN;
      } else {
        attr_info.type = DATATYPE::TIMESTAMP64;
      }
      attr_info.max_len = 3;
      break;
    case roachpb::SMALLINT:
      attr_info.type = DATATYPE::INT16;
      break;
    case roachpb::INT:
      attr_info.type = DATATYPE::INT32;
      break;
    case roachpb::BIGINT:
      attr_info.type = DATATYPE::INT64;
      break;
    case roachpb::FLOAT:
      attr_info.type = DATATYPE::FLOAT;
      break;
    case roachpb::DOUBLE:
      attr_info.type = DATATYPE::DOUBLE;
      break;
    case roachpb::BOOL:
      attr_info.type = DATATYPE::BYTE;
      break;
    case roachpb::CHAR:
      attr_info.type = DATATYPE::CHAR;
      attr_info.max_len = col.storage_len();
      break;
    case roachpb::BINARY:
    case roachpb::NCHAR:
      attr_info.type = DATATYPE::BINARY;
      attr_info.max_len = col.storage_len();
      break;
    case roachpb::VARCHAR:
      attr_info.type = DATATYPE::VARSTRING;
      attr_info.max_len = col.storage_len() - 1;  // because varchar len will +1 when store
      break;
    case roachpb::NVARCHAR:
    case roachpb::VARBINARY:
      attr_info.type = DATATYPE::VARBINARY;
      attr_info.max_len = col.storage_len();
      break;
    case roachpb::SDECHAR:
    case roachpb::SDEVARCHAR:
      attr_info.type = DATATYPE::STRING;
      attr_info.max_len = col.storage_len();
      break;
    default:
      LOG_ERROR("convert roachpb::KWDBKTSColumn to AttributeInfo failed: unknown column type[%d]", col.storage_type());
      return KStatus::FAIL;
  }

  attr_info.size = getDataTypeSize(attr_info);
  attr_info.id = col.column_id();
  attr_info.name = col.name();
  attr_info.length = col.storage_len();
  if (!col.nullable()) {
    attr_info.setFlag(AINFO_NOT_NULL);
  }
  if (col.dropped()) {
    attr_info.setFlag(AINFO_DROPPED);
  }
  attr_info.attr_type = (AttrType) col.col_type();
  attr_info.version = 1;

  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::GetMetricColumnInfo(kwdbContext_p ctx, struct AttributeInfo& attr_info, roachpb::KWDBKTSColumn& col) {
  col.clear_storage_len();
  switch (attr_info.type) {
    case DATATYPE::TIMESTAMP64_LSN:
      col.set_storage_type(roachpb::TIMESTAMPTZ);
      break;
    case DATATYPE::TIMESTAMP64:
      col.set_storage_type(roachpb::TIMESTAMP);
      break;
    case DATATYPE::INT16:
      col.set_storage_type(roachpb::SMALLINT);
      break;
    case DATATYPE::INT32:
      col.set_storage_type(roachpb::INT);
      break;
    case DATATYPE::INT64:
      col.set_storage_type(roachpb::BIGINT);
      break;
    case DATATYPE::FLOAT:
      col.set_storage_type(roachpb::FLOAT);
      break;
    case DATATYPE::DOUBLE:
      col.set_storage_type(roachpb::DOUBLE);
      break;
    case DATATYPE::BYTE:
      col.set_storage_type(roachpb::BOOL);
      break;
    case DATATYPE::CHAR:
      col.set_storage_type(roachpb::CHAR);
      col.set_storage_len(attr_info.max_len);
      break;
    case DATATYPE::BINARY:
      col.set_storage_type(roachpb::BINARY);
      col.set_storage_len(attr_info.max_len);
      break;
    case DATATYPE::VARSTRING:
      col.set_storage_type(roachpb::VARCHAR);
      col.set_storage_len(attr_info.max_len + 1);  // varchar(len) + 1
      break;
    case DATATYPE::VARBINARY:
      col.set_storage_type(roachpb::VARBINARY);
      col.set_storage_len(attr_info.max_len);
      break;
    case DATATYPE::STRING:
      col.set_storage_type(roachpb::SDECHAR);
      col.set_storage_len(attr_info.max_len);
      break;
    case DATATYPE::INVALID:
    default:
    return KStatus::FAIL;
  }

  col.set_column_id(attr_info.id);
  col.set_name(attr_info.name);
  col.set_nullable(true);
  col.set_dropped(false);
  if (!col.has_storage_len()) {
    col.set_storage_len(attr_info.length);
  }
  if (attr_info.isFlag(AINFO_NOT_NULL)) {
    col.set_nullable(false);
  }
  if (attr_info.isFlag(AINFO_DROPPED)) {
    col.set_dropped(true);
  }
  col.set_col_type((roachpb::KWDBKTSColumn_ColumnType)(attr_info.attr_type));
  return KStatus::SUCCESS;
}

KStatus
TsEntityGroup::GetTagColumnInfo(kwdbContext_p ctx, struct TagInfo& tag_info, roachpb::KWDBKTSColumn& col) {
  col.clear_storage_len();
  col.set_storage_len(tag_info.m_length);
  col.set_column_id(tag_info.m_id);
  col.set_col_type((roachpb::KWDBKTSColumn_ColumnType)((AttrType)tag_info.m_tag_type));
  switch (tag_info.m_data_type) {
    case DATATYPE::TIMESTAMP64_LSN:
      col.set_storage_type(roachpb::TIMESTAMPTZ);
      break;
    case DATATYPE::TIMESTAMP64:
      col.set_storage_type(roachpb::TIMESTAMP);
      break;
    case DATATYPE::INT16:
      col.set_storage_type(roachpb::SMALLINT);
      break;
    case DATATYPE::INT32:
      col.set_storage_type(roachpb::INT);
      break;
    case DATATYPE::INT64:
      col.set_storage_type(roachpb::BIGINT);
      break;
    case DATATYPE::FLOAT:
      col.set_storage_type(roachpb::FLOAT);
      break;
    case DATATYPE::DOUBLE:
      col.set_storage_type(roachpb::DOUBLE);
      break;
    case DATATYPE::BYTE:
      col.set_storage_type(roachpb::BOOL);
      break;
    case DATATYPE::CHAR:
      col.set_storage_type(roachpb::CHAR);
      break;
    case DATATYPE::BINARY:
      col.set_storage_type(roachpb::BINARY);
      break;
    case DATATYPE::VARSTRING:
      col.set_storage_type(roachpb::VARCHAR);
      break;
    case DATATYPE::VARBINARY:
      col.set_storage_type(roachpb::VARBINARY);
      break;
    case DATATYPE::STRING:
      col.set_storage_type(roachpb::SDECHAR);
      break;
    case DATATYPE::INVALID:
    default:
      return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

uint32_t TsTable::GetConsistentHashId(char* data, size_t length) {
  const uint32_t offset_basis = 2166136261;
  const uint32_t prime = 16777619;
  uint32_t hash_val = offset_basis;
  for (int i = 0; i < length; i++) {
    unsigned char b = data[i];
    hash_val *= prime;
    hash_val ^= b;
  }
  uint32_t range_num = 65535;
  return hash_val % range_num;
}

MMapMetricsTable* TsTable::CreateMMapEntityBigTable(string& db_path, string& tbl_sub_path, KTableKey table_id,
                                                    vector<AttributeInfo> schema, uint64_t partition_interval,
                                                    ErrorInfo& err_info) {
  vector<string> key = {};
  string key_order = "";

  int encoding = ENTITY_TABLE | NO_DEFAULT_TABLE;
  MMapMetricsTable* tmp_bt = new MMapMetricsTable();
  string bt_url = IdToEntityBigTableUrl(table_id);
  if (tmp_bt->open(bt_url, db_path, tbl_sub_path, MMAP_CREAT_EXCL, err_info) >= 0 ||
      err_info.errcode == BOECORR) {
    tmp_bt->create(schema, key, key_order, tbl_sub_path, "", tbl_sub_path,
                   s_emptyString(), partition_interval, encoding, err_info, false);
  }

  if (err_info.errcode < 0) {
    tmp_bt->setObjectReady();
    tmp_bt->remove();
    delete tmp_bt;
    tmp_bt = nullptr;
  } else {
    // tmp_bt->initEntityMeta(db_path, tbl_sub_path, BO_CREAT_EXCL, 0, table_id);
  }
  return tmp_bt;
}

MMapMetricsTable* TsTable::OpenMMapEntityBigTable(string& db_path, string& tbl_sub_path, KTableKey table_id,
                                                  ErrorInfo& err_info) {
  MMapMetricsTable* tmp_bt = new MMapMetricsTable();
  string bt_url = IdToEntityBigTableUrl(table_id);
  tmp_bt->open(bt_url, db_path, tbl_sub_path, MMAP_OPEN_NORECURSIVE, err_info);
  // new_bt->openInit(err_info);
  if (err_info.errcode < 0) {
    // LOG_ERROR("TsTable Init error : %s", err_info.errmsg.c_str());
    if (tmp_bt) {
      delete tmp_bt;
      tmp_bt = nullptr;
    }
    return nullptr;
  }
  // Compatible with older versions
  if (tmp_bt->partitionInterval() == 0) {
    tmp_bt->partitionInterval() = BigObjectConfig::iot_interval;
  }
  return tmp_bt;
}

TsTable::TsTable(kwdbContext_p ctx, const string& db_path, const KTableKey& table_id)
    : db_path_(db_path), table_id_(table_id) {
  entity_bt_ = nullptr;
  tbl_sub_path_ = std::to_string(table_id_) + "/";
  db_path_ = db_path_ + "/";
  is_dropped_.store(false);
  entity_groups_mtx_ = new TsTableEntityGrpsRwLatch(RWLATCH_ID_TS_TABLE_ENTITYGRPS_RWLOCK);
  snapshot_manage_mtx_ = new TsTableSnapshotLatch(LATCH_ID_TSTABLE_SNAPSHOT_MUTEX);
}

TsTable::~TsTable() {
  if (is_dropped_) {
    kwdbContext_t context;
    kwdbContext_p ctx = &context;
    KStatus s = InitServerKWDBContext(ctx);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("InitServerKWDBContext Error!");
    }
    s = DropAll(ctx);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("DropAll Error!");
    }
  }
  if (entity_bt_) {
    delete entity_bt_;
  }
  if (entity_groups_mtx_) {
    delete entity_groups_mtx_;
    entity_groups_mtx_ = nullptr;
  }
  if (snapshot_manage_mtx_) {
    delete snapshot_manage_mtx_;
    snapshot_manage_mtx_ = nullptr;
  }
}

// Check that the directory name is a numeric
bool IsNumber(struct dirent* dir) {
  for (int i = 0; i < strlen(dir->d_name); ++i) {
    if (!isdigit(dir->d_name[i])) {
      // Iterate over each character and determine if it's a number
      return false;
    }
  }
  return true;
}

KStatus TsTable::Init(kwdbContext_p ctx, std::unordered_map<uint64_t, int8_t>& range_groups, ErrorInfo& err_info) {
  // Check path
  string dir_path = db_path_ + tbl_sub_path_;
  if (access(dir_path.c_str(), 0)) {
    err_info.setError(BOENOOBJ, "invalid path: " + db_path_ + tbl_sub_path_);
    LOG_ERROR("invalid path : %s", ((db_path_ + tbl_sub_path_)).c_str())
    return KStatus::FAIL;
  }

  // Load entity bt
  entity_bt_ = OpenMMapEntityBigTable(db_path_, tbl_sub_path_, table_id_, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("TsTable Init error : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }

  DIR* dir_ptr = opendir((db_path_ + "/" + tbl_sub_path_).c_str());
  if (dir_ptr == nullptr) {
    LOG_ERROR("invalid path : %s", ((db_path_ + tbl_sub_path_)).c_str());
    return KStatus::FAIL;
  }
  // Traverse the range directory
  {
    RW_LATCH_X_LOCK(entity_groups_mtx_);
    Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
    struct dirent* group_dir;
    while ((group_dir = readdir(dir_ptr)) != nullptr) {
      if (!IsNumber(group_dir)) {  // Check if it is an entity group directory
        continue;
      }
      int64_t range_group_id = std::atol(group_dir->d_name);
      if (range_group_id < 1) {
        LOG_ERROR("invalid range group : %ld", range_group_id);
      }
      string range_tbl_sub_path = tbl_sub_path_ + group_dir->d_name + "/";
      // hash_range's default type is FOLLOWER when loaded locally
      RangeGroup hash_range{(uint64_t) range_group_id, EntityGroupType::UNINITIALIZED};
      auto it = range_groups.find(range_group_id);
      if (it != range_groups.end()) {
        hash_range.typ = it->second;
      }
      std::shared_ptr<TsEntityGroup> t_range;
      KStatus s = newEntityGroup(ctx, hash_range, range_tbl_sub_path, &t_range);
      if (s != KStatus::SUCCESS) {
        LOG_ERROR("TsTableRange OpenInit error : %lu %lu", table_id_, hash_range.range_group_id);
        continue;
      }

      entity_groups_[hash_range.range_group_id] = std::move(t_range);
    }
    closedir(dir_ptr);
  }
  is_dropped_.store(entity_bt_->isDropped());
  return KStatus::SUCCESS;
}

KStatus TsTable::newEntityGroup(kwdbContext_p ctx, RangeGroup hash_range, const string& range_tbl_sub_path,
                                std::shared_ptr<TsEntityGroup>* ent_group) {
  constructEntityGroup(ctx, hash_range, range_tbl_sub_path, ent_group);
  return (*ent_group)->OpenInit(ctx);
}

KStatus TsTable::Create(kwdbContext_p ctx, vector<AttributeInfo>& metric_schema, uint64_t partition_interval) {
  KStatus s;
  /*if (!meta->has_ts_table()) {
    LOG_ERROR("Create TsTable failed, some required information missing.");
    return KStatus::FAIL;
  }*/

  if (entity_bt_ != nullptr) {
    LOG_ERROR("Entity Bigtable already exist.");
    return KStatus::FAIL;
  }
  // Check path
  string dir_path = db_path_ + tbl_sub_path_;
  if (access(dir_path.c_str(), 0)) {
    if (!MakeDirectory(dir_path)) {
      return KStatus::FAIL;
    }
  }

  ErrorInfo err_info;

  // Create entity table
  entity_bt_ = CreateMMapEntityBigTable(db_path_, tbl_sub_path_, table_id_, metric_schema, partition_interval, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("createTable fail, table_id[%lu], msg[%s]", table_id_, err_info.errmsg.c_str());
  }

  if (entity_bt_ == nullptr) {
    return KStatus::FAIL;
  }

  return KStatus::SUCCESS;
}

KStatus TsTable::GetDataSchema(kwdbContext_p ctx, std::vector<AttributeInfo>* data_schema) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  *data_schema = entity_bt_->getSchemaInfo();
  return KStatus::SUCCESS;
}

KStatus TsTable::GetTagSchema(kwdbContext_p ctx, RangeGroup range, std::vector<TagColumn*>* tag_schema) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  if (entity_groups_.find(range.range_group_id) == entity_groups_.end()) {
    return KStatus::FAIL;
  }
  *tag_schema = entity_groups_[range.range_group_id]->GetSchema();
  return KStatus::SUCCESS;
}

KStatus TsTable::CreateEntityGroup(kwdbContext_p ctx, RangeGroup range, vector<TagInfo>& tag_schema,
                                   std::shared_ptr<TsEntityGroup>* entity_group) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  {
    RW_LATCH_S_LOCK(entity_groups_mtx_);
    Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
    if (entity_groups_.find(range.range_group_id) != entity_groups_.end()) {
      LOG_ERROR("TableRange already exist : %s", (tbl_sub_path_ + "." + std::to_string(range.range_group_id)).c_str());
      return KStatus::FAIL;
    }
  }

  string range_tbl_sub_path = tbl_sub_path_ + std::to_string(range.range_group_id) + "/";
  if (!MakeDirectory(db_path_ + range_tbl_sub_path)) {
    return KStatus::FAIL;
  }
  std::shared_ptr<TsEntityGroup> t_group;
  constructEntityGroup(ctx, range, range_tbl_sub_path, &t_group);

  KStatus s = t_group->Create(ctx, tag_schema);
  if (s != KStatus::SUCCESS) {
    LOG_ERROR("TsTableRange OpenInit error : %ld,%ld", table_id_, range.range_group_id);
    return KStatus::FAIL;
  }

  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  entity_groups_[range.range_group_id] = std::move(t_group);
  *entity_group = entity_groups_[range.range_group_id];
  return KStatus::SUCCESS;
}

KStatus TsTable::GetEntityGroups(kwdbContext_p ctx, RangeGroups *groups) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  groups->len = entity_groups_.size();
  groups->ranges = static_cast<RangeGroup*>(malloc(sizeof(RangeGroup) * groups->len));
  if (groups->ranges == nullptr) {
    LOG_ERROR("failed malloc RangeGroups");
    return KStatus::FAIL;
  }
  int i = 0;
  for (auto &p : entity_groups_) {
    groups->ranges[i].range_group_id = p.first;
    groups->ranges[i].typ = p.second->HashRange().typ;
    LOG_INFO("range_group_id: %lu, type: %d", groups->ranges[i].range_group_id, groups->ranges[i].typ);
    ++i;
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::UpdateEntityGroup(kwdbContext_p ctx, const RangeGroup& range) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  auto it = entity_groups_.find(range.range_group_id);
  if (it == entity_groups_.end()) {
    LOG_ERROR("no hash range : %ld", range.range_group_id);
    return KStatus::FAIL;
  }
  it->second->HashRange().typ = range.typ;
  return KStatus::SUCCESS;
}

KStatus
TsTable::GetEntityGroup(kwdbContext_p ctx, uint64_t range_group_id, std::shared_ptr<TsEntityGroup>* entity_group) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  KStatus s = KStatus::SUCCESS;
  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  auto it = entity_groups_.find(range_group_id);
  if (it != entity_groups_.end()) {
    *entity_group = it->second;
  } else {
    // s = CreateTableRange(ctx, range, table_range);
    LOG_ERROR("no hash range: %ld", range_group_id);
    return KStatus::FAIL;
  }
  return s;
}

KStatus TsTable::PutData(kwdbContext_p ctx, uint64_t range_group_id, TSSlice* payload, int payload_num,
                         uint64_t mtr_id, DedupResult* dedup_result, const DedupRule& dedup_rule) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  // entity_groups_: Data from entities in an EntityRangeGroup is stored in an EntityGroup.
  // EntityGroup is a logical unit to persist entities' data, including tag data and measurement data.
  auto it = entity_groups_.find(range_group_id);
  if (it == entity_groups_.end()) {
    LOG_ERROR("no entity group with id: %lu", range_group_id);
    return KStatus::FAIL;
  }
  KStatus s = it->second->PutData(ctx, payload, payload_num, mtr_id, dedup_result, dedup_rule);
  return s;
}

KStatus TsTable::FlushBuffer(kwdbContext_p ctx) {
  // No processing required
  return KStatus::SUCCESS;
}


KStatus TsTable::CreateCheckpoint(kwdbContext_p ctx) {
  // No processing required
  return KStatus::SUCCESS;
}

KStatus TsTable::Recover(kwdbContext_p ctx, const std::map<uint64_t, uint64_t>& applied_indexes) {
  // No processing required
  return KStatus::SUCCESS;
}

KStatus TsTable::GetAllLeaderEntityGroup(kwdbts::kwdbContext_p ctx,
                                         std::vector<std::shared_ptr<TsEntityGroup>>* leader_entity_groups) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  leader_entity_groups->clear();
  KStatus s = KStatus::SUCCESS;
  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  for (auto& entity_group : entity_groups_) {
    if (entity_group.second->HashRange().typ == EntityGroupType::UNINITIALIZED) {
      string err_msg = "table[" + std::to_string(table_id_) +
                       "] range group[" + std::to_string(entity_group.first) +
                       "] is uninitialized";
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_INVALID_STATUS, err_msg.c_str());
      LOG_ERROR("%s", err_msg.c_str());
      return KStatus::FAIL;
    }
    if (entity_group.second->HashRange().typ == EntityGroupType::LEADER) {
      leader_entity_groups->push_back(entity_group.second);
    }
  }
  return s;
}

KStatus TsTable::DropEntityGroup(kwdbContext_p ctx, uint64_t range_group_id, bool sync) {
  LOG_INFO("DropEntityGroup: range_group_id %lu", range_group_id);
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  auto it = entity_groups_.find(range_group_id);
  if (it == entity_groups_.end()) {
    LOG_ERROR("no hash range");
    RW_LATCH_UNLOCK(entity_groups_mtx_);
    return KStatus::FAIL;
  }
  auto entity_group = it->second;
  entity_groups_.erase(it);
  RW_LATCH_UNLOCK(entity_groups_mtx_);

  KStatus s = entity_group->Drop(ctx);
  if (s == KStatus::FAIL) {
    LOG_ERROR("entity group drop failed, range_group_id=%lu", range_group_id);
    RW_LATCH_X_LOCK(entity_groups_mtx_);
    Defer defer{[&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); }};
    entity_groups_[range_group_id] = entity_group;
    return KStatus::FAIL;
  }
  return s;
}

KStatus TsTable::DropAll(kwdbContext_p ctx, bool is_force) {
  LOG_INFO("TsTable::drop table %ld", table_id_);
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  KStatus s = KStatus::SUCCESS;
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  std::unordered_map<uint64_t, int8_t> range_groups;
  for (auto & entity_group : entity_groups_) {
    range_groups.insert({entity_group.second->HashRange().range_group_id, entity_group.second->HashRange().typ});
    s = entity_group.second->Drop(ctx, is_force);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("TsTableRange drop failed : %s", tbl_sub_path_.c_str());
      break;
    }
  }
  entity_groups_.clear();
  // clear tbl_ranges, and re-initialize
  if (s != KStatus::SUCCESS) {
    delete entity_bt_;
    entity_bt_ = nullptr;
    Init(ctx, range_groups);
    return s;
  }
  entity_bt_->remove();
  delete entity_bt_;
  entity_bt_ = nullptr;
  LOG_INFO("TsTable::remove table %ld files", table_id_);
  ::remove((db_path_ + tbl_sub_path_).c_str());
  LOG_INFO("TsTable::drop table %ld over", table_id_);
  return KStatus::SUCCESS;
}

KStatus TsTable::Compress(kwdbContext_p ctx, const KTimestamp& ts) {
  if (entity_bt_ == nullptr) {
    LOG_ERROR("TsTable not created : %s", tbl_sub_path_.c_str());
    return KStatus::FAIL;
  }
  KStatus s = KStatus::SUCCESS;
  std::vector<std::shared_ptr<TsEntityGroup>> compress_entity_groups;
  {
    RW_LATCH_X_LOCK(entity_groups_mtx_);
    Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
    // get all entity groups
    for (auto& entity_group : entity_groups_) {
      compress_entity_groups.push_back(entity_group.second);
    }
  }
  for (auto& entity_group : compress_entity_groups) {
    if (!entity_group) {
      continue;
    }
    s = entity_group->Compress(ctx, ts);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("TsTableRange compress failed : %s", tbl_sub_path_.c_str());
      break;
    }
  }

  return s;
}

KStatus TsTable::GetIterator(kwdbContext_p ctx, const std::vector<EntityResultIndex>& entity_ids,
                             std::vector<KwTsSpan> ts_spans, std::vector<k_uint32> scan_cols,
                             std::vector<Sumfunctype> scan_agg_types, TsTableIterator** iter,
                             bool reverse, k_uint32 table_version) {
  KWDB_DURATION(StStatistics::Get().get_iterator);
  if (scan_cols.empty()) {
    // LOG_ERROR("TsTable::GetIterator Error : no column");
    // return KStatus::FAIL;
  }
  auto ts_table_iterator = new TsTableIterator();
  KStatus s;
  Defer defer{[&]() {
    if (s == FAIL) {
      delete ts_table_iterator;
      ts_table_iterator = nullptr;
      *iter = nullptr;
    }
  }};

  entity_bt_->rdLock();
  auto actual_cols = entity_bt_->getActualCols();
  std::vector<k_uint32> ts_scan_cols;
  for (auto col : scan_cols) {
    if (col >= actual_cols.size()) {
      // In the concurrency scenario, after the storage has deleted the column,
      // kwsql sends query again
      LOG_ERROR("GetIterator Error : TsTable no column %d", col);
      entity_bt_->unLock();
      return KStatus::FAIL;
    }
    ts_scan_cols.emplace_back(actual_cols[col]);
  }
  entity_bt_->unLock();

  uint64_t entity_group_id = 0;
  uint32_t subgroup_id = 0;
  std::shared_ptr<TsEntityGroup> entity_group;
  std::vector<uint32_t> entities;
  for (auto& entity : entity_ids) {
    if (entity_group_id == 0 && subgroup_id == 0) {
      entity_group_id = entity.entityGroupId;
      subgroup_id = entity.subGroupId;
      s = GetEntityGroup(ctx, entity.entityGroupId, &entity_group);
      if (s == FAIL) return s;
    }
    if (entity.entityGroupId != entity_group_id || entity.subGroupId != subgroup_id) {
      TsIterator* ts_iter;
      s = entity_group->GetIterator(ctx, subgroup_id, entities, ts_spans, scan_cols, ts_scan_cols,
                                    scan_agg_types, &ts_iter, entity_group, reverse);
      if (s == FAIL) return s;
      ts_table_iterator->AddEntityIterator(ts_iter);

      subgroup_id = entity.subGroupId;
      entities.clear();
    }
    if (entity.entityGroupId != entity_group_id) {
      entity_group_id = entity.entityGroupId;
      entity_group.reset();
      s = GetEntityGroup(ctx, entity.entityGroupId, &entity_group);
      if (s == FAIL) return s;
    }
    entities.emplace_back(entity.entityId);
  }
  if (!entities.empty()) {
    TsIterator* ts_iter;
    s = entity_group->GetIterator(ctx, subgroup_id, entities, ts_spans, scan_cols, ts_scan_cols,
                                  scan_agg_types, &ts_iter, entity_group, reverse);
    if (s == FAIL) return s;
    ts_table_iterator->AddEntityIterator(ts_iter);
  }

  (*iter) = ts_table_iterator;

  return KStatus::SUCCESS;
}

KStatus TsTable::CreateSnapshot(kwdbContext_p ctx, uint64_t range_group_id, uint64_t begin_hash, uint64_t end_hash,
                                uint64_t* snapshot_id) {
  LOG_INFO("CreateSnapshot begin! [Snapshot ID:%lu, Ranggroup ID: %lu]", *snapshot_id, range_group_id);
  std::shared_ptr<TsEntityGroup> entity_group_src;
  KStatus s = GetEntityGroup(ctx, range_group_id, &entity_group_src);
  if (s == KStatus::FAIL || entity_group_src == nullptr) {
    // Add error messages
    LOG_ERROR("GetEntityGroup failed during CreateSnapshot, range_group_id=%lu, snapshot_id=%lu",
              range_group_id, *snapshot_id);
    return KStatus::FAIL;
  }

  SnapshotInfo ts_snapshot_info{};
  ts_snapshot_info.begin_hash = begin_hash;
  ts_snapshot_info.end_hash = end_hash;
  ts_snapshot_info.type = 0;
  // type = 0, source node build snapshot, need to generate snapshot id
  auto now = std::chrono::system_clock::now();
  // Converts time to milliseconds that have elapsed since January 1, 1970
  auto timestamp = std::chrono::duration_cast<std::chrono::milliseconds>(now.time_since_epoch()).count();
  ts_snapshot_info.id = timestamp;
  std::shared_ptr<TsTableSnapshot> ts_snapshot_table =
      std::make_shared<TsTableSnapshot>(db_path_, table_id_, tbl_sub_path_, entity_group_src, entity_bt_, ts_snapshot_info);
  s = ts_snapshot_table->Init(ctx);
  if (s != KStatus::SUCCESS) {
    LOG_ERROR("Init Snapshot table failed, range_group_id=%lu, snapshot_id=%lu", range_group_id, *snapshot_id);
    return KStatus::FAIL;
  }
  *snapshot_id = ts_snapshot_table->GetSnapshotId();
  snapshot_manage_pool_[ts_snapshot_table->GetSnapshotId()] = ts_snapshot_table;
  LOG_INFO("CreateSnapshot success! [Snapshot ID:%lu, Ranggroup ID: %lu]", *snapshot_id, range_group_id);
  return KStatus::SUCCESS;
}

KStatus TsTable::DropSnapshot(kwdbContext_p ctx, uint64_t range_group_id, uint64_t snapshot_id) {
  TsTableSnapshot* ts_snapshot_table = nullptr;
  MUTEX_LOCK(snapshot_manage_mtx_);
  if (snapshot_manage_pool_.find(snapshot_id) != snapshot_manage_pool_.end()) {
    ts_snapshot_table = snapshot_manage_pool_[snapshot_id].get();
  } else {
    LOG_ERROR("Snapshot table not found, range_group_id=%lu, snapshot_id=%lu", range_group_id, snapshot_id);
    MUTEX_UNLOCK(snapshot_manage_mtx_);
    return KStatus::FAIL;
  }

  if (ts_snapshot_table == nullptr) {
    LOG_ERROR("Snapshot table not found, range_group_id=%lu, snapshot_id=%lu", range_group_id, snapshot_id);
    MUTEX_UNLOCK(snapshot_manage_mtx_);
    return KStatus::FAIL;
  }

  KStatus s = ts_snapshot_table->DropAll();
  if (s == KStatus::FAIL) {
    LOG_ERROR("DropAll Snapshot, range_group_id=%lu, snapshot_id=%lu", range_group_id, snapshot_id);
    MUTEX_UNLOCK(snapshot_manage_mtx_);
    return KStatus::FAIL;
  }
  snapshot_manage_pool_.erase(snapshot_id);
  MUTEX_UNLOCK(snapshot_manage_mtx_);
  return KStatus::SUCCESS;
}

KStatus TsTable::GetSnapshotData(kwdbContext_p ctx, uint64_t range_group_id, uint64_t snapshot_id,
                                 size_t offset, size_t limit, TSSlice* data, size_t* total) {
  LOG_INFO("GetSnapshotData begin! snapshot_id:%ld, range_group_id: %ld", snapshot_id, range_group_id);
  if (snapshot_manage_pool_.find(snapshot_id) == snapshot_manage_pool_.end()) {
    LOG_ERROR("GetSnapshotData failed, range_group_id=%lu, snapshot_id=%ld", range_group_id, snapshot_id);
    return KStatus::FAIL;
  }
  std::shared_ptr<TsTableSnapshot> snap_shot = snapshot_manage_pool_.find(snapshot_id)->second;
  KStatus s = KStatus::FAIL;
  if (snapshot_get_size_pool_.find(snapshot_id) == snapshot_get_size_pool_.end()) {
    // todo(lfl): LSN needs to be added in the future
    TS_LSN lsn = 9999999999999;
    s = snap_shot->BuildSnapshot(ctx, lsn);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("BuildSnapshot failed, range_group_id=%lu, snapshot_id=%lu", range_group_id, snapshot_id);
      return s;
    }
    s = snap_shot->CompressSnapshot(ctx, total);
    if (s == KStatus::FAIL) {
      LOG_ERROR("CompressSnapshot failed during GetSnapshotData, range_group_id=%lu, snapshot_id=%ld",
                range_group_id, snapshot_id);
      return KStatus::FAIL;
    }
    snapshot_get_size_pool_[snapshot_id] = std::move(*total);
  }

  TsEntityGroup* snap_shot_gp = snap_shot->GetSnapshotEntityGroup();
  if (snap_shot_gp->GetSubEntityGroupTagbt()->size() == 0) {
    *total = 0;
    return KStatus::SUCCESS;
  }
  *total = snapshot_get_size_pool_[snapshot_id];
  s = snap_shot->GetSnapshotData(ctx, range_group_id, offset, limit, data, total);
  if (s == KStatus::FAIL) {
    LOG_ERROR("GetSnapshotData failed, range_group_id=%lu, snapshot_id=%ld",
              range_group_id, snapshot_id);
    return KStatus::FAIL;
  }
  LOG_INFO("GetSnapshotData success! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  return KStatus::SUCCESS;
}

KStatus TsTable::WriteSnapshotData(kwdbContext_p ctx, const uint64_t range_group_id, uint64_t snapshot_id,
                                   size_t offset, TSSlice data, bool finished) {
  LOG_INFO("WriteSnapshotData begin! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  std::shared_ptr<TsEntityGroup> entity_group_src;
  KStatus s = GetEntityGroup(ctx, range_group_id, &entity_group_src);
  if (s == KStatus::FAIL || entity_group_src == nullptr) {
    LOG_ERROR("GetEntityGroup failed during WriteSnapshotData, range_group_id=%lu, snapshot_id=%lu",
              range_group_id, snapshot_id);
    return KStatus::FAIL;
  }

#ifndef WITH_TESTS
  // The original leader role of the node does not allow snapshot data to be written
  if (entity_group_src->HashRange().typ == 0) {
    LOG_ERROR("The leader role of the original EntityGroup on the node is not allowed to write data, "
              "range_group_id=%lu, snapshot_id=%lu", range_group_id, snapshot_id);
    return KStatus::FAIL;
  }
#endif

  string tbl_sub_path = to_string(range_group_id) + "_" + to_string(snapshot_id);
  string range_tbl_sub_path = db_path_ + tbl_sub_path_;
  KString dir_path = range_tbl_sub_path + tbl_sub_path;
  KString file_name = dir_path + ".sqfs";

#ifndef WITH_TESTS
  std::ofstream write_data_handle(file_name, std::ios::binary | std::ios::app);
  if (!write_data_handle.is_open()) {
    LOG_ERROR("Open file failed during InitWrite, range_group_id[%ld], snapshot_id=%ld.",
              range_group_id, snapshot_id)
    return KStatus::FAIL;
  }
  write_data_handle.write(data.data, data.len);
  if (write_data_handle.fail()) {
    LOG_ERROR("Write compressed file failed, range_group_id=%lu, snapshot_id=%ld", range_group_id, snapshot_id);
    return KStatus::FAIL;
  }
  if (finished) {
    write_data_handle.close();
  }
#endif

  LOG_INFO("Write compressed file data range during WriteSnapshotData[snapshotId:%ld], start address:%ld, end address=%ld",
           snapshot_id, offset, offset + data.len - 1);

  if (finished) {
    KString cmd = "unsquashfs -q -n -d " + dir_path + " " + file_name;
    LOG_INFO("%s", cmd.c_str());
    int result = std::system(cmd.c_str());
    if (result == -1) {
      LOG_ERROR("Uncompressed file failed, range_group_id=%lu", range_group_id);
      return KStatus::FAIL;
    }

    KString rm_cmd = "rm -rf  " + file_name;
    result = std::system(rm_cmd.c_str());
    if (result == -1) {
      LOG_ERROR("Remove compressed file failed, range_group_id=%lu", range_group_id);
      return KStatus::FAIL;
    }
    s = ApplySnapshot(ctx, range_group_id, snapshot_id, true);
    if (s != KStatus::SUCCESS) {
      return s;
    }
  }
  LOG_INFO("WriteSnapshotData success! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  return KStatus::SUCCESS;
}

KStatus TsTable::ApplySnapshot(kwdbContext_p ctx, uint64_t range_group_id, uint64_t snapshot_id, bool delete_after_apply) {
  LOG_INFO("ApplySnapshot begin! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  std::shared_ptr<TsEntityGroup> entity_group_src;
  KStatus s = GetEntityGroup(ctx, range_group_id, &entity_group_src);
  if (s == KStatus::FAIL || entity_group_src == nullptr) {
    LOG_ERROR("GetEntityGroup failed during ApplySnapshot, range_group_id=%lu, snapshot_id=%lu",
              range_group_id, snapshot_id);
    return KStatus::FAIL;
  }

  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  SnapshotInfo ts_snapshot_info{};
  ts_snapshot_info.begin_hash = 0;
  ts_snapshot_info.end_hash = UINT64_MAX;
  ts_snapshot_info.type = 1;
  ts_snapshot_info.id = snapshot_id;

  std::shared_ptr<TsTableSnapshot> ts_snapshot_table =
      std::make_shared<TsTableSnapshot>(db_path_, table_id_, tbl_sub_path_, entity_group_src, entity_bt_, ts_snapshot_info);
  s = ts_snapshot_table->Init(ctx);
  if (s == KStatus::FAIL) {
    LOG_ERROR("Apply Snapshot failed, range_group_id=%lu", range_group_id);
    return KStatus::FAIL;
  }

  s = ts_snapshot_table->Apply();
  if (s == KStatus::FAIL) {
    LOG_ERROR("Apply Snapshot failed, range_group_id=%lu", range_group_id);
    return KStatus::FAIL;
  }
  snapshot_id = ts_snapshot_table->GetSnapshotId();
  snapshot_manage_pool_[snapshot_id] = ts_snapshot_table;

  if (delete_after_apply) {
    s = ts_snapshot_table->DropAll();
    if (s == KStatus::FAIL) {
      LOG_ERROR("Drop Snapshot table failed, range_group_id=%lu", range_group_id);
      return KStatus::FAIL;
    }
    snapshot_manage_pool_.erase(snapshot_id);
  }
  LOG_INFO("ApplySnapshot success! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  return KStatus::SUCCESS;
}

KStatus TsTable::EnableSnapshot(kwdbContext_p ctx, uint64_t range_group_id, uint64_t snapshot_id) {
  LOG_INFO("EnableSnapshot begin! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  std::shared_ptr<TsEntityGroup> entity_group;
  KStatus s = GetEntityGroup(ctx, range_group_id, &entity_group);
  if (s == KStatus::FAIL || entity_group == nullptr) {
    LOG_ERROR("GetEntityGroup failed during ApplySnapshot, range_group_id=%lu, snapshot_id=%lu",
              range_group_id, snapshot_id);
    return KStatus::FAIL;
  }
  // set subgroup available
  entity_group->SetAllSubgroupAvailable();
  LOG_INFO("EnableSnapshot success! [Snapshot ID:%ld, Ranggroup ID: %ld]", snapshot_id, range_group_id);
  return KStatus::SUCCESS;
}

KStatus TsTable::DeleteRangeEntities(kwdbContext_p ctx, const uint64_t& range_group_id, const HashIdSpan& hash_span,
                                     uint64_t* count, uint64_t mtr_id) {
  KStatus s = KStatus::FAIL;
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  auto range_group = entity_groups_.find(range_group_id);
  if (range_group == entity_groups_.end()) {
    LOG_ERROR("hash range not found, range_group_id=%lu", range_group_id);
    return s;
  }
  s = range_group->second->DeleteRangeEntities(ctx, hash_span, count, mtr_id);
  return s;
}

KStatus TsTable::DeleteRangeData(kwdbContext_p ctx, uint64_t range_group_id, HashIdSpan& hash_span,
                            const std::vector<KwTsSpan>& ts_spans, uint64_t* count, uint64_t mtr_id) {
  std::shared_ptr<TsEntityGroup> table_range;
  auto s = GetEntityGroup(ctx, range_group_id, &table_range);
  if (s == KStatus::FAIL) {
    LOG_ERROR("GetEntityGroup failed, tableID:%lu, rangeGroupID: %lu", table_id_, range_group_id)
    return s;
  }

  if (table_range) {
    s = table_range->DeleteRangeData(ctx, hash_span, 0, ts_spans, nullptr, count, mtr_id, false);
    if (s == KStatus::FAIL) {
      LOG_ERROR("DeleteRangeData failed, tableID:%lu, rangeGroupID: %lu, hashSpan[%lu,%lu]",
                table_id_, range_group_id, hash_span.begin, hash_span.end)
      return s;
    } else {
      LOG_INFO("DeleteRangeData succeed, tableID:%lu, rangeGroupID: %lu, hashSpan[%lu,%lu]",
               table_id_, range_group_id, hash_span.begin, hash_span.end);
      return KStatus::SUCCESS;
    }
  } else {
    LOG_ERROR("DeleteRangeData failed, range_group disappear, tableID:%lu, rangeGroupID: %lu, hashSpan[%lu,%lu]",
              table_id_, range_group_id, hash_span.begin, hash_span.end)
    return KStatus::FAIL;
  }
}

KStatus TsTable::DeleteData(kwdbContext_p ctx, uint64_t range_group_id, std::string& primary_tag,
                            const std::vector<KwTsSpan>& ts_spans, uint64_t* count, uint64_t mtr_id) {
  std::shared_ptr<TsEntityGroup> table_range;
  auto s = GetEntityGroup(ctx, range_group_id, &table_range);
  if (s == KStatus::FAIL) {
    LOG_ERROR("GetEntityGroup failed, tableID:%lu, rangeGroupID: %lu", table_id_, range_group_id)
    return s;
  }

  if (table_range) {
    s = table_range->DeleteData(ctx, primary_tag, 0, ts_spans, nullptr, count, mtr_id, false);
    if (s == KStatus::FAIL) {
      LOG_ERROR("DeleteData failed, tableID:%lu, rangeGroupID: %lu", table_id_, range_group_id)
      return s;
    } else {
      LOG_INFO("DeleteData succeed, tableID:%lu, rangeGroupID: %lu", table_id_, range_group_id);
      return KStatus::SUCCESS;
    }
  } else {
    LOG_ERROR("DeleteData failed, range_group disappear, tableID:%lu, rangeGroupID: %lu", table_id_, range_group_id)
    return KStatus::FAIL;
  }
}

KStatus TsTable::DeleteExpiredData(kwdbContext_p ctx, int64_t end_ts) {
  KStatus s = KStatus::FAIL;
  RW_LATCH_X_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  // Traverse through all entity groups to delete expired data
  for (auto& entity_group : entity_groups_) {
    s = entity_group.second->DeleteExpiredData(ctx, end_ts);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("EntityGroup delete expired data failed.");
      return s;
    }
  }
  return s;
}

KStatus TsTable::GetEntityIdList(kwdbContext_p ctx, const std::vector<void*>& primary_tags,
                                 const std::vector<uint32_t>& scan_tags,
                                 std::vector<EntityResultIndex>* entity_id_list, ResultSet* res, uint32_t* count) {
  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  for (const auto tbl_range : entity_groups_) {
    if (tbl_range.second->HashRange().typ == EntityGroupType::UNINITIALIZED) {
      string err_msg = "table[" + std::to_string(table_id_) +
                       "] range group[" + std::to_string(tbl_range.first) +
                       "] is uninitialized";
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_INVALID_STATUS, err_msg.c_str());
      LOG_ERROR("%s", err_msg.c_str());
      return KStatus::FAIL;
    }
    if (tbl_range.second->HashRange().typ != EntityGroupType::LEADER) {
      // not leader
      continue;
    }
    tbl_range.second->GetEntityIdList(ctx, primary_tags, scan_tags, entity_id_list, res, count);
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::GetTagIterator(kwdbContext_p ctx, std::vector<uint32_t> scan_tags,
                                TagIterator** iter, k_uint32 table_version) {
  std::vector<EntityGroupTagIterator*> eg_tag_iters;
  EntityGroupTagIterator* eg_tag_iter = nullptr;

  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  for (const auto tbl_range : entity_groups_) {
    if (tbl_range.second->HashRange().typ == EntityGroupType::UNINITIALIZED) {
      string err_msg = "table[" + std::to_string(table_id_) +
                        "] range group[" + std::to_string(tbl_range.first) +
                        "] is uninitialized";
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_INVALID_STATUS, err_msg.c_str());
      LOG_ERROR("%s", err_msg.c_str());
      return KStatus::FAIL;
    }
    if (tbl_range.second->HashRange().typ != EntityGroupType::LEADER) {
      // not leader
      continue;
    }
    tbl_range.second->GetTagIterator(ctx, scan_tags, &eg_tag_iter);
    if (!eg_tag_iter) {
      return KStatus::FAIL;
    }
    eg_tag_iters.emplace_back(std::move(eg_tag_iter));
    eg_tag_iter = nullptr;
  }

  TagIterator* tag_iter = new TagIterator(eg_tag_iters);
  if (KStatus::SUCCESS != tag_iter->Init()) {
    delete tag_iter;
    tag_iter = nullptr;
    *iter = nullptr;
    return KStatus::FAIL;
  }
  *iter = tag_iter;
  return KStatus::SUCCESS;
}

KStatus TsTable::GetMetaIterator(kwdbContext_p ctx, MetaIterator** iter, k_uint32 table_version) {
  std::vector<EntityGroupMetaIterator*> iters;
  EntityGroupMetaIterator* eg_meta_iter = nullptr;
  for (const auto tbl_range : entity_groups_) {
    if (tbl_range.second->HashRange().typ == EntityGroupType::UNINITIALIZED) {
      string err_msg = "table[" + std::to_string(table_id_) +
                       "] range group[" + std::to_string(tbl_range.first) +
                       "] is uninitialized";
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_INVALID_STATUS, err_msg.c_str());
      LOG_ERROR("%s", err_msg.c_str());
      return KStatus::FAIL;
    }
    if (tbl_range.second->HashRange().typ != EntityGroupType::LEADER) {
      // not leader
      continue;
    }
    if (KStatus::SUCCESS != tbl_range.second->GetMetaIterator(ctx, &eg_meta_iter)) {
      continue;
    }
    iters.emplace_back(std::move(eg_meta_iter));
    eg_meta_iter = nullptr;
  }
  MetaIterator* meta_iter = new MetaIterator(iters);
  if (KStatus::SUCCESS != meta_iter->Init()) {
    delete meta_iter;
    meta_iter = nullptr;
    *iter = nullptr;
    return KStatus::FAIL;
  }
  *iter = meta_iter;
  return KStatus::SUCCESS;
}

KStatus TsTable::TSxClean(kwdbContext_p ctx) {
  RW_LATCH_S_LOCK(entity_groups_mtx_);
  Defer defer([&]() { RW_LATCH_UNLOCK(entity_groups_mtx_); });
  for (const auto& tbl_range : entity_groups_) {
    KStatus s = tbl_range.second->TSxClean(ctx);
    if (s == FAIL) {
      LOG_ERROR("Failed to clean the entity group %lu of table %lu",
                tbl_range.second->HashRange().range_group_id, table_id_)
      return s;
    }
  }

  return SUCCESS;
}

KStatus TsTable::AddColumn(kwdbContext_p ctx, roachpb::KWDBKTSColumn* column, string& msg) {
  AttributeInfo attr_info;
  ErrorInfo err_info;
  KStatus s = TsEntityGroup::GetColAttributeInfo(ctx, *column, attr_info, false);
  if (s != KStatus::SUCCESS) {
    return s;
  }
  if (attr_info.isAttrType(ATTR_GENERAL_TAG)) {
    // Only general tag can be added
    TagInfo new_tag_schema = {column->column_id(), attr_info.type,
                              static_cast<uint32_t>(attr_info.length), 0,
                              static_cast<uint32_t>(attr_info.size),
                              GENERAL_TAG};
    for (const auto& entity_group : entity_groups_) {
      s = entity_group.second->AddTagInfo(ctx, new_tag_schema, err_info);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  } else {
    // database recovery may add column again, return success when column exists
    if (entity_bt_->getColumnIndex(attr_info) >= 0) {
      return KStatus::SUCCESS;
    }
    // Add metric column in table root meta
    s = addMetricsTableColumn(ctx, attr_info, err_info);
    if (s != KStatus::SUCCESS) {
      return s;
    }
    // Add column in table's all entity groups
    for (auto& entity_group : entity_groups_) {
      s = entity_group.second->AlterTable(ctx, AlterType::ADD_COLUMN, attr_info);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::DropColumn(kwdbContext_p ctx, roachpb::KWDBKTSColumn* column, string& msg) {
  AttributeInfo attr_info;
  ErrorInfo err_info;
  KStatus s = TsEntityGroup::GetColAttributeInfo(ctx, *column, attr_info, false);
  if (s != KStatus::SUCCESS) {
    return s;
  }
  if (attr_info.isAttrType(ATTR_GENERAL_TAG)) {
    // Only general tag can be dropped
    TagInfo old_tag_schema = {column->column_id(), attr_info.type,
                              static_cast<uint32_t>(attr_info.length), 0,
                              static_cast<uint32_t>(attr_info.size),
                              GENERAL_TAG};
    for (const auto& entity_group : entity_groups_) {
      s = entity_group.second->DropTagInfo(ctx, old_tag_schema, err_info);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  } else {
    // database recovery may add column again, return success when column does not exist
    if (entity_bt_->getColumnIndex(attr_info) < 0) {
      return KStatus::SUCCESS;
    }
    // Drop metric column in table root meta
    s = dropMetricsTableColumn(ctx, attr_info, err_info);
    if (s != KStatus::SUCCESS) {
      return s;
    }
    // Drop column in table's all entity groups
    for (auto& entity_group : entity_groups_) {
      s = entity_group.second->AlterTable(ctx, AlterType::DROP_COLUMN, attr_info);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::addMetricsTableColumn(kwdbContext_p ctx, AttributeInfo& attr_info, ErrorInfo& err_info) {
  entity_bt_->rdLock();
  // Get original schema information.
  vector<AttributeInfo> new_schema = entity_bt_->getSchemaInfo();
  entity_bt_->unLock();
  // Add attribute information.
  new_schema.emplace_back(attr_info);
  vector<string> key;
  int encoding = ENTITY_TABLE | NO_DEFAULT_TABLE;

  // Delete same name table file that may exist.
  ::remove((entity_bt_->realFilePath() + s_new()).c_str());
  // Create new table files
  auto* new_bt = new MMapMetricsTable();
  if (new_bt->open(entity_bt_->URL() + s_new(), db_path_, tbl_sub_path_, MMAP_CREAT_EXCL, err_info) >= 0 ||
      err_info.errcode == BOECORR) {
    new_bt->create(new_schema, key, "", tbl_sub_path_, "", tbl_sub_path_, s_emptyString(),
      entity_bt_->metaData()->partition_interval, encoding, err_info, false);
  }
  if (err_info.errcode < 0) {
    new_bt->setObjectReady();
    new_bt->remove();
    delete new_bt;
    new_bt = nullptr;
    LOG_ERROR("add column error: %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }

  // Copy the key information in the metadata
  SubEntityGroupManager::CopyMetaData(new_bt, entity_bt_);

  MMapMetricsTable* old_bt = entity_bt_;
  old_bt->wrLock();
  new_bt->wrLock();

  // Release the old `.bt`, and rename to `.bt.old`
  string file_path = entity_bt_->filePath();
  string bt_path = entity_bt_->realFilePath();
  string bt_path_bak = bt_path + s_old();

  // Change the entity table pointer to point to the new table
  entity_bt_ = new_bt;

  // Delete old table file that may exist.
  ::remove(bt_path_bak.c_str());
  // Rename old table file
  if (auto err = old_bt->rename(bt_path_bak, file_path + s_old()) < 0) {
    entity_bt_ = old_bt;
    old_bt->unLock();
    new_bt->unLock();
    delete new_bt;
    LOG_ERROR("rename %s to %s failed: %s", bt_path.c_str(), bt_path_bak.c_str(), ErrorInfo::errorCodeString(err));
    return KStatus::FAIL;
  }

  old_bt->unLock();
  delete old_bt;

  // use the `.bt.new` to replace `.bt`
  if (auto err = new_bt->rename(bt_path, file_path) < 0) {
    LOG_ERROR("rename %s to %s failed: %s", new_bt->realFilePath().c_str(), bt_path.c_str(),
              ErrorInfo::errorCodeString(err));
    new_bt->unLock();
    return KStatus::FAIL;
  }
  new_bt->unLock();

  return KStatus::SUCCESS;
}

KStatus TsTable::dropMetricsTableColumn(kwdbContext_p ctx, AttributeInfo& attr_info, ErrorInfo& err_info) {
  entity_bt_->DropColumn(attr_info, err_info);
  if (err_info.errcode < 0) {
    LOG_ERROR("drop column failed: %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::checkAlterValid(kwdbContext_p ctx, int col_index, DATATYPE origin_type,
                                 DATATYPE new_type, bool& is_valid, ErrorInfo& err_info) {
  // First, determine whether the original type is a type that supports modification.
  if (origin_type == DATATYPE::VARSTRING && (new_type == DATATYPE::INT16 || new_type == DATATYPE::INT32
  || new_type == DATATYPE::INT64 || new_type == DATATYPE::FLOAT || new_type == DATATYPE::DOUBLE)) {
    std::vector<void*> primary_tags;
    std::vector<uint32_t> scan_tags;
    std::vector<EntityResultIndex> entity_list;
    ResultSet res;
    uint32_t count;
    // Get all entity ids.
    KStatus s = GetEntityIdList(ctx, primary_tags, scan_tags, &entity_list , &res, &count);
    if (s != KStatus::SUCCESS) {
      return s;
    }

    std::map<uint64_t, std::map<uint32_t, std::vector<uint32_t>>> table_entities;
    // Build a map of the relationships between ids and groups and subgroups
    for (auto& entity_index : entity_list) {
      auto iter = table_entities.find(entity_index.entityGroupId);
      if (iter == table_entities.end()) {
        table_entities[entity_index.entityGroupId] = {{entity_index.subGroupId, {entity_index.entityId}}};
      } else {
        auto iter2 = iter->second.find(entity_index.subGroupId);
        if (iter2 == iter->second.end()) {
          iter->second[entity_index.subGroupId] = {entity_index.entityId};
        } else {
          iter2->second.emplace_back(entity_index.entityId);
        }
      }
    }

    // Iterate over the relationship map and check that whether the alter operations are valid or not.
    for (auto& group : table_entities) {
      std::shared_ptr<TsEntityGroup> entity_group;
      s = GetEntityGroup(ctx, group.first, &entity_group);
      if (s == FAIL) {
        return s;
      }
      s = entity_group->CheckAlterValid(ctx, group.second, col_index, new_type, is_valid, err_info);
      if (s == FAIL) {
        return s;
      }
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::AlterColumnType(kwdbContext_p ctx, roachpb::KWDBKTSColumn* column, string& errmsg) {
  AttributeInfo new_attr;
  ErrorInfo err_info;
  KStatus s = TsEntityGroup::GetColAttributeInfo(ctx, *column, new_attr, false);
  if (s != KStatus::SUCCESS) {
    return s;
  }
  getDataTypeSize(new_attr);  // update max_len
  // Alter tag column type
  if (new_attr.isAttrType(ATTR_GENERAL_TAG)) {
    // Only general tag can be modified
    TagInfo new_tag_schema = {column->column_id(), new_attr.type,
                              static_cast<uint32_t>(new_attr.length), 0,
                              static_cast<uint32_t>(new_attr.size),
                              GENERAL_TAG};
    for (const auto& entity_group : entity_groups_) {
      s = entity_group.second->AlterTagInfo(ctx, new_tag_schema, err_info);
      if (s != KStatus::SUCCESS) {
        errmsg = err_info.errmsg;
        return s;
      }
    }
  } else {
    // Alter metrics column type
    entity_bt_->rdLock();
    int col_index = entity_bt_->getColumnIndex(new_attr);
    entity_bt_->unLock();
    if (col_index < 0) {
      LOG_ERROR("alter column type failed: column (id %u) not exists, table id = %lu", new_attr.id, table_id_);
      return KStatus::FAIL;
    }
    bool is_valid = true;
    // Check whether we can change the column type in a database table
    s = checkAlterValid(ctx, col_index, static_cast<DATATYPE>(entity_bt_->getSchemaInfo()[col_index].type),
                        static_cast<DATATYPE>(new_attr.type), is_valid, err_info);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("CheckAlterValid error: table id = %lu",  table_id_);
      return s;
    }
    if (!is_valid) {
      errmsg = err_info.errmsg;
      LOG_WARN("data verification fails: %s", errmsg.c_str());
      return KStatus::FAIL;
    }
    s = alterMetricsTableColumn(ctx, col_index, new_attr);
    if (s != KStatus::SUCCESS) {
      return s;
    }
    for (auto& entity_group : entity_groups_) {
      // Validate the alter operation.
      s = entity_group.second->AlterTable(ctx, AlterType::MODIFY_COLUMN, new_attr);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::alterMetricsTableColumn(kwdbContext_p ctx, int col_index, const AttributeInfo& new_attr) {
  if (entity_bt_->AlterColumnType(col_index, new_attr) < 0) {
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::undoAddColumn(kwdbContext_p ctx, roachpb::KWDBKTSColumn* column) {
  AttributeInfo attr_info;
  ErrorInfo err_info;
  KStatus s = TsEntityGroup::GetColAttributeInfo(ctx, *column, attr_info, false);
  if (s != KStatus::SUCCESS) {
    return s;
  }
  if (attr_info.isAttrType(ATTR_GENERAL_TAG)) {
    // Undo operation of tag column.
    TagInfo new_tag_schema = {column->column_id(), attr_info.type,
                              static_cast<uint32_t>(attr_info.length), 0,
                              static_cast<uint32_t>(attr_info.size),
                              GENERAL_TAG};
    for (const auto& entity_group : entity_groups_) {
      s = entity_group.second->UndoAddTagInfo(ctx, new_tag_schema);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  } else {
    // Undo operation of metrics column.
    s = undoAddMetricsTableColumn(ctx, column->column_id());
    if (s != KStatus::SUCCESS) {
      return s;
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::undoDropColumn(kwdbContext_p ctx, roachpb::KWDBKTSColumn* column) {
  AttributeInfo attr_info;
  ErrorInfo err_info;
  KStatus s = TsEntityGroup::GetColAttributeInfo(ctx, *column, attr_info, false);
  if (s != KStatus::SUCCESS) {
    return s;
  }
  if (attr_info.isAttrType(ATTR_GENERAL_TAG)) {
    // Undo operation of tag column.
    TagInfo old_tag_schema = {column->column_id(), attr_info.type,
                              static_cast<uint32_t>(attr_info.length), 0,
                              static_cast<uint32_t>(attr_info.size),
                              GENERAL_TAG};
    for (const auto& entity_group : entity_groups_) {
      s = entity_group.second->UndoDropTagInfo(ctx, old_tag_schema);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  } else {
    // Undo operation of metric column.
    s = undoDropMetricsTableColumn(ctx, column->column_id());
    if (s != KStatus::SUCCESS) {
      return s;
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::UndoAlterTable(kwdbContext_p ctx, LogEntry* log) {
  auto alter_log = reinterpret_cast<DDLAlterEntry*>(log);
  // Depending on the type, different undo functions are executed
  switch (alter_log->getAlterType()) {
    case WALAlterType::ADD_COLUMN: {
      auto slice = alter_log->getColumnMeta();
      roachpb::KWDBKTSColumn column;
      bool res = column.ParseFromArray(slice.data, slice.len);
      if (!res) {
        LOG_ERROR("Failed to parse the WAL log.")
        return KStatus::FAIL;
      }
      if (undoAddColumn(ctx, &column) == FAIL) {
        return KStatus::FAIL;
      }
      break;
    }
    case WALAlterType::DROP_COLUMN: {
      auto slice = alter_log->getColumnMeta();
      roachpb::KWDBKTSColumn column;
      bool res = column.ParseFromArray(slice.data, slice.len);
      if (!res) {
        LOG_ERROR("Failed to parse the WAL log.")
        return KStatus::FAIL;
      }
      if (undoDropColumn(ctx, &column) == FAIL) {
        return KStatus::FAIL;
      }
      break;
    }
    case ALTER_COLUMN_TYPE: {
      auto slice = alter_log->getColumnMeta();
      roachpb::KWDBKTSColumn column;
      bool res = column.ParseFromArray(slice.data, slice.len);
      if (!res) {
        LOG_ERROR("Failed to parse the WAL log.")
        return KStatus::FAIL;
      }
      if (undoAlterColumnType(ctx, &column) == FAIL) {
        return KStatus::FAIL;
      }
      break;
    }
    case ALTER_PARTITION_INTERVAL: {
      uint64_t interval = 0;
      memcpy(&interval, alter_log->getData(), sizeof(interval));
      if (AlterPartitionInterval(ctx, interval) == FAIL) {
        return KStatus::FAIL;
      }
      break;
    }
  }

  return KStatus::SUCCESS;
}

KStatus TsTable::undoAddMetricsTableColumn(kwdbContext_p ctx, uint32_t  col_id) {
  if (entity_bt_->getColumnIndex(col_id) < 0) {
    LOG_INFO("This node does not contain the specified column.");
    return KStatus::SUCCESS;
  }
  ErrorInfo err_info;
  if (entity_bt_->UndoAddColumn(col_id, err_info) < 0) {
    LOG_ERROR("undoAddColumn failed.");
    return KStatus::FAIL;
  }

  return KStatus::SUCCESS;
}

KStatus TsTable::undoDropMetricsTableColumn(kwdbContext_p ctx, uint32_t  col_id) {
  if (entity_bt_->getColumnIndex(col_id) < 0) {
    LOG_INFO("This node does not contain the specified column.");
    return KStatus::SUCCESS;
  }
  ErrorInfo err_info;
  if (entity_bt_->UndoDropColumn(col_id, err_info) < 0) {
    LOG_ERROR("undoDropColumn failed.");
    return KStatus::FAIL;
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::undoAlterColumnType(kwdbContext_p ctx, roachpb::KWDBKTSColumn* origin_column) {
  AttributeInfo origin_attr_info;
  ErrorInfo err_info;
  KStatus s = TsEntityGroup::GetColAttributeInfo(ctx, *origin_column, origin_attr_info, false);
  if (s != KStatus::SUCCESS) {
    return s;
  }
  getDataTypeSize(origin_attr_info);  // update max_len
  if (origin_attr_info.isAttrType(ATTR_GENERAL_TAG)) {
    // Undo operation of tag column.
    TagInfo origin_tag_schema = {origin_column->column_id(), origin_attr_info.type,
                                 static_cast<uint32_t>(origin_attr_info.length), 0,
                                 static_cast<uint32_t>(origin_attr_info.size),
                                 GENERAL_TAG};
    for (const auto& entity_group : entity_groups_) {
      s = entity_group.second->UndoAlterTagInfo(ctx, origin_tag_schema);
      if (s != KStatus::SUCCESS) {
        return s;
      }
    }
  } else {
    // Undo operation of metric column.
    entity_bt_->rdLock();
    int col_index = entity_bt_->getColumnIndex(origin_attr_info);
    entity_bt_->unLock();
    if (col_index < 0) {
      LOG_ERROR("Undo alter column type failed: column (id %u) not exists, table id = %lu", origin_attr_info.id, table_id_);
      return KStatus::FAIL;
    }
    // alter column type using origin column meta
    s = alterMetricsTableColumn(ctx, col_index, origin_attr_info);
    if (s != KStatus::SUCCESS) {
      LOG_ERROR("undoAlterColumnType failed, table id = %lu",  table_id_);
      return s;
    }
  }
  return KStatus::SUCCESS;
}

KStatus TsTable::AlterPartitionInterval(kwdbContext_p ctx, uint64_t partition_interval) {
  entity_bt_->partitionInterval() = partition_interval;
  return KStatus::SUCCESS;
}

uint64_t TsTable::GetPartitionInterval() {
  return entity_bt_->partitionInterval();
}

void TsTable::SetDropped() {
  entity_bt_->setDropped();
  is_dropped_.store(true);
}

bool TsTable::IsDropped() {
  return is_dropped_.load();
}

KStatus TsEntityGroup::getTagTable(ErrorInfo& err_info) {
  if (tag_bt_ == nullptr) {
    tag_bt_ = OpenTagTable(db_path_, tbl_sub_path_, table_id_, err_info);
  }
  if (tag_bt_ == nullptr) {
    LOG_ERROR("open tag table error : %s", err_info.errmsg.c_str());
    return KStatus::FAIL;
  }
  tag_bt_->mutexLock();
  bool is_valid = tag_bt_->isValid();
  tag_bt_->mutexUnlock();
  if (!is_valid) {
    // avoid droping tag table
    err_info.errcode = -2;
    err_info.errmsg = "tag table status is invalid";
    LOG_ERROR("tag table status is invalid");
    return KStatus::FAIL;
  }
  tag_bt_->incRefCount();
  return KStatus::SUCCESS;
}

void TsEntityGroup::releaseTagTable() {
  if (tag_bt_ == nullptr) {
    LOG_WARN("release tag table object is nullptr.");
    return;
  }
  int ref_cnt = tag_bt_->decRefCount();
  if (ref_cnt <= 1) {
    KW_COND_SIGNAL(tag_bt_->m_ref_cnt_cv_);
  }
}

bool TsEntityGroup::payloadNextSlice(TsSubEntityGroup* sub_group, Payload& payload, timestamp64 last_p_time, int start_row,
                                     int32_t* end_row, timestamp64* cur_p_time) {
  // Inspect whether the initial row falls outside the data range
  if (start_row >= payload.GetRowCount()) {
    return false;
  }
  timestamp64 p_time = 0;

  // Search through the rows in the payload to locate the next partition time with a different timestamp
  for (int32_t row_id = start_row; row_id < payload.GetRowCount(); row_id++) {
    KTimestamp cur_ts_ms = payload.GetTimestamp(row_id);
    KTimestamp cur_ts = cur_ts_ms / 1000;

    timestamp64 max_ts;

    // Calculate the partition time for the current row and compare it with the previous partition time.
    // If there is a difference, it indicates the need to switch partitions.
    // First, return the information of the current partition to the upper layer for writing,
    // and then continue to search for the next partition.
    p_time = sub_group->PartitionTime(cur_ts, max_ts);
    if (p_time != last_p_time) {
      *cur_p_time = p_time;
      *end_row = row_id;
      return true;  // Locate the next valid partition time
    }
  }

  // If no partition time change is found after traversing all rows, set the last row as the end row
  // and return the current timestamp
  *end_row = payload.GetRowCount();
  *cur_p_time = p_time;
  return true;
}

bool TsEntityGroup::findPartitionPayload(TsSubEntityGroup* sub_group, Payload& payload,
                                         std::multimap<timestamp64, PartitionPayload>* partition_map) {
  // Inspect whether the initial row falls outside the data range
  int start_row = payload.GetStartRowId();
  KTimestamp first_ts_ms = payload.GetTimestamp(payload.GetStartRowId());
  KTimestamp first_ts = first_ts_ms / 1000;
  timestamp64 first_max_ts;

  timestamp64 last_p_time = sub_group->PartitionTime(first_ts, first_max_ts);

  if (start_row >= payload.GetRowCount()) {
    return false;
  }
  timestamp64 p_time = 0;
  // Search through the rows in the payload to locate the next partition time with a different timestamp
  for (int32_t row_id = payload.GetStartRowId(); row_id < payload.GetRowCount(); row_id++) {
    KTimestamp cur_ts_ms = payload.GetTimestamp(row_id);
    KTimestamp cur_ts = cur_ts_ms / 1000;

    timestamp64 max_ts;
    // Calculate the partition time for the current row and compare it with the previous partition time.
    // If there is a difference, it indicates the need to switch partitions.
    // First, return the information of the current partition to the upper layer for writing,
    // and then continue to search for the next partition.
    p_time = sub_group->PartitionTime(cur_ts, max_ts);
    if (p_time != last_p_time) {
      partition_map->insert({p_time, {start_row, row_id}});
      start_row = row_id;
    }
  }
  // If no partition time change is found after traversing all rows, set the last row as the end row and
  // return the current timestamp

  auto end_row = payload.GetRowCount();
  partition_map->insert({p_time, {start_row, end_row}});
  return true;
}

KStatus TsEntityGroup::FlushBuffer(kwdbContext_p ctx) {
  return SUCCESS;
}

KStatus TsEntityGroup::CreateCheckpoint(kwdbContext_p ctx) {
  return SUCCESS;
}

}  //  namespace kwdbts
