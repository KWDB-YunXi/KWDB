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
	"time"

	"gitee.com/kwbasedb/kwbase/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// ChaosTimer configures a chaos schedule.
type ChaosTimer interface {
	Timing() (time.Duration, time.Duration)
}

// Periodic is a chaos timing using fixed durations.
type Periodic struct {
	Period, DownTime time.Duration
}

// Timing implements ChaosTimer.
func (p Periodic) Timing() (time.Duration, time.Duration) {
	return p.Period, p.DownTime
}

// Chaos stops and restarts nodes in a cluster.
type Chaos struct {
	// Timing is consulted before each chaos event. It provides the duration of
	// the downtime and the subsequent chaos-free duration.
	Timer ChaosTimer
	// Target is consulted before each chaos event to determine the node(s) which
	// should be killed.
	Target func() nodeListOption
	// Stopper is a channel that the chaos agent listens on. The agent will
	// terminate cleanly once it receives on the channel.
	Stopper <-chan time.Time
	// DrainAndQuit is used to determine if want to kill the node vs draining it
	// first and shutting down gracefully.
	DrainAndQuit bool
}

// Runner returns a closure that runs chaos against the given cluster without
// setting off the monitor. The process returns without an error after the chaos
// duration.
func (ch *Chaos) Runner(c *cluster, m *monitor) func(context.Context) error {
	return func(ctx context.Context) (err error) {
		l, err := c.l.ChildLogger("CHAOS")
		if err != nil {
			return err
		}
		defer func() {
			l.Printf("chaos stopping: %v", err)
		}()
		t := timeutil.Timer{}
		{
			p, _ := ch.Timer.Timing()
			t.Reset(p)
		}
		for {
			select {
			case <-ch.Stopper:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				t.Read = true
			}

			period, downTime := ch.Timer.Timing()

			target := ch.Target()
			m.ExpectDeath()

			if ch.DrainAndQuit {
				l.Printf("stopping and draining %v\n", target)
				if err := c.StopE(ctx, target, stopArgs("--sig=15")); err != nil {
					return errors.Wrapf(err, "could not stop node %s", target)
				}
			} else {
				l.Printf("killing %v\n", target)
				if err := c.StopE(ctx, target); err != nil {
					return errors.Wrapf(err, "could not stop node %s", target)
				}
			}

			select {
			case <-ch.Stopper:
				// NB: the roachtest harness checks that at the end of the test,
				// all nodes that have data also have a running process.
				l.Printf("restarting %v (chaos is done)\n", target)
				if err := c.StartE(ctx, target); err != nil {
					return errors.Wrapf(err, "could not restart node %s", target)
				}
				return nil
			case <-ctx.Done():
				// NB: the roachtest harness checks that at the end of the test,
				// all nodes that have data also have a running process.
				l.Printf("restarting %v (chaos is done)\n", target)
				// Use a one-off context to restart the node because ours is
				// already canceled.
				tCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := c.StartE(tCtx, target); err != nil {
					return errors.Wrapf(err, "could not restart node %s", target)
				}
				return ctx.Err()
			case <-time.After(downTime):
			}
			l.Printf("restarting %v after %s of downtime\n", target, downTime)
			t.Reset(period)
			if err := c.StartE(ctx, target); err != nil {
				return errors.Wrapf(err, "could not restart node %s", target)
			}
		}
	}
}
