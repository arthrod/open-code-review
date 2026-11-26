// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package flock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func acquireTestLock(t *testing.T, path string) *Lock {
	t.Helper()
	lock, acquired, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock(%q): %v", path, err)
	}
	if !acquired || lock == nil {
		t.Fatalf("TryLock(%q) = (%v, %v, nil), want a held lock", path, lock, acquired)
	}
	t.Cleanup(func() {
		if err := lock.Unlock(); err != nil {
			t.Errorf("cleanup Unlock: %v", err)
		}
	})
	return lock
}

func TestTryLockCreatesMissingParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "missing", "claim.lock")
	acquireTestLock(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat lock file: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("lock mode = %v, want regular file", info.Mode())
	}
}

func TestTryLockContendsWithinProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.lock")
	acquireTestLock(t, path)
	second, acquired, err := TryLock(path)
	if second != nil {
		t.Cleanup(func() { _ = second.Unlock() })
	}
	if err != nil || acquired {
		t.Fatalf("second TryLock = (%v, %v, %v), want acquired=false and nil error", second, acquired, err)
	}
}

func TestTryLockSucceedsAfterUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.lock")
	first := acquireTestLock(t, path)
	if err := first.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	acquireTestLock(t, path)
}

func TestUnlockIsIdempotent(t *testing.T) {
	lock := acquireTestLock(t, filepath.Join(t.TempDir(), "claim.lock"))
	if err := lock.Unlock(); err != nil {
		t.Fatalf("first Unlock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
}

func TestUnlockNilAndZeroLock(t *testing.T) {
	for name, lock := range map[string]*Lock{"nil": nil, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			if err := lock.Unlock(); err != nil {
				t.Errorf("Unlock: %v", err)
			}
		})
	}
}

func TestTryLockRejectsRegularFileParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parent, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, acquired, err := TryLock(filepath.Join(parent, "claim.lock"))
	if lock != nil {
		t.Cleanup(func() { _ = lock.Unlock() })
	}
	if err == nil || acquired || lock != nil {
		t.Errorf("TryLock = (%v, %v, %v), want nil lock, error and acquired=false", lock, acquired, err)
	}
	data, err := os.ReadFile(parent)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sentinel" {
		t.Errorf("parent contents = %q, want sentinel", data)
	}
}

func TestTryLockRejectsDirectoryAsLockFile(t *testing.T) {
	path := t.TempDir()
	lock, acquired, err := TryLock(path)
	if lock != nil {
		t.Cleanup(func() { _ = lock.Unlock() })
	}
	if err == nil || acquired || lock != nil {
		t.Fatalf("TryLock = (%v, %v, %v), want nil lock, false and an error", lock, acquired, err)
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != path {
		t.Errorf("error = %v, want wrapped path error for %s", err, path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("lock directory was modified: info=%v, err=%v", info, err)
	}
}

func TestUnlockReportsClosedFileAndClearsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.lock")
	lock := acquireTestLock(t, path)
	if err := lock.File().Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Unlock = %v, want wrapped os.ErrClosed", err)
	}
	if lock.File() != nil {
		t.Error("Unlock must clear the file even on error")
	}
	if err := lock.Unlock(); err != nil {
		t.Errorf("repeated Unlock after error: %v", err)
	}
	acquireTestLock(t, path)
}

func TestUnlockKeepsLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.lock")
	lock := acquireTestLock(t, path)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("lock file must remain after Unlock: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("Unlock replaced the lock file")
	}
}

func TestFileReturnsLockedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.lock")
	lock := acquireTestLock(t, path)
	file := lock.File()
	if file == nil {
		t.Fatal("File returned nil for a held lock")
	}
	if file != lock.f {
		t.Fatal("File did not return the held file descriptor")
	}
	if lock.File() != file {
		t.Error("File must return the same open file on repeated calls")
	}
	got, err := file.Stat()
	if err != nil {
		t.Fatalf("File.Stat: %v", err)
	}
	want, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Error("File does not refer to the locked path")
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("File.Stat after Unlock = %v, want os.ErrClosed", err)
	}
}

func TestFileNilAndZeroLock(t *testing.T) {
	for name, lock := range map[string]*Lock{"nil": nil, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			if file := lock.File(); file != nil {
				t.Errorf("File = %v, want nil", file)
			}
		})
	}
}
