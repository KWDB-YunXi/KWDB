// Copyright 2018 The Cockroach Authors.
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

package roachpb

import "gitee.com/kwbasedb/kwbase/pkg/util/interval"

// A SpanGroup is a specialization of interval.RangeGroup which deals
// with key spans. The zero-value of a SpanGroup can be used immediately.
//
// A SpanGroup does not support concurrent use.
type SpanGroup struct {
	rg interval.RangeGroup
}

func (g *SpanGroup) checkInit() {
	if g.rg == nil {
		g.rg = interval.NewRangeTree()
	}
}

// Add will attempt to add the provided Spans to the SpanGroup,
// returning whether the addition increased the span of the group
// or not.
func (g *SpanGroup) Add(spans ...Span) bool {
	if len(spans) == 0 {
		return false
	}
	ret := false
	g.checkInit()
	for _, span := range spans {
		ret = g.rg.Add(s2r(span)) || ret
	}
	return ret
}

// Contains returns whether or not the provided Key is contained
// within the group of Spans in the SpanGroup.
func (g *SpanGroup) Contains(k Key) bool {
	if g.rg == nil {
		return false
	}
	return g.rg.Encloses(interval.Range{
		Start: interval.Comparable(k),
		// Use the next key since range-ends are exclusive.
		End: interval.Comparable(k.Next()),
	})
}

// Len returns the number of Spans currently within the SpanGroup.
// This will always be equal to or less than the number of spans added,
// as spans that overlap will merge to produce a single larger span.
func (g *SpanGroup) Len() int {
	if g.rg == nil {
		return 0
	}
	return g.rg.Len()
}

var _ = (*SpanGroup).Len

// Slice will return the contents of the SpanGroup as a slice of Spans.
func (g *SpanGroup) Slice() []Span {
	rg := g.rg
	if rg == nil {
		return nil
	}
	ret := make([]Span, 0, rg.Len())
	it := rg.Iterator()
	for {
		rng, next := it.Next()
		if !next {
			break
		}
		ret = append(ret, r2s(rng))
	}
	return ret
}

// s2r converts a Span to an interval.Range.  Since the Key and
// interval.Comparable types are both just aliases of []byte,
// we don't have to perform any other conversion.
func s2r(s Span) interval.Range {
	// Per docs on Span, if the span represents only a single key,
	// the EndKey value may be empty.  We'll handle this case by
	// ensuring we always have an exclusive end key value.
	var end = s.EndKey
	if len(end) == 0 || s.Key.Equal(s.EndKey) {
		end = s.Key.Next()
	}
	return interval.Range{
		Start: interval.Comparable(s.Key),
		End:   interval.Comparable(end),
	}
}

// r2s converts a Range to a Span
func r2s(r interval.Range) Span {
	return Span{
		Key:    Key(r.Start),
		EndKey: Key(r.End),
	}
}
