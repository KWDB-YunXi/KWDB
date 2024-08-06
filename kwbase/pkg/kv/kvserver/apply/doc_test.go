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

package apply_test

import (
	"context"
	"fmt"

	"gitee.com/kwbasedb/kwbase/pkg/kv/kvserver/apply"
)

func ExampleTask() {
	defer setLogging(true)()
	ctx := context.Background()
	ents := makeEntries(7)

	sm := getTestStateMachine()
	dec := newTestDecoder()
	dec.nonTrivial[5] = true
	dec.nonLocal[2] = true
	dec.nonLocal[6] = true
	dec.shouldReject[3] = true
	dec.shouldReject[6] = true
	fmt.Print(`
Setting up a batch of seven log entries:
 - index 2 and 6 are non-local
 - index 3 and 6 will be rejected
 - index 5 is not trivial
`)

	t := apply.MakeTask(sm, dec)
	defer t.Close()

	fmt.Println("\nDecode (note that index 2 and 6 are not local):")
	if err := t.Decode(ctx, ents); err != nil {
		panic(err)
	}

	fmt.Println("\nAckCommittedEntriesBeforeApplication:")
	if err := t.AckCommittedEntriesBeforeApplication(ctx, 10 /* maxIndex */); err != nil {
		panic(err)
	}
	fmt.Print(`
Above, only index 1 and 4 get acked early. The command at 5 is
non-trivial, so the first batch contains only 1, 2, 3, and 4. An entry
must be in the first batch to qualify for acking early. 2 is not local
(so there's nobody to ack), and 3 is rejected. We can't ack rejected
commands early because the state machine is free to handle them any way
it likes.
`)

	fmt.Println("\nApplyCommittedEntries:")
	if err := t.ApplyCommittedEntries(ctx); err != nil {
		panic(err)
	}
	// Output:
	//
	// Setting up a batch of seven log entries:
	//  - index 2 and 6 are non-local
	//  - index 3 and 6 will be rejected
	//  - index 5 is not trivial
	//
	// Decode (note that index 2 and 6 are not local):
	//  decoding command 1; local=true
	//  decoding command 2; local=false
	//  decoding command 3; local=true
	//  decoding command 4; local=true
	//  decoding command 5; local=true
	//  decoding command 6; local=false
	//  decoding command 7; local=true
	//
	// AckCommittedEntriesBeforeApplication:
	//  acknowledging command 1 before application
	//  acknowledging command 4 before application
	//
	// Above, only index 1 and 4 get acked early. The command at 5 is
	// non-trivial, so the first batch contains only 1, 2, 3, and 4. An entry
	// must be in the first batch to qualify for acking early. 2 is not local
	// (so there's nobody to ack), and 3 is rejected. We can't ack rejected
	// commands early because the state machine is free to handle them any way
	// it likes.
	//
	// ApplyCommittedEntries:
	//  applying batch with commands=[1 2 3 4]
	//  applying side-effects of command 1
	//  applying side-effects of command 2
	//  applying side-effects of command 3
	//  applying side-effects of command 4
	//  finishing command 1; rejected=false
	//  acknowledging and finishing command 2; rejected=false
	//  acknowledging and finishing command 3; rejected=true
	//  finishing command 4; rejected=false
	//  applying batch with commands=[5]
	//  applying side-effects of command 5
	//  acknowledging and finishing command 5; rejected=false
	//  applying batch with commands=[6 7]
	//  applying side-effects of command 6
	//  applying side-effects of command 7
	//  acknowledging and finishing command 6; rejected=true
	//  acknowledging and finishing command 7; rejected=false
}
