//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package flock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockCreatesPrivateParents(t *testing.T) {
	root := t.TempDir()
	acquireTestLock(t, filepath.Join(root, "x", "y", "z.lock"))
	for _, name := range []string{"x", "x/y"} {
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
