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

#include "ts_sampler_op.h"
#include "ee_scan_op.h"
#include "ee_tag_scan_op.h"
#include "pgcode.h"

namespace kwdbts {

void AssignDataToRow(SampledRow *row, Field *render, bool is_null, KWDBTypeFamily type) {
  if (is_null || !render) {
    row->data = std::nullopt;
    return;
  }

  switch (type) {
    case KWDBTypeFamily::TimestampFamily:
    case KWDBTypeFamily::TimestampTZFamily:
    case KWDBTypeFamily::IntFamily:
    case KWDBTypeFamily::BoolFamily:
    case KWDBTypeFamily::IntervalFamily: {
      k_int64 val = render->ValInt();
      row->data = val;
      break;
    }
    case KWDBTypeFamily::FloatFamily: {
      k_double64 val = render->ValReal();
      row->data = val;
      break;
    }
    case KWDBTypeFamily::StringFamily:
    case KWDBTypeFamily::BytesFamily:
    case KWDBTypeFamily::DateFamily: {
      String val = render->ValStr();
      KString in_str = {val.getptr(), val.length_};
      row->data = in_str;
      break;
    }
    default:
      row->data = nullopt;
  }
}

TsSamplerOperator::TsSamplerOperator(TABLE* table, BaseOperator* input,
                                     int32_t processor_id) : BaseOperator(table, processor_id), input_(input) {}

KStatus TsSamplerOperator::setup(const TSSamplerSpec* tsInfo) {
  KStatus code = FAIL;
  if (tsInfo == nullptr) {
    return code;
  }
  if (tsInfo->sketches_size() > 0) {
    normalCol_sketches_.reserve(tsInfo->sketches_size());
    primary_tag_sketches_.reserve(tsInfo->sketches_size());
    tag_sketches_.reserve(tsInfo->sketches_size());
    sample_size_ = uint32_t(tsInfo->sample_size());

    k_uint32 i {0};
    k_uint32 total_columns {0};
    k_uint32 normalColCount {0};
    for (auto & sk : tsInfo->sketches()) {
      // Currently only supports one algorithm
      if (SketchMethod_HLL_PLUS_PLUS != static_cast<SketchMethods>(sk.sketch_type())) {
        return code;
      }

      SketchSpec tempSpec{
          sk.sketch_type(),
          // precision has to be >= 4 and <= 18
          std::make_shared<Sketch>(14, true),
          sk.generatehistogram(),
          std::make_shared<SampleReservoir>(sample_size_),
          {},
          {},
          0,
          0,
          i
      };
      for (int s = 0; s < sk.col_idx_size(); s++) {
        tempSpec.statsCol_idx.emplace_back(sk.col_idx(s));
        total_columns++;
      }
      for (int s = 0; s < sk.col_type_size(); s++) {
        tempSpec.statsCol_typ.emplace_back(sk.col_type(s));
      }
      ++i;
      // Currently, the normal column, tag column and single PTag use the same sampling method,
      // and the multiple PTag columns are special.
      if (tempSpec.statsCol_typ.empty()) {
        return FAIL;
      }
      switch (tempSpec.statsCol_typ[0]) {
        case NormalCol:
          tempSpec.column_type = roachpb::KWDBKTSColumn::TYPE_DATA;
          normalCol_sketches_.emplace_back(tempSpec);
          normalColCount++;
          continue;
        case PrimaryTag:
          tempSpec.column_type = roachpb::KWDBKTSColumn::TYPE_PTAG;
          if (sk.hasallptag()) {
            primary_tag_sketches_.emplace_back(tempSpec);
          } else {
            normalCol_sketches_.emplace_back(tempSpec);
          }
          continue;
        case Tag:
          tempSpec.column_type = roachpb::KWDBKTSColumn::TYPE_TAG;
          normalCol_sketches_.emplace_back(tempSpec);
          continue;
        default:
          return FAIL;
      }
    }

    // Scan tag table only for tag statistics, so we need to minus normalColCount for all tag col idx.
    for (auto& ss : primary_tag_sketches_) {
      ss.statsCol_idx[0] -= normalColCount;
    }

    for (auto& ss : tag_sketches_) {
      ss.statsCol_idx[0] -= normalColCount;
    }

    rankCol_ = total_columns;
    sketchIdxCol_ = total_columns + 1;
    numRowsCol_ = total_columns + 2;
    numNullsCol_ = total_columns + 3;
    sketchCol_ = total_columns + 4;
    LOG_DEBUG("tsSamplerOperator setup success and table id is %d in create statistics", input_->table()->object_id_);
    code = SUCCESS;
  }
  return code;
}

template<>
EEIteratorErrCode TsSamplerOperator::mainLoop<NormalCol>(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;
  if (!current_thd) {
    Return(code)
  }

  std::vector<byte> buf;
  while (true) {
    code = input_->Next(ctx);
    if (EEIteratorErrCode::EE_OK != code) {
      break;
    }

    RowBatch* row_batch = input_->GetRowBatch(ctx);
    row_batch->ResetLine();
    const k_uint32 lines = row_batch->Count();
    for (k_uint32 line = 0; line < lines; ++line) {
      int i = 0;
      for (auto& sk : normalCol_sketches_) {
        k_uint32 col_idx {0};
        // TODO(zh): for multi-column sketches, we will need to do this for all
        if (!sk.statsCol_idx.empty()) {
          col_idx = sk.statsCol_idx[0];
        } else {
          LOG_ERROR("The collection column is empty in sampling")
          Return(code)
        }

        // Adds a row to the sketch and updates row counts.
        sk.addRow(input_, row_batch);

        // TODO(zh): Optimize reservoir sampling
        if (sk.histogram) {
          SampledRow row{};
          Field* render = input_->GetRender(static_cast<int>(col_idx));
          bool isNull = row_batch->IsNull(render->getColIdxInRs(), normalCol_sketches_[i].column_type);
          AssignDataToRow(&row, render, isNull, outRetrunTypes_[col_idx]);
          // Randomly generated rankings
          row.rank = static_cast<k_uint64>(normalCol_sketches_[i].reservoir->Int63());
          normalCol_sketches_[i].reservoir->SampleRow(row);
        }
        ++i;
      }
      row_batch->NextLine();
    }
  }

  if (code == EE_END_OF_RECORD) {
    total_sample_rows_ += normalCol_sketches_.size();
    for (auto& sk : normalCol_sketches_) {
      if (sk.histogram) {
        total_sample_rows_ += sk.reservoir->GetSampleSize();
      }
    }
  } else {
    LOG_ERROR("scanning normal column data fails in %u table during statistics collection", input_->table()->object_id_)
    EEPgErrorInfo::SetPgErrorInfo(ERRCODE_FETCH_DATA_FAILED,
                                  "scanning normal column data fail during statistics collection");
    Return(EE_ERROR);
  }

  Return(EE_Sample);
}

template<>
EEIteratorErrCode TsSamplerOperator::mainLoop<Tag>(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode code = EEIteratorErrCode::EE_ERROR;
  if (!current_thd) {
    Return(code)
  }
  auto* tableScanOp = dynamic_cast<TableScanOperator*>(input_);
  if (!tableScanOp) {
    Return(code)
  }

  BaseOperator* tagOp = tableScanOp->GetInput();
  if (!tagOp) {
    Return(code)
  }
  auto* tagScanOp = dynamic_cast<TagScanOperator*>(tagOp);
  if (!tagScanOp) {
    Return(code)
  }

  RowBatch* row_batch;
  while (true) {
    DataChunkPtr chunk = nullptr;
    code = tagScanOp->Next(ctx, chunk);
    if (EEIteratorErrCode::EE_OK != code) {
      break;
    }

    row_batch = tagScanOp->GetRowBatch(ctx);
    row_batch->ResetLine();
    const k_uint32 lines = row_batch->Count();
    if (lines > 0) {
      for (k_uint32 line = 0; line < lines; ++line) {
        for (auto & sk : primary_tag_sketches_) {
          if (sk.statsCol_idx.empty()) {
            LOG_ERROR("The collection column is empty in sampling primary tag columns")
            Return(code)
          }
          // Adds a row to the sketch and updates row counts.
          sk.addRow(tagScanOp, row_batch);
        }
        for (auto& sk : tag_sketches_) {
          if (sk.statsCol_idx.empty()) {
            LOG_ERROR("The collection column is empty in sampling tag columns")
            Return(code)
          }

          // Adds a row to the sketch and updates row counts.
          sk.addRow(tagScanOp, row_batch);
        }

        row_batch->NextLine();
      }
    }
  }

  if (code == EE_END_OF_RECORD) {
    total_sample_rows_ += primary_tag_sketches_.size() + tag_sketches_.size();;
    if (!normalCol_sketches_.empty()) {
      tagScanOp->Reset(ctx);
      code = tagScanOp->Start(ctx);
      if (EEIteratorErrCode::EE_OK != code) {
        Return(code)
      }
    }
  } else {
    LOG_ERROR("scanning tag columns data fails in %u table during statistics collection",
                input_->table()->object_id_)
    EEPgErrorInfo::SetPgErrorInfo(ERRCODE_FETCH_DATA_FAILED,
                                  "scanning tag columns data fail during statistics collection");
    Return(EE_ERROR);
  }

  Return(EE_Sample);
}

void TsSamplerOperator::AddData(const vector<optional<DataVariant>>& row_data, DataChunkPtr& chunk) {
  if (row_data.size() != chunk->ColumnNum()) {
    LOG_ERROR("out row size exceeds elements with table %d during getting sample result in create statistics",
              input_->table()->object_id_)
    return;
  }

  chunk->AddCount();
  k_int32 row = chunk->NextLine();
  if (row < 0) {
    return;
  }

  for (size_t col = 0; col < row_data.size(); ++col) {
    // Handling of null values
    if (!row_data[col].has_value()) {
      chunk->SetNull(row, col);
      continue;
    }

    switch (outStorageTypes_[col]) {
      case roachpb::DataType::TIMESTAMPTZ:
      case roachpb::DataType::TIMESTAMP:
      case roachpb::DataType::DATE:
      case roachpb::DataType::BIGINT: {
        const k_int64* val = std::get_if<k_int64>(&row_data[col].value());
        if (val != nullptr) {
          k_int64* mem = const_cast<k_int64*>(val);
          chunk->InsertData(row, col, reinterpret_cast<char*>(mem), sizeof(k_int64));
        }
        break;
      }
      case roachpb::DataType::SMALLINT: {
        const k_int64* val = std::get_if<k_int64>(&row_data[col].value());
        if (val != nullptr) {
          k_int16 val16 = *val;
          chunk->InsertData(row, col, reinterpret_cast<char*>(&val16), sizeof(k_int16));
        }
        break;
      }
      case roachpb::DataType::INT: {
        const k_int64* val = std::get_if<k_int64>(&row_data[col].value());
        if (val != nullptr) {
          k_int32 val32 = *val;
          chunk->InsertData(row, col, reinterpret_cast<char*>(&val32), sizeof(k_int32));
        }
        break;
      }
      case roachpb::DataType::BOOL: {
        const k_int64* val = std::get_if<k_int64>(&row_data[col].value());
        if (val != nullptr) {
          bool valbool = *val;
          chunk->InsertData(row, col, reinterpret_cast<char*>(&valbool), sizeof(bool));
        }
        break;
      }
      case roachpb::DataType::DOUBLE: {
        const k_double64* val = std::get_if<k_double64>(&row_data[col].value());
        if (val != nullptr) {
          k_double64* mem = const_cast<k_double64*>(val);
          chunk->InsertData(row, col, reinterpret_cast<char*>(mem), sizeof(k_double64));
        }
        break;
      }
      case roachpb::DataType::CHAR:
      case roachpb::DataType::NCHAR:
      case roachpb::DataType::BINARY:
      case roachpb::DataType::NVARCHAR:
      case roachpb::DataType::VARCHAR:
      case roachpb::DataType::VARBINARY: {
        const std::string* val = std::get_if<std::string>(&row_data[col].value());
        if (val != nullptr) {
          char* mem = const_cast<char*>(val->c_str());
          chunk->InsertData(row, col, mem, val->length());
        }
        break;
      }
      default:
        break;
    }
  }
}

EEIteratorErrCode TsSamplerOperator::Init(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode ret = EEIteratorErrCode::EE_ERROR;
  do {
    // Pre-initialize lower-level operators
    ret = input_->Init(ctx);
    if (EEIteratorErrCode::EE_OK != ret) {
      break;
    }

    Field** renders = input_->GetRender();
    k_uint32 num = input_->GetRenderSize();
    // Resolve outCols type and store length
    for (int i = 0; i < num; ++i) {
      if (!renders[i]) {
        Return(EE_ERROR)
      }
      outRetrunTypes_.emplace_back(renders[i]->get_return_type());
      outStorageTypes_.emplace_back(renders[i]->get_storage_type());
      outLens_.emplace_back(renders[i]->get_storage_length());
    }
    // Append extra columns
    // Rank col / SketchIdx col / NumRows col / NullRows col
    for (int i = 0; i < 4; i++) {
      outRetrunTypes_.emplace_back(KWDBTypeFamily::IntFamily);
      outStorageTypes_.emplace_back(roachpb::DataType::BIGINT);
      outLens_.emplace_back(8);
    }
    // Sketch value Col
    outRetrunTypes_.emplace_back(KWDBTypeFamily::BytesFamily);
    outStorageTypes_.emplace_back(roachpb::DataType::VARBINARY);
    // The maximum byte length used for different value calculations
    outLens_.emplace_back(MAX_SKETCH_LEN);
  } while (false);

  Return(ret);
}


EEIteratorErrCode TsSamplerOperator::Start(kwdbContext_p ctx) {
  EnterFunc();
  EEIteratorErrCode ret = EEIteratorErrCode::EE_ERROR;
  KStatus code;
  do {
    ret = input_->Start(ctx);
    if (EEIteratorErrCode::EE_OK != ret) {
      break;
    }
  } while (false);

  Return(ret);
}

EEIteratorErrCode TsSamplerOperator::Next(kwdbContext_p ctx, DataChunkPtr& chunk) {
  EnterFunc();
  LOG_DEBUG("start collecting timeseries table %d statistics", input_->table()->object_id_);
  LOG_DEBUG("normal columns num: %zu; primary key columns num: %zu", normalCol_sketches_.size(),
             primary_tag_sketches_.size());
  if (!current_thd) {
    Return(EE_ERROR)
  }

  if (is_done_) {
    Return(EE_END_OF_RECORD)
  }

  auto processSketches = [&](statsColType colType) {
    switch (colType) {
      case NormalCol:
        return mainLoop<NormalCol>(ctx);
      case Tag:
        return mainLoop<Tag>(ctx);
      default:
        return EE_ERROR;
    }
  };

  // Collect tag column's statistic
  if ((!primary_tag_sketches_.empty() || !tag_sketches_.empty()) && processSketches(Tag) != EE_Sample) Return(EE_ERROR);

  // Collect normal column's statistic
  if (!normalCol_sketches_.empty() && processSketches(NormalCol) != EE_Sample) Return(EE_ERROR);

  KStatus ret = GetSampleResult(ctx, chunk);
  if (ret != SUCCESS) {
    Return(EE_ERROR);
  }

  is_done_ = true;
  LOG_DEBUG("complete collecting timeseries table %d statistics", input_->table()->object_id_);
  Return(EE_OK);
}

KStatus TsSamplerOperator::GetSampleResult(kwdbContext_p ctx, DataChunkPtr& chunk) {
  EnterFunc();

  if (chunk == nullptr) {
    // Initializes the column information
    std::vector<ColumnInfo> col_info;
    col_info.reserve(outRetrunTypes_.size());
    for (int i = 0; i < outRetrunTypes_.size(); i++) {
      col_info.emplace_back(outLens_[i], outStorageTypes_[i], outRetrunTypes_[i]);
    }

    chunk = std::make_unique<DataChunk>(col_info, total_sample_rows_);
    if (chunk->Initialize() != true) {
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_OUT_OF_MEMORY, "Insufficient memory");
      chunk = nullptr;
      Return(KStatus::FAIL);
    }
  }

  // Process different types of sketches
  if (SUCCESS != ProcessSketches(ctx, normalCol_sketches_, chunk)) {
    Return(FAIL)
  }
  if (SUCCESS != ProcessSketches(ctx, tag_sketches_, chunk)) {
    Return(FAIL)
  }
  if (SUCCESS != ProcessSketches(ctx, primary_tag_sketches_, chunk)) {
    Return(FAIL)
  }

  LOG_DEBUG("sampling result set is collected successes");
  Return(SUCCESS);
}

KStatus TsSamplerOperator::ProcessSketches(kwdbContext_p ctx, const std::vector<SketchSpec>& sketches, DataChunkPtr& chunk) {
  EnterFunc();

  std::vector<byte> sketchVal;
  for (auto& sk : sketches) {
    if (sk.histogram) {
      vector<SampledRow> SampledRows = sk.reservoir->GetSamples();
      for (const auto& SampledRow : SampledRows) {
        std::vector<optional<DataVariant>> out_row;
        out_row.resize(outRetrunTypes_.size(), std::nullopt);
        out_row[sk.statsCol_idx[0]] = SampledRow.data;
        if (SampledRow.rank < kInt64Max) {
          out_row[rankCol_] = static_cast<k_int64>(SampledRow.rank);
        } else {
          out_row[rankCol_] = kInt64Max;
        }
        AddData(out_row, chunk);
      }
    }
    std::vector<optional<DataVariant>> out_row;
    out_row.resize(outRetrunTypes_.size(), std::nullopt);
    out_row[sketchIdxCol_] = static_cast<k_int64>(sk.sketchIdx);
    out_row[numRowsCol_] = static_cast<k_int64>(sk.numRows);
    out_row[numNullsCol_] = static_cast<k_int64>(sk.numNulls);
    sketchVal = sk.sketch->MarshalBinary();
    if (sketchVal.size() > MAX_SKETCH_LEN) {
      // Avoid over length
      LOG_ERROR("sketch column over length when scanning table %d", input_->table()->object_id_)
      char buffer[256];
      snprintf(buffer, sizeof(buffer), "sketch column %u over length during statistics collection. ", sk.sketchIdx);
      EEPgErrorInfo::SetPgErrorInfo(ERRCODE_NUMERIC_VALUE_OUT_OF_RANGE, buffer);
      Return(FAIL)
    }
    if (!sketchVal.empty()) {
      std::string sketchStr(sketchVal.begin(), sketchVal.end());
      out_row[sketchCol_] = sketchStr;
    }
    AddData(out_row, chunk);
  }

  Return(SUCCESS)
}

KStatus TsSamplerOperator::Close(kwdbContext_p ctx) {
  EnterFunc();
  KStatus ret = input_->Close(ctx);
  Reset(ctx);

  Return(ret);
}

k_uint32 TsSamplerOperator::GetSampleSize() const {
  return sample_size_;
}

template<>
std::vector<SketchSpec> TsSamplerOperator::GetSketches<NormalCol>() const {
  return normalCol_sketches_;
}

template<>
std::vector<SketchSpec> TsSamplerOperator::GetSketches<Tag>() const {
  return tag_sketches_;
}

template<>
std::vector<SketchSpec> TsSamplerOperator::GetSketches<PrimaryTag>() const {
  return primary_tag_sketches_;
}

EEIteratorErrCode TsSamplerOperator::Reset(kwdbContext_p ctx) {
  EnterFunc();
  input_->Reset(ctx);

  Return(EEIteratorErrCode::EE_OK)
}

void SketchSpec::addRow(BaseOperator* input, RowBatch* data_handle) {
  numRows++;
  bool isNull {false};
  k_uint32 colIdx {0};
  std::vector<byte> buf;
  Field* render {nullptr};
  bool useFastPath {false};

  if (statsCol_idx.size() == 1) {
    colIdx = statsCol_idx[0];

    render = input->GetRender(static_cast<int>(colIdx));
    isNull = data_handle->IsNull(render->getColIdxInRs(), column_type);
    if (isNull) {
      numNulls++;
      buf.push_back(ValuesEncoding::encodedNull);
      sketch->Insert(buf);
      return;
    }
    useFastPath = (render->get_return_type() == KWDBTypeFamily::IntFamily ||
        render->get_return_type() == KWDBTypeFamily::BoolFamily ||
        render->get_return_type() == KWDBTypeFamily::TimestampTZFamily ||
        render->get_return_type() == KWDBTypeFamily::TimestampFamily) && !isNull;
  }

  if (useFastPath) {
    k_int64 val = render->ValInt();
    PutUint64LittleEndian(&buf, static_cast<u_int64_t>(val));
    sketch->Insert(buf);
    return;
  } else {
    isNull = true;
    for (auto idx :  statsCol_idx) {
      render = input->GetRender(static_cast<int>(idx));
      if (isNull) {
        isNull = data_handle->IsNull(render->getColIdxInRs(), column_type);
      }
      buf = EncodeBytes(render, isNull, buf);
    }
    if (isNull) {
      numNulls++;
    }
    sketch->Insert(buf);
  }
}

std::vector<byte> EncodeBytes(Field* render, bool isNull, std::vector<byte>& appendTo) {
  if (render == nullptr) {
    LOG_ERROR("scanning the column in the fields is nullptr in create statistics")
    return appendTo;
  }

  if (isNull) {
    appendTo.push_back(ValuesEncoding::encodedNull);
    return appendTo;
  }
  switch (render->get_return_type()) {
    case KWDBTypeFamily::BoolFamily:
    case KWDBTypeFamily::IntFamily:
    case KWDBTypeFamily::TimestampTZFamily:
    case KWDBTypeFamily::TimestampFamily: {
      k_int64 val = render->ValInt();
      appendTo = EncodeVarintAscending(appendTo,  val);
      break;
    }
    case KWDBTypeFamily::FloatFamily: {
      k_float64 val = render->ValReal();
      appendTo = EncodeFloatAscending(appendTo, static_cast<k_float64>(val));
      break;
    }
    case KWDBTypeFamily::StringFamily:
    case KWDBTypeFamily::BytesFamily: {
      String val = render->ValStr();
      KString in_str = {val.getptr(), val.length_};
      appendTo = EncodeStringAscending(appendTo, in_str);
      break;
    }
    default:
      break;
  }
  return appendTo;
}

}   // namespace kwdbts
