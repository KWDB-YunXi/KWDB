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
	"bytes"
	"fmt"
	"io"
	"sort"
	"unicode"

	"gitee.com/kwbasedb/kwbase/pkg/sql/opt/optgen/lang"
)

// opsGen generates the enumeration of all operator types.
type opsGen struct {
	compiled *lang.CompiledExpr
	w        io.Writer
	sorted   lang.DefineSetExpr
}

func (g *opsGen) generate(compiled *lang.CompiledExpr, w io.Writer) {
	g.compiled = compiled
	g.w = w
	g.sorted = sortDefines(compiled.Defines)

	fmt.Fprintf(g.w, "package opt\n\n")

	g.genOperatorEnum()
	g.genOperatorNames()
	g.genOperatorSyntaxTags()
	g.genOperatorsByTag()
}

func (g *opsGen) genOperatorEnum() {
	fmt.Fprintf(g.w, "const (\n")
	fmt.Fprintf(g.w, "  UnknownOp Operator = iota\n")

	for _, define := range g.sorted {
		fmt.Fprintf(g.w, "\n")
		generateComments(g.w, define.Comments, string(define.Name), string(define.Name))
		fmt.Fprintf(g.w, "  %sOp\n", define.Name)
	}
	fmt.Fprintf(g.w, "\nNumOperators\n")
	fmt.Fprintf(g.w, ")\n\n")
}

func (g *opsGen) genOperatorNames() {
	var names bytes.Buffer
	var indexes bytes.Buffer

	fmt.Fprint(&names, "unknown")
	fmt.Fprint(&indexes, "0, ")

	for _, define := range g.sorted {
		fmt.Fprintf(&indexes, "%d, ", names.Len())
		fmt.Fprint(&names, dashCase(string(define.Name)))
	}

	fmt.Fprintf(g.w, "const opNames = \"%s\"\n\n", names.String())

	fmt.Fprintf(g.w, "var opNameIndexes = [...]uint32{%s%d}\n\n", indexes.String(), names.Len())
}

func (g *opsGen) genOperatorSyntaxTags() {
	var names bytes.Buffer
	var indexes bytes.Buffer

	fmt.Fprint(&names, "UNKNOWN")
	fmt.Fprint(&indexes, "0, ")

	for _, define := range g.sorted {
		fmt.Fprintf(&indexes, "%d, ", names.Len())
		fmt.Fprint(&names, syntaxCase(string(define.Name)))
	}

	fmt.Fprintf(g.w, "const opSyntaxTags = \"%s\"\n\n", names.String())

	fmt.Fprintf(g.w, "var opSyntaxTagIndexes = [...]uint32{%s%d}\n\n", indexes.String(), names.Len())
}

func (g *opsGen) genOperatorsByTag() {
	for _, tag := range g.compiled.DefineTags {
		fmt.Fprintf(g.w, "var %sOperators = [...]Operator{\n", tag)
		for _, define := range g.sorted.WithTag(tag) {
			fmt.Fprintf(g.w, "  %sOp,\n", define.Name)
		}
		fmt.Fprintf(g.w, "}\n\n")

		// Generate IsNormalTag function.
		fmt.Fprintf(g.w, "func Is%sOp(e Expr) bool {\n", tag)
		fmt.Fprintf(g.w, "  switch e.Op() {\n")
		fmt.Fprintf(g.w, "  case ")
		for i, define := range g.sorted.WithTag(tag) {
			if i != 0 {
				fmt.Fprintf(g.w, ", ")
			}
			if ((i + 1) % 5) == 0 {
				fmt.Fprintf(g.w, "\n    ")
			}
			fmt.Fprintf(g.w, "%sOp", define.Name)
		}
		fmt.Fprintf(g.w, ":\n")
		fmt.Fprintf(g.w, "    return true\n")
		fmt.Fprintf(g.w, "  }\n")
		fmt.Fprintf(g.w, "  return false\n")
		fmt.Fprintf(g.w, "}\n\n")
	}
}

// sortDefines returns a copy of the given expression definitions, sorted by
// name.
func sortDefines(defines lang.DefineSetExpr) lang.DefineSetExpr {
	sorted := make(lang.DefineSetExpr, len(defines))
	copy(sorted, defines)
	sort.Slice(sorted, func(i, j int) bool {
		return string(sorted[i].Name) < string(sorted[j].Name)
	})
	return sorted
}

// dashCase converts camel-case identifiers into "dash case", where uppercase
// letters in the middle of the identifier are replaced by a dash followed
// by the lowercase version the letter. Example:
//
//	InnerJoinApply => inner-join-apply
func dashCase(s string) string {
	var buf bytes.Buffer
	for i, ch := range s {
		if unicode.IsUpper(ch) {
			if i != 0 {
				buf.WriteByte('-')
			}

			buf.WriteRune(unicode.ToLower(ch))
		} else {
			buf.WriteRune(ch)
		}
	}
	return buf.String()
}

// syntaxCase converts camel-case identifiers into "syntax case", where
// uppercase letters in the middle of the identifier are interpreted as new
// words and separated by a space from the previous word. Example:
//
//	InnerJoinApply => INNER JOIN APPLY
func syntaxCase(s string) string {
	var buf bytes.Buffer
	for i, ch := range s {
		if unicode.IsUpper(ch) && i != 0 {
			buf.WriteByte(' ')
		}
		buf.WriteRune(unicode.ToUpper(ch))
	}
	return buf.String()
}
