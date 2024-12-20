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

package cluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/security"
)

const certsDir = ".localcluster.certs"

// keyLen is the length (in bits) of the generated CA and node certs.
const keyLen = 1024

// GenerateCerts generates CA and client certificates and private keys to be
// used with a cluster. It returns a function that will clean up the generated
// files.
func GenerateCerts(ctx context.Context) func() {
	maybePanic(os.RemoveAll(certsDir))

	maybePanic(security.CreateCAPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey),
		keyLen, 96*time.Hour, false, false))

	// Root user.
	maybePanic(security.CreateClientPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey),
		1024, 48*time.Hour, false, security.RootUser, true /* generate pk8 key */))

	// Test user.
	maybePanic(security.CreateClientPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey),
		1024, 48*time.Hour, false, "testuser", true /* generate pk8 key */))

	// Certs for starting a kwbase server. Key size is from cli/cert.go:defaultKeySize.
	maybePanic(security.CreateNodePair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey),
		2048, 48*time.Hour, false, []string{"localhost", "kwbase"}))

	// Store a copy of the client certificate and private key in a PKCS#12
	// bundle, which is the only format understood by Npgsql (.NET).
	{
		execCmd("openssl", "pkcs12", "-export", "-password", "pass:",
			"-in", filepath.Join(certsDir, "client.root.crt"),
			"-inkey", filepath.Join(certsDir, "client.root.key"),
			"-out", filepath.Join(certsDir, "client.root.pk12"))
	}

	return func() { _ = os.RemoveAll(certsDir) }
}

// GenerateCerts is only called in a file protected by a build tag. Suppress the
// unused linter's warning.
var _ = GenerateCerts

func execCmd(args ...string) {
	cmd := exec.Command(args[0], args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("error: %s: %s\nout: %s\n", args, err, out))
	}
}
