// Copyright 2017 The Cockroach Authors.
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

package contextutil

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func TestRunWithTimeout(t *testing.T) {
	ctx := context.TODO()
	err := RunWithTimeout(ctx, "foo", 1, func(ctx context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatal("RunWithTimeout shouldn't return a timeout error if nobody touched the context.")
	}

	err = RunWithTimeout(ctx, "foo", 1, func(ctx context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return ctx.Err()
	})
	expectedMsg := "operation \"foo\" timed out after 1ns"
	if err.Error() != expectedMsg {
		t.Fatalf("expected %s, actual %s", expectedMsg, err.Error())
	}
	netError, ok := err.(net.Error)
	if !ok {
		t.Fatal("RunWithTimeout should return a net.Error")
	}
	if !netError.Timeout() || !netError.Temporary() {
		t.Fatal("RunWithTimeout should return a timeout and temporary error")
	}
	if errors.Cause(err) != context.DeadlineExceeded {
		t.Fatalf("RunWithTimeout should return an error with a DeadlineExceeded cause")
	}

	err = RunWithTimeout(ctx, "foo", 1, func(ctx context.Context) error {
		time.Sleep(10 * time.Millisecond)
		return errors.Wrap(ctx.Err(), "custom error")
	})
	expExtended := expectedMsg + ": custom error: context deadline exceeded"
	if err.Error() != expExtended {
		t.Fatalf("expected %s, actual %s", expExtended, err.Error())
	}
	netError, ok = err.(net.Error)
	if !ok {
		t.Fatal("RunWithTimeout should return a net.Error")
	}
	if !netError.Timeout() || !netError.Temporary() {
		t.Fatal("RunWithTimeout should return a timeout and temporary error")
	}
	if errors.Cause(err) != context.DeadlineExceeded {
		t.Fatalf("RunWithTimeout should return an error with a DeadlineExceeded cause")
	}
}

// TestRunWithTimeoutWithoutDeadlineExceeded ensures that when a timeout on the
// context occurs but the underlying error does not have
// context.DeadlineExceeded as its Cause (perhaps due to serialization) the
// returned error is still a TimeoutError. In this case however the underlying
// cause should be the returned error and not context.DeadlineExceeded.
func TestRunWithTimeoutWithoutDeadlineExceeded(t *testing.T) {
	ctx := context.TODO()
	notContextDeadlineExceeded := errors.New(context.DeadlineExceeded.Error())
	err := RunWithTimeout(ctx, "foo", 1, func(ctx context.Context) error {
		<-ctx.Done()
		return notContextDeadlineExceeded
	})
	netError, ok := err.(net.Error)
	if !ok {
		t.Fatal("RunWithTimeout should return a net.Error")
	}
	if !netError.Timeout() || !netError.Temporary() {
		t.Fatal("RunWithTimeout should return a timeout and temporary error")
	}
	if errors.Cause(err) != notContextDeadlineExceeded {
		t.Fatalf("RunWithTimeout should return an error caused by the underlying " +
			"returned error")
	}
}

func TestCancelWithReason(t *testing.T) {
	ctx := context.Background()

	var cancel CancelWithReasonFunc
	ctx, cancel = WithCancelReason(ctx)

	e := errors.New("hodor")
	go func() {
		cancel(e)
	}()

	<-ctx.Done()

	expected := "context canceled"
	found := ctx.Err().Error()
	assert.Equal(t, expected, found)
	assert.Equal(t, e, GetCancelReason(ctx))
}
