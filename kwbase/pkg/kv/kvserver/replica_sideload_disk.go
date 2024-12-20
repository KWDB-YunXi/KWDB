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

package kvserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/settings/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
)

var _ SideloadStorage = &diskSideloadStorage{}

type diskSideloadStorage struct {
	st         *cluster.Settings
	limiter    *rate.Limiter
	dir        string
	dirCreated bool
	eng        storage.Engine
}

func deprecatedSideloadedPath(
	baseDir string, rangeID roachpb.RangeID, replicaID roachpb.ReplicaID,
) string {
	return filepath.Join(
		baseDir,
		"sideloading",
		fmt.Sprintf("%d", rangeID%1000), // sharding
		fmt.Sprintf("%d.%d", rangeID, replicaID),
	)
}

func sideloadedPath(baseDir string, rangeID roachpb.RangeID) string {
	// Use one level of sharding to avoid too many items per directory. For
	// example, ext3 and older ext4 support only 32k and 64k subdirectories
	// per directory, respectively. Newer FS typically have no such limitation,
	// but still.
	//
	// For example, r1828 will end up in baseDir/r1XXX/r1828.
	return filepath.Join(
		baseDir,
		"sideloading",
		fmt.Sprintf("r%dXXXX", rangeID/10000), // sharding
		fmt.Sprintf("r%d", rangeID),
	)
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func newDiskSideloadStorage(
	st *cluster.Settings,
	rangeID roachpb.RangeID,
	replicaID roachpb.ReplicaID,
	baseDir string,
	limiter *rate.Limiter,
	eng storage.Engine,
) (*diskSideloadStorage, error) {
	path := deprecatedSideloadedPath(baseDir, rangeID, replicaID)
	newPath := sideloadedPath(baseDir, rangeID)
	// NB: this call to exists() is in the hot path when the server starts
	// as it will be called once for each replica. However, during steady
	// state (i.e. when the version variable hasn't *just* flipped), we're
	// expecting `path` to not exist (since it refers to the legacy path at
	// the moment). A stat call for a directory that doesn't exist isn't
	// very expensive (on the order of 1000s of ns). For example, on a 2017
	// MacBook Pro, this case averages ~3245ns and on a gceworker it's
	// ~1200ns. At 50k replicas, that's on the order of a tenth of a second;
	// not enough to matter.
	//
	// On the other hand, successful (i.e. directory found) calls take ~23k
	// ns on my laptop, but only around 2.2k ns on the gceworker. Still,
	// even on the laptop, 50k replicas would only add 1.2s which is also
	// acceptable given that it'll happen only once.
	exists, err := exists(path)
	if err != nil {
		return nil, errors.Wrap(err, "checking pre-migration sideloaded directory")
	}
	if exists {
		if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
			return nil, errors.Wrap(err, "creating migrated sideloaded directory")
		}
		if err := os.Rename(path, newPath); err != nil {
			return nil, errors.Wrap(err, "while migrating sideloaded directory")
		}
	}
	path = newPath

	ss := &diskSideloadStorage{
		dir:     path,
		eng:     eng,
		st:      st,
		limiter: limiter,
	}
	return ss, nil
}

func (ss *diskSideloadStorage) createDir() error {
	err := os.MkdirAll(ss.dir, 0755)
	ss.dirCreated = ss.dirCreated || err == nil
	return err
}

// Dir implements SideloadStorage.
func (ss *diskSideloadStorage) Dir() string {
	return ss.dir
}

// Put implements SideloadStorage.
func (ss *diskSideloadStorage) Put(ctx context.Context, index, term uint64, contents []byte) error {
	filename := ss.filename(ctx, index, term)
	// There's a chance the whole path is missing (for example after Clear()),
	// in which case handle that transparently.
	for {
		// Use 0644 since that's what RocksDB uses:
		// https://github.com/facebook/rocksdb/blob/56656e12d67d8a63f1e4c4214da9feeec2bd442b/env/env_posix.cc#L171
		if err := writeFileSyncing(ctx, filename, contents, ss.eng, 0644, ss.st, ss.limiter); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		// createDir() ensures ss.dir exists but will not create any subdirectories
		// within ss.dir because filename() does not make subdirectories in ss.dir.
		if err := ss.createDir(); err != nil {
			return err
		}
		continue
	}
}

// Get implements SideloadStorage.
func (ss *diskSideloadStorage) Get(ctx context.Context, index, term uint64) ([]byte, error) {
	filename := ss.filename(ctx, index, term)
	b, err := ss.eng.ReadFile(filename)
	if os.IsNotExist(err) {
		return nil, errSideloadedFileNotFound
	}
	return b, err
}

// Filename implements SideloadStorage.
func (ss *diskSideloadStorage) Filename(ctx context.Context, index, term uint64) (string, error) {
	return ss.filename(ctx, index, term), nil
}

func (ss *diskSideloadStorage) filename(ctx context.Context, index, term uint64) string {
	return filepath.Join(ss.dir, fmt.Sprintf("i%d.t%d", index, term))
}

// Purge implements SideloadStorage.
func (ss *diskSideloadStorage) Purge(ctx context.Context, index, term uint64) (int64, error) {
	return ss.purgeFile(ctx, ss.filename(ctx, index, term))
}

func (ss *diskSideloadStorage) fileSize(filename string) (int64, error) {
	// TODO(tschottdorf): this should all be done through the env. As written,
	// the sizes returned here will be wrong if encryption is on. We want the
	// size of the unencrypted payload.
	//
	// See #31913.
	info, err := os.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, errSideloadedFileNotFound
		}
		return 0, err
	}
	return info.Size(), nil
}

func (ss *diskSideloadStorage) purgeFile(ctx context.Context, filename string) (int64, error) {
	size, err := ss.fileSize(filename)
	if err != nil {
		return 0, err
	}
	if err := ss.eng.DeleteFile(filename); err != nil {
		if os.IsNotExist(err) {
			return 0, errSideloadedFileNotFound
		}
		return 0, err
	}
	return size, nil
}

// Clear implements SideloadStorage.
func (ss *diskSideloadStorage) Clear(_ context.Context) error {
	err := ss.eng.DeleteDirAndFiles(ss.dir)
	if os.IsNotExist(err) {
		ss.dirCreated = false
		return nil
	}
	ss.dirCreated = ss.dirCreated && err != nil
	return err
}

// TruncateTo implements SideloadStorage.
func (ss *diskSideloadStorage) TruncateTo(
	ctx context.Context, firstIndex uint64,
) (bytesFreed, bytesRetained int64, _ error) {
	deletedAll := true
	if err := ss.forEach(ctx, func(index uint64, filename string) error {
		if index >= firstIndex {
			size, err := ss.fileSize(filename)
			if err != nil {
				return err
			}
			bytesRetained += size
			deletedAll = false
			return nil
		}
		fileSize, err := ss.purgeFile(ctx, filename)
		if err != nil {
			return err
		}
		bytesFreed += fileSize
		return nil
	}); err != nil {
		return 0, 0, err
	}

	if deletedAll {
		// The directory may not exist, or it may exist and have been empty.
		// Not worth trying to figure out which one, just try to delete.
		err := os.Remove(ss.dir)
		if !os.IsNotExist(err) {
			return bytesFreed, 0, errors.Wrapf(err, "while purging %q", ss.dir)
		}
	}
	return bytesFreed, bytesRetained, nil
}

func (ss *diskSideloadStorage) forEach(
	ctx context.Context, visit func(index uint64, filename string) error,
) error {
	matches, err := filepath.Glob(filepath.Join(ss.dir, "i*.t*"))
	if err != nil {
		return err
	}
	for _, match := range matches {
		base := filepath.Base(match)
		if len(base) < 1 || base[0] != 'i' {
			continue
		}
		base = base[1:]
		upToDot := strings.SplitN(base, ".", 2)
		logIdx, err := strconv.ParseUint(upToDot[0], 10, 64)
		if err != nil {
			return errors.Wrapf(err, "while parsing %q during TruncateTo", match)
		}
		if err := visit(logIdx, match); err != nil {
			return errors.Wrap(err, match)
		}
	}
	return nil
}

// String lists the files in the storage without guaranteeing an ordering.
func (ss *diskSideloadStorage) String() string {
	var buf strings.Builder
	var count int
	if err := ss.forEach(context.Background(), func(_ uint64, filename string) error {
		count++
		_, _ = fmt.Fprintln(&buf, filename)
		return nil
	}); err != nil {
		return err.Error()
	}
	fmt.Fprintf(&buf, "(%d files)\n", count)
	return buf.String()
}
