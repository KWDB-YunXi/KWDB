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

package buildutil

import (
	"go/build"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/pkg/errors"
)

func init() {
	// NB: This is a hack to disable the use of go modules with
	// build.Import. This will probably break with a future version of Go, but
	// suffices until we move to using go modules. See go/build.Context.importGo
	// (https://github.com/golang/go/blob/master/src/go/build/build.go#L999) and
	// the logic to skip using `go list` if the env far "GO111MODULE" is set to
	// "off".
	_ = os.Setenv("GO111MODULE", "off")
}

func short(in string) string {
	return strings.Replace(in, "gitee.com/kwbasedb/kwbase/pkg/", "./pkg/", -1)
}

// VerifyNoImports verifies that a package doesn't depend (directly or
// indirectly) on forbidden packages. The forbidden packages are specified as
// either exact matches or prefix matches.
// A match is not reported if the package that includes the forbidden package
// is listed in the whitelist.
// If GOPATH isn't set, it is an indication that the source is not available and
// the test is skipped.
func VerifyNoImports(
	t testing.TB,
	pkgPath string,
	cgo bool,
	forbiddenPkgs, forbiddenPrefixes []string,
	whitelist ...string,
) {

	// Skip test if source is not available.
	if build.Default.GOPATH == "" {
		t.Skip("GOPATH isn't set")
	}

	buildContext := build.Default
	buildContext.CgoEnabled = cgo

	checked := make(map[string]struct{})

	var check func(string) error
	check = func(path string) error {
		pkg, err := buildContext.Import(path, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pkg.Imports {
			for _, forbidden := range forbiddenPkgs {
				if forbidden == imp {
					whitelisted := false
					for _, w := range whitelist {
						if path == w {
							whitelisted = true
							break
						}
					}
					if !whitelisted {
						return errors.Errorf("%s imports %s, which is forbidden", short(path), short(imp))
					}
				}
				if forbidden == "c-deps" && imp == "C" && strings.HasPrefix(path, "gitee.com/kwbasedb/kwbase/pkg") {
					for _, name := range pkg.CgoFiles {
						if strings.Contains(name, "zcgo_flags") {
							return errors.Errorf("%s imports %s (%s), which is forbidden", short(path), short(imp), name)
						}
					}
				}
			}
			for _, prefix := range forbiddenPrefixes {
				if strings.HasPrefix(imp, prefix) {
					return errors.Errorf("%s imports %s which has prefix %s, which is forbidden", short(path), short(imp), prefix)
				}
			}

			// https://github.com/golang/tools/blob/master/refactor/importgraph/graph.go#L159
			if imp == "C" {
				continue // "C" is fake
			}

			importPkg, err := buildContext.Import(imp, pkg.Dir, build.FindOnly)
			if err != nil {
				// go/build does not know that gccgo's standard packages don't have
				// source, and will report an error saying that it can not find them.
				//
				// See https://github.com/golang/go/issues/16701
				// and https://github.com/golang/go/issues/23607.
				if runtime.Compiler == "gccgo" {
					continue
				}
				t.Fatal(err)
			}
			imp = importPkg.ImportPath
			if _, ok := checked[imp]; ok {
				continue
			}
			if err := check(imp); err != nil {
				return errors.Wrapf(err, "%s depends on", short(path))
			}
			checked[pkg.ImportPath] = struct{}{}
		}
		return nil
	}
	if err := check(pkgPath); err != nil {
		t.Fatal(err)
	}
}

// VerifyTransitiveWhitelist checks that the entire set of transitive
// dependencies of the given package is in a whitelist. Vendored and stdlib
// packages are always allowed.
func VerifyTransitiveWhitelist(t testing.TB, pkg string, allowedPkgs []string) {
	// Skip test if source is not available.
	if build.Default.GOPATH == "" {
		t.Skip("GOPATH isn't set")
	}

	checked := make(map[string]struct{})
	allowed := make(map[string]struct{}, len(allowedPkgs))
	for _, allowedPkg := range allowedPkgs {
		allowed[allowedPkg] = struct{}{}
	}

	var check func(string)
	check = func(path string) {
		if _, ok := checked[path]; ok {
			return
		}
		checked[path] = struct{}{}
		if strings.HasPrefix(path, "gitee.com/kwbasedb/kwbase/vendor") {
			return
		}

		pkg, err := build.Default.Import(path, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pkg.Imports {
			if !strings.HasPrefix(imp, "gitee.com/kwbasedb/kwbase/") {
				continue
			}
			if _, ok := allowed[imp]; !ok {
				t.Errorf("%s imports %s, which is forbidden", short(path), short(imp))
				// If we can't have this package, don't bother recursively checking the
				// deps, they'll just be noise.
				continue
			}

			// https://github.com/golang/tools/blob/master/refactor/importgraph/graph.go#L159
			if imp == "C" {
				continue // "C" is fake
			}

			importPkg, err := build.Default.Import(imp, pkg.Dir, build.FindOnly)
			if err != nil {
				t.Fatal(err)
			}
			check(importPkg.ImportPath)
		}
	}
	check(pkg)
}
