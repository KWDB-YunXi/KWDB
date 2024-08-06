// Copyright 2020 The Cockroach Authors.
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

package timeutil

import "time"

// TimeSource is used to interact with clocks and timers. Generally exposed for
// testing.
type TimeSource interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	NewTimer() TimerI
	NewTicker(duration time.Duration) TickerI
}

// TimerI is an interface wrapping Timer.
type TimerI interface {

	// Reset will set the timer to notify on Ch() after duration.
	Reset(duration time.Duration)

	// Stop must only be called one time per timer.
	Stop() bool

	// Ch returns the channel which will be notified when the timer reaches its
	// time.
	Ch() <-chan time.Time

	// MarkRead should be called when a value is read from the Ch() channel.
	// If MarkRead is not called, the resetting the timer is less efficient.
	MarkRead()
}

// TickerI is an interface wrapping Ticker.
type TickerI interface {
	// Reset stops a ticker and resets its period to the specified duration. The
	// next tick will arrive after the new period elapses.
	Reset(duration time.Duration)

	// Stop turns off a ticker. After Stop, no more ticks will be sent. Stop does
	// not close the channel, to prevent a concurrent goroutine reading from the
	// channel from seeing an erroneous "tick".
	Stop()

	// Ch returns the channel on which the ticks are delivered.
	Ch() <-chan time.Time
}

// DefaultTimeSource is a TimeSource using the system clock.
type DefaultTimeSource struct{}

var _ TimeSource = DefaultTimeSource{}

// Now returns timeutil.Now().
func (DefaultTimeSource) Now() time.Time {
	return Now()
}

// Since implements TimeSource interface
func (DefaultTimeSource) Since(t time.Time) time.Duration {
	return Since(t)
}

// NewTimer returns a TimerI wrapping *Timer.
func (DefaultTimeSource) NewTimer() TimerI {
	return (*timer)(NewTimer())
}

// NewTicker creates a new ticker.
func (DefaultTimeSource) NewTicker(duration time.Duration) TickerI {
	return (*ticker)(time.NewTicker(duration))
}

type timer Timer

var _ TimerI = (*timer)(nil)

func (t *timer) Reset(duration time.Duration) {
	(*Timer)(t).Reset(duration)
}

func (t *timer) Stop() bool {
	return (*Timer)(t).Stop()
}

func (t *timer) Ch() <-chan time.Time {
	return t.C
}

func (t *timer) MarkRead() {
	t.Read = true
}

type ticker time.Ticker

var _ TickerI = (*ticker)(nil)

// Reset is part of the TickerI interface.
func (t *ticker) Reset(duration time.Duration) {
	(*time.Ticker)(t).Reset(duration)
}

// Stop is part of the TickerI interface.
func (t *ticker) Stop() {
	(*time.Ticker)(t).Stop()
}

// Ch is part of the TickerI interface.
func (t *ticker) Ch() <-chan time.Time {
	return (*time.Ticker)(t).C
}
