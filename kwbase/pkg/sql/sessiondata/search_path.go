// Copyright 2016 The Cockroach Authors.
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

package sessiondata

import (
	"strings"

	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgcode"
	"gitee.com/kwbasedb/kwbase/pkg/sql/pgwire/pgerror"
)

// PgDatabaseName is the name of the default postgres system database.
const PgDatabaseName = "postgres"

// DefaultDatabaseName is the name ofthe default CockroachDB database used
// for connections without a current db set.
const DefaultDatabaseName = "defaultdb"

// PgCatalogName is the name of the pg_catalog system schema.
const PgCatalogName = "pg_catalog"

// PgSchemaPrefix is a prefix for Postgres system schemas. Users cannot
// create schemas with this prefix.
const PgSchemaPrefix = "pg_"

// PgTempSchemaName is the alias for temporary schemas across sessions.
const PgTempSchemaName = "pg_temp"

// SearchPath represents a list of namespaces to search builtins in.
// The names must be normalized (as per Name.Normalize) already.
type SearchPath struct {
	paths                []string
	containsPgCatalog    bool
	containsPgTempSchema bool
	tempSchemaName       string
}

// SetSearchPath replace paths on SearchPath. Keep others no change
func SetSearchPath(sp SearchPath, paths []string) SearchPath {
	newSp := MakeSearchPath(paths)
	newSp.tempSchemaName = sp.tempSchemaName
	newSp.containsPgTempSchema = sp.containsPgTempSchema
	return newSp
}

// MakeSearchPath returns a new immutable SearchPath struct. The paths slice
// must not be modified after hand-off to MakeSearchPath.
func MakeSearchPath(paths []string) SearchPath {
	containsPgCatalog := false
	containsPgTempSchema := false
	for _, e := range paths {
		if e == PgCatalogName {
			containsPgCatalog = true
		} else if e == PgTempSchemaName {
			containsPgTempSchema = true
		}
	}
	return SearchPath{
		paths:                paths,
		containsPgCatalog:    containsPgCatalog,
		containsPgTempSchema: containsPgTempSchema,
	}
}

// WithTemporarySchemaName returns a new immutable SearchPath struct with
// the tempSchemaName supplied and the same paths as before.
// This should be called every time a session creates a temporary schema
// for the first time.
func (s SearchPath) WithTemporarySchemaName(tempSchemaName string) SearchPath {
	return SearchPath{
		paths:                s.paths,
		containsPgCatalog:    s.containsPgCatalog,
		containsPgTempSchema: s.containsPgTempSchema,
		tempSchemaName:       tempSchemaName,
	}
}

// UpdatePaths returns a new immutable SearchPath struct with the paths supplied
// and the same tempSchemaName as before.
func (s SearchPath) UpdatePaths(paths []string) SearchPath {
	return MakeSearchPath(paths).WithTemporarySchemaName(s.tempSchemaName)
}

// MaybeResolveTemporarySchema returns the session specific temporary schema
// for the pg_temp alias (only if a temporary schema exists). It acts as a pass
// through for all other schema names.
func (s SearchPath) MaybeResolveTemporarySchema(schemaName string) (string, error) {
	// Only allow access to the session specific temporary schema.
	if strings.HasPrefix(schemaName, PgTempSchemaName) && schemaName != PgTempSchemaName && schemaName != s.tempSchemaName {
		return schemaName, pgerror.New(pgcode.FeatureNotSupported, "cannot access temporary tables of other sessions")
	}
	// If the schemaName is pg_temp and the tempSchemaName has been set, pg_temp
	// is an alias the session specific temp schema.
	if schemaName == PgTempSchemaName && s.tempSchemaName != "" {
		return s.tempSchemaName, nil
	}
	return schemaName, nil
}

// Iter returns an iterator through the search path. We must include the
// implicit pg_catalog and temporary schema at the beginning of the search path,
// unless they have been explicitly set later by the user.
// "The system catalog schema, pg_catalog, is always searched, whether it is
// mentioned in the path or not. If it is mentioned in the path then it will be
// searched in the specified order. If pg_catalog is not in the path then it
// will be searched before searching any of the path items."
// "Likewise, the current session's temporary-table schema, pg_temp_nnn, is
// always searched if it exists. It can be explicitly listed in the path by
// using the alias pg_temp. If it is not listed in the path then it is searched
// first (even before pg_catalog)."
// - https://www.postgresql.org/docs/9.1/static/runtime-config-client.html
func (s SearchPath) Iter() SearchPathIter {
	implicitPgTempSchema := !s.containsPgTempSchema && s.tempSchemaName != ""
	sp := SearchPathIter{
		paths:                s.paths,
		implicitPgCatalog:    !s.containsPgCatalog,
		implicitPgTempSchema: implicitPgTempSchema,
		tempSchemaName:       s.tempSchemaName,
	}
	return sp
}

// IterWithoutImplicitPGSchemas is the same as Iter, but does not include the
// implicit pg_temp and pg_catalog.
func (s SearchPath) IterWithoutImplicitPGSchemas() SearchPathIter {
	sp := SearchPathIter{
		paths:                s.paths,
		implicitPgCatalog:    false,
		implicitPgTempSchema: false,
		tempSchemaName:       s.tempSchemaName,
	}
	return sp
}

// GetPathArray returns the underlying path array of this SearchPath. The
// resultant slice is not to be modified.
func (s SearchPath) GetPathArray() []string {
	return s.paths
}

// GetTemporarySchemaName returns the temporary schema specific to the current
// session.
func (s SearchPath) GetTemporarySchemaName() string {
	return s.tempSchemaName
}

// Equals returns true if two SearchPaths are the same.
func (s SearchPath) Equals(other *SearchPath) bool {
	if s.containsPgCatalog != other.containsPgCatalog {
		return false
	}
	if s.containsPgTempSchema != other.containsPgTempSchema {
		return false
	}
	if len(s.paths) != len(other.paths) {
		return false
	}
	if s.tempSchemaName != other.tempSchemaName {
		return false
	}
	// Fast path: skip the check if it is the same slice.
	if &s.paths[0] != &other.paths[0] {
		for i := range s.paths {
			if s.paths[i] != other.paths[i] {
				return false
			}
		}
	}
	return true
}

func (s SearchPath) String() string {
	return strings.Join(s.paths, ", ")
}

// GetSchema return the first of paths, which is the Schema name
func (s SearchPath) GetSchema() string {
	if len(s.paths) <= 0 {
		return ""
	}
	return s.paths[0]
}

// SearchPathIter enables iteration over the search paths without triggering an
// allocation. Use one of the SearchPath.Iter methods to get an instance of the
// iterator, and then repeatedly call the Next method in order to iterate over
// each search path. The tempSchemaName in the iterator is only set if the session
// has created a temporary schema.
type SearchPathIter struct {
	paths                []string
	implicitPgCatalog    bool
	implicitPgTempSchema bool
	tempSchemaName       string
	i                    int
}

// Next returns the next search path, or false if there are no remaining paths.
func (iter *SearchPathIter) Next() (path string, ok bool) {
	// If the session specific temporary schema has not been created, we can
	// preempt the name resolution failure by simply skipping the implicit pg_temp.
	if iter.implicitPgTempSchema && iter.tempSchemaName != "" {
		iter.implicitPgTempSchema = false
		return iter.tempSchemaName, true
	}
	if iter.implicitPgCatalog {
		iter.implicitPgCatalog = false
		return PgCatalogName, true
	}

	if iter.i < len(iter.paths) {
		iter.i++
		// If pg_temp is explicitly present in the paths, it must be resolved to the
		// session specific temp schema (if one exists). tempSchemaName is set in the
		// iterator iff the session has created a temporary schema.
		if iter.paths[iter.i-1] == PgTempSchemaName {
			// If the session specific temporary schema has not been created we can
			// preempt the resolution failure and iterate to the next entry.
			if iter.tempSchemaName == "" {
				return iter.Next()
			}
			return iter.tempSchemaName, true
		}
		return iter.paths[iter.i-1], true
	}
	return "", false
}
