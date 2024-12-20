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

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"gitee.com/kwbasedb/kwbase/pkg/cmd/roachprod/cloud"
	"gitee.com/kwbasedb/kwbase/pkg/cmd/roachprod/config"
	"gitee.com/kwbasedb/kwbase/pkg/cmd/roachprod/install"
	"github.com/cockroachdb/errors"
)

func initDirs() error {
	hd := os.ExpandEnv(config.DefaultHostDir)
	if err := os.MkdirAll(hd, 0755); err != nil {
		return err
	}
	return os.MkdirAll(os.ExpandEnv(config.DefaultDebugDir), 0755)
}

func syncHosts(cloud *cloud.Cloud) error {
	hd := os.ExpandEnv(config.DefaultHostDir)

	// Write all host files. We're the only process doing this due to the file
	// lock acquired by syncAll, but other processes may be reading the host
	// files concurrently so we need to write the files atomically by writing to
	// a temporary file and renaming.
	for _, c := range cloud.Clusters {
		filename := path.Join(hd, c.Name)
		tmpFile := filename + ".tmp"

		err := func() error {
			file, err := os.Create(tmpFile)
			if err != nil {
				return errors.Wrapf(err, "problem creating file %s", filename)
			}
			defer file.Close()

			// Align columns left and separate with at least two spaces.
			tw := tabwriter.NewWriter(file, 0, 8, 2, ' ', 0)
			if _, err := tw.Write([]byte("# user@host\tlocality\tvpcId\n")); err != nil {
				return err
			}
			for _, vm := range c.VMs {
				if _, err := tw.Write([]byte(fmt.Sprintf(
					"%s@%s\t%s\t%s\n", vm.RemoteUser, vm.PublicIP, vm.Locality(), vm.VPC))); err != nil {
					return err
				}
			}
			if err := tw.Flush(); err != nil {
				return errors.Wrapf(err, "problem writing file %s", filename)
			}
			return nil
		}()
		if err != nil {
			return err
		}

		if err := os.Rename(tmpFile, filename); err != nil {
			return err
		}
	}

	return gcHostsFiles(cloud)
}

func gcHostsFiles(cloud *cloud.Cloud) error {
	hd := os.ExpandEnv(config.DefaultHostDir)
	files, err := ioutil.ReadDir(hd)
	if err != nil {
		return err
	}

	for _, file := range files {
		if !file.Mode().IsRegular() {
			continue
		}
		if _, ok := cloud.Clusters[file.Name()]; ok {
			continue
		}

		filename := filepath.Join(hd, file.Name())
		if err = os.Remove(filename); err != nil {
			log.Printf("failed to remove file %s", filename)
		}
	}
	return nil
}

func newInvalidHostsLineErr(line string) error {
	return fmt.Errorf("invalid hosts line, expected <username>@<host> [locality] [vpcId], got %q", line)
}

func loadClusters() error {
	hd := os.ExpandEnv(config.DefaultHostDir)
	files, err := ioutil.ReadDir(hd)
	if err != nil {
		return err
	}

	debugDir := os.ExpandEnv(config.DefaultDebugDir)

	for _, file := range files {
		if !file.Mode().IsRegular() {
			continue
		}
		if strings.HasSuffix(file.Name(), ".tmp") {
			continue
		}

		filename := filepath.Join(hd, file.Name())
		contents, err := ioutil.ReadFile(filename)
		if err != nil {
			return errors.Wrapf(err, "could not read %s", filename)
		}
		lines := strings.Split(string(contents), "\n")

		c := &install.SyncedCluster{
			Name:     file.Name(),
			DebugDir: debugDir,
		}

		for _, l := range lines {
			// We'll consume the fields as we go along
			fields := strings.Fields(l)
			if len(fields) == 0 {
				continue
			} else if len(fields[0]) > 0 && fields[0][0] == '#' {
				// Comment line.
				continue
			} else if len(fields) > 3 {
				return newInvalidHostsLineErr(l)
			}

			parts := strings.Split(fields[0], "@")
			fields = fields[1:]
			var n, u string
			if len(parts) == 1 {
				u = config.OSUser.Username
				n = parts[0]
			} else if len(parts) == 2 {
				u = parts[0]
				n = parts[1]
			} else {
				return newInvalidHostsLineErr(l)
			}
			// NB: it turns out we do see empty hosts here if we are concurrently
			// creating clusters and this sync is picking up a cluster that's not
			// ready yet. See:
			// https://gitee.com/kwbasedb/kwbase/issues/49542#issuecomment-634563130
			// if n == "" {
			// 	return newInvalidHostsLineErr(l)
			// }

			var locality string
			if len(fields) > 0 {
				locality = fields[0]
				fields = fields[1:]
			}

			var vpc string
			if len(fields) > 0 {
				vpc = fields[0]
				fields = fields[1:]
			}

			if len(fields) > 0 {
				return newInvalidHostsLineErr(l)
			}

			c.VMs = append(c.VMs, n)
			c.Users = append(c.Users, u)
			c.Localities = append(c.Localities, locality)
			c.VPCs = append(c.VPCs, vpc)
		}
		if len(c.VMs) == 0 {
			return errors.Errorf("found no VMs in %s", contents)
		}
		install.Clusters[file.Name()] = c
	}

	return nil
}
