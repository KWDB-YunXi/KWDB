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

package gc

import (
	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/rditer"
	"gitee.com/kwbasedb/kwbase/pkg/roachpb"
	"gitee.com/kwbasedb/kwbase/pkg/storage"
	"gitee.com/kwbasedb/kwbase/pkg/util/bufalloc"
)

// gcIterator wraps an rditer.ReplicaDataIterator which it reverse iterates for
// the purpose of discovering gc-able replicated data.
type gcIterator struct {
	it   *rditer.ReplicaDataIterator
	done bool
	err  error
	buf  gcIteratorRingBuf
}

func makeGCIterator(desc *roachpb.RangeDescriptor, snap storage.Reader) gcIterator {
	return gcIterator{
		it: rditer.NewReplicaDataIterator(desc, snap,
			true /* replicatedOnly */, true /* seekEnd */),
	}
}

type gcIteratorState struct {
	cur, next, afterNext *storage.MVCCKeyValue
}

// curIsNewest returns true if the current MVCCKeyValue in the gcIteratorState
// is the newest committed version of the key.
//
// It returns true if next is nil or if next is an intent.
func (s *gcIteratorState) curIsNewest() bool {
	return s.cur.Key.IsValue() &&
		(s.next == nil || (s.afterNext != nil && !s.afterNext.Key.IsValue()))
}

// curIsNotValue returns true if the current MVCCKeyValue in the gcIteratorState
// is not a value, i.e. does not have a timestamp.
func (s *gcIteratorState) curIsNotValue() bool {
	return !s.cur.Key.IsValue()
}

// curIsIntent returns true if the current MVCCKeyValue in the gcIteratorState
// is an intent.
func (s *gcIteratorState) curIsIntent() bool {
	return s.next != nil && !s.next.Key.IsValue()
}

// state returns the current state of the iterator. The state contains the
// current and the two following versions of the current key if they exist.
//
// If ok is false, further iteration is unsafe; either the end of iteration has
// been reached or an error has occurred. Callers should check it.err to
// determine whether an error has occurred in cases where ok is false.
//
// It is not safe to use values in the state after subsequent calls to
// it.step().
func (it *gcIterator) state() (s gcIteratorState, ok bool) {
	// The current key is the newest if the key which comes next is different or
	// the key which comes after the current key is an intent or this is the first
	// key in the range.
	s.cur, ok = it.peekAt(0)
	if !ok {
		return gcIteratorState{}, false
	}
	next, ok := it.peekAt(1)
	if !ok && it.err != nil { // cur is the first key in the range
		return gcIteratorState{}, false
	}
	if !ok || !next.Key.Key.Equal(s.cur.Key.Key) {
		return s, true
	}
	s.next = next
	afterNext, ok := it.peekAt(2)
	if !ok && it.err != nil { // cur is the first key in the range
		return gcIteratorState{}, false
	}
	if !ok || !afterNext.Key.Key.Equal(s.cur.Key.Key) {
		return s, true
	}
	s.afterNext = afterNext
	return s, true
}

func (it *gcIterator) step() {
	it.buf.removeFront()
}

func (it *gcIterator) peekAt(i int) (*storage.MVCCKeyValue, bool) {
	if it.buf.len <= i {
		if !it.fillTo(i + 1) {
			return nil, false
		}
	}
	return it.buf.at(i), true
}

func (it *gcIterator) fillTo(targetLen int) (ok bool) {
	for it.buf.len < targetLen {
		if ok, err := it.it.Valid(); !ok {
			it.err, it.done = err, err == nil
			return false
		}
		it.buf.pushBack(it.it)
		it.it.Prev()
	}
	return true
}

func (it *gcIterator) close() {
	it.it.Close()
	it.it = nil
}

// gcIteratorRingBufSize is 3 because the gcIterator.state method at most needs
// to look forward two keys ahead of the current key.
const gcIteratorRingBufSize = 3

type gcIteratorRingBuf struct {
	allocs [gcIteratorRingBufSize]bufalloc.ByteAllocator
	buf    [gcIteratorRingBufSize]storage.MVCCKeyValue
	len    int
	head   int
}

func (b *gcIteratorRingBuf) at(i int) *storage.MVCCKeyValue {
	if i >= b.len {
		panic("index out of range")
	}
	return &b.buf[(b.head+i)%gcIteratorRingBufSize]
}

func (b *gcIteratorRingBuf) removeFront() {
	if b.len == 0 {
		panic("cannot remove from empty gcIteratorRingBuf")
	}
	b.buf[b.head] = storage.MVCCKeyValue{}
	b.head = (b.head + 1) % gcIteratorRingBufSize
	b.len--
}

type iterator interface {
	UnsafeKey() storage.MVCCKey
	UnsafeValue() []byte
}

func (b *gcIteratorRingBuf) pushBack(it iterator) {
	if b.len == gcIteratorRingBufSize {
		panic("cannot add to full gcIteratorRingBuf")
	}
	i := (b.head + b.len) % gcIteratorRingBufSize
	b.allocs[i] = b.allocs[i][:0]
	k := it.UnsafeKey()
	v := it.UnsafeValue()
	b.allocs[i], k.Key = b.allocs[i].Copy(k.Key, len(v))
	b.allocs[i], v = b.allocs[i].Copy(v, 0)
	b.buf[i] = storage.MVCCKeyValue{
		Key:   k,
		Value: v,
	}
	b.len++
}
