// Copyright 2019 The Cockroach Authors.
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

package timeutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimeZoneStringToLocation(t *testing.T) {
	aus, err := time.LoadLocation("Australia/Sydney")
	require.NoError(t, err)

	testCases := []struct {
		tz             string
		std            TimeZoneStringToLocationStandard
		loc            *time.Location
		expectedResult bool
	}{
		{"UTC", TimeZoneStringToLocationISO8601Standard, time.UTC, true},
		{"Australia/Sydney", TimeZoneStringToLocationISO8601Standard, aus, true},
		{"fixed offset:3600 (3600)", TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation(3600, "3600"), true},
		{`GMT-3:00`, TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation(3*60*60, "GMT-3:00"), true},
		{"+10", TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation(10*60*60, "+10"), true},
		{"-10", TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation(-10*60*60, "-10"), true},
		{" +10", TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation(10*60*60, " +10"), true},
		{" -10 ", TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation(-10*60*60, " -10 "), true},
		{"-10:30", TimeZoneStringToLocationISO8601Standard, FixedOffsetTimeZoneToLocation((10*60*60 + 30*60), "-10:30"), true},
		{"10:30", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(-(10*60*60 + 30*60), "10:30"), true},
		{"asdf", TimeZoneStringToLocationISO8601Standard, nil, false},

		{"UTC", TimeZoneStringToLocationPOSIXStandard, time.UTC, true},
		{"Australia/Sydney", TimeZoneStringToLocationPOSIXStandard, aus, true},
		{"fixed offset:3600 (3600)", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(3600, "3600"), true},
		{`GMT-3:00`, TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(3*60*60, "GMT-3:00"), true},
		{"+10", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(-10*60*60, "+10"), true},
		{"-10", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(10*60*60, "-10"), true},
		{" +10", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(-10*60*60, " +10"), true},
		{" -10", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(10*60*60, " -10"), true},
		{"-10:30", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation((10*60*60 + 30*60), "-10:30"), true},
		{"10:30", TimeZoneStringToLocationPOSIXStandard, FixedOffsetTimeZoneToLocation(-(10*60*60 + 30*60), "10:30"), true},
		{"asdf", TimeZoneStringToLocationPOSIXStandard, nil, false},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("%s_%d", tc.tz, tc.std), func(t *testing.T) {
			loc, err := TimeZoneStringToLocation(tc.tz, tc.std)
			if tc.expectedResult {
				assert.NoError(t, err)
				assert.Equal(t, tc.loc, loc)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestTimeZoneOffsetStringConversion(t *testing.T) {
	testCases := []struct {
		timezone   string
		std        TimeZoneStringToLocationStandard
		offsetSecs int64
		ok         bool
	}{
		{`10`, TimeZoneStringToLocationPOSIXStandard, -10 * 60 * 60, true},
		{`-10`, TimeZoneStringToLocationPOSIXStandard, 10 * 60 * 60, true},

		{`10`, TimeZoneStringToLocationISO8601Standard, 10 * 60 * 60, true},
		{`-10`, TimeZoneStringToLocationISO8601Standard, -10 * 60 * 60, true},
		{`10:15`, TimeZoneStringToLocationISO8601Standard, -(10*60*60 + 15*60), true},
		{`+10:15`, TimeZoneStringToLocationISO8601Standard, -(10*60*60 + 15*60), true},
		{`-10:15`, TimeZoneStringToLocationISO8601Standard, (10*60*60 + 15*60), true},
		{`GMT+00:00`, TimeZoneStringToLocationISO8601Standard, 0, true},
		{`UTC-1:00:00`, TimeZoneStringToLocationISO8601Standard, 3600, true},
		{`UTC-1:0:00`, TimeZoneStringToLocationISO8601Standard, 3600, true},
		{`UTC+15:59:0`, TimeZoneStringToLocationISO8601Standard, -57540, true},
		{` GMT +167:59:00  `, TimeZoneStringToLocationISO8601Standard, -604740, true},
		{`GMT-15:59:59`, TimeZoneStringToLocationISO8601Standard, 57599, true},
		{`GMT-06:59`, TimeZoneStringToLocationISO8601Standard, 25140, true},
		{`GMT+167:59:00`, TimeZoneStringToLocationISO8601Standard, -604740, true},
		{`GMT+ 167: 59:0`, TimeZoneStringToLocationISO8601Standard, -604740, true},
		{`GMT+5`, TimeZoneStringToLocationISO8601Standard, -18000, true},
		{`UTC+5:9`, TimeZoneStringToLocationISO8601Standard, -(5*60*60 + 9*60), true},
		{`UTC-5:9:1`, TimeZoneStringToLocationISO8601Standard, (5*60*60 + 9*60 + 1), true},
		{`GMT-15:59:5Z`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT-15:99:1`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT+6:`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT-6:00:`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT+168:00:00`, TimeZoneStringToLocationISO8601Standard, 0, false},
		{`GMT-170:00:59`, TimeZoneStringToLocationISO8601Standard, 0, false},
	}

	for i, testCase := range testCases {
		offset, ok := timeZoneOffsetStringConversion(testCase.timezone, testCase.std)
		if offset != testCase.offsetSecs || ok != testCase.ok {
			t.Errorf("%d: Expected offset: %d, success: %v for time %s, but got offset: %d, success: %v",
				i, testCase.offsetSecs, testCase.ok, testCase.timezone, offset, ok)
		}
	}
}
