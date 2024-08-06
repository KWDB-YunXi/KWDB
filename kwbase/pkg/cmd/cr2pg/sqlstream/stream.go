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

// Package sqlstream streams an io.Reader into SQL statements.
package sqlstream

import (
	"bufio"
	"io"

	"gitee.com/kwbasedb/kwbase/pkg/sql/parser"
	// Include this because the parser assumes builtin functions exist.
	_ "gitee.com/kwbasedb/kwbase/pkg/sql/sem/builtins"
	"gitee.com/kwbasedb/kwbase/pkg/sql/sem/tree"
	"github.com/pkg/errors"
)

// Modified from importccl/read_import_pgdump.go.

// Stream streams an io.Reader into tree.Statements.
type Stream struct {
	scan *bufio.Scanner
}

// NewStream returns a new Stream to read from r.
func NewStream(r io.Reader) *Stream {
	const defaultMax = 1024 * 1024 * 32
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, defaultMax), defaultMax)
	p := &Stream{scan: s}
	s.Split(splitSQLSemicolon)
	return p
}

// splitSQLSemicolon is a bufio.SplitFunc that splits on SQL semicolon tokens.
func splitSQLSemicolon(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	if pos, ok := parser.SplitFirstStatement(string(data)); ok {
		return pos, data[:pos], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

// Next returns the next statement, or io.EOF if complete.
func (s *Stream) Next() (tree.Statement, error) {
	for s.scan.Scan() {
		t := s.scan.Text()
		stmts, err := parser.Parse(t)
		if err != nil {
			return nil, err
		}
		switch len(stmts) {
		case 0:
			// Got whitespace or comments; try again.
		case 1:
			return stmts[0].AST, nil
		default:
			return nil, errors.Errorf("unexpected: got %d statements", len(stmts))
		}
	}
	if err := s.scan.Err(); err != nil {
		if err == bufio.ErrTooLong {
			err = errors.New("line too long")
		}
		return nil, err
	}
	return nil, io.EOF
}
