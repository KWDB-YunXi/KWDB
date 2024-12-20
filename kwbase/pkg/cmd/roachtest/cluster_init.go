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
	"context"
	gosql "database/sql"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/server"
	"gitee.com/kwbasedb/kwbase/pkg/server/serverpb"
	"gitee.com/kwbasedb/kwbase/pkg/util/httputil"
	"gitee.com/kwbasedb/kwbase/pkg/util/retry"
	"golang.org/x/sync/errgroup"
)

func runClusterInit(ctx context.Context, t *test, c *cluster) {
	c.Put(ctx, kwbase, "./kwbase")

	addrs := c.InternalAddr(ctx, c.All())

	// TODO(tbg): this should never happen, but I saw it locally. The result
	// is the test hanging forever, because all nodes will create their own
	// single node cluster and waitForFullReplication never returns.
	if addrs[0] == "" {
		t.Fatal("no address for first node")
	}

	// We start all nodes with the same join flags and then issue an "init"
	// command to one of the nodes.
	for _, initNode := range []int{1, 2} {
		c.Wipe(ctx)

		func() {
			var g errgroup.Group
			for i := 1; i <= c.spec.NodeCount; i++ {
				i := i
				g.Go(func() error {
					return c.RunE(ctx, c.Node(i),
						fmt.Sprintf(
							`mkdir -p {log-dir} && `+
								`./kwbase start --insecure --background --store={store-dir} `+
								`--log-dir={log-dir} --cache=10%% --max-sql-memory=10%% `+
								`--listen-addr=:{pgport:%[1]d} --http-port=$[{pgport:%[1]d}+1] `+
								`--join=`+strings.Join(addrs, ",")+
								`> {log-dir}/kwbase.stdout 2> {log-dir}/kwbase.stderr`, i))
				})
			}

			urlMap := make(map[int]string)
			for i, addr := range c.ExternalAdminUIAddr(ctx, c.All()) {
				urlMap[i+1] = `http://` + addr
			}

			// Wait for the servers to bind their ports.
			if err := retry.ForDuration(10*time.Second, func() error {
				for i := 1; i <= c.spec.NodeCount; i++ {
					resp, err := httputil.Get(ctx, urlMap[i]+"/health")
					if err != nil {
						return err
					}
					resp.Body.Close()
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			var dbs []*gosql.DB
			for i := 1; i <= c.spec.NodeCount; i++ {
				db := c.Conn(ctx, i)
				defer db.Close()
				dbs = append(dbs, db)
			}

			// Initially, we can connect to any node, but queries issued will hang.
			errCh := make(chan error, len(dbs))
			for _, db := range dbs {
				db := db
				go func() {
					var val int
					errCh <- db.QueryRow("SELECT 1").Scan(&val)
				}()
			}

			// Give them time to get a "connection refused" or similar error if
			// the server isn't listening.
			time.Sleep(time.Second)
			select {
			case err := <-errCh:
				t.Fatalf("query finished prematurely with err %v", err)
			default:
			}

			// Check that the /health endpoint is functional even before cluster init,
			// whereas other debug endpoints return an appropriate error.
			httpTests := []struct {
				endpoint       string
				expectedStatus int
			}{
				{"/health", http.StatusOK},
				{"/health?ready=1", http.StatusServiceUnavailable},
				{"/_status/nodes", http.StatusNotFound},
			}
			for _, tc := range httpTests {
				for _, withCookie := range []bool{false, true} {
					req, err := http.NewRequest("GET", urlMap[1]+tc.endpoint, nil /* body */)
					if err != nil {
						t.Fatalf("unexpected error while constructing request for %s: %s", tc.endpoint, err)
					}
					if withCookie {
						// Prevent regression of #25771 by also sending authenticated
						// requests, like would be sent if an admin UI were open against
						// this node while it booted.
						cookie, err := server.EncodeSessionCookie(&serverpb.SessionCookie{
							// The actual contents of the cookie don't matter; the presence of
							// a valid encoded cookie is enough to trigger the authentication
							// code paths.
						}, false /* forHTTPSOnly - cluster is insecure */)
						if err != nil {
							t.Fatal(err)
						}
						req.AddCookie(cookie)
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatalf("unexpected error hitting %s endpoint: %v", tc.endpoint, err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != tc.expectedStatus {
						bodyBytes, _ := ioutil.ReadAll(resp.Body)
						t.Fatalf("unexpected response code %d (expected %d) hitting %s endpoint: %v",
							resp.StatusCode, tc.expectedStatus, tc.endpoint, string(bodyBytes))
					}
				}

			}

			c.Run(ctx, c.Node(initNode),
				fmt.Sprintf(`./kwbase init --insecure --port={pgport:%d}`, initNode))
			if err := g.Wait(); err != nil {
				t.Fatal(err)
			}

			// This will only succeed if 3 nodes joined the cluster.
			waitForFullReplication(t, dbs[0])

			execCLI := func(runNode int, extraArgs ...string) (string, error) {
				args := []string{"./kwbase"}
				args = append(args, extraArgs...)
				args = append(args, "--insecure")
				args = append(args, fmt.Sprintf("--port={pgport:%d}", runNode))
				buf, err := c.RunWithBuffer(ctx, c.l, c.Node(runNode), args...)
				t.l.Printf("%s\n", buf)
				return string(buf), err
			}

			{
				// Make sure that running init again returns the expected error message and
				// does not break the cluster. We have to use ExecCLI rather than OneShot in
				// order to actually get the output from the command.
				if output, err := execCLI(initNode, "init"); err == nil {
					t.Fatalf("expected error running init command on initialized cluster\n%s", output)
				} else if !strings.Contains(output, "cluster has already been initialized") {
					t.Fatalf("unexpected output when running init command on initialized cluster: %v\n%s",
						err, output)
				}
			}

			// Once initialized, the queries we started earlier will finish.
			deadline := time.After(10 * time.Second)
			for i := 0; i < len(dbs); i++ {
				select {
				case err := <-errCh:
					if err != nil {
						t.Fatalf("querying node %d: %s", i, err)
					}
				case <-deadline:
					t.Fatalf("timed out waiting for query %d", i)
				}
			}

			// New queries will work too.
			for i, db := range dbs {
				var val int
				if err := db.QueryRow("SELECT 1").Scan(&val); err != nil {
					t.Fatalf("querying node %d: %s", i, err)
				}
			}
		}()
	}
}
