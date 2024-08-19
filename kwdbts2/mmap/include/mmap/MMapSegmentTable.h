// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
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

#include <sys/stat.h>
#include <sys/statfs.h>
#include <linux/magic.h>
#include <payload.h>
#include <EntityMetaManager.h>
#include "DateTime.h"
#include "TSTableMMapObject.h"
#include "utils/ObjectUtils.h"
#include "MMapEntityMeta.h"
#include "ts_common.h"

using namespace std;

class MMapSegmentTable : public TSObject, public TsTableMMapObject {
 private:
  KRWLatch rw_latch_;

 protected:
  string name_;
  BLOCK_ID segment_id_;
  vector<MMapFile*> col_file_;
  // max row that has writen. sync to file while closing, and sync from file while opening.
  std::atomic<size_t> actual_writed_count_{0};
  uint64_t reserved_rows_ = 0;
  // block header size
  vector<uint32_t> col_block_header_size_;
  // datablock size of every column.
  vector<size_t> col_block_size_;
  // varchar or varbinary values store in stringfile, which can remap size.
  MMapStringFile* m_str_file_{nullptr};
  EntityMetaManager* entity_meta_{nullptr};
  // is this segment compressed.
  bool sqfs_is_exists_ = false;

  virtual int addColumnFile(int col, int flags, ErrorInfo& err_info);

  int open_(int char_code, const string& file_path, const std::string& db_path, const string& tbl_sub_path,
            int flags, ErrorInfo& err_info);

  int init(EntityMetaManager* entity_meta, const vector<AttributeInfo>& schema, int encoding, ErrorInfo& err_info);

  int initColumn(int flags, ErrorInfo& err_info);

  int setAttributeInfo(vector<AttributeInfo>& info, int encoding,
                       ErrorInfo& err_info);

  virtual void push_back_payload(kwdbts::Payload* payload, MetricRowID row_id, size_t segment_column,
                                 size_t payload_column, size_t start_row, size_t num);

  int magic() { return *reinterpret_cast<const int *>("MMET"); }

  /**
   * push_var_payload_direct writes variable-length data to the variable-length file: stringfile
   *
   * @param payload Record the data to be written.
   * @param row_id Specify the position of data in the table.
   * @param segment_column Indicate which column the data should be written to.
   * @param payload_column The column corresponding to the payload.
   * @param payload_start_row The starting row number of the payload.
   * @param payload_num The number of data rows to be written.
   * @return Return the operation error code, 0 indicates success, and a negative value indicates failure.
 */
  int push_var_payload_direct(kwdbts::Payload* payload, MetricRowID row_id, size_t segment_column,
                                     size_t payload_column, size_t payload_start_row, size_t payload_num);

  virtual void push_null_bitmap(kwdbts::Payload* payload, MetricRowID row_id, size_t segment_column,
                                size_t payload_column, size_t payload_start_row, size_t payload_num);

  /**
   *  @brief get datablock memory address of certain column, by block index in current segment
   *
   * @param block_idx  block id.
   * @param c          column idx
   * @return Return address
 */
  inline void* internalBlockHeader(BLOCK_ID block_idx, size_t c) const {
    return reinterpret_cast<void*>((intptr_t) col_file_[c]->memAddr() + block_idx * getDataBlockSize(c));
  }

 public:
  MMapSegmentTable();

  virtual ~MMapSegmentTable();

  int rdLock() override;
  int wrLock() override;
  int unLock() override;

  /**
   * @brief sqfs file path of current segment, file no need must exist.
  */
  inline string getSqfsFilePath() {
    string file_path = db_path_ + tbl_sub_path_;
    if (file_path.back() == '/') {
      file_path = file_path.substr(0, file_path.size() - 1);
    }
    file_path += ".sqfs";
    return file_path;
  }

  inline SegmentStatus getSegmentStatus() {
    return static_cast<SegmentStatus>(TsTableMMapObject::status());
  }

  inline void setSegmentStatus(SegmentStatus s_status) {
    TsTableMMapObject::setStatus(s_status);
  }

  /**
   * @brief  check if current segment schema is same with root table.
  */
  void verifySchema(const vector<AttributeInfo>& root_schema, bool& is_consistent);

  /**
   * PushPayload is used to write data into files.
   *
   * @param entity_id entity ID.
   * @param start_row identifies the beginning position where the table should be inserted.
   * @param payload   The data to be written.
   * @return int The result status code returned after the operation, 0 for success, non-zero error codes for failure.
   */
  int PushPayload(uint32_t entity_id, MetricRowID start_row, kwdbts::Payload* payload,
                  size_t start_in_payload, const BlockSpan& span, kwdbts::DedupInfo& dedup_info);

  /**
   * pushToColumn writes data by column, updating the bitmap and aggregate results.
   * Among them, fixed-length data is directly copied, while variable-length data is written to a string file one line at a time,
   * and its offset is recorded in the data block.
   *
   * @param start_row        Start row ID.
   * @param payload_col      Column index where the data is written.
   * @param payload          Data to be written.
   * @param start_in_payload Payload position where writing begins.
   * @param span             Range of data blocks where writing occurs.
   * @param dedup_info       Deduplication information.
   * @return Return error code, 0 indicates success.
   */
  int pushToColumn(MetricRowID start_row, size_t payload_col, kwdbts::Payload* payload,
                   size_t start_in_payload, const BlockSpan& span, kwdbts::DedupInfo& dedup_info);

  /**
   * @brief .data file format
   *         The .data file is divided into blocks of the same size and distributed to different entities.
   * +---------+---------+---------+---------+---------+---------+
   * | Block 1 | Block 2 | Block 3 | Block 4 |   ...   | Block N |
   * +---------+---------+---------+---------+---------+---------+
   *
   *
   * @brief block format
   *        The beginning of each block stores a null bitmap, counting and maximum/minimum/sum statistics information,
   *        followed by config_block_rows data.
   * +-------------+-------+-----+-----+-----+---------------------------------------------------------+
   * | null bitmap | count | max | min | sum |                       column data                       |
   * +-------------+-------+-----+-----+-----+---------------------------------------------------------+
   *
   * (1) null bitmap: entity_meta_->getBlockBitmapSize() bytes
   * (2) count: 2 bytes
   * (3) max/min/sum: column size
   * (4) column data: config_block_rows_ * column size
   *
   */
  inline size_t getDataBlockSize(uint16_t col_idx) const {
    assert(col_idx < col_block_size_.size());
    return col_block_size_[col_idx];
  }

  /**
   * @brief get block header address.
  */
  inline void* getBlockHeader(BLOCK_ID data_block_id, size_t c) const {
    assert(data_block_id > segment_id_);
    return internalBlockHeader(data_block_id - segment_id_, c);
  }

  /**
   * @brief get bitmap address of certain column in block
  */
  inline void* columnNullBitmapAddr(BLOCK_ID block_id, size_t c) const {
    if (isColExist(c)) {
      return getBlockHeader(block_id, c);
    } else {
      return nullptr;
    }
  }

  /**
   * @brief get address of certain agg result in block.
   * @param[in] data_block_id   block id
   * @param[in] agg_type        agg type
   * @return void*  agg result address. nullptr if no this agg result
  */
  inline void* columnAggAddr(BLOCK_ID data_block_id, size_t c, kwdbts::Sumfunctype agg_type) const {
    // Calculate the offset of the address where the agg_type aggregation type is located
    size_t agg_offset = 0;
    switch (agg_type) {
      case kwdbts::Sumfunctype::MAX :
        agg_offset = BLOCK_AGG_COUNT_SIZE;
        break;
      case kwdbts::Sumfunctype::MIN :
        agg_offset = BLOCK_AGG_COUNT_SIZE + hrchy_info_[c].size;
        break;
      case kwdbts::Sumfunctype::SUM :
        agg_offset = BLOCK_AGG_COUNT_SIZE + hrchy_info_[c].size * 2;
        break;
      case kwdbts::Sumfunctype::COUNT :
        break;
      default:
        return nullptr;
    }
    // agg result address: block addr + null bitmap size + agg_type offset
    size_t offset = entity_meta_->getBlockBitmapSize() + agg_offset;
    return reinterpret_cast<void*>((intptr_t) getBlockHeader(data_block_id, c) + offset);
  }

  // store agg result address of certain block and certain column
  struct AggDataAddresses {
    void* count;
    void* min;
    void* max;
    void* sum;
  };

  inline void calculateAggAddr(BLOCK_ID data_block_id, size_t c, AggDataAddresses& addresses) {
    size_t offset = entity_meta_->getBlockBitmapSize();
    addresses.count = reinterpret_cast<void*>((intptr_t) getBlockHeader(data_block_id, c) + offset);
    addresses.max = reinterpret_cast<void*>((intptr_t) addresses.count + BLOCK_AGG_COUNT_SIZE);
    addresses.min = reinterpret_cast<void*>((intptr_t) addresses.max + hrchy_info_[c].size);
    addresses.sum = reinterpret_cast<void*>((intptr_t) addresses.max + hrchy_info_[c].size * 2);
  }

  void setNullBitmap(MetricRowID row_id, size_t c) {
    if (!isColExist(c)) {
      return;
    }
    // 0 ~ config_block_rows_ - 1
    size_t row = row_id.offset_row - 1;
    size_t byte = row >> 3;
    size_t bit = 1 << (row & 7);
    char* bitmap = static_cast<char*>(columnNullBitmapAddr(row_id.block_id, c));
    bitmap[byte] |= bit;
  }

  bool isNullValue(MetricRowID row_id, size_t c) const {
    if (!isColExist(c)) {
      return true;
    }
    // 0 ~ config_block_rows_ - 1
    size_t row = row_id.offset_row - 1;
    size_t byte = row >> 3;
    size_t bit = 1 << (row & 7);
    char* bitmap = static_cast<char*>(columnNullBitmapAddr(row_id.block_id, c));
    return bitmap[byte] & bit;
  }

  // check if column exists, by file and column index.
  inline bool isColExist(size_t idx) const {
    if (idx >= col_file_.size()) {
      return false;
    }
    return col_file_[idx] != nullptr;
  }

  bool isBlockFirstRow(MetricRowID row_id) {
    return row_id.offset_row == 1;
  }

  virtual int swap_nolock(TSObject* other, ErrorInfo& err_info);

  virtual int create(EntityMetaManager* entity_meta, const vector<AttributeInfo>& schema,
                     int encoding, ErrorInfo& err_info);
  /**
   * @brief	open a big object.
   *
   * @param 	url			big object URL to be opened.
   * @param 	flag		option to open a file; O_CREAT to create new file.
   * @return	0 succeed, otherwise -1.
   */
  virtual int open(EntityMetaManager* entity_meta, BLOCK_ID segment_id, const string& file_path, const std::string& db_path,
                   const string& tbl_sub_path, int flags, bool lazy_open, ErrorInfo& err_info);

  virtual int reopen(bool lazy_open, ErrorInfo& err_info);

  virtual int close(ErrorInfo& err_info);

  virtual void sync(int flags);

  virtual int remove();

//  virtual bool isTemporary() const;

  /*--------------------------------------------------------------------
   * data model functions
   *--------------------------------------------------------------------
   */
  virtual uint64_t dataLength() const;

  virtual const vector<AttributeInfo>& getSchemaInfo() const;

  virtual int reserveBase(size_t size);

  virtual int reserve(size_t size);

  virtual int truncate();

  virtual int addColumn(AttributeInfo& col_info, ErrorInfo& err_info);

  // Concurrent insertion optimization requires returning actual records stored in BO
  virtual size_t size() const {
    return actual_writed_count_.load();
  }

  virtual string description() const { return (mem_) ? smartPointerURL(meta_data_->description) : string(""); }

  virtual string URL() const override ;

  virtual timestamp64& minTimestamp() { return meta_data_->min_ts; }

  virtual timestamp64& maxTimestamp() { return meta_data_->max_ts; }

  virtual uint64_t recordSize() const { return meta_data_->record_size; }

  // num of column in this segment
  virtual int numColumn() const { return meta_data_->level; };

  const string & tbl_sub_path() const { return tbl_sub_path_; }

  BLOCK_ID segment_id() const {
    return segment_id_;
  }

  // get actual length of certain row and column
  inline uint16_t varColumnLen(MetricRowID row_id, size_t c) const {
    size_t offset = *reinterpret_cast<uint64_t*>(columnAddr(row_id, c));
    m_str_file_->rdLock();
    char* data = m_str_file_->getStringAddr(offset);
    uint16_t len = *(reinterpret_cast<uint16_t*>(data));
    m_str_file_->unLock();
    return len;
  }

  inline timestamp64 getBlockMinTs(BLOCK_ID block_id) {
    return *reinterpret_cast<timestamp64*>(
        columnAggAddr(block_id, 0, kwdbts::Sumfunctype::MIN));
  }

  inline timestamp64 getBlockMaxTs(BLOCK_ID block_id) {
    return *reinterpret_cast<timestamp64*>(
        columnAggAddr(block_id, 0, kwdbts::Sumfunctype::MAX));
  }

  inline void* columnAddr(MetricRowID row_id, size_t c) const {
    // return: block address + bitmap size + aggs size + count size(2 bytes) + row index in block * column value size
    size_t offset_size = col_block_header_size_[c] + hrchy_info_[c].size * (row_id.offset_row - 1);
    return reinterpret_cast<void*>((intptr_t) internalBlockHeader(row_id.block_id - segment_id_, c) + offset_size);
  }

  // get vartype column value, value is copied from stringfile.
  inline std::shared_ptr<void> varColumnAddr(MetricRowID row_id, size_t c) const {
    size_t offset = *reinterpret_cast<uint64_t*>(columnAddr(row_id, c));
    m_str_file_->rdLock();
    char* data = m_str_file_->getStringAddr(offset);
    uint16_t len = *(reinterpret_cast<uint16_t*>(data));
    void* var_data = std::malloc(len + MMapStringFile::kStringLenLen);
    memcpy(var_data, data, len + MMapStringFile::kStringLenLen);
    std::shared_ptr<void> ptr(var_data, free);
    m_str_file_->unLock();
    return ptr;
  }

  // get vartype column agg result address.
  inline std::shared_ptr<void> varColumnAggAddr(MetricRowID row_id, size_t c, kwdbts::Sumfunctype agg_type) const {
    size_t offset = *reinterpret_cast<uint64_t*>(columnAggAddr(row_id.block_id, c, agg_type));
    m_str_file_->rdLock();
    char* data = m_str_file_->getStringAddr(offset);
    uint16_t len = *(reinterpret_cast<uint16_t*>(data));
    void* var_data = std::malloc(len + MMapStringFile::kStringLenLen);
    memcpy(var_data, data, len + MMapStringFile::kStringLenLen);
    std::shared_ptr<void> ptr(var_data, free);
    m_str_file_->unLock();
    return ptr;
  }

  // get vartype column values address, row start_real_r ~ end_real_r
  // only used in putdata for agg.
  inline std::shared_ptr<void> varColumnAddr(MetricRowID start_real_r, MetricRowID end_real_r, size_t c) const {
    size_t start_offset = *reinterpret_cast<uint64_t*>(columnAddr(start_real_r, c));
    size_t end_offset = *reinterpret_cast<uint64_t*>(columnAddr(end_real_r, c));
    m_str_file_->rdLock();
    char* start_data = m_str_file_->getStringAddr(start_offset);
    char* end_data = m_str_file_->getStringAddr(end_offset);
    uint16_t end_data_len = *(reinterpret_cast<uint16_t*>(end_data)) + MMapStringFile::kStringLenLen;
    size_t total_var_len = (end_offset - start_offset) + end_data_len;

    void* var_data = std::malloc(total_var_len);
    memcpy(var_data, start_data, total_var_len);
    std::shared_ptr<void> ptr(var_data, free);
    m_str_file_->unLock();
    return ptr;
  }

  inline void* columnAddrByBlk(BLOCK_ID block_id, size_t r, size_t c) const {
    uint64_t offset_row = r;
    BLOCK_ID block_idx = block_id - segment_id_;
    size_t offset_size = col_block_header_size_[c] + hrchy_info_[c].size * offset_row;
    return reinterpret_cast<void*>((intptr_t) internalBlockHeader(block_idx, c) + offset_size);
  }

  // get vartype column value addrees
  inline std::shared_ptr<void> varColumnAddrByBlk(BLOCK_ID block_id, size_t r, size_t c) const {
    size_t offset = *reinterpret_cast<uint64_t*>(columnAddrByBlk(block_id, r, c));
    m_str_file_->rdLock();
    char* data = m_str_file_->getStringAddr(offset);
    uint16_t len = *(reinterpret_cast<uint16_t*>(data));
    void* var_data = std::malloc(len + MMapStringFile::kStringLenLen);
    memcpy(var_data, data, len + MMapStringFile::kStringLenLen);
    std::shared_ptr<void> ptr(var_data, free);
    m_str_file_->unLock();
    return ptr;
  }

  // get actual leng of vartype column value
  inline uint16_t varColumnLenByBlk(BLOCK_ID block_id, size_t r, size_t c) const {
    size_t offset = *reinterpret_cast<uint64_t*>(columnAddrByBlk(block_id, r, c));
    m_str_file_->rdLock();
    char* data = m_str_file_->getStringAddr(offset);
    uint16_t len = *(reinterpret_cast<uint16_t*>(data));
    m_str_file_->unLock();
    return len;
  }

  inline AttributeInfo GetActualCol(size_t c) const {
    return hrchy_info_[actual_cols_[c]];
  }

  inline uint32_t GetActualColType(size_t c) const {
    return hrchy_info_[actual_cols_[c]].type;
  }

  inline uint32_t GetActualColIdx(size_t col) const {
    return actual_cols_[col];
  }

  // check if current segment can writing data
  inline bool canWrite() {
    return !sqfs_is_exists_ && getObjectStatus() == OBJ_READY && getSegmentStatus() < InActiveSegment;
  }

  // check if current segment is compressed
  inline bool sqfsIsExists() const {
    return sqfs_is_exists_;
  }

  // check if column values are all null in block.
  inline bool isAllNullValue(BLOCK_ID block_id, size_t count, vector<kwdbts::k_uint32> c) const {
    size_t null_size = (count - 1) / 8 + 1;
    for (auto& col : c) {
      if (!isColExist(col)) {
        continue;
      }
      char* bitmap = static_cast<char*>(getBlockHeader(block_id, col));
      for (int i = 0; i < null_size; ++i) {
        if (i == null_size - 1 && count % 8) {
          for (size_t j = 0 ; j < count % 8 ; ++j) {
            size_t bit = 1 << (j & 7);
            if (!(bitmap[i] & bit)) {
              return false;
            }
          }
        } else {
          if (*(reinterpret_cast<unsigned char*>((intptr_t) bitmap) + i) < 0xFF) {
            return false;
          }
        }
      }
    }
    return true;
  }

  // check if column has valid value in front rows of block.
  inline bool hasValue(MetricRowID start_row, size_t count, size_t c) const {
    if (!isColExist(c)) {
      return false;
    }
    assert(start_row.offset_row > 0);
    // 0 ~ config_block_rows_ - 1
    assert((start_row.offset_row - 1 + count) < entity_meta_->getBlockMaxRows());
    char* bitmap = static_cast<char*>(columnNullBitmapAddr(start_row.block_id, c));
    return !isAllDeleted(bitmap, start_row.offset_row, count);
  }

};

int convertVarToNum(const std::string& str, DATATYPE new_type, char* data, int32_t old_len,
                    ErrorInfo& err_info);

std::shared_ptr<void> convertFixedToVar(DATATYPE old_type, DATATYPE new_type, char* data, ErrorInfo& err_info);