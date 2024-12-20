// Copyright 2020 The Cockroach Authors.
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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func registerPebble(r *testRegistry) {
	pebble := os.Getenv("PEBBLE_BIN")
	if pebble == "" {
		pebble = "./pebble.linux"
	}

	run := func(ctx context.Context, t *test, c *cluster, size int) {
		c.Put(ctx, pebble, "./pebble")

		const initialKeys = 10_000_000
		const cache = 4 << 30 // 4 GB
		const duration = 10 * time.Minute
		const dataDir = "$(dirname {store-dir})"
		const dataTar = dataDir + "/data.tar"
		const benchDir = dataDir + "/bench"

		runCmd := func(cmd string) {
			c.l.PrintfCtx(ctx, "> %s", cmd)
			err := c.RunL(ctx, c.l, c.All(), cmd)
			c.l.Printf("> result: %+v", err)
			if err := ctx.Err(); err != nil {
				c.l.Printf("(note: incoming context was canceled: %s", err)
			}
			if err != nil {
				t.Fatal(err)
			}
		}

		// Generate the initial DB state. This is somewhat time consuming for
		// larger value sizes, so we do this once and reuse the same DB state on
		// all of the workloads.
		runCmd(fmt.Sprintf(
			"(./pebble bench ycsb %s"+
				" --wipe "+
				" --workload=read=100"+
				" --concurrency=1"+
				" --values=%d"+
				" --initial-keys=%d"+
				" --cache=%d"+
				" --num-ops=1 && "+
				"rm -f %s && tar cvPf %s %s) > init.log 2>&1",
			benchDir, size, initialKeys, cache, dataTar, dataTar, benchDir))

		for _, workload := range []string{"A", "B", "C", "D", "E"} {
			keys := "zipf"
			switch workload {
			case "D":
				keys = "uniform"
			}

			runCmd(fmt.Sprintf(
				"rm -fr %s && tar xPf %s &&"+
					" ./pebble bench ycsb %s"+
					" --workload=%s"+
					" --concurrency=256"+
					" --values=%d"+
					" --keys=%s"+
					" --initial-keys=0"+
					" --prepopulated-keys=%d"+
					" --cache=%d"+
					" --duration=%s > ycsb.log 2>&1",
				benchDir, dataTar, benchDir, workload, size, keys, initialKeys, cache, duration))

			dest := filepath.Join(t.artifactsDir, fmt.Sprintf("ycsb_%s.log", workload))
			if err := c.Get(ctx, c.l, "ycsb.log", dest, c.All()); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, size := range []int{64, 1024} {
		size := size
		r.Add(testSpec{
			Name:       fmt.Sprintf("pebble/ycsb/size=%d", size),
			Owner:      OwnerStorage,
			Timeout:    2 * time.Hour,
			MinVersion: "v20.1.0",
			Cluster:    makeClusterSpec(5, cpu(16)),
			Tags:       []string{"pebble"},
			Run: func(ctx context.Context, t *test, c *cluster) {
				run(ctx, t, c, size)
			},
		})
	}
}
