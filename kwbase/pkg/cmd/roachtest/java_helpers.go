// Copyright 2019 The Cockroach Authors.
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
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

var issueRegexp = regexp.MustCompile(`See: https://[^\s]+issues?/(\d+)`)

type status int

const (
	statusPass status = iota
	statusFail
	statusSkip
)

// extractFailureFromJUnitXML parses an XML report to find all failed tests. The
// return values are:
// - slice of all test names.
// - slice of status for each test.
// - map from name of a failed test to a github issue that explains the failure,
//   if the error message contained a reference to an issue.
// - error if there was a problem parsing the XML.
func extractFailureFromJUnitXML(contents []byte) ([]string, []status, map[string]string, error) {
	type Failure struct {
		Message string `xml:"message,attr"`
	}
	type Error struct {
		Message string `xml:"message,attr"`
	}
	type TestCase struct {
		Name      string    `xml:"name,attr"`
		ClassName string    `xml:"classname,attr"`
		Failure   Failure   `xml:"failure,omitempty"`
		Error     Error     `xml:"error,omitempty"`
		Skipped   *struct{} `xml:"skipped,omitempty"`
	}
	type TestSuite struct {
		XMLName   xml.Name   `xml:"testsuite"`
		TestCases []TestCase `xml:"testcase"`
	}
	type TestSuites struct {
		XMLName    xml.Name    `xml:"testsuites"`
		TestSuites []TestSuite `xml:"testsuite"`
	}

	var testSuite TestSuite
	_ = testSuite.XMLName
	var testSuites TestSuites
	_ = testSuites.XMLName

	var tests []string
	var testStatuses []status
	var failedTestToIssue = make(map[string]string)
	processTestSuite := func(testSuite TestSuite) {
		for _, testCase := range testSuite.TestCases {
			testName := fmt.Sprintf("%s.%s", testCase.ClassName, testCase.Name)
			testPassed := len(testCase.Failure.Message) == 0 && len(testCase.Error.Message) == 0
			tests = append(tests, testName)
			if testCase.Skipped != nil {
				testStatuses = append(testStatuses, statusSkip)
			} else if testPassed {
				testStatuses = append(testStatuses, statusPass)
			} else {
				testStatuses = append(testStatuses, statusFail)
				message := testCase.Failure.Message
				if len(message) == 0 {
					message = testCase.Error.Message
				}

				issue := "unknown"
				match := issueRegexp.FindStringSubmatch(message)
				if match != nil {
					issue = match[1]
				}
				failedTestToIssue[testName] = issue
			}
		}
	}

	// First, we try to parse the XML with an assumption that there are multiple
	// test suites in contents.
	if err := xml.Unmarshal(contents, &testSuites); err == nil {
		// The parsing was successful, so we process each test suite.
		for _, testSuite := range testSuites.TestSuites {
			processTestSuite(testSuite)
		}
	} else {
		// The parsing wasn't successful, so now we try to parse the XML with an
		// assumption that there is a single test suite.
		if err := xml.Unmarshal(contents, &testSuite); err != nil {
			return nil, nil, nil, err
		}
		processTestSuite(testSuite)
	}

	return tests, testStatuses, failedTestToIssue, nil
}

// parseJUnitXML parses testOutputInJUnitXMLFormat and updates the receiver
// accordingly.
func (r *ormTestsResults) parseJUnitXML(
	t *test, expectedFailures, ignorelist blocklist, testOutputInJUnitXMLFormat []byte,
) {
	tests, statuses, issueHints, err := extractFailureFromJUnitXML(testOutputInJUnitXMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	for testName, issue := range issueHints {
		r.allIssueHints[testName] = issue
	}
	for i, test := range tests {
		// There is at least a single test that's run twice, so if we already
		// have a result, skip it.
		if _, alreadyTested := r.results[test]; alreadyTested {
			continue
		}
		r.allTests = append(r.allTests, test)
		ignoredIssue, expectedIgnored := ignorelist[test]
		issue, expectedFailure := expectedFailures[test]
		if len(issue) == 0 || issue == "unknown" {
			issue = issueHints[test]
		}
		status := statuses[i]
		switch {
		case expectedIgnored:
			r.results[test] = fmt.Sprintf("--- IGNORE: %s due to %s (expected)", test, ignoredIssue)
			r.ignoredCount++
		case status == statusSkip:
			r.results[test] = fmt.Sprintf("--- SKIP: %s", test)
			r.skipCount++
		case status == statusPass && !expectedFailure:
			r.results[test] = fmt.Sprintf("--- PASS: %s (expected)", test)
			r.passExpectedCount++
		case status == statusPass && expectedFailure:
			r.results[test] = fmt.Sprintf("--- PASS: %s - %s (unexpected)",
				test, maybeAddGithubLink(issue),
			)
			r.passUnexpectedCount++
		case status == statusFail && expectedFailure:
			r.results[test] = fmt.Sprintf("--- FAIL: %s - %s (expected)",
				test, maybeAddGithubLink(issue),
			)
			r.failExpectedCount++
			r.currentFailures = append(r.currentFailures, test)
		case status == statusFail && !expectedFailure:
			r.results[test] = fmt.Sprintf("--- FAIL: %s - %s (unexpected)",
				test, maybeAddGithubLink(issue))
			r.failUnexpectedCount++
			r.currentFailures = append(r.currentFailures, test)
		}
		r.runTests[test] = struct{}{}
	}
}

// parseAndSummarizeJavaORMTestsResults parses the test output of running a
// test suite for some Java ORM against kwbase and summarizes it. If an
// unexpected result is observed (for example, a test unexpectedly failed or
// passed), a new blocklist is populated.
func parseAndSummarizeJavaORMTestsResults(
	ctx context.Context,
	t *test,
	c *cluster,
	node nodeListOption,
	ormName string,
	testOutput []byte,
	blocklistName string,
	expectedFailures blocklist,
	ignorelist blocklist,
	version string,
	tag string,
) {
	results := newORMTestsResults()
	filesRaw := strings.Split(string(testOutput), "\n")

	// There is always at least one entry that's just space characters, remove
	// it.
	var files []string
	for _, f := range filesRaw {
		file := strings.TrimSpace(f)
		if len(file) > 0 {
			files = append(files, file)
		}
	}
	for i, file := range files {
		t.l.Printf("Parsing %d of %d: %s\n", i+1, len(files), file)
		fileOutput, err := repeatRunWithBuffer(
			ctx,
			c,
			t.l,
			node,
			fmt.Sprintf("fetching results file %s", file),
			fmt.Sprintf("cat %s", file),
		)
		if err != nil {
			t.Fatal(err)
		}

		results.parseJUnitXML(t, expectedFailures, ignorelist, fileOutput)
	}

	results.summarizeAll(
		t, ormName, blocklistName, expectedFailures, version, tag,
	)
}
