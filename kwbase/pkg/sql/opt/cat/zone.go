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

package cat

import (
	"bytes"
	"fmt"

	"gitee.com/kwbasedb/kwbase/pkg/util/treeprinter"
)

// Zone is an interface to zone configuration information used by the optimizer.
// The optimizer prefers indexes with constraints that best match the locality
// of the gateway node that plans the query.
type Zone interface {
	// ReplicaConstraintsCount returns the number of replica constraint sets that
	// are part of this zone.
	ReplicaConstraintsCount() int

	// ReplicaConstraints returns the ith set of replica constraints in the zone,
	// where i < ReplicaConstraintsCount.
	ReplicaConstraints(i int) ReplicaConstraints

	// LeasePreferenceCount returns the number of lease preferences that are part
	// of this zone.
	LeasePreferenceCount() int

	// LeasePreference returns the ith lease preference in the zone, where
	// i < LeasePreferenceCount.
	LeasePreference(i int) ConstraintSet
}

// ConstraintSet is a set of constraints that apply to a range, restricting
// which nodes can host that range or stating which nodes are preferred as the
// leaseholder.
type ConstraintSet interface {
	// ConstraintCount returns the number of constraints in the set.
	ConstraintCount() int

	// Constraint returns the ith constraint in the set, where
	// i < ConstraintCount.
	Constraint(i int) Constraint
}

// ReplicaConstraints is a set of constraints that apply to one or more replicas
// of a range, restricting which nodes can host that range. For example, if a
// table range has three replicas, then two of the replicas might be pinned to
// nodes in one region, whereas the third might be pinned to another region.
type ReplicaConstraints interface {
	ConstraintSet

	// ReplicaCount returns the number of replicas that should abide by this set
	// of constraints. If 0, then the constraints apply to all replicas of the
	// range (and there can be only one ReplicaConstraints in the Zone).
	ReplicaCount() int32
}

// Constraint governs placement of range replicas on nodes. A constraint can
// either be required or prohibited. A required constraint's key/value pair must
// match one of the tiers of a node's locality for the range to locate there.
// A prohibited constraint's key/value pair must *not* match any of the tiers of
// a node's locality for the range to locate there. For example:
//
//   +region=east     Range can only be placed on nodes in region=east locality.
//   -region=west     Range cannot be placed on nodes in region=west locality.
//
type Constraint interface {
	// IsRequired is true if this is a required constraint, or false if this is
	// a prohibited constraint (signified by initial + or - character).
	IsRequired() bool

	// GetKey returns the constraint's string key (to left of =).
	GetKey() string

	// GetValue returns the constraint's string value (to right of =).
	GetValue() string
}

// FormatZone nicely formats a catalog zone using a treeprinter for debugging
// and testing.
func FormatZone(zone Zone, tp treeprinter.Node) {
	if zone.ReplicaConstraintsCount() == 0 && zone.LeasePreferenceCount() == 0 {
		return
	}
	zoneChild := tp.Childf("ZONE")

	replicaChild := zoneChild
	if zone.ReplicaConstraintsCount() > 1 {
		replicaChild = replicaChild.Childf("replica constraints")
	}
	for i, n := 0, zone.ReplicaConstraintsCount(); i < n; i++ {
		replConstraint := zone.ReplicaConstraints(i)
		constraintStr := formatConstraintSet(replConstraint)
		if zone.ReplicaConstraintsCount() > 1 {
			numReplicas := replConstraint.ReplicaCount()
			replicaChild.Childf("%d replicas: %s", numReplicas, constraintStr)
		} else {
			replicaChild.Childf("constraints: %s", constraintStr)
		}
	}

	leaseChild := zoneChild
	if zone.LeasePreferenceCount() > 1 {
		leaseChild = leaseChild.Childf("lease preferences")
	}
	for i, n := 0, zone.LeasePreferenceCount(); i < n; i++ {
		leasePref := zone.LeasePreference(i)
		constraintStr := formatConstraintSet(leasePref)
		if zone.LeasePreferenceCount() > 1 {
			leaseChild.Child(constraintStr)
		} else {
			leaseChild.Childf("lease preference: %s", constraintStr)
		}
	}
}

func formatConstraintSet(set ConstraintSet) string {
	var buf bytes.Buffer
	buf.WriteRune('[')
	for i, n := 0, set.ConstraintCount(); i < n; i++ {
		constraint := set.Constraint(i)
		if i != 0 {
			buf.WriteRune(',')
		}
		if constraint.IsRequired() {
			buf.WriteRune('+')
		} else {
			buf.WriteRune('-')
		}
		if constraint.GetKey() != "" {
			fmt.Fprintf(&buf, "%s=%s", constraint.GetKey(), constraint.GetValue())
		} else {
			buf.WriteString(constraint.GetValue())
		}
	}
	buf.WriteRune(']')
	return buf.String()
}
