//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadGHTokenUsesHostnameAndReturnsOutput(t *testing.T) {
	bin := t.TempDir()
	snapWrite(t, bin, "gh", "#!/bin/sh\n[ \"$#\" -eq 4 ] && [ \"$1\" = auth ] && [ \"$2\" = token ] && [ \"$3\" = --hostname ] && [ \"$4\" = ghe.example.com:8443 ] || exit 2\nprintf '  fixture-token\\n'\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	token, err := readGHToken(t.Context(), "ghe.example.com:8443")
	if err != nil || token != "  fixture-token\n" {
		t.Fatalf("readGHToken = (%q, %v), want raw token output", token, err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if got := ResolveToken(t.Context(), "ghe.example.com:8443", nil, nil); got != "fixture-token" {
		t.Errorf("ResolveToken = %q, want trimmed CLI token", got)
	}
}

func TestReadGHTokenDiscardsOutputOnFailure(t *testing.T) {
	bin := t.TempDir()
	snapWrite(t, bin, "gh", "#!/bin/sh\nprintf 'discard-this-token\\n'\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	token, err := readGHToken(t.Context(), "github.com")
	var exitErr *exec.ExitError
	if token != "" || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("readGHToken = (%q, %v), want empty token and wrapped exit code 1", token, err)
	}
	if !strings.Contains(err.Error(), "read gh authentication token") || strings.Contains(err.Error(), "discard-this-token") {
		t.Errorf("error must identify token lookup without exposing output: %v", err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	if got := ResolveToken(t.Context(), "github.com", nil, nil); got != "" {
		t.Errorf("ResolveToken after CLI failure = %q, want anonymous", got)
	}
}
