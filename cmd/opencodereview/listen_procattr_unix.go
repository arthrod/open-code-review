//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureListenChild puts the review child into its own process
// group, so a Ctrl+C in the terminal reaches the listener alone and never the
// running review, and makes the child stop on one interrupt instead of being
// killed.
func configureListenChild(cmd *exec.Cmd, claim *os.File) {
	// The claim belongs to the open file description, so inheritance keeps
	// it held if the listener dies during a review.
	if claim != nil {
		cmd.ExtraFiles = []*os.File{claim}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(os.Interrupt)
	}
}

// configureListenHook puts the on-review hook into its own process group, so
// a Ctrl+C in the terminal cannot stop a hook that runs exactly once, and
// kills the whole group when the hook runs out of time, so commands it started
// do not outlive it.
func configureListenHook(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group is gone: the hook exited on its own right as its
			// timeout fired, and os/exec must not count that as a failure.
			return os.ErrProcessDone
		}
		return err
	}
}
