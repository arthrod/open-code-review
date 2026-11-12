//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/prsnapshot"
	"golang.org/x/sys/unix"
)

func init() {
	if mode := os.Getenv("OCR_CLAIM_FD_TEST"); mode != "" {
		fd, err := strconv.Atoi(os.Getenv("OCR_CLAIM_FD"))
		if err != nil {
			panic(err)
		}
		if mode == "review" {
			cmd := newReviewPRCmd(&reviewPROptions{})
			cmd.SetArgs([]string{"--claim-fd", strconv.Itoa(fd)})
			// Missing review input ends RunE after it protects the inherited claim.
			if err := cmd.Execute(); err == nil {
				panic("expected missing review input")
			}
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err != nil || flags&unix.FD_CLOEXEC == 0 {
				panic(fmt.Sprintf("claim is closed or inheritable: flags=%d err=%v", flags, err))
			}
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), "OCR_CLAIM_FD_TEST=grandchild")
			if out, err := child.CombinedOutput(); err != nil {
				panic(fmt.Sprintf("grandchild: %v: %s", err, out))
			}
		} else {
			inherited, err := os.NewFile(uintptr(fd), "claim").Stat()
			original, statErr := os.Stat(os.Getenv("OCR_CLAIM_PATH"))
			if statErr != nil {
				panic(statErr)
			}
			if err == nil && os.SameFile(inherited, original) {
				panic("grandchild inherited the claim")
			}
		}
		os.Exit(0)
	}

	if os.Getenv("OCR_LSN_FAKE_PGID") == "1" {
		fmt.Fprintln(os.Stdout, syscall.Getpgrp())
		os.Exit(0)
	}
}

func TestLsnHookRunsInItsOwnProcessGroup(t *testing.T) {
	tmp := t.TempDir()
	lsnInstallSeams(t, lsnFakeChild(t, tmp, lsnChild{}), "token", []prsnapshot.Pull{lsnPull()})
	out := filepath.Join(tmp, "hook-pgid")
	hook := "OCR_LSN_FAKE_PGID=1 GORACE=atexit_sleep_ms=0 " + lsnShellQuote(os.Args[0]) + " > " + lsnShellQuote(out)
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"), "--state-dir", filepath.Join(tmp, "state"),
		"--on-review", hook, "--once"))
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the hook did not run: %v", err)
	}
	pgid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pgid == syscall.Getpgrp() {
		t.Errorf("hook process group = %q, the listener's is %d; a Ctrl+C would reach the hook", data, syscall.Getpgrp())
	}
}

func TestLsnHookCancelAfterReapReportsProcessDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 0")
	configureListenHook(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("the hook did not succeed: %v", err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel after the reaped hook = %v, want os.ErrProcessDone", err)
	}
}

func TestLsnHookTimeoutStopsWhatTheHookStarted(t *testing.T) {
	tmp := t.TempDir()
	lsnInstallSeams(t, lsnFakeChild(t, tmp, lsnChild{}), "token", []prsnapshot.Pull{lsnPull()})
	pidFile := filepath.Join(tmp, "background-pid")
	hook := "sleep 30 & echo $! > " + lsnShellQuote(pidFile) + "; wait"
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"), "--state-dir", filepath.Join(tmp, "state"),
		"--review-timeout", "500ms", "--on-review", hook, "--once"))
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the hook did not start its command: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d started by the hook outlived the hook's timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("signal 0 to %d = %v, want ESRCH", pid, err)
	}
}

func TestLsnClaimDoesNotReachReviewGrandchildren(t *testing.T) {
	claim, err := os.CreateTemp(t.TempDir(), "claim")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	cmd := exec.CommandContext(context.Background(), os.Args[0])
	configureListenChild(cmd, claim)
	if len(cmd.Args) != 3 || cmd.Args[1] != "--claim-fd" {
		t.Fatalf("missing claim descriptor argument: %v", cmd.Args)
	}
	cmd.Env = append(os.Environ(), "OCR_CLAIM_FD_TEST=review",
		"OCR_CLAIM_FD="+cmd.Args[2], "OCR_CLAIM_PATH="+claim.Name())
	// The Go test binary does not parse review-pr flags.
	cmd.Args = cmd.Args[:1]
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("review process: %v: %s", err, out)
	}
}

func TestProtectListenClaimRejectsInvalidDescriptors(t *testing.T) {
	for _, fd := range []int{-1, 1, 2} {
		if err := protectListenClaim(fd); err == nil {
			t.Errorf("descriptor %d accepted", fd)
		}
	}
	if err := protectListenClaim(0); err != nil {
		t.Fatal(err)
	}
}
