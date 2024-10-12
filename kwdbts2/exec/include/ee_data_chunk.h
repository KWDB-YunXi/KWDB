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

#include <map>
#include <memory>
#include <numeric>
#include <queue>
#include <vector>

#include "ee_data_container.h"
#include "cm_kwdb_context.h"
#include "ee_encoding.h"
#include "ee_executor.h"
#include "ee_field.h"
#include "ee_field_func.h"
#include "ee_global.h"
#include "ee_row_batch.h"
#include "ee_string_info.h"
#include "kwdb_type.h"
#include "me_metadata.pb.h"

namespace kwdbts {

/**
 * The data chunk class is the intermediate representation used by the execution
 * engine. DataChunk is initialized by the operators who needs to send data to
 * the father operators. For example:
 *           .----------------------.
 *           |    Synchronizer Op   |
 *           .----------------------.
 *                      ^
 *                      |  DataChuck
 *                      |
 *           .----------------------.
 *           |     TableScan Op     |
 *           .----------------------.
 *                      ^
 *                      |  Batch
 *                      |
 *             +-----------------+
 *             |     Storage     |
 *             +-----------------+
 *
 * Data in the chunk is organized in column format as following example. In
 * addition to holding the data, the DataChuck also owns columns' type/length
 * information and calculates the column offsets during initialization.
 *    - extra 2 bytes for strings column to keep string length
 *    - reserves maximum space for varchar/varbytes column. (Enhancement in the
 * future)
 *    - null bitmap at the begin of each row, length = (column_num + 7) / 8
 *
 * Notes: Originally DataChunk class is the base class. DataContainer leverages
 * on Datachunk to provide sorting capability. But the class hierarchy is
 * changed after DiskRowContainer implementation.
 */
class DataChunk : public IChunk {
 public:
  /* Constructor & Deconstructor */
  explicit DataChunk(vector<ColumnInfo>& column_info, k_uint32 capacity = 0);

  explicit DataChunk(vector<ColumnInfo>& column_info, const char* buf,
                     k_uint32 count, k_uint32 capacity);

  virtual ~DataChunk();
  /**
   * @return return false if memory allocation fails
   */
  k_bool Initialize();

  /* Getter && Setter */
  [[nodiscard]] inline k_uint32 ColumnNum() const { return col_num_; }

  [[nodiscard]] inline k_uint32 RowSize() const { return row_size_; }

  [[nodiscard]] inline char* GetData() const { return data_; }

  std::vector<ColumnInfo>& GetColumnInfo() override { return col_info_; }

  [[nodiscard]] inline k_uint32 Capacity() const { return capacity_; }

  [[nodiscard]] bool isDisorder() const { return disorder_; }

  void setDisorder(bool disorder) { disorder_ = disorder; }

  /* override methods */
  DatumPtr GetData(k_uint32 row, k_uint32 col) override;

  DatumPtr GetData(k_uint32 col) override;

  k_int32 NextLine() override;

  k_uint32 Count() override;

  bool IsNull(k_uint32 row, k_uint32 col) override;

  bool IsNull(k_uint32 col) override;

  virtual KStatus Append(DataChunk* chunk);

  virtual KStatus Append(std::queue<DataChunkPtr>& buffer);

  // Append all columns whose row number are in [begin_row, end_row)
  KStatus Append(DataChunk* chunk, k_uint32 begin_row, k_uint32 end_row);

  ////////////////   Basic Methods   ///////////////////

  /**
   * @brief Check if the datachunk is full
   */
  [[nodiscard]] inline bool isFull() const { return count_ == capacity_; }

  /**
   * @brief increase the count
   */
  void AddCount() { ++count_; }

  /**
   * @brief reset current read line
   */
  void ResetLine();

  /**
   * @brief Set null at (row, col)
   * @param[in] row
   * @param[in] col
   */
  void SetNull(k_uint32 row, k_uint32 col);

  /**
   * @brief Set not null at (row, col)
   * @param[in] row
   * @param[in] col
   */
  void SetNotNull(k_uint32 row, k_uint32 col);

  /**
   * @brief Set all fields null in the data chunk
   */
  void SetAllNull();

  /**
   * @brief Get string pointer at  (row, col), and return the
   * string length
   * @param[in] row
   * @param[in] col
   * @param[in/out] string length
   */
  DatumPtr GetData(k_uint32 row, k_uint32 col, k_uint16& len) override;

  ////////////////   Insert/Copy Data   ///////////////////

  /**
   * @brief Insert data into location at (row, col)
   * @param[in] row
   * @param[in] col
   * @param[in] value data pointer to insert
   * @param[in] len data length
   */
  KStatus InsertData(k_uint32 row, k_uint32 col, DatumPtr value, k_uint16 len);

  /**
   * @brief Insert one row from value or renders
   * @param[in] ctx
   * @param[in] value
   * @param[in] renders
   */
  KStatus InsertData(kwdbContext_p ctx, IChunk* value, Field** renders);

  /**
   * @brief Insert data into location at (row, col). Expected return
   * type of the column is KWDBTypeFamily::DecimalFamily, however there is no
   * primitive decimal in C++. We use mixed double64/int64 as
   * workaround and an extra bool value indicates whether it is a double.
   *
   * For example: to sum up all values in an int64 column, if the result is
   * larger than int64 max value (9223372036854775807), the column type casts to
   * double.
   * @param[in] row
   * @param[in] col
   * @param[in] value data pointer to insert
   * @param[in] is_double whether it's a double64 value or int64 value
   */
  KStatus InsertDecimal(k_uint32 row, k_uint32 col, DatumPtr value,
                        k_bool is_double);

  /**
   * @brief Copy data from another data chunk
   * @param[in] other
   * @param[in] begin
   * @param[in] end
   */
  void CopyFrom(std::unique_ptr<DataChunk>& other, k_uint32 begin,
                k_uint32 end) {
    count_ = end - begin + 1;
    size_t batch_buf_length = other->RowSize() * count_;
    size_t offset = begin * RowSize();
    memcpy(data_ + offset, other->GetData(), batch_buf_length);
  }

  /**
   * @brief Copy current line using row mode
   * @param[in] ctx
   * @param[in] renders
   */
  void AddRecordByRow(kwdbContext_p ctx, RowBatch* row_batch, k_uint32 col, Field* field);

  /**
 * @brief Copy current line using column mode
 * @param[in] ctx
 * @param[in] renders
 */
  KStatus AddRecordByColumn(kwdbContext_p ctx, RowBatch* row_batch, Field** renders);

  /**
   * @brief Copy all data from the RowBatch
   * @param[in] ctx
   * @param[in] row_batch
   * @param[in] renders
   */
  KStatus AddRowBatchData(kwdbContext_p ctx, RowBatch* row_batch,
                          Field** renders, bool batch_copy = false);

  ////////////////   Encoding func  ///////////////////

  /**
   * @brief Encode data at coordinate location (row, col) using kwbase protocol.
   * @param[in] ctx
   * @param[in] row
   * @param[in] col
   * @param[in] info
   */
  KStatus EncodingValue(kwdbContext_p ctx, k_uint32 row, k_uint32 col,
                        const EE_StringInfo& info);

  /**
   * @brief Encode one row using pgwire protocol.
   * @param[in] ctx
   * @param[in] row
   * @param[in] info
   */
  KStatus PgResultData(kwdbContext_p ctx, k_uint32 row,
                       const EE_StringInfo& info);

  // add data of analyse to chunk
  KStatus AddAnalyse(kwdbContext_p ctx, int32_t processor_id, int64_t duration,
                     int64_t read_row_num, int64_t bytes_read,
                     int64_t max_allocated_mem, int64_t max_allocated_disk);

  // get data of analyse from chunk
  KStatus GetAnalyse(kwdbContext_p ctx);

  /**
   * @brief Encode decimal value (actually double64 or int64) using kwbase
   * protocol.
   * @param[in] raw
   * @param[in] info
   */
  template<typename T>
  void EncodeDecimal(DatumPtr raw, const EE_StringInfo& info) {
    T val;
    std::memcpy(&val, raw, sizeof(T));
    if constexpr (std::is_floating_point<T>::value) {
      // encode floating number
      k_int32 len = ValueEncoding::EncodeComputeLenFloat(0);
      KStatus ret = ee_enlargeStringInfo(info, len);
      if (ret != SUCCESS) {
        return;
      }
      CKSlice slice;
      slice.data = info->data + info->len;
      slice.len = len;
      ValueEncoding::EncodeFloatValue(&slice, 0, val);
      info->len = info->len + len;
    } else {
      k_int32 len = ValueEncoding::EncodeComputeLenInt(0, val);
      KStatus ret = ee_enlargeStringInfo(info, len);
      if (ret != SUCCESS) {
        return;
      }
      CKSlice slice;
      slice.data = info->data + info->len;
      slice.len = len;
      ValueEncoding::EncodeIntValue(&slice, 0, val);
      info->len = info->len + len;
    }
  }

  /**
   * @brief Encode decimal value (actually double64 or int64) using pgwire
   * protocol.
   * @param[in] raw
   * @param[in] info
   */
  template<typename T>
  KStatus PgEncodeDecimal(DatumPtr raw, const EE_StringInfo& info) {
    T val;
    std::memcpy(&val, raw, sizeof(T));

    if constexpr (std::is_same_v<T, k_int64>) {
      k_int64 data = val;
      char val_char[32];
      snprintf(val_char, sizeof(val_char), "%ld", data);

      // Write the length of the column value
      if (ee_sendint(info, strlen(val_char), 4) != SUCCESS) {
        return FAIL;
      }
      // Write the string form of the column value
      if (ee_appendBinaryStringInfo(info, val_char, strlen(val_char)) !=
          SUCCESS) {
        return FAIL;
      }
    } else {
      k_char buf[30] = {0};
      double d = static_cast<double>(val);
      k_int32 n = snprintf(buf, sizeof(buf), "%.8g", d);

      // Write the length of the column value
      if (ee_sendint(info, n, 4) != SUCCESS) {
        return FAIL;
      }
      // Write the string form of the column value
      if (ee_appendBinaryStringInfo(info, buf, n) != SUCCESS) {
        return FAIL;
      }
    }

    return SUCCESS;
  }

  //  use to limit the return size in Next functions.
  static const int SIZE_LIMIT = ROW_BUFFER_SIZE;
  static const int MIN_CAPACITY = 1;

  static k_uint32 EstimateCapacity(vector<ColumnInfo>& column_info);

  static k_uint32 ComputeRowSize(vector<ColumnInfo>& column_info);

 protected:
  bool is_data_owner_{true};
  char* data_{nullptr};  // Multiple rows of column data（not tag）
  std::vector<ColumnInfo> col_info_;  // column info
  std::vector<k_uint32> col_offset_;  // column offset
  std::vector<k_uint32> bitmap_offset_;  // bitmap offset

  k_uint32 capacity_{0};     // data capacity
  k_uint32 count_{0};        // total number
  k_uint32 bitmap_size_{0};  // length of bitmap
  k_uint32 row_size_{0};     // the total length of one row
  k_bits32 col_num_{0};      // the number of col

  k_int32 current_line_{-1};  // current row

 private:
  bool disorder_{false};
};

}  //  namespace kwdbts
