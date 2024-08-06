// Copyright 2020 The Cockroach Authors.
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

package kvnemesis

import (
	"strings"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngine(t *testing.T) {
	defer leaktest.AfterTest(t)()

	k := func(s string, ts hlc.Timestamp) storage.MVCCKey {
		return storage.MVCCKey{Key: []byte(s), Timestamp: ts}
	}
	var missing roachpb.Value
	v := func(s string, ts hlc.Timestamp) roachpb.Value {
		v := roachpb.MakeValueFromString(s)
		v.Timestamp = ts
		return v
	}
	ts := func(i int) hlc.Timestamp {
		return hlc.Timestamp{WallTime: int64(i)}
	}

	e, err := MakeEngine()
	require.NoError(t, err)
	defer e.Close()
	assert.Equal(t, missing, e.Get(roachpb.Key(`a`), ts(1)))
	e.Put(k(`a`, ts(1)), roachpb.MakeValueFromString(`a-1`).RawBytes)
	e.Put(k(`a`, ts(2)), roachpb.MakeValueFromString(`a-2`).RawBytes)
	e.Put(k(`b`, ts(2)), roachpb.MakeValueFromString(`b-2`).RawBytes)
	assert.Equal(t, v(`a-2`, ts(2)), e.Get(roachpb.Key(`a`), ts(3)))
	assert.Equal(t, v(`a-2`, ts(2)), e.Get(roachpb.Key(`a`), ts(2)))
	assert.Equal(t, v(`a-1`, ts(1)), e.Get(roachpb.Key(`a`), ts(2).Prev()))
	assert.Equal(t, v(`a-1`, ts(1)), e.Get(roachpb.Key(`a`), ts(1)))
	assert.Equal(t, missing, e.Get(roachpb.Key(`a`), ts(1).Prev()))
	assert.Equal(t, v(`b-2`, ts(2)), e.Get(roachpb.Key(`b`), ts(3)))
	assert.Equal(t, v(`b-2`, ts(2)), e.Get(roachpb.Key(`b`), ts(2)))
	assert.Equal(t, missing, e.Get(roachpb.Key(`b`), ts(1)))

	assert.Equal(t, strings.TrimSpace(`
"a" 0.000000002,0 -> /BYTES/a-2
"a" 0.000000001,0 -> /BYTES/a-1
"b" 0.000000002,0 -> /BYTES/b-2
	`), e.DebugPrint(""))
}
