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

#include <string>
#include <vector>
#include "st_wal_types.h"

namespace kwdbts {

class LogEntry {
 public:
  LogEntry() = delete;

  LogEntry(TS_LSN lsn, WALLogType type, uint64_t x_id);

  LogEntry(const LogEntry& entry) = delete;

  virtual ~LogEntry() = default;

  WALLogType getType();

  [[nodiscard]] TS_LSN getLSN() const;

  [[nodiscard]] uint64_t getXID() const;

  virtual char* encode();

  virtual size_t getLen();

  [[nodiscard]] string getTsxID() {
    return std::string{tsx_id_, TS_TRANS_ID_LEN};
  }

  static const size_t LOG_TYPE_SIZE = sizeof(WALLogType);
  static const uint8_t TS_TRANS_ID_LEN = 16;
  constexpr static const char DEFAULT_TS_TRANS_ID[TS_TRANS_ID_LEN] = {};

  virtual void prettyPrint() {}

 protected:
  WALLogType type_;
  TS_LSN lsn_;
  size_t len_;
  uint64_t x_id_;
  char tsx_id_[TS_TRANS_ID_LEN]{};
};

class InsertLogEntry : public LogEntry {
 public:
  InsertLogEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type);

  InsertLogEntry(const InsertLogEntry& entry) = delete;

  ~InsertLogEntry() override = default;

  WALTableType getTableType();

  size_t getLen() override;

 protected:
  WALTableType table_type_;
};

class InsertLogTagsEntry : public InsertLogEntry {
 public:
  int64_t time_partition_;
  uint64_t offset_;
  uint64_t length_;
  char* data_{nullptr};

  InsertLogTagsEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type, int64_t time_partition,
                     uint64_t offset, uint64_t length, char* data);

  InsertLogTagsEntry(const InsertLogTagsEntry& entry) = delete;

  ~InsertLogTagsEntry() override;

  TSSlice getPayload();

  char* encode() override {
    return construct(type_, x_id_, table_type_, time_partition_, offset_, length_, data_);
  }

  size_t getLen() override;

  void prettyPrint() override;

  static const size_t header_length = sizeof(time_partition_) +
                                      sizeof(offset_) +
                                      sizeof(length_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     sizeof(time_partition_) +
                                     sizeof(table_type_) +
                                     sizeof(offset_) +
                                     sizeof(length_);

  static char* construct(const WALLogType type, const uint64_t x_id, const WALTableType table_type,
                         const int64_t time_partition, const uint64_t offset,
                         const uint64_t length, const char* data) {
    size_t len = fixed_length + length;

    char* log_ptr = KNEW char[len];
    size_t location = 0;
    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, &table_type, sizeof(table_type_));
    location += sizeof(table_type_);
    memcpy(log_ptr + location, &time_partition, sizeof(time_partition_));
    location += sizeof(time_partition_);
    memcpy(log_ptr + location, &offset, sizeof(offset_));
    location += sizeof(offset_);
    memcpy(log_ptr + location, &length, sizeof(length_));
    location += sizeof(length_);
    memcpy(log_ptr + location, data, length);

    return log_ptr;
  }
};

class InsertLogMetricsEntry : public InsertLogEntry {
 public:
  int64_t time_partition_;
  uint64_t offset_;
  uint64_t length_;
  char* data_{nullptr};
  size_t p_tag_len_;
  char* encoded_primary_tags_{nullptr};

  InsertLogMetricsEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type, int64_t time_partition,
                        uint64_t offset, uint64_t length, char* data, size_t p_tag_len, char* encoded_primary_tags);

  InsertLogMetricsEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type, int64_t time_partition,
                        uint64_t offset, uint64_t length, size_t p_tag_len, char* data);


  ~InsertLogMetricsEntry() override;

  [[nodiscard]] string getPrimaryTag() const;

  TSSlice getPayload();

  char* encode() override {
    return construct(type_, x_id_, table_type_, time_partition_, offset_, length_, data_, p_tag_len_,
                     encoded_primary_tags_);
  }

  size_t getLen() override;

  void prettyPrint() override;

  static const size_t header_length = sizeof(time_partition_) +
                                      sizeof(offset_) +
                                      sizeof(length_) +
                                      sizeof(p_tag_len_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     sizeof(table_type_) +
                                     sizeof(time_partition_) +
                                     sizeof(offset_) +
                                     sizeof(length_) +
                                     sizeof(p_tag_len_);

  static char* construct(const WALLogType type, const uint64_t x_id, const WALTableType table_type,
                         const uint64_t time_partition, const uint64_t offset,
                         const uint64_t length, const char* data, const size_t p_tag_len,
                         const char* encoded_primary_tags) {
    size_t len = fixed_length + length + p_tag_len;

    char* log_ptr = KNEW char[len];
    size_t location = 0;
    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, &table_type, sizeof(table_type_));
    location += sizeof(table_type_);
    memcpy(log_ptr + location, &time_partition, sizeof(time_partition_));
    location += sizeof(time_partition_);
    memcpy(log_ptr + location, &offset, sizeof(offset));
    location += sizeof(offset);
    memcpy(log_ptr + location, &length, sizeof(length_));
    location += sizeof(length_);
    memcpy(log_ptr + location, &p_tag_len, sizeof(p_tag_len_));
    location += sizeof(p_tag_len_);
    memcpy(log_ptr + location, encoded_primary_tags, p_tag_len);
    location += p_tag_len;
    memcpy(log_ptr + location, data, length);

    return log_ptr;
  }
};

class UpdateLogEntry : public LogEntry {
 public:
  UpdateLogEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type);

  UpdateLogEntry(const UpdateLogEntry& entry) = delete;

  ~UpdateLogEntry() override = default;

  WALTableType getTableType();

  size_t getLen() override;

 protected:
  WALTableType table_type_;
};

class UpdateLogTagsEntry : public UpdateLogEntry {
 public:
  int64_t time_partition_;
  uint64_t offset_;
  uint64_t length_;
  uint64_t old_len_;
  char* data_{nullptr};
  char* old_data_{nullptr};

  UpdateLogTagsEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type, int64_t time_partition,
                     uint64_t offset, uint64_t length, uint64_t old_len, char* data);

  UpdateLogTagsEntry(const UpdateLogTagsEntry& entry) = delete;

  ~UpdateLogTagsEntry() override;

  TSSlice getPayload();

  TSSlice getOldPayload();

  char* encode() override {
    return construct(type_, x_id_, table_type_, time_partition_, offset_, length_, old_len_, data_, old_data_);
  }

  size_t getLen() override;

  void prettyPrint() override;

  static const size_t header_length = sizeof(time_partition_) +
                                      sizeof(offset_) +
                                      sizeof(length_) +
                                      sizeof(old_len_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     sizeof(time_partition_) +
                                     sizeof(table_type_) +
                                     sizeof(offset_) +
                                     sizeof(length_) +
                                     sizeof(old_len_);

  static char* construct(const WALLogType type, const uint64_t x_id, const WALTableType table_type,
                         const int64_t time_partition, const uint64_t offset, const uint64_t length,
                         uint64_t old_len, const char* data, const char* old_data) {
    size_t len = fixed_length + length  + old_len;

    char* log_ptr = KNEW char[len];
    size_t location = 0;
    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, &table_type, sizeof(table_type_));
    location += sizeof(table_type_);
    memcpy(log_ptr + location, &time_partition, sizeof(time_partition_));
    location += sizeof(time_partition_);
    memcpy(log_ptr + location, &offset, sizeof(offset_));
    location += sizeof(offset_);
    memcpy(log_ptr + location, &length, sizeof(length_));
    location += sizeof(length_);
    memcpy(log_ptr + location, &old_len, sizeof(old_len_));
    location += sizeof(old_len_);
    memcpy(log_ptr + location, data, length);
    location += length;
    memcpy(log_ptr + location, old_data, old_len);

    return log_ptr;
  }
};

class DeleteLogEntry : public LogEntry {
 public:
  DeleteLogEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type);

  ~DeleteLogEntry() override = default;

  WALTableType getTableType();

  char* encode() override;

 protected:
  WALTableType table_type_;

 public:
  static const size_t header_length = sizeof(x_id_) + sizeof(table_type_);
};

class DeleteLogMetricsEntry : public DeleteLogEntry {
 public:
  size_t p_tag_len_;
  KTimestamp start_ts_;
  KTimestamp end_ts_;
  uint64_t range_size_;
  char* encoded_primary_tags_{nullptr};
  DelRowSpan* row_spans_;

  DeleteLogMetricsEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type, size_t p_tag_len,
                        uint64_t range_size, char* data);

  ~DeleteLogMetricsEntry() override;

  char* encode() override {
    return construct(type_, x_id_, table_type_, p_tag_len_, start_ts_, end_ts_, range_size_, encoded_primary_tags_,
                     row_spans_);
  }

  size_t getLen() override;

  [[nodiscard]] string getPrimaryTag() const;

  [[nodiscard]] vector<DelRowSpan> getRowSpans() const;

 public:
  static const size_t header_length = sizeof(p_tag_len_) +
                                      sizeof(start_ts_) +
                                      sizeof(end_ts_) +
                                      sizeof(range_size_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     sizeof(table_type_) +
                                     sizeof(p_tag_len_) +
                                     sizeof(start_ts_) +
                                     sizeof(end_ts_) +
                                     sizeof(range_size_);


  static char* construct(const WALLogType type, const uint64_t x_id, const WALTableType table_type,
                         const size_t p_tag_len, const KTimestamp start_ts, const KTimestamp end_ts,
                         const uint64_t range_size, const char* encoded_primary_tags, const DelRowSpan* row_spans) {
    size_t len = fixed_length + (range_size) * sizeof(DelRowSpan) + p_tag_len;

    char* log_ptr = KNEW char[len];
    size_t offset = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    offset += sizeof(type_);
    memcpy(log_ptr + offset, &x_id, sizeof(x_id_));
    offset += sizeof(x_id_);
    memcpy(log_ptr + offset, &table_type, sizeof(table_type_));
    offset += sizeof(table_type_);

    memcpy(log_ptr + offset, &p_tag_len, sizeof(p_tag_len_));
    offset += sizeof(p_tag_len_);
    memcpy(log_ptr + offset, &start_ts, sizeof(start_ts_));
    offset += sizeof(start_ts_);
    memcpy(log_ptr + offset, &end_ts, sizeof(end_ts_));
    offset += sizeof(end_ts_);
    memcpy(log_ptr + offset, &range_size, sizeof(range_size_));
    offset += sizeof(range_size_);

    memcpy(log_ptr + offset, encoded_primary_tags, p_tag_len);
    offset += p_tag_len;

    for (int i = 0; i < range_size; i++) {
      memcpy(log_ptr + offset, &row_spans[i], sizeof(DelRowSpan));
      offset += sizeof(DelRowSpan);
    }
    return log_ptr;
  }
};

class DeleteLogTagsEntry : public DeleteLogEntry {
 public:
  uint32_t group_id_;
  uint32_t entity_id_;
  size_t p_tag_len_;
  size_t tag_len_;
  char* encoded_primary_tags_{nullptr};
  char* encoded_tags_{nullptr};

  DeleteLogTagsEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, WALTableType table_type, uint32_t group_id,
                     uint32_t entity_id, size_t p_tag_len, size_t tag_len, char* encoded_data);

  ~DeleteLogTagsEntry() override;

  char* encode() override {
    return construct(type_, x_id_, table_type_, group_id_, entity_id_, p_tag_len_, encoded_primary_tags_,
                     tag_len_, encoded_tags_);
  }

  size_t getLen() override;

  [[nodiscard]] TSSlice getPrimaryTag() const;

  TSSlice getTags();

 public:
  static const size_t header_length = sizeof(group_id_) + sizeof(entity_id_) + sizeof(p_tag_len_) + sizeof(tag_len_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     sizeof(table_type_) +
                                     sizeof(group_id_) +
                                     sizeof(entity_id_) +
                                     sizeof(p_tag_len_) +
                                     sizeof(tag_len_);

  static char* construct(const WALLogType type, const uint64_t x_id, const WALTableType table_type,
                         const uint32_t group_id, const uint32_t entity_id, const size_t p_tag_len,
                         const char* encoded_primary_tags, const size_t tag_len, const char* encoded_tags) {
    size_t len = fixed_length + p_tag_len + tag_len;

    char* log_ptr = KNEW char[len];
    uint64_t location = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, &table_type, sizeof(table_type_));
    location += sizeof(table_type_);
    memcpy(log_ptr + location, &group_id, sizeof(group_id_));
    location += sizeof(group_id_);
    memcpy(log_ptr + location, &entity_id, sizeof(entity_id_));
    location += sizeof(entity_id_);
    memcpy(log_ptr + location, &p_tag_len, sizeof(p_tag_len_));
    location += sizeof(p_tag_len_);
    memcpy(log_ptr + location, &tag_len, sizeof(tag_len_));
    location += sizeof(tag_len_);

    memcpy(log_ptr + location, encoded_primary_tags, p_tag_len);
    location += p_tag_len;
    memcpy(log_ptr + location, encoded_tags, tag_len);

    return log_ptr;
  }
};

class CheckpointEntry : public LogEntry {
 public:
  CheckpointEntry(TS_LSN lsn, WALLogType type, char* header_data);

  ~CheckpointEntry() override;

  char* encode() override {
    return construct(type_, x_id_, checkpoint_no_, tag_offset_, partition_number_, data_);
  }

  size_t getLen() override;

  [[nodiscard]] size_t getPartitionLen() const;

  void setCheckpointPartitions(char* data);

  void prettyPrint() override;

 private:
  uint32_t checkpoint_no_{};
  uint64_t tag_offset_{};
  uint64_t partition_number_{};
  CheckpointPartition* data_;

  size_t partition_len_{};

 public:
  static const size_t header_length = sizeof(x_id_) +
                                      sizeof(checkpoint_no_) +
                                      sizeof(tag_offset_) + sizeof(partition_number_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     sizeof(checkpoint_no_) +
                                     sizeof(tag_offset_) +
                                     sizeof(partition_number_);

  static char* construct(const WALLogType type, const uint64_t x_id, const uint32_t checkpoint_no,
                         const uint64_t tag_offset, const uint64_t partition_number, const CheckpointPartition* data) {
    uint64_t partition_len = sizeof(CheckpointPartition) * partition_number;
    uint64_t len = fixed_length + partition_len;

    char* log_ptr = KNEW char[len];
    int location = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, &checkpoint_no, sizeof(checkpoint_no_));
    location += sizeof(checkpoint_no_);
    memcpy(log_ptr + location, &tag_offset, sizeof(tag_offset_));
    location += sizeof(tag_offset_);
    memcpy(log_ptr + location, &partition_number, sizeof(partition_number_));
    location += sizeof(partition_number_);

    for (int i = 0; i < partition_number; i++) {
      memcpy(log_ptr + location, &data[i], sizeof(CheckpointPartition));
      location += sizeof(CheckpointPartition);
    }

    return log_ptr;
  }
};

class MTREntry : public LogEntry {
 public:
  MTREntry(TS_LSN lsn, WALLogType type, uint64_t x_id, const char* tsx_id);

  ~MTREntry() override = default;

  char* encode() override {
    return construct(type_, x_id_, tsx_id_);
  }

  void prettyPrint() override;

 public:
  static const size_t fixed_length = sizeof(type_) + sizeof(x_id_) + TS_TRANS_ID_LEN;

  static char* construct(const WALLogType type, const uint64_t x_id, const char* tsx_id) {
    char* log_ptr = KNEW char[fixed_length];

    int location = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, tsx_id, TS_TRANS_ID_LEN);

    return log_ptr;
  }
};

class MTRBeginEntry : public MTREntry {
 public:
  MTRBeginEntry(TS_LSN lsn, uint64_t x_id, const char* tsx_id, uint64_t range_id, uint64_t index);

  ~MTRBeginEntry() override = default;

  char* encode() override;

  [[nodiscard]] uint64_t getRangeID() const;

  [[nodiscard]] uint64_t getIndex() const;

 private:
  uint64_t range_id_;
  uint64_t index_;

 public:
  static const size_t header_length = sizeof(x_id_) + TS_TRANS_ID_LEN + sizeof(range_id_) + sizeof(index_);

  static const size_t fixed_length = sizeof(type_) +
                                     sizeof(x_id_) +
                                     TS_TRANS_ID_LEN +
                                     sizeof(range_id_) +
                                     sizeof(index_);

  static char* construct(const WALLogType type, const uint64_t x_id, const char* tsx_id, const uint64_t range_id,
                         const uint64_t index) {
    char* log_ptr = KNEW char[fixed_length];
    int location = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, tsx_id, TS_TRANS_ID_LEN);
    location += TS_TRANS_ID_LEN;
    memcpy(log_ptr + location, &range_id, sizeof(range_id_));
    location += sizeof(range_id_);
    memcpy(log_ptr + location, &index, sizeof(index_));

    return log_ptr;
  }
};

class TTREntry : public LogEntry {
 public:
  TTREntry(TS_LSN lsn, WALLogType type, uint64_t x_id, char* tsx_id);

  ~TTREntry() override = default;

  char* encode() override {
    return construct(type_, x_id_, tsx_id_);
  };

 public:
  static const size_t fixed_length = sizeof(type_) + sizeof(x_id_) + TS_TRANS_ID_LEN;

  static char* construct(const WALLogType type, uint64_t x_id, const char* ts_trans_id) {
    char* log_ptr = KNEW char[fixed_length];
    int location = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    location += sizeof(type_);
    memcpy(log_ptr + location, &x_id, sizeof(x_id_));
    location += sizeof(x_id_);
    memcpy(log_ptr + location, ts_trans_id, TS_TRANS_ID_LEN);

    return log_ptr;
  }

  static string constructUUID(const char* tsx_id) {
    return std::string{tsx_id, TS_TRANS_ID_LEN};
  }
};

class DDLEntry : public LogEntry {
 public:
  DDLEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, uint64_t object_id);

  ~DDLEntry() override = default;

  [[nodiscard]] uint64_t getObjectID() const;

 protected:
  uint64_t object_id_;
};

class DDLCreateEntry : public DDLEntry {
 public:
  DDLCreateEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, uint64_t object_id, int meta_length,
                 uint64_t range_size, roachpb::CreateTsTable* meta, RangeGroup* ranges);

  explicit DDLCreateEntry(TS_LSN lsn, WALLogType type, char* header_data);

  ~DDLCreateEntry() override;

  char* encode() override {
    return construct(type_, x_id_, object_id_, meta_length_, range_size_, meta_, ranges_);
  }

  size_t getLen() override;

  [[nodiscard]] roachpb::CreateTsTable* getMeta() const;

  [[nodiscard]] int getMetaLength() const;

  [[nodiscard]] size_t getRangeGroupLen() const;

  void setRangeGroups(char* data);

  int meta_length_{0};
  uint64_t range_size_{0};

  roachpb::CreateTsTable* meta_;
  RangeGroup* ranges_;


 public:
  static const size_t head_length = sizeof(x_id_) + sizeof(object_id_) + sizeof(meta_length_) + sizeof(range_size_);

  static const size_t fixed_length = sizeof(type_) + sizeof(x_id_) +
                                     sizeof(object_id_) + sizeof(meta_length_) + sizeof(range_size_);

  static const size_t range_length = sizeof(uint64_t) + sizeof(int8_t);

  static char* construct(WALLogType type, uint64_t x_id, uint64_t object_id, int meta_length,
                         uint64_t range_size, roachpb::CreateTsTable* meta, RangeGroup* ranges) {
    size_t len = fixed_length + (range_size) * range_length + meta_length;
    char* log_ptr = KNEW char[len];
    int offset = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    offset += sizeof(type_);
    memcpy(log_ptr + offset, &x_id, sizeof(x_id_));
    offset += sizeof(x_id_);
    memcpy(log_ptr + offset, &object_id, sizeof(object_id_));
    offset += sizeof(object_id_);
    memcpy(log_ptr + offset, &meta_length, sizeof(meta_length_));
    offset += sizeof(meta_length_);
    memcpy(log_ptr + offset, &range_size, sizeof(range_size_));
    offset += sizeof(range_size_);

    auto* buffer = KNEW char[meta_length];
    meta->SerializeToArray(buffer, meta_length);
    memcpy(log_ptr + offset, buffer, meta_length);

    delete[] buffer;

    offset += meta_length;
    for (int i = 0; i < range_size; i++) {
      memcpy(log_ptr + offset, &ranges[i].range_group_id, sizeof(ranges[i].range_group_id));
      offset += sizeof(ranges[i].range_group_id);
      memcpy(log_ptr + offset, &ranges[i].typ, sizeof(ranges[i].typ));
      offset += sizeof(ranges[i].typ);
    }

    return log_ptr;
  }
};

class DDLDropEntry : public DDLEntry {
 public:
  DDLDropEntry(TS_LSN lsn, WALLogType type, uint64_t x_id, uint64_t object_id);

  ~DDLDropEntry() override = default;

  char* encode() override {
    return construct(type_, x_id_, object_id_);
  }

  size_t getLen() override;

  void prettyPrint() override;

 public:
  static const size_t fixed_length = sizeof(type_) + sizeof(x_id_) + sizeof(object_id_);

  static char* construct(WALLogType type, uint64_t x_id, uint64_t object_id) {
    char* log_ptr = KNEW char[fixed_length];
    int offset = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    offset += sizeof(type_);
    memcpy(log_ptr + offset, &x_id, sizeof(x_id_));
    offset += sizeof(x_id_);
    memcpy(log_ptr + offset, &object_id, sizeof(object_id_));

    return log_ptr;
  }
};

class DDLAlterEntry : public DDLEntry {
 public:
  DDLAlterEntry(TS_LSN lsn, WALLogType type, char* header_data);

  ~DDLAlterEntry() override;

  char* encode() override {
    TSSlice column_meta{data_, length_};
    return construct(type_, x_id_, object_id_, alter_type_, column_meta);
  }

  size_t getLen() override;

  [[nodiscard]] WALAlterType getAlterType() const;

  [[nodiscard]] uint64_t getLength() const;

  [[nodiscard]] char* getData() const;

  [[nodiscard]] TSSlice getColumnMeta() const;

 private:
  WALAlterType alter_type_{1};
  uint64_t length_{0};
  char* data_{nullptr};

 public:
  static const size_t head_length = sizeof(x_id_) + sizeof(object_id_) + sizeof(alter_type_) + sizeof(length_);

  static const size_t fixed_length = sizeof(type_) + sizeof(x_id_)
      + sizeof(object_id_) + sizeof(alter_type_) + sizeof(length_);

  static char* construct(WALLogType type, uint64_t x_id, uint64_t object_id, WALAlterType alter_type, TSSlice& column_meta) {
    char* log_ptr = KNEW char[fixed_length + column_meta.len];
    int offset = 0;

    memcpy(log_ptr, &type, sizeof(type_));
    offset += sizeof(type_);
    memcpy(log_ptr + offset, &x_id, sizeof(x_id_));
    offset += sizeof(x_id_);
    memcpy(log_ptr + offset, &object_id, sizeof(object_id_));
    offset += sizeof(object_id_);
    memcpy(log_ptr + offset, &alter_type, sizeof(alter_type_));
    offset += sizeof(alter_type_);
    memcpy(log_ptr + offset, &column_meta.len, sizeof(length_));
    offset += sizeof(length_);
    memcpy(log_ptr + offset, column_meta.data, column_meta.len);

    return log_ptr;
  }
};

}  // namespace kwdbts