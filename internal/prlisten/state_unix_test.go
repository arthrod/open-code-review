//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prlisten

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPollCreatesPrivateDirectories(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	// Keep review outputs under the state root so the walk checks their parents too.
	cfg.Dir = cfg.StateDir
	var reviewedJob Job
	cfg.Review = func(ctx context.Context, job Job) error {
		reviewedJob = job
		if err := os.WriteFile(job.LogPath, []byte("review complete\n"), 0o600); err != nil {
			return err
		}
		return listenTestWriteResult(ctx, job)
	}
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Reviewed: 1})
	listenTestRead(t, reviewedJob.ResultPath)
	listenTestRead(t, reviewedJob.LogPath)
	listenTestRead(t, listenTestStatePath(cfg, "done", fake.repos["o/r"].pulls[0]))
	if err := filepath.WalkDir(cfg.StateDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == cfg.StateDir || !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(cfg.StateDir, path)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("created directory %s has mode %#o, want no group or other permission bits", name, info.Mode().Perm())
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
}

func TestWriteFileAtomicCreatesPrivateParents(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "p", "q", "file")
	if err := writeFileAtomic(path, []byte("review result")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	for _, name := range []string{"p", "p/q"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("Stat %s: %v", name, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", name)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("created directory %s has mode %#o, want no group or other permission bits", name, info.Mode().Perm())
		}
	}
}

func listenTestReadOnlyDirectory(t *testing.T, dir string) {
	t.Helper()
	listenTestRequireUnixPermissions(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, info.Mode().Perm()); err != nil {
			t.Errorf("restore directory permissions: %v", err)
		}
	})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFileAtomicPreservesExistingFileOnPermissionError(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome")
	listenTestWrite(t, path, "keep")
	listenTestReadOnlyDirectory(t, dir)
	err := writeFileAtomic(path, []byte("replacement"))
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "create a temporary file in "+dir) {
		t.Fatalf("writeFileAtomic = %v, want wrapped temporary-file permission error", err)
	}
	if got := string(listenTestRead(t, path)); got != "keep" {
		t.Errorf("failed write changed outcome: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "outcome" {
		t.Errorf("failed write left temporary files: %v", entries)
	}
}

func TestWriteDonePropagatesPermissionError(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	store := newStateStore(t.TempDir())
	components := []string{"github.com", "o", "r"}
	path := store.donePath(components, "key")
	listenTestReadOnlyDirectory(t, filepath.Dir(path))
	err := store.writeDone(components, "key", doneRecord{Pull: 7, HeadSHA: listenTestSHA, Result: "result.json"})
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), filepath.Dir(path)) {
		t.Fatalf("writeDone = %v, want permission error naming done directory", err)
	}
	listenTestAbsent(t, path)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed writeDone left files: %v", entries)
	}
}

func TestPollLogsAttemptWriteFailureAndReleasesClaim(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	path := listenTestStatePath(cfg, "attempts", pull)
	listenTestWrite(t, path, "1")
	listenTestReadOnlyDirectory(t, filepath.Dir(path))
	var logs bytes.Buffer
	cfg.Log = &logs
	calls, hooks := 0, 0
	cfg.Review = func(context.Context, Job) error { calls++; return errors.New("review failed") }
	cfg.OnReviewed = func(context.Context, Job) error { hooks++; return nil }
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1})
	if calls != 1 || hooks != 0 || !strings.Contains(logs.String(), "recording the attempt of "+listenTestKey(cfg, pull)+" failed") {
		t.Errorf("attempt write failure: reviews=%d, hooks=%d, log=%q", calls, hooks, logs.String())
	}
	listenTestAttempts(t, cfg, pull, 1)
	listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
	listenTestLockAvailable(t, listenTestStatePath(cfg, "locks", pull)+".lock")
}

func TestPollSkipsWhenDoneDirectoryCannotBeInspected(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	dir := filepath.Dir(listenTestStatePath(cfg, "done", pull))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cfg.Log = &logs
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1})
	if calls != 0 || !strings.Contains(logs.String(), "reading the outcome of o/r#7 failed") {
		t.Errorf("outcome inspection failure: reviews=%d, log=%q", calls, logs.String())
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "attempts", pull))
	listenTestLockAvailable(t, listenTestStatePath(cfg, "locks", pull)+".lock")
}
