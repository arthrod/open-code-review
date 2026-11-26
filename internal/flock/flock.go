// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package flock provides non-blocking, exclusive OS advisory locks on files.
//
// The kernel releases the lock when the holding process exits, however it
// exits, so a crashed holder never leaves a stale claim behind. That makes it
// the coordination primitive for several ocr processes on one machine; it is
// not reliable across machines sharing a network filesystem.
package flock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Lock is a held advisory lock. The zero value is not usable.
type Lock struct {
	f *os.File
}

// TryLock creates path (and its parent directories) when missing and tries to
// take an exclusive lock on it without blocking. It returns acquired=false
// with a nil error when another holder, in this process or another one,
// already owns the lock.
//
// The lock file is never removed: unlinking it while another process has it
// open would let two holders lock two different inodes under one name.
func TryLock(path string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock file: %w", err)
	}
	acquired, err := tryLock(f)
	if err != nil || !acquired {
		closeErr := f.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return nil, false, fmt.Errorf("acquire lock: %w", err)
		}
		return nil, false, nil
	}
	return &Lock{f: f}, true, nil
}

// File returns the locked file. On unix, a child process that inherits it
// (through exec.Cmd.ExtraFiles) shares the lock: it belongs to the open file
// description, so it stays held until every process holding that description
// has exited or closed it. Unlock is the exception: it releases the lock for
// every holder at once, so call it only after such a child has exited. On
// Windows a lock cannot be shared this way: it goes away with the process
// that took it.
func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.f
}

// Unlock releases the lock and closes the file. It is safe to call more than
// once and on a nil *Lock.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	unlockErr := unlock(f)
	closeErr := f.Close()
	if err := errors.Join(unlockErr, closeErr); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	return nil
}
