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

package cli

import (
	"bytes"
	"os"
	"testing"
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/leaktest"
	"gitee.com/kwbasedb/kwbase/pkg/workload/histogram"
	"github.com/stretchr/testify/require"
)

func Example_text_formatter() {
	testFormatter(&textFormatter{})

	// output:
	// _elapsed___errors__ops/sec(inst)___ops/sec(cum)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)
	//     0.5s        0            2.0            2.0    503.3    503.3    503.3    503.3 read
	//     1.5s        0            0.7            1.3    335.5    335.5    335.5    335.5 read
	//
	// _elapsed___errors_____ops(total)___ops/sec(cum)__avg(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__total
	//     2.0s        0              2            1.0    411.0    335.5    503.3    503.3    503.3  read
	//
	// _elapsed___errors_____ops(total)___ops/sec(cum)__avg(ms)__p50(ms)__p95(ms)__p99(ms)_pMax(ms)__result
	//     4.0s        0              2            0.5    411.0    335.5    503.3    503.3    503.3  woo
}

func Example_json_formatter() {
	testFormatter(&jsonFormatter{w: os.Stdout})

	// output:
	// {"time":"0001-01-01T00:00:00.5Z","errs":0,"avgt":2.0,"avgl":2.0,"p50l":503.3,"p95l":503.3,"p99l":503.3,"maxl":503.3,"type":"read"}
	// {"time":"0001-01-01T00:00:01.5Z","errs":0,"avgt":0.7,"avgl":1.3,"p50l":335.5,"p95l":335.5,"p99l":335.5,"maxl":335.5,"type":"read"}
}

func testFormatter(formatter outputFormat) {
	reg := histogram.NewRegistry(time.Second)

	start := time.Time{}

	reg.GetHandle().Get("read").Record(time.Second / 2)
	reg.Tick(func(t histogram.Tick) {
		// Make output deterministic.
		t.Elapsed = time.Second / 2
		t.Now = start.Add(t.Elapsed)

		formatter.outputTick(t.Elapsed, t)
	})

	reg.GetHandle().Get("read").Record(time.Second / 3)
	reg.Tick(func(t histogram.Tick) {
		// ditto.
		t.Elapsed = 3 * time.Second / 2
		t.Now = start.Add(t.Elapsed)

		formatter.outputTick(t.Elapsed, t)
	})

	resultTick := histogram.Tick{Name: "woo"}
	reg.Tick(func(t histogram.Tick) {
		// ditto.
		t.Elapsed = 2 * time.Second
		t.Now = start.Add(t.Elapsed)

		formatter.outputTotal(t.Elapsed, t)
		resultTick.Now = t.Now
		resultTick.Cumulative = t.Cumulative
	})
	formatter.outputResult(4*time.Second, resultTick)
}

// TestJSONStructure ensures that the JSON output is parsable.
func TestJSONStructure(t *testing.T) {
	defer leaktest.AfterTest(t)()

	var buf bytes.Buffer
	f := jsonFormatter{w: &buf}
	reg := histogram.NewRegistry(time.Second)

	start := time.Time{}

	reg.GetHandle().Get("read").Record(time.Second / 2)
	reg.Tick(func(t histogram.Tick) {
		// Make output deterministic.
		t.Elapsed = time.Second
		t.Now = start.Add(t.Elapsed)

		f.outputTick(time.Second, t)
	})

	const expected = `
{
"time":"0001-01-01T00:00:01Z",
"avgl":1,
"avgt":1,
"errs":0,
"maxl":503.3,
"p50l":503.3,
"p95l":503.3,
"p99l":503.3,
"type":"read"
}`
	require.JSONEq(t, expected, buf.String())
}
