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

#include <mmap/MMapTagColumnTableAux.h>
#include <map>
#include <vector>
#include <string>
#include <unordered_map>
#include <memory>
#include <utility>
#include "ts_table.h"
#include "st_logged_entity_group.h"
#include "mmap/MMapTagColumnTable.h"

namespace kwdbts {
class LoggedTsTable : public TsTable {
 public:
  LoggedTsTable(kwdbContext_p ctx, const string& db_path, const KTableKey& table_id, EngineOptions* opt);

  ~LoggedTsTable() override;

  KStatus Init(kwdbContext_p ctx, std::unordered_map<uint64_t, int8_t>& range_groups,
               ErrorInfo& err_info = getDummyErrorInfo()) override;

  KStatus FlushBuffer(kwdbContext_p ctx) override;

  KStatus CreateCheckpoint(kwdbContext_p ctx) override;

  KStatus Recover(kwdbContext_p ctx, const std::map<uint64_t, uint64_t>& applied_indexes) override;

 protected:
  void constructEntityGroup(kwdbContext_p ctx,
                            const RangeGroup& hash_range,
                            const string& range_tbl_sub_path,
                            std::shared_ptr<TsEntityGroup>* entity_group) override {
    auto t_range = std::make_shared<LoggedTsEntityGroup>(ctx, entity_bt_, db_path_, table_id_, hash_range,
                                                         range_tbl_sub_path, engine_opt_);
    *entity_group = std::move(t_range);
  }

 private:
  EngineOptions* engine_opt_{nullptr};
  using LoggedTsTableRWLatch = KRWLatch;
  LoggedTsTableRWLatch* logged_tstable_rwlock_;
};
}  // namespace kwdbts