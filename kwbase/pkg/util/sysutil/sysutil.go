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

// Package sysutil is a cross-platform compatibility layer on top of package
// syscall. It exposes APIs for common operations that require package syscall
// and re-exports several symbols from package syscall that are known to be
// safe. Using package syscall directly from other packages is forbidden.
package sysutil

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/cockroachdb/errors"
)

// Signal is syscall.Signal.
type Signal = syscall.Signal

// Errno is syscall.Errno.
type Errno = syscall.Errno

// Exported syscall.Errno constants.
const (
	ECONNRESET   = syscall.ECONNRESET
	ECONNREFUSED = syscall.ECONNREFUSED
)

// FSInfo describes a filesystem. It is returned by StatFS.
type FSInfo struct {
	FreeBlocks  int64
	AvailBlocks int64
	TotalBlocks int64
	BlockSize   int64
}

// ExitStatus returns the exit status contained within an exec.ExitError.
func ExitStatus(err *exec.ExitError) int {
	// err.Sys() is of type syscall.WaitStatus on all supported platforms.
	// syscall.WaitStatus has a different type on Windows, but that type has an
	// ExitStatus method with an identical signature, so no need for conditional
	// compilation.
	return err.Sys().(syscall.WaitStatus).ExitStatus()
}

// DumpBacktraceSignal is used to dump all thread and goroutine stack backtrace
var DumpBacktraceSignal os.Signal = syscall.SIGUSR1

// NotifyThreadDumpSignal is used signal thread to dump stack backtrace
var NotifyThreadDumpSignal os.Signal = syscall.SIGUSR2

// AeDiedSignal is used to detect the ae died event
var AeDiedSignal os.Signal = syscall.SIGCHLD

const refreshSignal = syscall.SIGHUP

// RefreshSignaledChan returns a channel that will receive an os.Signal whenever
// the process receives a "refresh" signal (currently SIGHUP). A refresh signal
// indicates that the user wants to apply nondisruptive updates, like reloading
// certificates and flushing log files.
//
// On Windows, the returned channel will never receive any values, as Windows
// does not support signals. Consider exposing a refresh trigger through other
// means if Windows support is important.
func RefreshSignaledChan() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, refreshSignal)
	return ch
}

// IsErrConnectionReset returns true if an
// error is a "connection reset by peer" error.
func IsErrConnectionReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}

// IsErrConnectionRefused returns true if an error is a "connection refused" error.
func IsErrConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// SyscallMeStartTs starts up AE_ts.
func SyscallMeStartTs(pid int) error {
	err := syscall.Kill(pid, syscall.SIGUSR1)
	return err
}

// GetPid call syscall.Getpid()
func GetPid() int {
	return syscall.Getpid()
}

// GetDeviceNumber call syscall.Stat_t
func GetDeviceNumber(path string) (int64, error) {
	// Get file information
	fileInfo, err := os.Stat(path)
	if err != nil {
		return 0, err
	}

	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("failed to convert Sys() to *syscall.Stat_t")
	}

	return int64(stat.Dev), nil
}
