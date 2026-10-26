//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// configureListenChild stops the review child when the listener is
// canceled. Windows has no process groups, and a console program cannot act
// on an interrupt sent from outside, so the child is killed. ExtraFiles is
// unsupported, and inheriting the claim would not help: a LockFileEx lock is
// released when the process holding it dies.
func configureListenChild(cmd *exec.Cmd, _ *os.File) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}

// configureListenHook starts the on-review hook in a new process group, which
// a Ctrl+C in the console does not reach, so a hook that runs exactly once is
// not stopped by it. On timeout only the hook's own process is killed.
func configureListenHook(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
