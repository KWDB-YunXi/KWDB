// Copyright 2016 The Cockroach Authors.
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

package zerofields

import (
	"fmt"
	"reflect"
)

type zeroFieldErr struct {
	field string
}

func (err zeroFieldErr) Error() string {
	return fmt.Sprintf("expected %s field to be non-zero", err.field)
}

// NoZeroField returns nil if none of the fields of the struct underlying the
// interface are equal to the zero value, and an error otherwise.
// It will panic if the struct has unexported fields and for any non-struct.
func NoZeroField(v interface{}) error {
	ele := reflect.Indirect(reflect.ValueOf(v))
	eleT := ele.Type()
	for i := 0; i < ele.NumField(); i++ {
		f := ele.Field(i)
		n := eleT.Field(i).Name
		switch f.Kind() {
		case reflect.Struct:
			if err := NoZeroField(f.Interface()); err != nil {
				zfe := err.(zeroFieldErr)
				zfe.field = fmt.Sprintf("%s.%s", n, zfe.field)
				return zfe
			}
		default:
			zero := reflect.Zero(f.Type())
			if reflect.DeepEqual(f.Interface(), zero.Interface()) {
				switch field := eleT.Field(i).Name; field {
				case "XXX_NoUnkeyedLiteral", "XXX_DiscardUnknown", "XXX_sizecache":
					// Ignore these special protobuf fields.
				default:
					return zeroFieldErr{field: n}
				}
			}
		}
	}
	return nil
}
