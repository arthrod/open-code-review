// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prlisten

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicRefusesRegularFileParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	listenTestWrite(t, parent, "keep")
	err := writeFileAtomic(filepath.Join(parent, "outcome"), []byte("new"))
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || !strings.Contains(err.Error(), "create "+parent) {
		t.Fatalf("writeFileAtomic = %v, want wrapped directory creation error", err)
	}
	if got := string(listenTestRead(t, parent)); got != "keep" {
		t.Errorf("parent changed: %q", got)
	}
}

func TestWriteFileAtomicCleansTemporaryFileAfterRenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome")
	listenTestWrite(t, filepath.Join(path, "precious"), "keep")
	err := writeFileAtomic(path, []byte("new"))
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) || linkErr.New != path || !strings.Contains(err.Error(), "rename ") {
		t.Fatalf("writeFileAtomic = %v, want wrapped rename error for %s", err, path)
	}
	if got := string(listenTestRead(t, filepath.Join(path, "precious"))); got != "keep" {
		t.Errorf("destination contents changed: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "outcome" || !entries[0].IsDir() {
		t.Errorf("failed write left temporary files: %v", entries)
	}
}

func TestPollSkipsWhenClaimParentIsRegularFile(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	parent := filepath.Dir(listenTestStatePath(cfg, "locks", pull))
	listenTestWrite(t, parent, "keep")
	var logs bytes.Buffer
	cfg.Log = &logs
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1})
	if calls != 0 || !strings.Contains(logs.String(), "claiming o/r#7 failed") {
		t.Errorf("claim failure: reviews=%d, log=%q", calls, logs.String())
	}
	if got := string(listenTestRead(t, parent)); got != "keep" {
		t.Errorf("claim parent changed: %q", got)
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
	listenTestAbsent(t, listenTestStatePath(cfg, "attempts", pull))
}

func TestPollRecordsAttemptWhenOutputDirectoryCannotBeCreated(t *testing.T) {
	for _, kind := range []string{"reviews", "logs", "work"} {
		t.Run(kind, func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			parent := filepath.Join(cfg.Dir, "ocr-listen", kind)
			listenTestWrite(t, parent, "keep")
			var logs bytes.Buffer
			cfg.Log = &logs
			calls, hooks := 0, 0
			cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
			cfg.OnReviewed = func(context.Context, Job) error { hooks++; return nil }
			listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1})
			if calls != 0 || hooks != 0 || !strings.Contains(logs.String(), "creating "+parent) {
				t.Errorf("output directory failure: reviews=%d, hooks=%d, log=%q", calls, hooks, logs.String())
			}
			if got := string(listenTestRead(t, parent)); got != "keep" {
				t.Errorf("output parent changed: %q", got)
			}
			listenTestAttempts(t, cfg, pull, 1)
			listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
			listenTestLockAvailable(t, listenTestStatePath(cfg, "locks", pull)+".lock")
		})
	}
}

type listenTestDirectory struct {
	syncs, closes     int
	syncErr, closeErr error
}

func (d *listenTestDirectory) Sync() error  { d.syncs++; return d.syncErr }
func (d *listenTestDirectory) Close() error { d.closes++; return d.closeErr }

func TestWriteFileAtomicSyncsParentAfterRename(t *testing.T) {
	for _, failure := range []string{"none", "open", "sync", "close"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "outcome")
			listenTestWrite(t, path, "before")
			directory := &listenTestDirectory{}
			if failure == "sync" {
				directory.syncErr = errors.New("directory sync unsupported")
			}
			if failure == "close" {
				directory.closeErr = errors.New("directory close failed")
			}
			opens := 0
			openDirectory := func(dir string) (directorySyncer, error) {
				opens++
				if dir != filepath.Dir(path) {
					t.Errorf("opened %q, want parent %q", dir, filepath.Dir(path))
				}
				if got := string(listenTestRead(t, path)); got != "after" {
					t.Errorf("directory opened before rename: contents = %q", got)
				}
				if failure == "open" {
					return nil, errors.New("directory open unsupported")
				}
				return directory, nil
			}
			if err := writeFileAtomicWithDirectory(path, []byte("after"), openDirectory); err != nil {
				t.Fatalf("best-effort directory sync failed the write: %v", err)
			}
			wantSyncs := 1
			if failure == "open" {
				wantSyncs = 0
			}
			if opens != 1 || directory.syncs != wantSyncs || directory.closes != wantSyncs {
				t.Errorf("parent directory opens=%d, syncs=%d, closes=%d; want 1, %d, %d", opens, directory.syncs, directory.closes, wantSyncs, wantSyncs)
			}
		})
	}
}
