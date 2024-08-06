// Copyright 2018 The Cockroach Authors.
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

package cat

// Object is implemented by all objects in the catalog.
type Object interface {
	// ID is the unique, stable identifier for this object. See the comment for
	// StableID for more detail.
	ID() StableID

	// DescriptorID is the descriptor ID for this object. This can differ from the
	// ID of this object in some cases. This is only important for reporting the
	// Postgres-compatible identifiers for objects in the various object catalogs.
	// In the vast majority of cases, you should use ID() instead.
	PostgresDescriptorID() StableID

	// Equals returns true if this object is identical to the given Object.
	//
	// Two objects are identical if they have the same identifier and there were
	// no changes to schema or table statistics between the times the two objects
	// were resolved.
	//
	// Used for invalidating cached plans.
	Equals(other Object) bool
}
