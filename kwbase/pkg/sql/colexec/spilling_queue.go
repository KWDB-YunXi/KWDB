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

package colexec

import (
	"context"
	"fmt"

	"gitee.com/kwbasedb/kwbase/pkg/col/coldata"
	"gitee.com/kwbasedb/kwbase/pkg/col/coltypes"
	"gitee.com/kwbasedb/kwbase/pkg/sql/colcontainer"
	"gitee.com/kwbasedb/kwbase/pkg/sql/colexec/execerror"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/mon"
	"github.com/cockroachdb/errors"
	"github.com/marusama/semaphore"
)

// spillingQueue is a Queue that uses a fixed-size in-memory circular buffer
// and spills to disk if spillingQueue.items has no more slots available to hold
// a reference to an enqueued batch or the allocator reports that more memory
// than the caller-provided maxMemoryLimit is in use.
// When spilling to disk, a DiskQueue will be created. When spilling batches to
// disk, their memory will first be released using the allocator. When batches
// are read from disk back into memory, that memory will be reclaimed.
// NOTE: When a batch is returned, that batch's memory will still be tracked
// using the allocator. Since the memory in use is fixed, a previously returned
// batch may be overwritten by a batch read from disk. This new batch's memory
// footprint will replace the footprint of the previously returned batch. Since
// batches are unsafe for reuse, it is assumed that the previously returned
// batch is not kept around and thus its referenced memory will be GCed as soon
// as the batch is updated.
type spillingQueue struct {
	unlimitedAllocator *Allocator
	maxMemoryLimit     int64

	typs             []coltypes.T
	items            []coldata.Batch
	curHeadIdx       int
	curTailIdx       int
	numInMemoryItems int
	numOnDiskItems   int
	closed           bool

	diskQueueCfg   colcontainer.DiskQueueCfg
	diskQueue      colcontainer.Queue
	fdSemaphore    semaphore.Semaphore
	dequeueScratch coldata.Batch

	rewindable      bool
	rewindableState struct {
		numItemsDequeued int
	}

	diskAcc *mon.BoundAccount
}

// newSpillingQueue creates a new spillingQueue. An unlimited allocator must be
// passed in. The spillingQueue will use this allocator to check whether memory
// usage exceeds the given memory limit and use disk if so.
// If fdSemaphore is nil, no Acquire or Release calls will happen. The caller
// may want to do this if requesting FDs up front.
func newSpillingQueue(
	unlimitedAllocator *Allocator,
	typs []coltypes.T,
	memoryLimit int64,
	cfg colcontainer.DiskQueueCfg,
	fdSemaphore semaphore.Semaphore,
	batchSize int,
	diskAcc *mon.BoundAccount,
) *spillingQueue {
	// Reduce the memory limit by what the DiskQueue may need to buffer
	// writes/reads.
	memoryLimit -= int64(cfg.BufferSizeBytes)
	if memoryLimit < 0 {
		memoryLimit = 0
	}
	itemsLen := memoryLimit / int64(estimateBatchSizeBytes(typs, batchSize))
	if itemsLen == 0 {
		// Make items at least of length 1. Even though batches will spill to disk
		// directly (this can only happen with a very low memory limit), it's nice
		// to have at least one item in order to be able to deserialize from disk
		// into this slice.
		itemsLen = 1
	}
	return &spillingQueue{
		unlimitedAllocator: unlimitedAllocator,
		maxMemoryLimit:     memoryLimit,
		typs:               typs,
		items:              make([]coldata.Batch, itemsLen),
		diskQueueCfg:       cfg,
		fdSemaphore:        fdSemaphore,
		dequeueScratch:     unlimitedAllocator.NewMemBatchWithSize(typs, 0 /* size */),
		diskAcc:            diskAcc,
	}
}

// newRewindableSpillingQueue creates a new spillingQueue that can be rewinded
// in order to dequeue all enqueued batches all over again. An unlimited
// allocator must be passed in. The queue will use this allocator to check
// whether memory usage exceeds the given memory limit and use disk if so.
func newRewindableSpillingQueue(
	unlimitedAllocator *Allocator,
	typs []coltypes.T,
	memoryLimit int64,
	cfg colcontainer.DiskQueueCfg,
	fdSemaphore semaphore.Semaphore,
	batchSize int,
	diskAcc *mon.BoundAccount,
) *spillingQueue {
	q := newSpillingQueue(unlimitedAllocator, typs, memoryLimit, cfg, fdSemaphore, batchSize, diskAcc)
	q.rewindable = true
	return q
}

func (q *spillingQueue) enqueue(ctx context.Context, batch coldata.Batch) error {
	if batch.Length() == 0 {
		if q.diskQueue != nil {
			if err := q.diskQueue.Enqueue(ctx, batch); err != nil {
				return err
			}
		}
		return nil
	}

	if q.numOnDiskItems > 0 || q.unlimitedAllocator.Used() > q.maxMemoryLimit || q.numInMemoryItems == len(q.items) {
		// In this case, there is not enough memory available to keep this batch in
		// memory, or the in-memory circular buffer has no slots available (we do
		// an initial estimate of how many batches would fit into the buffer, which
		// might be wrong). The tail of the queue might also already be on disk, in
		// which case that is where the batch must be enqueued to maintain order.
		if err := q.maybeSpillToDisk(ctx); err != nil {
			return err
		}
		q.unlimitedAllocator.ReleaseBatch(batch)
		if err := q.diskQueue.Enqueue(ctx, batch); err != nil {
			return err
		}
		q.numOnDiskItems++
		return nil
	}

	q.items[q.curTailIdx] = batch
	q.curTailIdx++
	if q.curTailIdx == len(q.items) {
		q.curTailIdx = 0
	}
	q.numInMemoryItems++
	return nil
}

func (q *spillingQueue) dequeue(ctx context.Context) (coldata.Batch, error) {
	if q.empty() {
		return coldata.ZeroBatch, nil
	}

	if (q.rewindable && q.numInMemoryItems <= q.rewindableState.numItemsDequeued) ||
		(!q.rewindable && q.numInMemoryItems == 0) {
		// No more in-memory items. Fill the circular buffer as much as possible.
		// Note that there must be at least one element on disk.
		if !q.rewindable && q.curHeadIdx != q.curTailIdx {
			execerror.VectorizedInternalPanic(fmt.Sprintf("assertion failed in spillingQueue: curHeadIdx != curTailIdx, %d != %d", q.curHeadIdx, q.curTailIdx))
		}
		// NOTE: Only one item is dequeued from disk since a deserialized batch is
		// only valid until the next call to Dequeue. In practice we could Dequeue
		// up until a new file region is loaded (which will overwrite the memory of
		// the previous batches), but Dequeue calls are already amortized, so this
		// is acceptable.
		// Release a batch to make space for a new batch from disk.
		q.unlimitedAllocator.ReleaseBatch(q.dequeueScratch)
		ok, err := q.diskQueue.Dequeue(ctx, q.dequeueScratch)
		if err != nil {
			return nil, err
		}
		if !ok {
			// There was no batch to dequeue from disk. This should not really
			// happen, as it should have been caught by the q.empty() check above.
			execerror.VectorizedInternalPanic("disk queue was not empty but failed to dequeue element in spillingQueue")
		}
		// Account for this batch's memory.
		q.unlimitedAllocator.RetainBatch(q.dequeueScratch)
		if q.rewindable {
			q.rewindableState.numItemsDequeued++
			return q.dequeueScratch, nil
		}
		q.numOnDiskItems--
		q.numInMemoryItems++
		q.items[q.curTailIdx] = q.dequeueScratch
		q.curTailIdx++
		if q.curTailIdx == len(q.items) {
			q.curTailIdx = 0
		}
	}

	res := q.items[q.curHeadIdx]
	q.curHeadIdx++
	if q.curHeadIdx == len(q.items) {
		q.curHeadIdx = 0
	}
	if q.rewindable {
		q.rewindableState.numItemsDequeued++
	} else {
		q.numInMemoryItems--
	}
	return res, nil
}

func (q *spillingQueue) numFDsOpenAtAnyGivenTime() int {
	if q.diskQueueCfg.CacheMode != colcontainer.DiskQueueCacheModeDefault {
		// The access pattern must be write-everything then read-everything so
		// either a read FD or a write FD are open at any one point.
		return 1
	}
	// Otherwise, both will be open.
	return 2
}

func (q *spillingQueue) maybeSpillToDisk(ctx context.Context) error {
	if q.diskQueue != nil {
		return nil
	}
	var err error
	// Acquire two file descriptors for the DiskQueue: one for the write file and
	// one for the read file.
	if q.fdSemaphore != nil {
		if err = q.fdSemaphore.Acquire(ctx, q.numFDsOpenAtAnyGivenTime()); err != nil {
			return err
		}
	}
	log.VEvent(ctx, 1, "spilled to disk")
	var diskQueue colcontainer.Queue
	if q.rewindable {
		diskQueue, err = colcontainer.NewRewindableDiskQueue(ctx, q.typs, q.diskQueueCfg, q.diskAcc)
	} else {
		diskQueue, err = colcontainer.NewDiskQueue(ctx, q.typs, q.diskQueueCfg, q.diskAcc)
	}
	if err != nil {
		return err
	}
	// Only assign q.diskQueue if there was no error, otherwise the returned value
	// may be non-nil but invalid.
	q.diskQueue = diskQueue
	return nil
}

// empty returns whether there are currently no items to be dequeued.
func (q *spillingQueue) empty() bool {
	if q.rewindable {
		return q.numInMemoryItems+q.numOnDiskItems == q.rewindableState.numItemsDequeued
	}
	return q.numInMemoryItems == 0 && q.numOnDiskItems == 0
}

func (q *spillingQueue) spilled() bool {
	return q.diskQueue != nil
}

func (q *spillingQueue) close(ctx context.Context) error {
	if q.closed {
		return nil
	}
	if q.diskQueue != nil {
		if err := q.diskQueue.Close(ctx); err != nil {
			return err
		}
		if q.fdSemaphore != nil {
			q.fdSemaphore.Release(q.numFDsOpenAtAnyGivenTime())
		}
		q.closed = true
		return nil
	}
	return nil
}

func (q *spillingQueue) rewind() error {
	if !q.rewindable {
		return errors.Newf("unexpectedly rewind() called when spilling queue is not rewindable")
	}
	if q.diskQueue != nil {
		if err := q.diskQueue.(colcontainer.RewindableQueue).Rewind(); err != nil {
			return err
		}
	}
	q.curHeadIdx = 0
	q.rewindableState.numItemsDequeued = 0
	return nil
}

func (q *spillingQueue) reset(ctx context.Context) {
	if err := q.close(ctx); err != nil {
		execerror.VectorizedInternalPanic(err)
	}
	q.diskQueue = nil
	q.closed = false
	q.numInMemoryItems = 0
	q.numOnDiskItems = 0
	q.curHeadIdx = 0
	q.curTailIdx = 0
	q.rewindableState.numItemsDequeued = 0
}
