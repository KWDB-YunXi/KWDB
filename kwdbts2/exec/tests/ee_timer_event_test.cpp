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
#include "ee_timer_event.h"

#include <iostream>
#include <memory>
#include <mutex>

#include "gtest/gtest.h"
#include "th_kwdb_dynamic_thread_pool.h"
namespace kwdbts {
extern kwdbContext_p g_kwdb_context;
kwdbContext_p ctx_ = g_kwdb_context;
class TestTimerEvent : public ::testing::Test {
 protected:
  static void SetUpTestCase() {}

  static void TearDownTestCase() {}
};

// Verify the timer
TEST_F(TestTimerEvent, TestTimerEventPool) {
  kwdbts::kwdbContext_t g_pool_context;
  kwdbts::kwdbContext_p thread_pool_ctx_ = &g_pool_context;
  InitServerKWDBContext(thread_pool_ctx_);
  kwdbts::KWDBDynamicThreadPool::GetThreadPool().Init(1, thread_pool_ctx_);
  kwdbts::TimerEventPool pool(102400);
  pool.Init();
  // start dispose thread
  pool.Start();
  // init TimerEvent
  kwdbts::TimerEventPtr te_ptr =
      kwdbts::TimerEventPtr(new kwdbts::TimerEvent(2000));
  EXPECT_TRUE(te_ptr != nullptr);
  kwdbts::k_time_point start_time;
  kwdbts::TimerEvent::SecondsAfter(2, &start_time);
  kwdbts::TimerEventPtr te_ptr2 = kwdbts::TimerEventPtr(KNEW kwdbts::TimerEvent(
      start_time, kwdbts::TimerEventType::TE_TIME_POINT));
  // PushTimeEvent
  pool.PushTimeEvent(te_ptr);
  pool.PushTimeEvent(te_ptr2);
  std::this_thread::sleep_for(std::chrono::seconds(3));
  bool succ = te_ptr2->Stop();
  EXPECT_TRUE(succ);
  if (succ == true) {
    std::cout << "time event stop succ!" << std::endl;
  }
  std::this_thread::sleep_for(std::chrono::seconds(2));
  pool.Stop();
  kwdbts::KWDBDynamicThreadPool::GetThreadPool().Stop();
}

}  // namespace kwdbts
