// Copyright 2017 The Cockroach Authors.
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

package bulk

import (
	"bytes"
	"context"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/clusterversion"
	"gitee.com/kwbasedb/kwbase/pkg/keys"
	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvclient/kvcoord"
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/storagebase"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/settings"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/storage/enginepb"
	"gitee.com/kwbasedb/kwbase/pkg/util/hlc"
	"gitee.com/kwbasedb/kwbase/pkg/util/humanizeutil"
	"gitee.com/kwbasedb/kwbase/pkg/util/log"
	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"github.com/pkg/errors"
)

var (
	tooSmallSSTSize = settings.RegisterByteSizeSetting(
		"kv.bulk_io_write.small_write_size",
		"size below which a 'bulk' write will be performed as a normal write instead",
		400*1<<10, // 400 Kib
	)
)

type sz int64

func (b sz) String() string {
	return humanizeutil.IBytes(int64(b))
}

// SSTBatcher is a helper for bulk-adding many KVs in chunks via AddSSTable. An
// SSTBatcher can be handed KVs repeatedly and will make them into SSTs that are
// added when they reach the configured size, tracking the total added rows,
// bytes, etc. If configured with a non-nil, populated range cache, it will use
// it to attempt to flush SSTs before they cross range boundaries to minimize
// expensive on-split retries.
type SSTBatcher struct {
	db         SSTSender
	rc         *kvcoord.RangeDescriptorCache
	settings   *cluster.Settings
	maxSize    func() int64
	splitAfter func() int64

	// allows ingestion of keys where the MVCC.Key would shadow an existing row.
	disallowShadowing bool
	// skips duplicate keys (iff they are buffered together). This is true when
	// used to backfill an inverted index. An array in JSONB with multiple values
	// which are the same, will all correspond to the same kv in the inverted
	// index. The method which generates these kvs does not dedup, thus we rely on
	// the SSTBatcher to dedup them (by skipping), rather than throwing a
	// DuplicateKeyError. This is also true when used with IMPORT. Import
	// generally prohibits the ingestion of KVs which will shadow existing data,
	// with the exception of duplicates having the same value and timestamp. To
	// maintain uniform behavior, duplicates in the same batch with equal values
	// will not raise a DuplicateKeyError.
	skipDuplicates bool

	// The rest of the fields accumulated state as opposed to configuration. Some,
	// like totalRows, are accumulated _across_ batches and are not reset between
	// batches when Reset() is called.
	totalRows   roachpb.BulkOpSummary
	flushCounts struct {
		total   int
		split   int
		sstSize int
		files   int // a single flush might create multiple files.

		sendWait  time.Duration
		splitWait time.Duration
	}
	// Tracking for if we have "filled" a range in case we want to split/scatter.
	flushedToCurrentRange int64
	lastFlushKey          []byte

	// The rest of the fields are per-batch and are reset via Reset() before each
	// batch is started.
	sstWriter       storage.SSTWriter
	sstFile         *storage.MemFile
	batchStartKey   []byte
	batchEndKey     []byte
	batchEndValue   []byte
	flushKeyChecked bool
	flushKey        roachpb.Key
	// stores on-the-fly stats for the SST if disallowShadowing is true.
	ms enginepb.MVCCStats
	// rows written in the current batch.
	rowCounter storage.RowCounter

	shouldLogLogicalOp bool
}

// MakeSSTBatcher makes a ready-to-use SSTBatcher.
func MakeSSTBatcher(
	ctx context.Context, db SSTSender, settings *cluster.Settings, flushBytes func() int64,
) (*SSTBatcher, error) {
	b := &SSTBatcher{db: db, settings: settings, maxSize: flushBytes, disallowShadowing: true}
	err := b.Reset(ctx)
	return b, err
}

// MakeStreamSSTBatcher creates a batcher configured to ingest duplicate keys
// that might be received from a cluster to cluster stream.
func MakeStreamSSTBatcher(
	ctx context.Context, db *kv.DB, settings *cluster.Settings,
) (*SSTBatcher, error) {
	//b := &SSTBatcher{db: db, settings: settings, ingestAll: true, mem: mem, limiter: sendLimiter}
	b := &SSTBatcher{db: db, settings: settings}
	err := b.Reset(ctx)
	return b, err
}

func (b *SSTBatcher) updateMVCCStats(key storage.MVCCKey, value []byte) {
	metaKeySize := int64(len(key.Key)) + 1
	metaValSize := int64(0)
	b.ms.LiveBytes += metaKeySize
	b.ms.LiveCount++
	b.ms.KeyBytes += metaKeySize
	b.ms.ValBytes += metaValSize
	b.ms.KeyCount++

	totalBytes := int64(len(value)) + storage.MVCCVersionTimestampSize
	b.ms.LiveBytes += totalBytes
	b.ms.KeyBytes += storage.MVCCVersionTimestampSize
	b.ms.ValBytes += int64(len(value))
	b.ms.ValCount++
}

// AddMVCCKey adds a key+timestamp/value pair to the batch (flushing if needed).
// This is only for callers that want to control the timestamp on individual
// keys -- like RESTORE where we want the restored data to look the like backup.
// Keys must be added in order.
func (b *SSTBatcher) AddMVCCKey(ctx context.Context, key storage.MVCCKey, value []byte) error {
	if len(b.batchEndKey) > 0 && bytes.Equal(b.batchEndKey, key.Key) {
		if b.skipDuplicates && bytes.Equal(b.batchEndValue, value) {
			return nil
		}
		var err storagebase.DuplicateKeyError
		err.Key = append(err.Key, key.Key...)
		err.Value = append(err.Value, value...)
		return err
	}
	// Check if we need to flush current batch *before* adding the next k/v --
	// the batcher may want to flush the keys it already has, either because it
	// is full or because it wants this key in a separate batch due to splits.
	if err := b.flushIfNeeded(ctx, key.Key); err != nil {
		return err
	}
	// Update the range currently represented in this batch, as necessary.
	if len(b.batchStartKey) == 0 {
		b.batchStartKey = append(b.batchStartKey[:0], key.Key...)
	}
	b.batchEndKey = append(b.batchEndKey[:0], key.Key...)
	b.batchEndValue = append(b.batchEndValue[:0], value...)
	if err := b.rowCounter.Count(key.Key); err != nil {
		return err
	}
	// If we do not allowing shadowing of keys when ingesting an SST via
	// AddSSTable, then we can update the MVCCStats on the fly because we are
	// guaranteed to ingest unique keys. This saves us an extra iteration in
	// AddSSTable which has been identified as a significant performance
	// regression for IMPORT.
	if b.disallowShadowing {
		b.updateMVCCStats(key, value)
	}
	return b.sstWriter.Put(key, value)
}

// AddMVCCKeyForRepl adds a key+timestamp/value pair to the batch (flushing if needed).
// This is only for callers that want to control the timestamp on individual
// keys -- like RESTORE where we want the restored data to look the like backup.
// Keys must be added in order.
func (b *SSTBatcher) AddMVCCKeyForRepl(
	ctx context.Context, key storage.MVCCKey, value []byte,
) error {
	if len(b.batchEndKey) > 0 && bytes.Equal(b.batchEndKey, key.Key) {
		if b.skipDuplicates && bytes.Equal(b.batchEndValue, value) {
			return nil
		}
	}
	// Check if we need to flush current batch *before* adding the next k/v --
	// the batcher may want to flush the keys it already has, either because it
	// is full or because it wants this key in a separate batch due to splits.
	if err := b.flushIfNeeded(ctx, key.Key); err != nil {
		return err
	}

	// Update the range currently represented in this batch, as necessary.
	if len(b.batchStartKey) == 0 {
		b.batchStartKey = append(b.batchStartKey[:0], key.Key...)
	}
	b.batchEndKey = append(b.batchEndKey[:0], key.Key...)
	b.batchEndValue = append(b.batchEndValue[:0], value...)

	if err := b.rowCounter.Count(key.Key); err != nil {
		return err
	}

	// If we do not allowing shadowing of keys when ingesting an SST via
	// AddSSTable, then we can update the MVCCStats on the fly because we are
	// guaranteed to ingest unique keys. This saves us an extra iteration in
	// AddSSTable which has been identified as a significant performance
	// regression for IMPORT.
	if b.disallowShadowing {
		b.updateMVCCStats(key, value)
	}

	return b.sstWriter.Put(key, value)
}

// Reset clears all state in the batcher and prepares it for reuse.
func (b *SSTBatcher) Reset(ctx context.Context) error {
	b.sstWriter.Close()
	b.sstFile = &storage.MemFile{}
	// Create "Ingestion" SSTs in the newer RocksDBv2 format only if  all nodes
	// in the cluster can support it. Until then, for backward compatibility,
	// create SSTs in the leveldb format ("backup" ones).
	if b.settings.Version.IsActive(ctx, clusterversion.VersionStart20_1) {
		b.sstWriter = storage.MakeIngestionSSTWriter(b.sstFile)
	} else {
		b.sstWriter = storage.MakeBackupSSTWriter(b.sstFile)
	}
	b.batchStartKey = b.batchStartKey[:0]
	b.batchEndKey = b.batchEndKey[:0]
	b.batchEndValue = b.batchEndValue[:0]
	b.flushKey = nil
	b.flushKeyChecked = false
	b.ms.Reset()

	b.rowCounter.BulkOpSummary.Reset()
	return nil
}

const (
	manualFlush = iota
	sizeFlush
	rangeFlush
)

func (b *SSTBatcher) flushIfNeeded(ctx context.Context, nextKey roachpb.Key) error {
	// If this is the first key we have seen (since being reset), attempt to find
	// the end of the range it is in so we can flush the SST before crossing it,
	// because AddSSTable cannot span ranges and will need to be split and retried
	// from scratch if we generate an SST that ends up doing so.
	if !b.flushKeyChecked && b.rc != nil {
		b.flushKeyChecked = true
		if k, err := keys.Addr(nextKey); err != nil {
			log.Warningf(ctx, "failed to get RKey for flush key lookup")
		} else {
			r, err := b.rc.GetCachedRangeDescriptor(k, false /* inverted */)
			if err != nil {
				log.Warningf(ctx, "failed to determine where to split SST: %+v", err)
			} else if r != nil {
				b.flushKey = r.EndKey.AsRawKey()
				log.VEventf(ctx, 3, "building sstable that will flush before %v", b.flushKey)
			} else {
				log.VEventf(ctx, 3, "no cached range desc available to determine sst flush key")
			}
		}
	}

	if b.flushKey != nil && b.flushKey.Compare(nextKey) <= 0 {
		if err := b.doFlush(ctx, rangeFlush, nil); err != nil {
			return err
		}
		return b.Reset(ctx)
	}

	//TODO(mike): batch.maxSize maybe not set on the replication
	if b.maxSize == nil {
		if b.sstWriter.DataSize >= 16637779 {
			prevRow, prevErr := keys.EnsureSafeSplitKey(b.batchEndKey)
			nextRow, nextErr := keys.EnsureSafeSplitKey(nextKey)
			if prevErr == nil && nextErr == nil && bytes.Equal(prevRow, nextRow) {
				// An error decoding either key implies it is not a valid row key and thus
				// not the same row for our purposes; we don't care what the error is.
				return nil // keep going to row boundary.
			}
			if err := b.doFlush(ctx, sizeFlush, nextKey); err != nil {
				return err
			}
			return b.Reset(ctx)
		}
	} else {
		if b.sstWriter.DataSize >= b.maxSize() {
			prevRow, prevErr := keys.EnsureSafeSplitKey(b.batchEndKey)
			nextRow, nextErr := keys.EnsureSafeSplitKey(nextKey)
			if prevErr == nil && nextErr == nil && bytes.Equal(prevRow, nextRow) {
				// An error decoding either key implies it is not a valid row key and thus
				// not the same row for our purposes; we don't care what the error is.
				return nil // keep going to row boundary.
			}
			if err := b.doFlush(ctx, sizeFlush, nextKey); err != nil {
				return err
			}
			return b.Reset(ctx)
		}
	}

	return nil
}

// Flush sends the current batch, if any.
func (b *SSTBatcher) Flush(ctx context.Context) error {
	return b.doFlush(ctx, manualFlush, nil)
}

func (b *SSTBatcher) doFlush(ctx context.Context, reason int, nextKey roachpb.Key) error {
	if b.sstWriter.DataSize == 0 {
		return nil
	}
	b.flushCounts.total++

	hour := hlc.Timestamp{WallTime: timeutil.Now().Add(time.Hour).UnixNano()}

	start := roachpb.Key(append([]byte(nil), b.batchStartKey...))
	// The end key of the WriteBatch request is exclusive, but batchEndKey is
	// currently the largest key in the batch. Increment it.
	end := roachpb.Key(append([]byte(nil), b.batchEndKey...)).Next()

	size := b.sstWriter.DataSize
	if reason == sizeFlush {
		b.flushCounts.sstSize++

		// On first flush, if it is due to size, we introduce one split at the start
		// of our span, since size means we didn't already hit one. When adders have
		// non-overlapping keyspace this split partitions off "our" target space for
		// future splitting/scattering, while if they don't, doing this only once
		// minimizes impact on other adders (e.g. causing extra SST splitting).
		if b.flushCounts.total == 1 {
			if splitAt, err := keys.EnsureSafeSplitKey(start); err != nil {
				log.Warning(ctx, err)
			} else {
				// NB: Passing 'hour' here is technically illegal until 19.2 is
				// active, but the value will be ignored before that, and we don't
				// have access to the cluster version here.
				if err := b.db.SplitAndScatter(ctx, splitAt, hour); err != nil {
					log.Warning(ctx, err)
				}
			}
		}
	} else if reason == rangeFlush {
		log.VEventf(ctx, 3, "flushing %s SST due to range boundary %s", sz(size), b.flushKey)
		b.flushCounts.split++
	}

	err := b.sstWriter.Finish()
	if err != nil {
		return errors.Wrapf(err, "finishing constructed sstable")
	}

	// If the stats have been computed on-the-fly, set the last updated time
	// before ingesting the SST.
	if (b.ms != enginepb.MVCCStats{}) {
		b.ms.LastUpdateNanos = timeutil.Now().UnixNano()
	}

	beforeSend := timeutil.Now()
	files, err := AddSSTable(ctx, b.db, start, end, b.sstFile.Data(), b.disallowShadowing, b.ms, b.settings, b.shouldLogLogicalOp)
	if err != nil {
		return err
	}
	b.flushCounts.sendWait += timeutil.Since(beforeSend)

	b.flushCounts.files += files
	if b.flushKey != nil {
		// If the flush-before key hasn't changed we know we don't think we passed
		// a range boundary, and if the files-added count is 1 we didn't hit an
		// unexpected split either, so assume we added to the same range.
		if reason == sizeFlush && bytes.Equal(b.flushKey, b.lastFlushKey) && files == 1 {
			b.flushedToCurrentRange += size
		} else {
			// Assume we started adding to new different range with this SST.
			b.lastFlushKey = append(b.lastFlushKey[:0], b.flushKey...)
			b.flushedToCurrentRange = size
		}
		if b.splitAfter != nil {
			if splitAfter := b.splitAfter(); b.flushedToCurrentRange > splitAfter && nextKey != nil {
				if splitAt, err := keys.EnsureSafeSplitKey(nextKey); err != nil {
					log.Warning(ctx, err)
				} else {
					beforeSplit := timeutil.Now()

					log.VEventf(ctx, 2, "%s added since last split, splitting/scattering for next range at %v", sz(b.flushedToCurrentRange), end)
					// NB: Passing 'hour' here is technically illegal until 19.2 is
					// active, but the value will be ignored before that, and we don't
					// have access to the cluster version here.
					if err := b.db.SplitAndScatter(ctx, splitAt, hour); err != nil {
						log.Warningf(ctx, "failed to split and scatter during ingest: %+v", err)
					}
					b.flushCounts.splitWait += timeutil.Since(beforeSplit)
				}
				b.flushedToCurrentRange = 0
			}
		}
	}

	b.totalRows.Add(b.rowCounter.BulkOpSummary)
	b.totalRows.DataSize += b.sstWriter.DataSize
	return nil
}

// Close closes the underlying SST builder.
func (b *SSTBatcher) Close() {
	b.sstWriter.Close()
}

// GetSummary returns this batcher's total added rows/bytes/etc.
func (b *SSTBatcher) GetSummary() roachpb.BulkOpSummary {
	return b.totalRows
}

// SSTSender is an interface to send SST data to an engine.
type SSTSender interface {
	AddSSTable(
		ctx context.Context, begin, end interface{}, data []byte, disallowShadowing bool, stats *enginepb.MVCCStats, ingestAsWrites bool, shouldLogLogicalOp bool,
	) error
	SplitAndScatter(ctx context.Context, key roachpb.Key, expirationTime hlc.Timestamp) error
}

type sstSpan struct {
	start, end        roachpb.Key
	sstBytes          []byte
	disallowShadowing bool
	stats             enginepb.MVCCStats
}

// AddSSTable retries db.AddSSTable if retryable errors occur, including if the
// SST spans a split, in which case it is iterated and split into two SSTs, one
// for each side of the split in the error, and each are retried.
func AddSSTable(
	ctx context.Context,
	db SSTSender,
	start, end roachpb.Key,
	sstBytes []byte,
	disallowShadowing bool,
	ms enginepb.MVCCStats,
	settings *cluster.Settings,
	shouLogLoicalOps bool,
) (int, error) {
	var files int
	now := timeutil.Now()
	iter, err := storage.NewMemSSTIterator(sstBytes, true)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	var stats enginepb.MVCCStats
	if (ms == enginepb.MVCCStats{}) {
		stats, err = storage.ComputeStatsGo(iter, start, end, now.UnixNano())
		if err != nil {
			return 0, errors.Wrapf(err, "computing stats for SST [%s, %s)", start, end)
		}
	} else {
		stats = ms
	}

	work := []*sstSpan{{start: start, end: end, sstBytes: sstBytes, disallowShadowing: disallowShadowing, stats: stats}}
	const maxAddSSTableRetries = 10
	for len(work) > 0 {
		item := work[0]
		work = work[1:]
		if err := func() error {
			var err error
			for i := 0; i < maxAddSSTableRetries; i++ {
				log.VEventf(ctx, 2, "sending %s AddSSTable [%s,%s)", sz(len(item.sstBytes)), item.start, item.end)
				before := timeutil.Now()
				// If this SST is "too small", the fixed costs associated with adding an
				// SST – in terms of triggering flushes, extra compactions, etc – would
				// exceed the savings we get from skipping regular, key-by-key writes,
				// and we're better off just putting its contents in a regular batch.
				// This isn't perfect: We're still incurring extra overhead constructing
				// SSTables just for use as a wire-format, but the rest of the
				// implementation of bulk-ingestion assumes certainly semantics of the
				// AddSSTable API - like ingest at arbitrary timestamps or collision
				// detection - making it is simpler to just always use the same API
				// and just switch how it writes its result.
				ingestAsWriteBatch := false
				if settings != nil && int64(len(item.sstBytes)) < tooSmallSSTSize.Get(&settings.SV) {
					log.VEventf(ctx, 2, "ingest data is too small (%d keys/%d bytes) for SSTable, adding via regular batch", item.stats.KeyCount, len(item.sstBytes))
					ingestAsWriteBatch = true
				}
				// This will fail if the range has split but we'll check for that below.
				err = db.AddSSTable(ctx, item.start, item.end, item.sstBytes, item.disallowShadowing, &item.stats, ingestAsWriteBatch, shouLogLoicalOps)
				if err == nil {
					log.VEventf(ctx, 3, "adding %s AddSSTable [%s,%s) took %v", sz(len(item.sstBytes)), item.start, item.end, timeutil.Since(before))
					return nil
				}
				// This range has split -- we need to split the SST to try again.
				if m, ok := errors.Cause(err).(*roachpb.RangeKeyMismatchError); ok {
					split := m.MismatchedRange.EndKey.AsRawKey()
					log.Infof(ctx, "SSTable cannot be added spanning range bounds %v, retrying...", split)
					left, right, err := createSplitSSTable(ctx, db, item.start, split, item.disallowShadowing, iter, settings)
					if err != nil {
						return err
					}

					right.stats, err = storage.ComputeStatsGo(
						iter, right.start, right.end, now.UnixNano(),
					)
					if err != nil {
						return err
					}
					left.stats = item.stats
					left.stats.Subtract(right.stats)

					// Add more work.
					work = append([]*sstSpan{left, right}, work...)
					return nil
				}
				// Retry on AmbiguousResult.
				if _, ok := err.(*roachpb.AmbiguousResultError); ok {
					log.Warningf(ctx, "addsstable [%s,%s) attempt %d failed: %+v", start, end, i, err)
					continue
				}
			}
			return errors.Wrapf(err, "addsstable [%s,%s)", item.start, item.end)
		}(); err != nil {
			return files, err
		}
		files++
		// explicitly deallocate SST. This will not deallocate the
		// top level SST which is kept around to iterate over.
		item.sstBytes = nil
	}
	log.VEventf(ctx, 3, "AddSSTable [%v, %v) added %d files and took %v", start, end, files, timeutil.Since(now))
	return files, nil
}

// createSplitSSTable is a helper for splitting up SSTs. The iterator
// passed in is over the top level SST passed into AddSSTTable().
func createSplitSSTable(
	ctx context.Context,
	db SSTSender,
	start, splitKey roachpb.Key,
	disallowShadowing bool,
	iter storage.SimpleIterator,
	settings *cluster.Settings,
) (*sstSpan, *sstSpan, error) {
	sstFile := &storage.MemFile{}
	var w storage.SSTWriter
	if settings.Version.IsActive(ctx, clusterversion.VersionStart20_1) {
		w = storage.MakeIngestionSSTWriter(sstFile)
	} else {
		w = storage.MakeBackupSSTWriter(sstFile)
	}
	defer w.Close()

	split := false
	var first, last roachpb.Key
	var left, right *sstSpan

	iter.SeekGE(storage.MVCCKey{Key: start})
	for {
		if ok, err := iter.Valid(); err != nil {
			return nil, nil, err
		} else if !ok {
			break
		}

		key := iter.UnsafeKey()

		if !split && key.Key.Compare(splitKey) >= 0 {
			err := w.Finish()

			if err != nil {
				return nil, nil, err
			}
			left = &sstSpan{
				start:             first,
				end:               last.PrefixEnd(),
				sstBytes:          sstFile.Data(),
				disallowShadowing: disallowShadowing,
			}
			*sstFile = storage.MemFile{}
			if settings.Version.IsActive(ctx, clusterversion.VersionStart20_1) {
				w = storage.MakeIngestionSSTWriter(sstFile)
			} else {
				w = storage.MakeBackupSSTWriter(sstFile)
			}
			split = true
			first = nil
			last = nil
		}

		if len(first) == 0 {
			first = append(first[:0], key.Key...)
		}
		last = append(last[:0], key.Key...)

		if err := w.Put(key, iter.UnsafeValue()); err != nil {
			return nil, nil, err
		}

		iter.Next()
	}

	err := w.Finish()
	if err != nil {
		return nil, nil, err
	}
	right = &sstSpan{
		start:             first,
		end:               last.PrefixEnd(),
		sstBytes:          sstFile.Data(),
		disallowShadowing: disallowShadowing,
	}
	return left, right, nil
}
