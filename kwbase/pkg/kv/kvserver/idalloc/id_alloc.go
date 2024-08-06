// Copyright 2014 The Cockroach Authors.
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

package idalloc

import (
	"context"
	"sync"

	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/retry"
	"gitee.com/kwbasedb/kwbase/pkg/util/stop"
	"github.com/pkg/errors"
)

// Incrementer abstracts over the database which holds the key counter.
type Incrementer func(_ context.Context, _ roachpb.Key, inc int64) (updated int64, _ error)

// DBIncrementer wraps a suitable subset of *kv.DB for use with an allocator.
func DBIncrementer(
	db interface {
		Inc(ctx context.Context, key interface{}, value int64) (kv.KeyValue, error)
	},
) Incrementer {
	return func(ctx context.Context, key roachpb.Key, inc int64) (int64, error) {
		res, err := db.Inc(ctx, key, inc)
		if err != nil {
			return 0, err
		}
		return res.Value.GetInt()
	}
}

// Options are the options passed to NewAllocator.
type Options struct {
	AmbientCtx  log.AmbientContext
	Key         roachpb.Key
	Incrementer Incrementer
	BlockSize   int64
	Stopper     *stop.Stopper
	Fatalf      func(context.Context, string, ...interface{}) // defaults to log.Fatalf
}

// An Allocator is used to increment a key in allocation blocks of arbitrary
// size.
type Allocator struct {
	log.AmbientContext
	opts Options

	ids  chan int64 // Channel of available IDs
	once sync.Once
}

// NewAllocator creates a new ID allocator which increments the specified key in
// allocation blocks of size blockSize. If the key exists, it's assumed to have
// an int value (and it needs to be positive since id 0 is a sentinel used
// internally by the allocator that can't be generated). The first value
// returned is the existing value + 1, or 1 if the key did not previously exist.
func NewAllocator(opts Options) (*Allocator, error) {
	if opts.BlockSize == 0 {
		return nil, errors.Errorf("blockSize must be a positive integer: %d", opts.BlockSize)
	}
	if opts.Fatalf == nil {
		opts.Fatalf = log.Fatalf
	}
	opts.AmbientCtx.AddLogTag("idalloc", nil)
	return &Allocator{
		AmbientContext: opts.AmbientCtx,
		opts:           opts,
		ids:            make(chan int64, opts.BlockSize/2+1),
	}, nil
}

// Allocate allocates a new ID from the global KV DB.
func (ia *Allocator) Allocate(ctx context.Context) (int64, error) {
	ia.once.Do(ia.start)

	select {
	case id := <-ia.ids:
		// when the channel is closed, the zero value is returned.
		if id == 0 {
			return id, errors.Errorf("could not allocate ID; system is draining")
		}
		return id, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (ia *Allocator) start() {
	ctx := ia.AnnotateCtx(context.Background())
	ia.opts.Stopper.RunWorker(ctx, func(ctx context.Context) {
		defer close(ia.ids)

		var prevValue int64 // for assertions
		for {
			var newValue int64
			var err error
			for r := retry.Start(base.DefaultRetryOptions()); r.Next(); {
				if stopperErr := ia.opts.Stopper.RunTask(ctx, "idalloc: allocating block",
					func(ctx context.Context) {
						newValue, err = ia.opts.Incrementer(ctx, ia.opts.Key, ia.opts.BlockSize)
					}); stopperErr != nil {
					return
				}
				if err == nil {
					break
				}

				log.Warningf(
					ctx,
					"unable to allocate %d ids from %s: %+v",
					ia.opts.BlockSize,
					ia.opts.Key,
					err,
				)
			}
			if err != nil {
				ia.opts.Fatalf(ctx, "unexpectedly exited id allocation retry loop: %s", err)
				return
			}
			if prevValue != 0 && newValue < prevValue+ia.opts.BlockSize {
				ia.opts.Fatalf(
					ctx,
					"counter corrupt: incremented to %d, expected at least %d + %d",
					newValue, prevValue, ia.opts.BlockSize,
				)
				return
			}

			end := newValue + 1
			start := end - ia.opts.BlockSize
			if start <= 0 {
				ia.opts.Fatalf(ctx, "allocator initialized with negative key")
				return
			}
			prevValue = newValue

			// Add all new ids to the channel for consumption.
			for i := start; i < end; i++ {
				select {
				case ia.ids <- i:
				case <-ia.opts.Stopper.ShouldStop():
					return
				}
			}
		}
	})
}
