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

package workload

import (
	gosql "database/sql"
	"sync/atomic"
)

// RoundRobinDB is a wrapper around *gosql.DB's that round robins individual
// queries among the different databases that it was created with.
type RoundRobinDB struct {
	handles []*gosql.DB
	current uint32
}

// NewRoundRobinDB creates a RoundRobinDB from the input list of
// database connection URLs.
func NewRoundRobinDB(urls []string) (*RoundRobinDB, error) {
	r := &RoundRobinDB{current: 0, handles: make([]*gosql.DB, 0, len(urls))}
	for _, url := range urls {
		db, err := gosql.Open(`kwbase`, url)
		if err != nil {
			return nil, err
		}
		r.handles = append(r.handles, db)
	}
	return r, nil
}

func (db *RoundRobinDB) next() *gosql.DB {
	return db.handles[(atomic.AddUint32(&db.current, 1)-1)%uint32(len(db.handles))]
}

// QueryRow executes (*gosql.DB).QueryRow on the next available DB.
func (db *RoundRobinDB) QueryRow(query string, args ...interface{}) *gosql.Row {
	return db.next().QueryRow(query, args...)
}

// Exec executes (*gosql.DB).Exec on the next available DB.
func (db *RoundRobinDB) Exec(query string, args ...interface{}) (gosql.Result, error) {
	return db.next().Exec(query, args...)
}
