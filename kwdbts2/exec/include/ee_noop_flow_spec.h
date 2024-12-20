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

#include <memory>

#include "ee_flow_param.h"

namespace kwdbts {

class NoopPostResolve : public BasePostResolve {
 public:
  using BasePostResolve::BasePostResolve;

  void RenderSize(kwdbContext_p ctx, k_uint32 *num) override;

  EEIteratorErrCode ResolveReference(
      kwdbContext_p ctx, const std::shared_ptr<VirtualField> &virtualField,
      Field **field) override;

  EEIteratorErrCode HandleRender(kwdbContext_p ctx, Field **render,
                                 k_uint32 num) override;
};
}  // namespace kwdbts
