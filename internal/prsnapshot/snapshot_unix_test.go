//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestSnapshotPrepareDirCreatesPrivateParents(t *testing.T) {
	root := t.TempDir()
	if err := PrepareDir(filepath.Join(root, "a", "b")); err != nil {
		t.Fatalf("PrepareDir: %v", err)
	}
	for _, name := range []string{"a", "a/b"} {
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

func TestSnapshotPrepareDirPreservesExistingDirectoryMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir); err != nil {
		t.Fatalf("PrepareDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("existing directory has mode %#o, want 0755", info.Mode().Perm())
	}
}

func snapChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission denial requires a non-root user")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil && !os.IsNotExist(err) {
			t.Errorf("restore permissions: %v", err)
		}
	})
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDirectoryOperationsPropagateInspectionError(t *testing.T) {
	for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			snapWrite(t, parent, "snapshot/precious", "keep")
			snapChmod(t, parent, 0)
			err := operation(filepath.Join(parent, "snapshot"))
			if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "inspect snapshot directory") {
				t.Fatalf("%s = %v, want wrapped inspection permission error", name, err)
			}
			if err := os.Chmod(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if got := snapRead(t, parent, "snapshot/precious"); got != "keep" {
				t.Errorf("inspection failure modified contents: %q", got)
			}
		})
	}
}

func TestSnapshotPrepareDirPropagatesReadError(t *testing.T) {
	dir := t.TempDir()
	snapWrite(t, dir, "precious", "keep")
	snapChmod(t, dir, 0)
	err := PrepareDir(dir)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "read snapshot directory") {
		t.Fatalf("PrepareDir = %v, want wrapped directory read error", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := snapRead(t, dir, "precious"); got != "keep" {
		t.Errorf("read failure modified contents: %q", got)
	}
	snapAssertAbsent(t, dir, MarkerFile)
}

func TestSnapshotPrepareDirPropagatesMarkerWriteError(t *testing.T) {
	dir := t.TempDir()
	snapChmod(t, dir, 0o500)
	err := PrepareDir(dir)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "write snapshot marker") {
		t.Fatalf("PrepareDir = %v, want wrapped marker write error", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed marker write left files: %v", entries)
	}
}

func TestSnapshotPrepareDirPropagatesEmptyingError(t *testing.T) {
	dir := t.TempDir()
	snapWrite(t, dir, MarkerFile, "")
	snapWrite(t, dir, ".git/ocr-review-pr", "")
	snapWrite(t, dir, "precious", "keep")
	snapChmod(t, dir, 0o500)
	err := PrepareDir(dir)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "empty snapshot directory") {
		t.Fatalf("PrepareDir = %v, want wrapped removal error", err)
	}
	if got := snapRead(t, dir, "precious"); got != "keep" {
		t.Errorf("failed removal modified contents: %q", got)
	}
	if got := snapRead(t, dir, MarkerFile); got != "" {
		t.Errorf("failed removal modified marker: %q", got)
	}
}

func TestSnapshotPrepareDirPreservesOwnershipAfterFailedEmptying(t *testing.T) {
	for _, phase := range []string{"worktree", "git"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			snapWrite(t, dir, MarkerFile, "")
			snapWrite(t, dir, filepath.Join(".git", gitMarkerFile), "")
			// This entry must sort after the marker to catch marker-first deletion.
			gitObstacle := filepath.Join(".git", gitMarkerFile+"-stuck")
			snapWrite(t, dir, filepath.Join(gitObstacle, "keep"), "git data")
			snapWrite(t, dir, "work/keep", "worktree data")
			beforeGit := snapDirectoryState(t, filepath.Join(dir, ".git"))
			obstacle := filepath.Join(dir, "work")
			if phase == "git" {
				obstacle = filepath.Join(dir, gitObstacle)
			}
			snapChmod(t, obstacle, 0o500)
			err := PrepareDir(dir)
			if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("PrepareDir = %v, want permission failure while emptying", err)
			}
			if info, err := os.Lstat(filepath.Join(dir, MarkerFile)); err != nil || !info.Mode().IsRegular() {
				t.Errorf("failed emptying removed the root ownership marker")
			}
			if info, err := os.Lstat(filepath.Join(dir, ".git", gitMarkerFile)); err != nil || !info.Mode().IsRegular() {
				t.Errorf("failed emptying removed the git ownership marker")
			}
			if phase == "worktree" {
				if after := snapDirectoryState(t, filepath.Join(dir, ".git")); !reflect.DeepEqual(after, beforeGit) {
					t.Errorf("git contents changed before the work tree was emptied")
				}
			} else if _, err := os.Lstat(filepath.Join(dir, "work")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("work tree still exists when git emptying failed")
			}
			snapChmod(t, obstacle, 0o700)
			if err := os.RemoveAll(obstacle); err != nil {
				t.Fatal(err)
			}
			if err := PrepareDir(dir); err != nil {
				t.Logf("retry preparation: %v", err)
				t.Errorf("PrepareDir could not recover after the obstacle was removed")
			}
		})
	}
}

func TestSnapshotRemoveDirPreservesOwnershipAfterFailedRemoval(t *testing.T) {
	dir := t.TempDir()
	snapWrite(t, dir, MarkerFile, "")
	snapWrite(t, dir, filepath.Join(".git", gitMarkerFile), "")
	snapWrite(t, dir, ".git/objects/x", "git data")
	snapWrite(t, dir, "work/keep", "worktree data")
	// Bracket the obstacle with .git and the root marker to expose deletion on either side.
	obstacle := filepath.Join(dir, ".middle-stuck")
	snapWrite(t, obstacle, "keep", "stuck worktree data")
	if err := requireMarker(dir); err != nil {
		t.Fatalf("snapshot fixture is not owned: %v", err)
	}
	snapChmod(t, obstacle, 0o500)
	if err := RemoveDir(dir); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("RemoveDir = %v, want permission failure while removing worktree data", err)
	}
	if err := requireMarker(dir); err != nil {
		t.Error("failed removal lost snapshot ownership: requireMarker must still succeed")
	}
	if err := os.Chmod(obstacle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDir(dir); err != nil {
		t.Error("RemoveDir could not recover after permissions were restored")
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("snapshot directory still exists after retrying RemoveDir")
	}
}

func TestSnapshotBuildInterruptedAfterGitInitKeepsDirectoryReusable(t *testing.T) {
	for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
		t.Run(name, func(t *testing.T) {
			origin := snapNewOrigin(t, map[string]string{"file": "base"}, map[string]string{"file": "head"})
			opts := snapOptions(t, origin)
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			realGit, err = filepath.Abs(realGit)
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			t.Setenv("SNAP_REAL_GIT", realGit)
			t.Setenv("SNAP_GIT_MARKER", gitMarkerFile)
			t.Setenv("SNAP_INIT_OBSERVATION", filepath.Join(bin, "observation"))
			snapWrite(t, bin, "git", `#!/bin/sh
for arg do
    if [ "$arg" = init ]; then
        if [ -f ".git/$SNAP_GIT_MARKER" ]; then
            printf present > "$SNAP_INIT_OBSERVATION"
        else
            printf missing > "$SNAP_INIT_OBSERVATION"
        fi
        "$SNAP_REAL_GIT" "$@" || exit $?
        printf 'snap interrupted after git init\n' >&2
        exit 97
    fi
done
exec "$SNAP_REAL_GIT" "$@"
`)
			if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if _, err := Build(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "snap interrupted after git init") {
				t.Fatalf("Build = %v, want the injected interruption after git init", err)
			}
			if got := snapRead(t, bin, "observation"); got != "present" {
				t.Errorf("git ownership marker was missing when git init started")
			}
			if _, err := os.Stat(filepath.Join(opts.Dir, ".git", "HEAD")); err != nil {
				t.Fatalf("git init did not complete: %v", err)
			}
			if err := operation(opts.Dir); err != nil {
				t.Logf("directory operation: %v", err)
				t.Errorf("%s could not recover after git init was interrupted", name)
			}
		})
	}
}

func TestSnapshotBuildIgnoresInheritedConfigEnvironment(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"file": "trusted base bytes"}, map[string]string{"file": "untrusted head bytes"})
	home := t.TempDir()
	marker := filepath.Join(home, "inherited-code-ran")
	script := "#!/bin/sh\nprintf ran > '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\nprintf corrupted\n"
	for _, name := range []string{"hooks/post-checkout", "hooks/reference-transaction", "smudge"} {
		snapWrite(t, home, name, script)
		if err := os.Chmod(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	snapWrite(t, home, "attributes", "* filter=hostile\n")
	config := [][2]string{
		{"core.hooksPath", filepath.Join(home, "hooks")},
		{"core.attributesFile", filepath.Join(home, "attributes")},
		{"filter.hostile.smudge", filepath.Join(home, "smudge")},
		{"filter.hostile.required", "true"},
	}
	for _, style := range []string{"count", "parameters"} {
		t.Run(style, func(t *testing.T) {
			if style == "count" {
				t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(config)))
				for i, entry := range config {
					t.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(i), entry[0])
					t.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(i), entry[1])
				}
			} else {
				var parameters []string
				for _, entry := range config {
					parameters = append(parameters, "'"+entry[0]+"'='"+entry[1]+"'")
				}
				t.Setenv("GIT_CONFIG_PARAMETERS", strings.Join(parameters, " "))
			}
			s := snapBuild(t, snapOptions(t, origin))
			snapAssertAbsent(t, home, "inherited-code-ran")
			if got := snapRead(t, s.Dir, "file"); got != "trusted base bytes" {
				t.Errorf("inherited smudge changed base content: %q", got)
			}
		})
	}
}
