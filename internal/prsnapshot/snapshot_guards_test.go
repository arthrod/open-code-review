// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotBuildDisablesLazyFetchWithoutEnvironment(t *testing.T) {
	origin := snapMainFixture(t)
	oid := snapTree(t, origin.dir, origin.head)["private/unchanged.txt"].oid
	s := snapBuild(t, snapOptions(t, origin))
	if s.SourceBase != origin.base || s.SourceHead != origin.head {
		t.Errorf("source ids changed: (%s, %s)", s.SourceBase, s.SourceHead)
	}
	for _, id := range snapObjects(t, s.Dir, "blob") {
		if id == oid {
			t.Fatal("unchanged blob was fetched during Build")
		}
	}
	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-e", oid)
	cmd.Dir = s.Dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("cat-file without GIT_NO_LAZY_FETCH downloaded the unchanged blob: %s", out)
	}
	for _, id := range snapObjects(t, s.Dir, "blob") {
		if id == oid {
			t.Error("unchanged blob is present after cat-file")
		}
	}
	g := snapshotGit{dir: s.Dir}
	if out, err := g.run(t.Context(), "config", "--get-regexp", "promisor|partialclone"); out != "" || err == nil {
		t.Errorf("promisor configuration remains: %q (error: %v)", out, err)
	}
	if out := snapGit(t, s.Dir, "for-each-ref", "--format=%(refname)", "refs/ocr/source-base", "refs/ocr/source-head"); out != "" {
		t.Errorf("source refs remain: %q", out)
	}
}

func snapDirectoryState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if path == dir && os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		var content string
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content = string(data)
		case info.Mode()&os.ModeSymlink != 0:
			content, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		state[name] = info.Mode().String() + "\x00" + content
		return nil
	})
	if err != nil {
		t.Fatalf("capture directory state: %v", err)
	}
	return state
}

func TestSnapshotDirectoryOperationsRefuseTrackedMarker(t *testing.T) {
	for _, tracked := range []string{"marker and data", "marker only"} {
		for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
			t.Run(tracked+"/"+name, func(t *testing.T) {
				files := map[string]string{MarkerFile: "tracked marker"}
				if tracked == "marker and data" {
					files["precious"] = "keep"
				}
				origin := snapNewOrigin(t, files, files)
				before := snapDirectoryState(t, origin.dir)
				if err := operation(origin.dir); err == nil {
					t.Errorf("%s accepted a repository with only a tracked root marker", name)
				}
				if after := snapDirectoryState(t, origin.dir); !reflect.DeepEqual(after, before) {
					t.Error("unowned repository contents changed")
				}
			})
		}
	}
}

func TestSnapshotDirectoryOperationsRefuseNonRegularMarker(t *testing.T) {
	for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
		for _, kind := range []string{"directory", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				snapWrite(t, dir, "precious", "keep")
				marker := filepath.Join(dir, MarkerFile)
				if kind == "directory" {
					if err := os.Mkdir(marker, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(filepath.Join(dir, "precious"), marker); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				}
				before := snapDirectoryState(t, dir)
				if err := operation(dir); err == nil {
					t.Errorf("%s accepted a %s root marker", name, kind)
				}
				if after := snapDirectoryState(t, dir); !reflect.DeepEqual(after, before) {
					t.Error("unowned directory contents changed")
				}
			})
		}
	}
}

func TestSnapshotDirectoryOperationsRefuseNonRegularGitMarker(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
				t.Run(name, func(t *testing.T) {
					dir, outside := t.TempDir(), t.TempDir()
					snapWrite(t, dir, MarkerFile, "")
					snapWrite(t, dir, ".git/objects/x", "git data")
					snapWrite(t, dir, "work/precious", "worktree data")
					snapWrite(t, outside, "marker", "outside data")
					marker := filepath.Join(dir, ".git", gitMarkerFile)
					if kind == "directory" {
						if err := os.Mkdir(marker, 0o755); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Symlink(filepath.Join(outside, "marker"), marker); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
					before := snapDirectoryState(t, dir)
					beforeOutside := snapDirectoryState(t, outside)
					if err := operation(dir); err == nil {
						t.Errorf("%s accepted a %s git marker", name, kind)
					}
					if after := snapDirectoryState(t, dir); !reflect.DeepEqual(after, before) {
						t.Error("non-regular git marker allowed snapshot contents to change")
					}
					if after := snapDirectoryState(t, outside); !reflect.DeepEqual(after, beforeOutside) {
						t.Error("non-regular git marker allowed outside contents to change")
					}
				})
			}
		})
	}
}

func TestSnapshotGitInitializeRefusesExistingGitDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := t.TempDir()
	g := snapshotGit{
		opts:   Options{Dir: dir, RemoteURL: "https://example.invalid/o/r.git"},
		gitDir: filepath.Join(dir, ".git"),
		dir:    dir,
	}
	if err := os.Mkdir(g.gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.initialize(context.Background()); !errors.Is(err, fs.ErrExist) {
		t.Errorf("initialize error = %v, want fs.ErrExist for an existing .git directory", err)
	}
	if _, err := os.Lstat(filepath.Join(g.gitDir, gitMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("initialize wrote an ownership marker into a pre-existing .git directory")
	}
}

func TestSnapshotDirectoryOperationsRefuseSymlinkedGitDirectory(t *testing.T) {
	for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
		t.Run(name, func(t *testing.T) {
			dir, outside := t.TempDir(), t.TempDir()
			snapWrite(t, dir, MarkerFile, "")
			snapWrite(t, outside, "ocr-review-pr", "")
			snapWrite(t, outside, "keep", "precious")
			if err := os.Symlink(outside, filepath.Join(dir, ".git")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := operation(dir); err == nil {
				t.Errorf("%s accepted a symlinked git directory", name)
			}
			if got := snapRead(t, outside, "keep"); got != "precious" {
				t.Errorf("outside file = %q, want precious", got)
			}
		})
	}
}

func TestSnapshotBuildWritesGitOwnershipMarker(t *testing.T) {
	origin := snapMainFixture(t)
	s := snapBuild(t, snapOptions(t, origin))
	info, err := os.Lstat(filepath.Join(s.Dir, ".git", "ocr-review-pr"))
	if err != nil {
		t.Fatalf("git ownership marker missing: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("git ownership marker mode = %s, want regular file", info.Mode())
	}
	if err := RemoveDir(s.Dir); err != nil {
		t.Fatalf("remove owned snapshot: %v", err)
	}
	snapAssertAbsent(t, filepath.Dir(s.Dir), filepath.Base(s.Dir))
}

func TestSnapshotBuildContextSkipsExcludedAncestor(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"pkg": "base file"}, map[string]string{"pkg/a.go": "head file"})
	opts := snapOptions(t, origin)
	opts.Paths = []string{"pkg/a.go"}
	opts.Context = []string{"pkg"}
	s := snapBuild(t, opts)
	if s.Changed != 1 || s.ContextFiles != 0 {
		t.Errorf("counts = (%d, %d), want (1, 0)", s.Changed, s.ContextFiles)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, map[string]snapEntry{})
	snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), "pkg/a.go"))
	if got := snapGit(t, s.Dir, "diff", "--name-status", "-z", s.BaseCommit, s.HeadCommit); got != "A\x00pkg/a.go\x00" {
		t.Errorf("diff = %q, want only added pkg/a.go", got)
	}
}

func TestSnapshotAddContextSkipsPathCollisions(t *testing.T) {
	origin := snapNewOrigin(t,
		map[string]string{"pkg": "parent file", "pkgish/keep": "sibling"},
		map[string]string{"pkg/a.go": "child file", "pkgish/keep": "sibling"})
	sibling := snapTree(t, origin.dir, origin.base)["pkgish/keep"]
	for _, side := range []string{"base", "head"} {
		for _, relation := range []string{"ancestor", "descendant"} {
			t.Run(side+"/"+relation, func(t *testing.T) {
				base, head := map[string]treeEntry{}, map[string]treeEntry{}
				changed := base
				if side == "head" {
					changed = head
				}
				source, changedPath := origin.base, "pkg/a.go"
				if relation == "descendant" {
					source, changedPath = origin.head, "pkg"
				}
				changed[changedPath] = treeEntry{mode: "100644", oid: "changed"}
				g := snapshotGit{dir: origin.dir}
				count, err := g.addContext(t.Context(), source, []string{"pkg", "pkgish"}, base, head)
				if err != nil {
					t.Fatal(err)
				}
				if count != 1 {
					t.Errorf("ContextFiles = %d, want only the sibling (1)", count)
				}
				for name, tree := range map[string]map[string]treeEntry{"base": base, "head": head} {
					want := map[string]treeEntry{"pkgish/keep": {mode: sibling.mode, oid: sibling.oid}}
					if name == side {
						want[changedPath] = treeEntry{mode: "100644", oid: "changed"}
					}
					if !reflect.DeepEqual(tree, want) {
						t.Errorf("%s entries = %v, want %v", name, tree, want)
					}
				}
			})
		}
	}
}

func TestSnapshotDirectoryOperationsRefuseRootMarkerAlongsideUnownedData(t *testing.T) {
	for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			snapWrite(t, dir, MarkerFile, "")
			snapWrite(t, dir, "notes/precious.txt", "keep")
			before := snapDirectoryState(t, dir)
			if err := operation(dir); err == nil {
				t.Errorf("%s accepted a root marker next to data without a git marker", name)
			}
			if after := snapDirectoryState(t, dir); !reflect.DeepEqual(after, before) {
				t.Error("unowned directory contents changed")
			}
		})
	}
}

func TestSnapshotDirectoryOperationsRecoverInterruptedEmptyGit(t *testing.T) {
	for name, operation := range map[string]func(string) error{"prepare": PrepareDir, "remove": RemoveDir} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := PrepareDir(dir); err != nil {
				t.Fatal(err)
			}
			if err := operation(dir); err != nil {
				t.Fatalf("%s refused a root marker alone: %v", name, err)
			}
			if err := PrepareDir(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			snapWrite(t, dir, "precious", "keep")
			before := snapDirectoryState(t, dir)
			if err := operation(dir); err == nil {
				t.Fatalf("%s accepted unowned data alongside an empty .git", name)
			}
			if after := snapDirectoryState(t, dir); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed unowned data alongside an empty .git", name)
			}
			if err := os.Remove(filepath.Join(dir, "precious")); err != nil {
				t.Fatal(err)
			}
			if err := operation(dir); err != nil {
				t.Logf("directory operation: %v", err)
				t.Fatalf("%s refused an interrupted snapshot with an empty .git", name)
			}
			if name == "remove" {
				snapAssertAbsent(t, filepath.Dir(dir), filepath.Base(dir))
				return
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != MarkerFile {
				t.Errorf("prepared directory entries = %v, want only the root marker", entries)
			}
		})
	}
}

func TestSnapshotBuildRejectsCredentialsInRemoteURL(t *testing.T) {
	for _, remote := range []string{
		"https://user:s3cret@example.com/o/r.git",
		"https://s3cret@example.com/o/r.git",
		"HTTP://s3cret:x-oauth-basic@example.com/o/r",
		"ssh://git:s3cret@example.com/o/r.git",
		"https://user:pass@host/repo%zz",
		"https://user@host/repo%zz",
	} {
		t.Run(remote, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "snapshot")
			_, err := Build(t.Context(), Options{Dir: dir, RemoteURL: remote, Base: "main", Head: "main"})
			if err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("Build error = %v, want a refusal of credentials in the remote URL", err)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error repeats the credential: %v", err)
			}
			snapAssertAbsent(t, filepath.Dir(dir), "snapshot")
		})
	}
	for _, remote := range []string{
		"https://example.com/o/r.git", "ssh://git@example.com/o/r.git",
		"git@example.com:o/r.git", "file:///srv/r.git", "/srv/r.git",
		"ssh://git@host/repo%zz", "git@host:o/r",
	} {
		if err := checkRemoteURL(remote); err != nil {
			t.Errorf("checkRemoteURL(%q) = %v, want nil", remote, err)
		}
	}
}

func TestSnapshotCheckRemoteURLRejectsUnparseableCredentials(t *testing.T) {
	for _, remote := range []string{
		"https://user:pass@host/repo%zz", "https://user@host/repo%zz",
		"HTTPS://user@host/repo%zz", "ssh://git:pass@host/repo%zz",
		"ssh://user:pass@proxy@host/repo%zz",
	} {
		t.Run(remote, func(t *testing.T) {
			err := checkRemoteURL(remote)
			if err == nil || !strings.Contains(err.Error(), "the remote URL must not carry credentials") {
				t.Fatalf("checkRemoteURL error = %v, want a refusal of credentials", err)
			}
			if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "user@") {
				t.Errorf("error repeats credentials: %v", err)
			}
		})
	}
	for _, remote := range []string{
		"ssh://git@host/repo%zz", "git@host:o/r", "https://host/repo%zz",
		"https://host/repo%zz?user:pass@host", "https://host?user:pass@host/%zz",
		"https://host#user:pass@host/%zz",
	} {
		if err := checkRemoteURL(remote); err != nil {
			t.Errorf("checkRemoteURL(%q) = %v, want nil", remote, err)
		}
	}
}
