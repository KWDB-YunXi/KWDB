// Copyright 2015 The Cockroach Authors.
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

package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gitee.com/kwbasedb/kwbase/pkg/acceptance/cluster"
	"gitee.com/kwbasedb/kwbase/pkg/base"
	"gitee.com/kwbasedb/kwbase/pkg/security"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

func defaultContainerConfig() container.Config {
	return container.Config{
		Image: acceptanceImage,
		Env: []string{
			fmt.Sprintf("PGUSER=%s", security.RootUser),
			fmt.Sprintf("PGPORT=%s", base.DefaultPort),
			"PGSSLCERT=/certs/client.root.crt",
			"PGSSLKEY=/certs/client.root.key",
		},
		Entrypoint: []string{"autouseradd", "-u", "roach", "-C", "/home/roach", "--"},
	}
}

// testDockerFail ensures the specified docker cmd fails.
func testDockerFail(ctx context.Context, t *testing.T, name string, cmd []string) {
	containerConfig := defaultContainerConfig()
	containerConfig.Cmd = cmd
	if err := testDockerSingleNode(ctx, t, name, containerConfig); err == nil {
		t.Error("expected failure")
	}
}

// testDockerSuccess ensures the specified docker cmd succeeds.
func testDockerSuccess(ctx context.Context, t *testing.T, name string, cmd []string) {
	containerConfig := defaultContainerConfig()
	containerConfig.Cmd = cmd
	if err := testDockerSingleNode(ctx, t, name, containerConfig); err != nil {
		t.Error(err)
	}
}

const (
	// Iterating against a locally built version of the docker image can be done
	// by changing acceptanceImage to the hash of the container.
	acceptanceImage = "docker.io/kwbasedb/acceptance:20200303-091324"
)

func testDocker(
	ctx context.Context, t *testing.T, num int, name string, containerConfig container.Config,
) error {
	var err error
	RunDocker(t, func(t *testing.T) {
		cfg := cluster.TestConfig{
			Name:     name,
			Duration: *flagDuration,
		}
		for i := 0; i < num; i++ {
			cfg.Nodes = append(cfg.Nodes, cluster.NodeConfig{Stores: []cluster.StoreConfig{{}}})
		}
		l := StartCluster(ctx, t, cfg).(*cluster.DockerCluster)
		defer l.AssertAndStop(ctx, t)

		if len(l.Nodes) > 0 {
			containerConfig.Env = append(containerConfig.Env, "PGHOST="+l.Hostname(0))
		}
		var pwd string
		pwd, err = os.Getwd()
		if err != nil {
			return
		}
		hostConfig := container.HostConfig{
			NetworkMode: "host",
			Binds:       []string{filepath.Join(pwd, "testdata") + ":/mnt/data"},
		}
		err = l.OneShot(
			ctx, acceptanceImage, types.ImagePullOptions{}, containerConfig, hostConfig, "docker-"+name,
		)
		preserveLogs := err != nil
		l.Cleanup(ctx, preserveLogs)
	})
	return err
}

func testDockerSingleNode(
	ctx context.Context, t *testing.T, name string, containerConfig container.Config,
) error {
	return testDocker(ctx, t, 1, name, containerConfig)
}

func testDockerOneShot(
	ctx context.Context, t *testing.T, name string, containerConfig container.Config,
) error {
	return testDocker(ctx, t, 0, name, containerConfig)
}
