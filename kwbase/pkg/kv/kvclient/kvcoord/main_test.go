// Copyright 2015 The Cockroach Authors.
// Copyright (c) 2022-present, Shanghai Yunxi Technology Co, Ltd. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This software (KWDB) is licensed under Mulan PSL v2.
// You can use this software according to the terms and conditions of the Mulan PSL v2.
// You may obtain a copy of Mulan PSL v2 at:
//          http://license.coscl.org.cn/MulanPSL2
// THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, WITHOUT WARRANTIES OF ANY KIND,
// EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO NON-INFRINGEMENT,
// MERCHANTABILITY OR FIT FOR A PARTICULAR PURPOSE.
// See the Mulan PSL v2 for more details.

package kvcoord_test

import (
	"os"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/security"
	"gitee.com/kwbasedb/kwbase/pkg/security/securitytest"
	"gitee.com/kwbasedb/kwbase/pkg/server"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/buildutil"
	"gitee.com/kwbasedb/kwbase/pkg/testutils/serverutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
)

//go:generate ../../../util/leaktest/add-leaktest.sh *_test.go

func init() {
	security.SetAssetLoader(securitytest.EmbeddedAssets)
}

func TestForbiddenDeps(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Verify kv does not depend on storage (or any of its subpackages).
	buildutil.VerifyNoImports(t,
		"gitee.com/kwbasedb/kwbase/pkg/kv", true,
		// TODO(tschottdorf): should really disallow ./storage/... but at the
		// time of writing there's a (legit) dependency on `enginepb`.
		[]string{
			"gitee.com/kwbasedb/kwbase/pkg/storage",
			"gitee.com/kwbasedb/kwbase/pkg/storage/engine",
		},
		[]string{})
}

func TestMain(m *testing.M) {
	serverutils.InitTestServerFactory(server.TestServerFactory)
	os.Exit(m.Run())
}
