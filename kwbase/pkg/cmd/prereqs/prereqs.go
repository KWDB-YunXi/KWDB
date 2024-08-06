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

// prereqs generates Make prerequisites for Go binaries. It works much like the
// traditional makedepend tool for C.
//
// Given the path to a Go package, prereqs will traverse the package's
// dependency graph to determine what files impact its compilation. It then
// outputs a Makefile that expresses these dependencies. For example:
//
//     $ prereqs ./pkg/cmd/foo
//     # Code generated by prereqs. DO NOT EDIT!
//
//     bin/foo: ./pkg/cmd/foo/foo.go ./some/dep.go ./some/other_dep.go
//
//     ./pkg/cmd/foo/foo.go:
//     ./some/dep.go:
//     ./some/other_dep.go:
//
// The intended usage is automatic dependency generation from another Makefile:
//
//     bin/target:
//      prereqs ./pkg/cmd/target > bin/target.d
//      go build -o $@ ./pkg/cmd/target
//
//     include bin/target.d
//
// Notice that depended-upon files are mentioned not only in the prerequisites
// list but also as a rule with no prerequisite or recipe. This prevents Make
// from complaining if the prerequisite is deleted. See [0] for details on the
// approach.
//
// [0]: http://make.mad-scientist.net/papers/advanced-auto-dependency-generation/
package main

import (
	"flag"
	"fmt"
	"go/build"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "gitee.com/kwbasedb/kwbase/pkg/testutils/buildutil"
)

var buildCtx = func() build.Context {
	bc := build.Default
	bc.CgoEnabled = true
	return bc
}()

func collectFiles(path string, includeTest bool) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	// Symlinks in cwd confuse the relative path computation in collectFilesImpl.
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}

	srcDir := cwd // top-level relative imports are relative to cwd
	return collectFilesImpl(cwd, path, srcDir, includeTest, map[string]struct{}{})
}

func collectFilesImpl(
	cwd, path, srcDir string, includeTest bool, seen map[string]struct{},
) ([]string, error) {
	// Skip packages we've seen before.
	if _, ok := seen[path]; ok {
		return nil, nil
	}
	seen[path] = struct{}{}

	// Skip standard library packages.
	if isStdlibPackage(path) {
		return nil, nil
	}

	// Import the package.
	pkg, err := buildCtx.Import(path, srcDir, 0)
	if err != nil {
		return nil, err
	}

	sourceFileSets := [][]string{
		// Include all Go and Cgo source files.
		pkg.GoFiles, pkg.CgoFiles, pkg.IgnoredGoFiles, pkg.InvalidGoFiles,
		pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles,
		pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles,

		// Include the package directory itself so that the target is considered
		// out-of-date if a new file is added to the directory.
		{"."},
	}
	importSets := [][]string{pkg.Imports}
	if includeTest {
		sourceFileSets = append(sourceFileSets, pkg.TestGoFiles, pkg.XTestGoFiles)
		importSets = append(importSets, pkg.TestImports, pkg.XTestImports)
	}

	// Collect files recursively from the package and its dependencies.
	var out []string
	for _, sourceFiles := range sourceFileSets {
		for _, sourceFile := range sourceFiles {
			if isFileAlwaysIgnored(sourceFile) || strings.HasPrefix(sourceFile, "zcgo_flags") {
				continue
			}
			f, err := filepath.Rel(cwd, filepath.Join(pkg.Dir, sourceFile))
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
	}
	for _, imports := range importSets {
		for _, imp := range imports {
			// Only the root package's tests are included in test binaries, so
			// unconditionally disable includeTest for imported packages.
			files, err := collectFilesImpl(cwd, imp, pkg.Dir, false /* includeTest */, seen)
			if err != nil {
				return nil, err
			}
			out = append(out, files...)
		}
	}

	return out, nil
}

func isStdlibPackage(path string) bool {
	// Standard library packages never contain a dot; second- and third-party
	// packages nearly always do. Consider "gitee.com/kwbasedb/kwbase",
	// where the domain provides the dot, or "./pkg/sql", where the relative
	// import provides the dot.
	//
	// This logic is not foolproof, but it's the same logic used by goimports.
	return !strings.Contains(path, ".")
}

func isFileAlwaysIgnored(name string) bool {
	// build.Package.IgnoredGoFiles does not distinguish between Go files that are
	// always ignored and Go files that are temporarily ignored due to build tags.
	// Duplicate some logic from go/build [0] here so we can tell the difference.
	//
	// [0]: https://github.com/golang/go/blob/9ecf899b2/src/go/build/build.go#L1065-L1068
	return (strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) && filepath.Ext(name) == ".go"
}

// See: https://www.cmcrossroads.com/article/gnu-make-escaping-walk-wild-side
var filenameEscaper = strings.NewReplacer(
	`[`, `\[`,
	`]`, `\]`,
	`*`, `\*`,
	`?`, `\?`,
	`~`, `\~`,
	`$`, `$$`,
	`#`, `\#`,
)

func run(w io.Writer, path string, includeTest bool, binName string) error {
	files, err := collectFiles(path, includeTest)
	if err != nil {
		return err
	}

	for i := range files {
		files[i] = filenameEscaper.Replace(files[i])
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if binName == "" {
		binName = filepath.Base(absPath)
	}

	fmt.Fprintln(w, "# Code generated by prereqs. DO NOT EDIT!")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "bin/%s: %s\n", binName, strings.Join(files, " "))
	fmt.Fprintln(w)
	for _, f := range files {
		fmt.Fprintf(w, "%s:\n", f)
	}

	return nil
}

func main() {
	includeTest := flag.Bool("test", false, "include test dependencies")
	binName := flag.String("bin-name", "", "custom binary name (defaults to bin/<package name>)")
	flag.Usage = func() { fmt.Fprintf(os.Stderr, "usage: %s [-test] <package>\n", os.Args[0]) }
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	if err := run(os.Stdout, flag.Arg(0), *includeTest, *binName); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", os.Args[0], err)
		os.Exit(1)
	}
}
