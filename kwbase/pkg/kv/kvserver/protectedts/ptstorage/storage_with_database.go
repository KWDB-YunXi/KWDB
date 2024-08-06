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

package ptstorage

import (
	"context"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/protectedts"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/protectedts/ptpb"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/uuid"
)

// WithDatabase wraps s such that any calls made with a nil *Txn will be wrapped
// in a call to db.Txn. This is often convenient in testing.
func WithDatabase(s protectedts.Storage, db *kv.DB) protectedts.Storage {
	return &storageWithDatabase{s: s, db: db}
}

type storageWithDatabase struct {
	db *kv.DB
	s  protectedts.Storage
}

func (s *storageWithDatabase) Protect(ctx context.Context, txn *kv.Txn, r *ptpb.Record) error {
	if txn == nil {
		return s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			return s.s.Protect(ctx, txn, r)
		})
	}
	return s.s.Protect(ctx, txn, r)
}

func (s *storageWithDatabase) GetRecord(
	ctx context.Context, txn *kv.Txn, id uuid.UUID,
) (r *ptpb.Record, err error) {
	if txn == nil {
		err = s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			r, err = s.s.GetRecord(ctx, txn, id)
			return err
		})
		return r, err
	}
	return s.s.GetRecord(ctx, txn, id)
}

func (s *storageWithDatabase) MarkVerified(ctx context.Context, txn *kv.Txn, id uuid.UUID) error {
	if txn == nil {
		return s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			return s.s.Release(ctx, txn, id)
		})
	}
	return s.s.Release(ctx, txn, id)
}

func (s *storageWithDatabase) Release(ctx context.Context, txn *kv.Txn, id uuid.UUID) error {
	if txn == nil {
		return s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			return s.s.Release(ctx, txn, id)
		})
	}
	return s.s.Release(ctx, txn, id)
}

func (s *storageWithDatabase) GetMetadata(
	ctx context.Context, txn *kv.Txn,
) (md ptpb.Metadata, err error) {
	if txn == nil {
		err = s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			md, err = s.s.GetMetadata(ctx, txn)
			return err
		})
		return md, err
	}
	return s.s.GetMetadata(ctx, txn)
}

func (s *storageWithDatabase) GetState(
	ctx context.Context, txn *kv.Txn,
) (state ptpb.State, err error) {
	if txn == nil {
		err = s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			state, err = s.s.GetState(ctx, txn)
			return err
		})
		return state, err
	}
	return s.s.GetState(ctx, txn)
}

func (s *storageWithDatabase) UpdateTimestamp(
	ctx context.Context, txn *kv.Txn, id uuid.UUID, timestamp hlc.Timestamp,
) (err error) {
	if txn == nil {
		err = s.db.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			return s.s.UpdateTimestamp(ctx, txn, id, timestamp)
		})
		return err
	}
	return s.s.UpdateTimestamp(ctx, txn, id, timestamp)
}
