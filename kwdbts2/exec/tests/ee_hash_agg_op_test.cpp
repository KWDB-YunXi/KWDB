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

#include "ee_exec_pool.h"
#include "ee_iterator_create_test.h"
#include "ee_test_util.h"
#include "ee_kwthd_context.h"
#include "gtest/gtest.h"
#include "th_kwdb_dynamic_thread_pool.h"

string kDbPath = "./test_db";

const string TestBigTableInstance::kw_home =
    kDbPath;  // The current directory is the storage directory of the big table
const string TestBigTableInstance::db_name = "tsdb";  // database name
const uint64_t TestBigTableInstance::iot_interval = 3600;

namespace kwdbts {

class TestAggIterator : public TestBigTableInstance {
 public:
  kwdbContext_t g_kwdb_context;
  kwdbContext_p ctx_ = &g_kwdb_context;

  TestAggIterator() { InitServerKWDBContext(ctx_); }

 protected:
  static void SetUpTestCase() {}

  static void TearDownTestCase() {}

  virtual void SetUp() {
    system(("rm -rf " + kDbPath + "/*").c_str());
    TestBigTableInstance::SetUp();
    // meta_.SetUp(ctx_);
    engine_.SetUp(ctx_, kDbPath, tableid_);
    thd_ = new KWThdContext();
    current_thd = thd_;
    ASSERT_EQ(ExecPool::GetInstance().Init(ctx_), SUCCESS);
    ASSERT_TRUE(current_thd != nullptr);
    parallelGroup_ = KNEW ParallelGroup();
    parallelGroup_->SetDegree(1);
    current_thd->SetParallelGroup(parallelGroup_);
    agg_.SetUp(ctx_, tableid_);
  }

  virtual void TearDown() {
    TestBigTableInstance::TearDown();
    system(("rm -rf " + kDbPath + "/*").c_str());
    agg_.TearDown();
    CloseTestTsEngine(ctx_);
    ExecPool::GetInstance().Stop();
    SafeDelete(thd_);
    delete parallelGroup_;
  }

  //   CreateMeta meta_;
  CreateEngine engine_;
  CreateAggregate agg_;
  KDatabaseId tableid_{10};
  KWThdContext *thd_{nullptr};
  ParallelGroup* parallelGroup_{nullptr};
};

TEST_F(TestAggIterator, AggTest) {
  KStatus ret = KStatus::FAIL;
  BaseOperator *agg = agg_.iter_;
  ASSERT_EQ(agg->Init(ctx_), EE_OK);

  ASSERT_EQ(agg->Start(ctx_), EE_OK);

  DataChunkPtr chunk = nullptr;
  k_int32 j = 2;
  while (j--) {
    if (1 == j) {
      ASSERT_EQ(agg->Next(ctx_, chunk), EE_OK);
    } else {
      ASSERT_EQ(agg->Next(ctx_, chunk), EE_END_OF_RECORD);
    }
  }

  ret = agg->Close(ctx_);
  EXPECT_EQ(ret, KStatus::SUCCESS);
}

}  // namespace kwdbts
