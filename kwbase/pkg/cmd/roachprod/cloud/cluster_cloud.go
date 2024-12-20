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

package cloud

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/cmd/roachprod/config"
	"gitee.com/kwbasedb/kwbase/pkg/cmd/roachprod/vm"
	"github.com/cockroachdb/errors"
)

const vmNameFormat = "user-<clusterid>-<nodeid>"

// Cloud TODO(peter): document
type Cloud struct {
	Clusters map[string]*Cluster `json:"clusters"`
	// Any VM in this list can be expected to have at least one element
	// in its Errors field.
	BadInstances vm.List `json:"bad_instances"`
}

// Clone creates a deep copy of the receiver.
func (c *Cloud) Clone() *Cloud {
	cc := *c
	cc.Clusters = make(map[string]*Cluster, len(c.Clusters))
	for k, v := range c.Clusters {
		cc.Clusters[k] = v
	}
	return &cc
}

// BadInstanceErrors TODO(peter): document
func (c *Cloud) BadInstanceErrors() map[error]vm.List {
	ret := map[error]vm.List{}

	// Expand instances and errors
	for _, vm := range c.BadInstances {
		for _, err := range vm.Errors {
			ret[err] = append(ret[err], vm)
		}
	}

	// Sort each List to make the output prettier
	for _, v := range ret {
		sort.Sort(v)
	}

	return ret
}

func newCloud() *Cloud {
	return &Cloud{
		Clusters: make(map[string]*Cluster),
	}
}

// A Cluster is created by querying various vm.Provider instances.
//
// TODO(benesch): unify with syncedCluster.
type Cluster struct {
	Name string `json:"name"`
	User string `json:"user"`
	// This is the earliest creation and shortest lifetime across VMs.
	CreatedAt time.Time     `json:"created_at"`
	Lifetime  time.Duration `json:"lifetime"`
	VMs       vm.List       `json:"vms"`
}

// Clouds returns the names of all of the various cloud providers used
// by the VMs in the cluster.
func (c *Cluster) Clouds() []string {
	present := make(map[string]bool)
	for _, m := range c.VMs {
		present[m.Provider] = true
	}

	var ret []string
	for provider := range present {
		ret = append(ret, provider)
	}
	sort.Strings(ret)
	return ret
}

// ExpiresAt TODO(peter): document
func (c *Cluster) ExpiresAt() time.Time {
	return c.CreatedAt.Add(c.Lifetime)
}

// GCAt TODO(peter): document
func (c *Cluster) GCAt() time.Time {
	// NB: GC is performed every hour. We calculate the lifetime of the cluster
	// taking the GC time into account to accurately reflect when the cluster
	// will be destroyed.
	return c.ExpiresAt().Add(time.Hour - 1).Truncate(time.Hour)
}

// LifetimeRemaining TODO(peter): document
func (c *Cluster) LifetimeRemaining() time.Duration {
	return time.Until(c.GCAt())
}

func (c *Cluster) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s: %d", c.Name, len(c.VMs))
	if !c.IsLocal() {
		fmt.Fprintf(&buf, " (%s)", c.LifetimeRemaining().Round(time.Second))
	}
	return buf.String()
}

// PrintDetails TODO(peter): document
func (c *Cluster) PrintDetails() {
	fmt.Printf("%s: %s ", c.Name, c.Clouds())
	if !c.IsLocal() {
		l := c.LifetimeRemaining().Round(time.Second)
		if l <= 0 {
			fmt.Printf("expired %s ago\n", -l)
		} else {
			fmt.Printf("%s remaining\n", l)
		}
	} else {
		fmt.Printf("(no expiration)\n")
	}
	for _, vm := range c.VMs {
		fmt.Printf("  %s\t%s\t%s\t%s\n", vm.Name, vm.DNS, vm.PrivateIP, vm.PublicIP)
	}
}

// IsLocal TODO(peter): document
func (c *Cluster) IsLocal() bool {
	return c.Name == config.Local
}

func namesFromVM(v vm.VM) (string, string, error) {
	if v.IsLocal() {
		return config.Local, config.Local, nil
	}
	name := v.Name
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return "", "", fmt.Errorf("expected VM name in the form %s, got %s", vmNameFormat, name)
	}
	return parts[0], strings.Join(parts[:len(parts)-1], "-"), nil
}

// ListCloud TODO(peter): document
func ListCloud() (*Cloud, error) {
	cloud := newCloud()

	for _, p := range vm.Providers {
		vms, err := p.List()
		if err != nil {
			return nil, err
		}

		for _, v := range vms {
			// Parse cluster/user from VM name, but only for non-local VMs
			userName, clusterName, err := namesFromVM(v)
			if err != nil {
				v.Errors = append(v.Errors, vm.ErrInvalidName)
			}

			// Anything with an error gets tossed into the BadInstances slice, and we'll correct
			// the problem later on.
			if len(v.Errors) > 0 {
				cloud.BadInstances = append(cloud.BadInstances, v)
				continue
			}

			if _, ok := cloud.Clusters[clusterName]; !ok {
				cloud.Clusters[clusterName] = &Cluster{
					Name:      clusterName,
					User:      userName,
					CreatedAt: v.CreatedAt,
					Lifetime:  v.Lifetime,
					VMs:       nil,
				}
			}

			// Bound the cluster creation time and overall lifetime to the earliest and/or shortest VM
			c := cloud.Clusters[clusterName]
			c.VMs = append(c.VMs, v)
			if v.CreatedAt.Before(c.CreatedAt) {
				c.CreatedAt = v.CreatedAt
			}
			if v.Lifetime < c.Lifetime {
				c.Lifetime = v.Lifetime
			}
		}
	}

	// Sort VMs for each cluster. We want to make sure we always have the same order.
	for _, c := range cloud.Clusters {
		sort.Sort(c.VMs)
	}

	return cloud, nil
}

// CreateCluster TODO(peter): document
func CreateCluster(nodes int, opts vm.CreateOpts) error {
	providerCount := len(opts.VMProviders)
	if providerCount == 0 {
		return errors.New("no VMProviders configured")
	}

	// Allocate vm names over the configured providers
	vmLocations := map[string][]string{}
	for i, p := 1, 0; i <= nodes; i++ {
		pName := opts.VMProviders[p]
		vmName := vm.Name(opts.ClusterName, i)
		vmLocations[pName] = append(vmLocations[pName], vmName)

		p = (p + 1) % providerCount
	}

	return vm.ProvidersParallel(opts.VMProviders, func(p vm.Provider) error {
		return p.Create(vmLocations[p.Name()], opts)
	})
}

// DestroyCluster TODO(peter): document
func DestroyCluster(c *Cluster) error {
	return vm.FanOut(c.VMs, func(p vm.Provider, vms vm.List) error {
		// Enable a fast-path for providers that can destroy a cluster in one shot.
		if x, ok := p.(vm.DeleteCluster); ok {
			return x.DeleteCluster(c.Name)
		}
		return p.Delete(vms)
	})
}

// ExtendCluster TODO(peter): document
func ExtendCluster(c *Cluster, extension time.Duration) error {
	newLifetime := c.Lifetime + extension

	return vm.FanOut(c.VMs, func(p vm.Provider, vms vm.List) error {
		return p.Extend(vms, newLifetime)
	})
}
