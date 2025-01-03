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
	"os"
	"sort"
	"strings"

	"github.com/nlopes/slack"
)

var slackToken string

func makeSlackClient() *slack.Client {
	if slackToken == "" {
		return nil
	}
	return slack.New(slackToken)
}

func findChannel(client *slack.Client, name string) (string, error) {
	if client != nil {
		channels, err := client.GetChannels(true)
		if err != nil {
			return "", err
		}
		for _, channel := range channels {
			if channel.Name == name {
				return channel.ID, nil
			}
		}
	}
	return "", fmt.Errorf("not found")
}

func sortTests(tests []*test) {
	sort.Slice(tests, func(i, j int) bool {
		return tests[i].Name() < tests[j].Name()
	})
}

func postSlackReport(pass, fail, skip map[*test]struct{}) {
	client := makeSlackClient()
	if client == nil {
		return
	}

	channel, _ := findChannel(client, "production")
	if channel == "" {
		return
	}

	params := slack.PostMessageParameters{
		Username: "roachtest",
	}

	branch := "<unknown branch>"
	if b := os.Getenv("TC_BUILD_BRANCH"); b != "" {
		branch = b
	}

	var prefix string
	switch {
	case cloud != "":
		prefix = strings.ToUpper(cloud)
	case local:
		prefix = "LOCAL"
	default:
		prefix = "GCE"
	}
	message := fmt.Sprintf("[%s] %s: %d passed, %d failed, %d skipped",
		prefix, branch, len(pass), len(fail), len(skip))

	{
		status := "good"
		if len(fail) > 0 {
			status = "warning"
		}
		var link string
		if buildID := os.Getenv("TC_BUILD_ID"); buildID != "" {
			link = fmt.Sprintf("https://teamcity.kwbasedb.com/viewLog.html?"+
				"buildId=%s&buildTypeId=Cockroach_Nightlies_WorkloadNightly",
				buildID)
		}
		params.Attachments = append(params.Attachments,
			slack.Attachment{
				Color:     status,
				Title:     message,
				TitleLink: link,
				Fallback:  message,
			})
	}

	data := []struct {
		tests map[*test]struct{}
		title string
		color string
	}{
		{pass, "Successes", "good"},
		{fail, "Failures", "danger"},
		{skip, "Skipped", "warning"},
	}
	for _, d := range data {
		tests := make([]*test, 0, len(d.tests))
		for t := range d.tests {
			tests = append(tests, t)
		}
		sortTests(tests)

		var buf bytes.Buffer
		for _, t := range tests {
			fmt.Fprintf(&buf, "%s\n", t.Name())
		}
		params.Attachments = append(params.Attachments,
			slack.Attachment{
				Color:    d.color,
				Title:    fmt.Sprintf("%s: %d", d.title, len(tests)),
				Text:     buf.String(),
				Fallback: message,
			})
	}

	if _, _, err := client.PostMessage(channel, "", params); err != nil {
		fmt.Println("unable to post slack report: ", err)
	}
}
