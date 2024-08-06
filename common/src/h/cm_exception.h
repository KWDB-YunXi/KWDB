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

#ifndef COMMON_SRC_H_CM_EXCEPTION_H_
#define COMMON_SRC_H_CM_EXCEPTION_H_

#include <iostream>
#include <functional>

namespace kwdbts {

typedef std::function<void (int sig, std::ostream& os)> PostExceptionCb;

int32_t RegisterExceptionHandler(PostExceptionCb cb = nullptr);

}  // namespace kwdbts

#endif  // COMMON_SRC_H_CM_EXCEPTION_H_
