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

package tree

import (
	"context"

	"gitee.com/kwbasedb/kwbase/pkg/kv"
	"gitee.com/kwbasedb/kwbase/pkg/sql/types"
)

// Table generators, also called "set-generating functions", are
// special functions that return an entire table.
//
// Overview of the concepts:
//
// - ValueGenerator is an interface that offers a
//   Start/Next/Values/Stop API similar to sql.planNode.
//
// - because generators are regular functions, it is possible to use
//   them in any expression context. This is useful to e.g
//   pass an entire table as argument to the ARRAY( ) conversion
//   function.
//
// - the data source mechanism in the sql package has a special case
//   for generators appearing in FROM contexts and knows how to
//   construct a special row source from them.

// ValueGenerator is the interface provided by the value generator
// functions for SQL SRfs. Objects that implement this interface are
// able to produce rows of values in a streaming fashion (like Go
// iterators or generators in Python).
type ValueGenerator interface {
	// ResolvedType returns the type signature of this value generator.
	ResolvedType() *types.T

	// Start initializes the generator. Must be called once before
	// Next() and Values(). It can be called again to restart
	// the generator after Next() has returned false.
	//
	// txn represents the txn that the generator will run inside of. The generator
	// is expected to hold on to this txn and use it in Next() calls.
	Start(ctx context.Context, txn *kv.Txn) error

	// Next determines whether there is a row of data available.
	Next(context.Context) (bool, error)

	// Values retrieves the current row of data.
	Values() Datums

	// Close must be called after Start() before disposing of the
	// ValueGenerator. It does not need to be called if Start() has not
	// been called yet. It must not be called in-between restarts.
	Close()
}

// GeneratorFactory is the type of constructor functions for
// ValueGenerator objects.
type GeneratorFactory func(ctx *EvalContext, args Datums) (ValueGenerator, error)
