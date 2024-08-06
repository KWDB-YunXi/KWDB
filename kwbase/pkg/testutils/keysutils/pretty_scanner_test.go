// Copyright 2019 The Cockroach Authors.
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

package keysutils

import (
	"fmt"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sqlbase"
	"gitee.com/kwbasedb/kwbase/pkg/testutils"
	"gitee.com/kwbasedb/kwbase/pkg/util/encoding"
)

func TestPrettyScanner(t *testing.T) {
	tests := []struct {
		prettyKey    string
		expKey       func() roachpb.Key
		expRemainder string
	}{
		{
			prettyKey: "/Table/t1",
			expKey: func() roachpb.Key {
				return keys.MakeTablePrefix(50)
			},
		},
		{
			prettyKey: "/Table/t1/pk",
			expKey: func() roachpb.Key {
				return sqlbase.EncodeTableIDIndexID(nil /* key */, 50, 1)
			},
		},
		{
			prettyKey: "/Table/t1/pk/1/2/3",
			expKey: func() roachpb.Key {
				k := sqlbase.EncodeTableIDIndexID(nil /* key */, 50, 1)
				k = encoding.EncodeVarintAscending(k, 1)
				k = encoding.EncodeVarintAscending(k, 2)
				k = encoding.EncodeVarintAscending(k, 3)
				return k
			},
		},
		{
			prettyKey:    "/Table/t1/pk/1/2/3/foo",
			expKey:       nil,
			expRemainder: "/foo",
		},
		{
			prettyKey: "/Table/t1/idx1/1/2/3",
			expKey: func() roachpb.Key {
				k := sqlbase.EncodeTableIDIndexID(nil /* key */, 50, 5)
				k = encoding.EncodeVarintAscending(k, 1)
				k = encoding.EncodeVarintAscending(k, 2)
				k = encoding.EncodeVarintAscending(k, 3)
				return k
			},
		},
	}

	tableToID := map[string]int{"t1": 50}
	idxToID := map[string]int{"t1.idx1": 5}
	scanner := MakePrettyScannerForNamedTables(tableToID, idxToID)
	for _, test := range tests {
		t.Run(test.prettyKey, func(t *testing.T) {
			k, err := scanner.Scan(test.prettyKey)
			if err != nil {
				if test.expRemainder != "" {
					if testutils.IsError(err, fmt.Sprintf("can't parse\"%s\"", test.expRemainder)) {
						t.Fatalf("expected remainder: %s, got err: %s", test.expRemainder, err)
					}
				} else {
					t.Fatal(err)
				}
			}
			if test.expRemainder != "" && err == nil {
				t.Fatalf("expected a remainder but got none: %s", test.expRemainder)
			}
			if test.expKey == nil {
				if k != nil {
					t.Fatalf("unexpected key returned: %s", k)
				}
				return
			}
			expKey := test.expKey()
			if !k.Equal(expKey) {
				t.Fatalf("expected: %+v, got %+v", []byte(expKey), []byte(k))
			}
		})
	}
}
