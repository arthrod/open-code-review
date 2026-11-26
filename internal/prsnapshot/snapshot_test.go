// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type snapOrigin struct {
	dir, remote, base, head string
}

type snapEntry struct {
	mode, kind, oid string
}

var snapRepositoryEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
	"GIT_PREFIX", "GIT_QUARANTINE_PATH",
}

func snapUnsetenv(t *testing.T, key string) {
	t.Helper()
	value, existed := os.LookupEnv(key)
	t.Cleanup(func() {
		var err error
		if existed {
			err = os.Setenv(key, value)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Errorf("restore %s: %v", key, err)
		}
	})
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

func snapGitVersion(t *testing.T) (int, int) {
	t.Helper()
	out, err := exec.Command("git", "version").CombinedOutput()
	if err != nil {
		t.Skipf("git is unavailable: %v: %s", err, out)
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(out), "git version %d.%d", &major, &minor); err != nil {
		t.Fatalf("parse git version %q: %v", out, err)
	}
	return major, minor
}

func snapSetup(t *testing.T) {
	t.Helper()
	major, minor := snapGitVersion(t)
	if major < 2 || (major == 2 && minor < 29) {
		t.Skip("snapshot integration tests require git >= 2.29")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ATTR_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_DEFAULT_HASH", "sha1")
	for _, key := range append(append([]string{}, snapRepositoryEnv...),
		"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_PARAMETERS",
		"GIT_TEMPLATE_DIR", "GIT_NO_LAZY_FETCH", "GIT_LITERAL_PATHSPECS",
		"GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS") {
		snapUnsetenv(t, key)
	}
}

func snapGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env,
		"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_DEFAULT_HASH=sha1",
		"GIT_NO_LAZY_FETCH=1", "GIT_LITERAL_PATHSPECS=1",
		"GIT_AUTHOR_NAME=Snapshot Fixture", "GIT_AUTHOR_EMAIL=snapshot@example.invalid",
		"GIT_COMMITTER_NAME=Snapshot Fixture", "GIT_COMMITTER_EMAIL=snapshot@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00+0000", "GIT_COMMITTER_DATE=2000-01-01T00:00:00+0000")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture git %q in %s: %v: %s", args, dir, err, stderr.String())
	}
	return string(out)
}

func snapWrite(t *testing.T, dir, path, content string) {
	t.Helper()
	name := filepath.Join(dir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapRead(t *testing.T, dir, path string) string {
	t.Helper()
	out, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(out)
}

func snapFileURL(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	urlPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	return (&url.URL{Scheme: "file", Path: urlPath}).String()
}

func snapCommit(t *testing.T, dir, message string) string {
	t.Helper()
	snapGit(t, dir, "add", "--all")
	snapGit(t, dir, "commit", "--allow-empty", "-qm", message)
	return strings.TrimSpace(snapGit(t, dir, "rev-parse", "HEAD"))
}

func snapNewOrigin(t *testing.T, base, head map[string]string) snapOrigin {
	t.Helper()
	snapSetup(t)
	dir := filepath.Join(t.TempDir(), "origin space # percent%")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	snapGit(t, dir, "-c", "init.defaultBranch=main", "init", "--template=", "-q")
	snapGit(t, dir, "config", "uploadpack.allowFilter", "true")
	snapGit(t, dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	snapGit(t, dir, "config", "core.fileMode", "true")
	for path, content := range base {
		snapWrite(t, dir, path, content)
	}
	b := snapCommit(t, dir, "fixture base")
	snapGit(t, dir, "update-ref", "refs/heads/target", b)
	for path := range base {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range head {
		snapWrite(t, dir, path, content)
	}
	h := snapCommit(t, dir, "fixture head")
	return snapOrigin{dir: dir, remote: snapFileURL(t, dir), base: b, head: h}
}

func snapOptions(t *testing.T, origin snapOrigin) Options {
	t.Helper()
	return Options{Dir: filepath.Join(t.TempDir(), "snapshot"), RemoteURL: origin.remote, Base: origin.base, Head: origin.head}
}

func snapBuild(t *testing.T, opts Options) *Snapshot {
	t.Helper()
	snapshot, err := Build(t.Context(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if snapshot == nil {
		t.Fatal("Build returned a nil snapshot without an error")
	}
	if want, err := filepath.Abs(opts.Dir); err != nil || snapshot.Dir != want {
		t.Errorf("snapshot Dir = %q, want %q (%v)", snapshot.Dir, want, err)
	}
	if snapshot.BaseCommit == "" || snapshot.HeadCommit == "" {
		t.Fatalf("Build returned empty synthetic commit ids: %+v", snapshot)
	}
	return snapshot
}

func TestSnapshotBuildReturnsAbsoluteDirFromRelativeInput(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"file": "base"}, map[string]string{"file": "head"})
	opts := snapOptions(t, origin)
	t.Chdir(t.TempDir())
	opts.Dir = "snapshot"
	s := snapBuild(t, opts)
	if !filepath.IsAbs(s.Dir) {
		t.Fatalf("Snapshot.Dir is relative and depends on the caller's working directory")
	}
	t.Chdir(t.TempDir())
	if got := snapRead(t, s.Dir, "file"); got != "base" {
		t.Errorf("snapshot file after changing directory = %q, want base", got)
	}
}

func snapTree(t *testing.T, dir, ref string) map[string]snapEntry {
	t.Helper()
	entries := map[string]snapEntry{}
	out := snapGit(t, dir, "ls-tree", "-r", "-z", "--full-tree", ref)
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		metadata, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			t.Fatalf("malformed ls-tree record %q", record)
		}
		if _, duplicate := entries[path]; duplicate {
			t.Fatalf("duplicate tree path %q", path)
		}
		entries[path] = snapEntry{mode: fields[0], kind: fields[1], oid: fields[2]}
	}
	return entries
}

func snapSubset(t *testing.T, tree map[string]snapEntry, paths ...string) map[string]snapEntry {
	t.Helper()
	result := map[string]snapEntry{}
	for _, path := range paths {
		entry, ok := tree[path]
		if !ok {
			t.Fatalf("fixture tree lacks %q", path)
		}
		result[path] = entry
	}
	return result
}

func snapAssertTree(t *testing.T, dir, ref string, want map[string]snapEntry) {
	t.Helper()
	if got := snapTree(t, dir, ref); !reflect.DeepEqual(got, want) {
		t.Errorf("tree %s = %#v, want %#v", ref, got, want)
	}
}

func snapObjects(t *testing.T, dir, kind string) []string {
	t.Helper()
	// Enumerating only local objects cannot turn an absence check into a lazy fetch.
	out := snapGit(t, dir, "cat-file", "--batch-all-objects", "--batch-check")
	objects := []string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("malformed object inventory %q", line)
		}
		if kind == "" || fields[1] == kind {
			objects = append(objects, fields[0])
		}
	}
	sort.Strings(objects)
	return objects
}

func snapBlobIDs(t *testing.T, trees ...map[string]snapEntry) []string {
	t.Helper()
	set := map[string]bool{}
	for _, tree := range trees {
		for _, entry := range tree {
			if entry.kind == "blob" {
				set[entry.oid] = true
			}
		}
	}
	ids := []string{}
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func snapAssertAbsent(t *testing.T, dir, path string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(path))); !os.IsNotExist(err) {
		t.Errorf("%q should be absent, got %v", path, err)
	}
}

func snapMainFixture(t *testing.T) snapOrigin {
	t.Helper()
	oldRename := strings.Repeat("shared rename content with enough detail\n", 40) + "old ending\n"
	newRename := strings.Repeat("shared rename content with enough detail\n", 40) + "edited ending\n"
	return snapNewOrigin(t, map[string]string{
		"modified.txt": "old modified sentinel\n", "deleted.txt": "deleted unique sentinel\n",
		"old-name.txt": oldRename, "private/unchanged.txt": "private unique sentinel\n",
		"context/rules.txt": "context unique sentinel\n",
	}, map[string]string{
		"modified.txt": "new modified sentinel\n", "added.txt": "added unique sentinel\n",
		"new-name.txt": newRename, "private/unchanged.txt": "private unique sentinel\n",
		"context/rules.txt": "context unique sentinel\n",
	})
}

func TestSnapshotBuildChangedSidesAndRename(t *testing.T) {
	origin := snapMainFixture(t)
	s := snapBuild(t, snapOptions(t, origin))
	if s.Changed != 5 || s.ContextFiles != 0 {
		t.Errorf("counts = (%d, %d), want (5, 0)", s.Changed, s.ContextFiles)
	}
	if s.SourceBase != origin.base || s.SourceHead != origin.head {
		t.Errorf("source ids = (%s, %s), want (%s, %s)", s.SourceBase, s.SourceHead, origin.base, origin.head)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), "modified.txt", "deleted.txt", "old-name.txt"))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), "modified.txt", "added.txt", "new-name.txt"))
	for _, path := range []string{"modified.txt", "added.txt", "new-name.txt"} {
		got := snapGit(t, s.Dir, "show", s.HeadCommit+":"+path)
		if want := snapGit(t, origin.dir, "show", origin.head+":"+path); got != want {
			t.Errorf("head content of %q = %q, want %q", path, got, want)
		}
	}
	if got := strings.TrimSpace(snapGit(t, s.Dir, "merge-base", s.BaseCommit, s.HeadCommit)); got != s.BaseCommit {
		t.Errorf("merge base = %s, want %s", got, s.BaseCommit)
	}
	if got := strings.TrimSpace(snapGit(t, s.Dir, "rev-parse", s.HeadCommit+"^")); got != s.BaseCommit {
		t.Errorf("synthetic head parent = %s, want %s", got, s.BaseCommit)
	}
	out := snapGit(t, s.Dir, "diff", "--find-renames", "-M50%", "--name-status", "-z", s.BaseCommit, s.HeadCommit)
	records := strings.Split(out, "\x00")
	found := false
	for i := 0; i+2 < len(records); i++ {
		if strings.HasPrefix(records[i], "R") && records[i+1] == "old-name.txt" && records[i+2] == "new-name.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("engine diff lost edited rename: %q", out)
	}
}

func TestSnapshotBuildChecksOutTrustedBaseBranch(t *testing.T) {
	origin := snapMainFixture(t)
	s := snapBuild(t, snapOptions(t, origin))
	if got := strings.TrimSpace(snapGit(t, s.Dir, "symbolic-ref", "HEAD")); got != BaseRef {
		t.Errorf("HEAD branch = %q, want %q", got, BaseRef)
	}
	for ref, want := range map[string]string{"HEAD": s.BaseCommit, BaseRef: s.BaseCommit, HeadRef: s.HeadCommit} {
		if got := strings.TrimSpace(snapGit(t, s.Dir, "rev-parse", ref)); got != want {
			t.Errorf("%s = %s, want %s", ref, got, want)
		}
	}
	for _, path := range []string{"modified.txt", "deleted.txt", "old-name.txt"} {
		if got, want := snapRead(t, s.Dir, path), snapGit(t, origin.dir, "show", origin.base+":"+path); got != want {
			t.Errorf("work tree %q = %q, want trusted base %q", path, got, want)
		}
	}
	for _, path := range []string{"added.txt", "new-name.txt", "private/unchanged.txt"} {
		snapAssertAbsent(t, s.Dir, path)
	}
}

func TestSnapshotBuildFetchesExactlyRequiredBlobs(t *testing.T) {
	origin := snapMainFixture(t)
	opts := snapOptions(t, origin)
	opts.Context = []string{"context"}
	old := snapSubset(t, snapTree(t, origin.dir, origin.base), "modified.txt", "deleted.txt", "old-name.txt", "context/rules.txt")
	new := snapSubset(t, snapTree(t, origin.dir, origin.head), "modified.txt", "added.txt", "new-name.txt", "context/rules.txt")
	s := snapBuild(t, opts)
	if got, want := snapObjects(t, s.Dir, "blob"), snapBlobIDs(t, old, new); !reflect.DeepEqual(got, want) {
		t.Errorf("local blobs = %v, want exactly %v", got, want)
	}
	if got := strings.TrimSpace(snapGit(t, s.Dir, "rev-parse", "--is-shallow-repository")); got != "true" {
		t.Errorf("is-shallow-repository = %q, want true", got)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, old)
	snapAssertTree(t, s.Dir, s.HeadCommit, new)
	if s.ContextFiles != 1 {
		t.Errorf("ContextFiles = %d, want 1", s.ContextFiles)
	}
}

func TestSnapshotBuildFiltersChangedPaths(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"selected/a.txt": "old a", "other/b.txt": "old b"},
		map[string]string{"selected/a.txt": "new a", "selected/new.txt": "new selected", "other/b.txt": "new b"})
	opts := snapOptions(t, origin)
	opts.Paths = []string{"selected"}
	s := snapBuild(t, opts)
	if s.Changed != 2 {
		t.Errorf("Changed = %d, want 2", s.Changed)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), "selected/a.txt"))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), "selected/a.txt", "selected/new.txt"))
	if got, want := snapObjects(t, s.Dir, "blob"), snapBlobIDs(t, snapTree(t, s.Dir, s.BaseCommit), snapTree(t, s.Dir, s.HeadCommit)); !reflect.DeepEqual(got, want) {
		t.Errorf("filtered blobs = %v, want %v", got, want)
	}
}

func TestSnapshotBuildTreatsPathspecsLiterally(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture paths use characters forbidden on Windows")
	}
	for _, literal := range []string{":(exclude)selected", "star*"} {
		t.Run(literal, func(t *testing.T) {
			origin := snapNewOrigin(t, map[string]string{literal: "literal old", "selected": "other old"},
				map[string]string{literal: "literal new", "selected": "other new"})
			opts := snapOptions(t, origin)
			opts.Paths = []string{literal}
			s := snapBuild(t, opts)
			if s.Changed != 1 {
				t.Errorf("Changed = %d, want 1", s.Changed)
			}
			snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), literal))
			snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), literal))
		})
	}
}

func TestSnapshotBuildContextPreservesBothChangedVersions(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{
		"modified.txt": "trusted rule", "deleted.txt": "deleted context", "ctx/one.txt": "one", "ctx/two.txt": "two", "hidden.txt": "hidden",
	}, map[string]string{
		"modified.txt": "untrusted rule", "added.txt": "head only", "ctx/one.txt": "one", "ctx/two.txt": "two", "hidden.txt": "hidden",
	})
	opts := snapOptions(t, origin)
	opts.Context = []string{"ctx", "ctx/one.txt", "modified.txt", "deleted.txt", "added.txt", "missing"}
	s := snapBuild(t, opts)
	if s.Changed != 3 || s.ContextFiles != 2 {
		t.Errorf("counts = (%d, %d), want (3, 2)", s.Changed, s.ContextFiles)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), "modified.txt", "deleted.txt", "ctx/one.txt", "ctx/two.txt"))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), "modified.txt", "added.txt", "ctx/one.txt", "ctx/two.txt"))
	wantDiff := "added.txt\x00deleted.txt\x00modified.txt\x00"
	if got := snapGit(t, s.Dir, "diff", "--name-only", "-z", s.BaseCommit, s.HeadCommit); got != wantDiff {
		t.Errorf("diff paths = %q, want %q", got, wantDiff)
	}
	if got := snapRead(t, s.Dir, "modified.txt"); got != "trusted rule" {
		t.Errorf("work tree rules = %q, want trusted rule", got)
	}
}

func TestSnapshotBuildTreatsContextLiterally(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture paths use characters forbidden on Windows")
	}
	origin := snapNewOrigin(t, map[string]string{"change": "old", ":(exclude)ctx": "literal context", "ctx": "private context"},
		map[string]string{"change": "new", ":(exclude)ctx": "literal context", "ctx": "private context"})
	opts := snapOptions(t, origin)
	opts.Context = []string{":(exclude)ctx"}
	s := snapBuild(t, opts)
	if s.ContextFiles != 1 {
		t.Errorf("ContextFiles = %d, want 1", s.ContextFiles)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), "change", ":(exclude)ctx"))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), "change", ":(exclude)ctx"))
}

func TestSnapshotBuildDeterministicAcrossDirectoriesAndUserConfig(t *testing.T) {
	origin := snapMainFixture(t)
	first := snapBuild(t, snapOptions(t, origin))
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	snapWrite(t, home, ".gitconfig", "[i18n]\n\tcommitEncoding = ISO-8859-1\n")
	snapWrite(t, xdg, "git/config", "[i18n]\n\tcommitEncoding = ISO-8859-1\n")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	second := snapBuild(t, snapOptions(t, origin))
	if first.BaseCommit != second.BaseCommit || first.HeadCommit != second.HeadCommit {
		t.Errorf("commit ids changed with user config: (%s, %s) != (%s, %s)", first.BaseCommit, first.HeadCommit, second.BaseCommit, second.HeadCommit)
	}
}

func TestSnapshotBuildReusesMarkedDirectory(t *testing.T) {
	origin := snapMainFixture(t)
	opts := snapOptions(t, origin)
	first := snapBuild(t, opts)
	if _, err := os.Stat(filepath.Join(opts.Dir, MarkerFile)); err != nil {
		t.Fatalf("root marker missing: %v", err)
	}
	snapWrite(t, opts.Dir, "stale/untracked", "left over from earlier use")
	snapWrite(t, opts.Dir, ".git/stale", "left over git data")
	second := snapBuild(t, opts)
	if first.BaseCommit != second.BaseCommit || first.HeadCommit != second.HeadCommit {
		t.Errorf("rerun changed commit ids: %+v vs %+v", first, second)
	}
	snapAssertAbsent(t, opts.Dir, "stale/untracked")
	snapAssertAbsent(t, opts.Dir, ".git/stale")
}

func TestSnapshotPrepareDirCreatesRootMarker(t *testing.T) {
	for _, state := range []string{"missing", "empty", "marked", "marked-without-git"} {
		t.Run(state, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "nested", "snapshot")
			if state != "missing" {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(state, "marked") {
				snapWrite(t, dir, MarkerFile, "")
				if state == "marked" {
					snapWrite(t, dir, "old/data", "stale")
					snapWrite(t, dir, ".git/config", "stale git config")
					snapWrite(t, dir, ".git/ocr-review-pr", "")
				}
			}
			if err := PrepareDir(dir); err != nil {
				t.Fatalf("PrepareDir(%s): %v", state, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != MarkerFile || entries[0].IsDir() {
				t.Fatalf("prepared directory entries = %v, want only a root marker file", entries)
			}
		})
	}
}

func TestSnapshotPrepareDirRefusesUnownedData(t *testing.T) {
	for _, gitDir := range []bool{false, true} {
		t.Run(fmt.Sprintf("git=%t", gitDir), func(t *testing.T) {
			dir := t.TempDir()
			snapWrite(t, dir, "precious.txt", "do not delete")
			if gitDir {
				snapWrite(t, dir, ".git/config", "ordinary repo")
			}
			err := PrepareDir(dir)
			if err == nil || !strings.Contains(err.Error(), "refusing to use non-empty directory") || !strings.Contains(err.Error(), dir) {
				t.Errorf("PrepareDir error = %v, want refusal naming %s", err, dir)
			}
			if got := snapRead(t, dir, "precious.txt"); got != "do not delete" {
				t.Errorf("unowned file changed: %q", got)
			}
			if gitDir && snapRead(t, dir, ".git/config") != "ordinary repo" {
				t.Error("unowned git config changed")
			}
			snapAssertAbsent(t, dir, MarkerFile)
		})
	}
}

func TestSnapshotPrepareDirRefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	snapWrite(t, dir, "file", "keep")
	path := filepath.Join(dir, "file")
	if err := PrepareDir(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("PrepareDir error = %v, want error naming non-directory %s", err, path)
	}
	if got := snapRead(t, dir, "file"); got != "keep" {
		t.Errorf("file was modified: %q", got)
	}
}

func TestSnapshotRemoveDirRefusesUnownedData(t *testing.T) {
	dir := t.TempDir()
	snapWrite(t, dir, "precious.txt", "keep")
	snapWrite(t, dir, ".git/config", "ordinary repo")
	if err := RemoveDir(dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("RemoveDir error = %v, want refusal naming %s", err, dir)
	}
	if got := snapRead(t, dir, "precious.txt"); got != "keep" {
		t.Errorf("unowned data modified: %q", got)
	}
	if got := snapRead(t, dir, ".git/config"); got != "ordinary repo" {
		t.Errorf("unowned repo modified: %q", got)
	}
}

func TestSnapshotRemoveDirMissingIsNoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if err := RemoveDir(dir); err != nil {
		t.Fatalf("RemoveDir missing directory: %v", err)
	}
	snapAssertAbsent(t, filepath.Dir(dir), filepath.Base(dir))
}

func TestSnapshotRemoveDirRefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	snapWrite(t, dir, "file", "keep")
	path := filepath.Join(dir, "file")
	if err := RemoveDir(path); err == nil || !strings.Contains(err.Error(), "not a snapshot directory") || !strings.Contains(err.Error(), path) {
		t.Errorf("RemoveDir error = %v, want refusal naming regular file %s", err, path)
	}
	if got := snapRead(t, dir, "file"); got != "keep" {
		t.Errorf("file was modified: %q", got)
	}
}

func TestSnapshotBuildUnknownEndpointsLeavesReusableDirectory(t *testing.T) {
	for _, endpoint := range []string{"base", "head"} {
		t.Run(endpoint, func(t *testing.T) {
			origin := snapNewOrigin(t, map[string]string{"file": "base"}, map[string]string{"file": "head"})
			opts := snapOptions(t, origin)
			missing := strings.Repeat("f", 40)
			if endpoint == "base" {
				opts.Base = missing
			} else {
				opts.Head = missing
			}
			snapshot, err := Build(t.Context(), opts)
			if snapshot != nil || err == nil || !strings.Contains(err.Error(), "git fetch:") || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Build = (%+v, %v), want fetch error naming missing %s", snapshot, err, endpoint)
			}
			if got := snapRead(t, opts.Dir, MarkerFile); got != "" {
				t.Errorf("snapshot marker = %q, want empty", got)
			}
			opts.Base, opts.Head = origin.base, origin.head
			s := snapBuild(t, opts)
			if s.SourceBase != origin.base || s.SourceHead != origin.head || s.Changed != 1 {
				t.Errorf("retry produced unexpected snapshot: %+v", s)
			}
			snapAssertTree(t, s.Dir, s.BaseCommit, snapTree(t, origin.dir, origin.base))
			snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
		})
	}
}

func TestSnapshotSameEnvKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"GIT_DIR", "GIT_DIR", true},
		{"GIT_DIR", "git_dir", runtime.GOOS == "windows"},
		{"GIT_DIR", "GIT_WORK_TREE", false},
		{"GIT_DIR", "GIT_DIR_EXTRA", false},
		{"", "", true},
	} {
		t.Run(tc.a+"/"+tc.b, func(t *testing.T) {
			if got := sameEnvKey(tc.a, tc.b); got != tc.want {
				t.Errorf("sameEnvKey(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSnapshotRemoveDirRemovesRootMarkedDirectory(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(fmt.Sprintf("initialized=%t", initialized), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "snapshot")
			snapWrite(t, dir, MarkerFile, "")
			if initialized {
				snapWrite(t, dir, "file", "snapshot data")
				snapWrite(t, dir, ".git/config", "snapshot config")
				snapWrite(t, dir, ".git/ocr-review-pr", "")
			}
			if err := RemoveDir(dir); err != nil {
				t.Fatalf("RemoveDir root-marked directory: %v", err)
			}
			snapAssertAbsent(t, filepath.Dir(dir), filepath.Base(dir))
		})
	}
}

func TestSnapshotBuildPreservesExecutableModeChange(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"script.sh": "echo fixture\n"}, map[string]string{"script.sh": "echo fixture\n"})
	snapGit(t, origin.dir, "update-index", "--chmod=+x", "script.sh")
	snapGit(t, origin.dir, "commit", "-qm", "make executable")
	origin.head = strings.TrimSpace(snapGit(t, origin.dir, "rev-parse", "HEAD"))
	s := snapBuild(t, snapOptions(t, origin))
	old := snapTree(t, origin.dir, origin.base)
	new := snapTree(t, origin.dir, origin.head)
	if old["script.sh"].mode != "100644" || new["script.sh"].mode != "100755" {
		t.Fatal("fixture did not create an executable mode change")
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, old)
	snapAssertTree(t, s.Dir, s.HeadCommit, new)
	if s.Changed != 1 {
		t.Errorf("Changed = %d, want 1", s.Changed)
	}
}

func TestSnapshotBuildContextIncludesSymlinkedDirectoryPrefix(t *testing.T) {
	files := map[string]string{"main.txt": "base", "shared/docs/rules.md": "Rule.", "shared/docs/private.md": "Private."}
	origin := snapNewOrigin(t, files, files)
	if err := os.Symlink("shared/docs", filepath.Join(origin.dir, "docs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	origin.base = snapCommit(t, origin.dir, "base with directory link")
	snapWrite(t, origin.dir, "main.txt", "head")
	origin.head = snapCommit(t, origin.dir, "head")
	for _, path := range []string{"docs/rules.md", "shared/docs/missing.md"} {
		t.Run(path, func(t *testing.T) {
			opts := snapOptions(t, origin)
			opts.Context = []string{path}
			s := snapBuild(t, opts)
			paths := []string{"main.txt"}
			wantContext := 0
			if path == "docs/rules.md" {
				paths = append(paths, "docs")
				wantContext = 1
			}
			if s.ContextFiles != wantContext {
				t.Errorf("ContextFiles = %d, want %d", s.ContextFiles, wantContext)
			}
			snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), paths...))
			snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), paths...))
			snapAssertAbsent(t, s.Dir, "shared")
		})
	}
}

func TestSnapshotBuildPreservesAddedSymlink(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"target": "target data"}, map[string]string{"target": "target data"})
	if err := os.Symlink("target", filepath.Join(origin.dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	origin.head = snapCommit(t, origin.dir, "add symlink")
	s := snapBuild(t, snapOptions(t, origin))
	want := snapSubset(t, snapTree(t, origin.dir, origin.head), "link")
	if want["link"].mode != "120000" {
		t.Fatal("fixture did not create a symlink")
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, map[string]snapEntry{})
	snapAssertTree(t, s.Dir, s.HeadCommit, want)
	if got := snapGit(t, s.Dir, "show", s.HeadCommit+":link"); got != "target" {
		t.Errorf("symlink blob = %q, want target", got)
	}
	if s.Changed != 1 {
		t.Errorf("Changed = %d, want 1", s.Changed)
	}
	snapAssertAbsent(t, s.Dir, "link")
}

func TestSnapshotBuildPreservesFileToSymlinkChange(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"entry": "regular base file"}, map[string]string{"entry": "regular base file"})
	if err := os.Remove(filepath.Join(origin.dir, "entry")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("destination", filepath.Join(origin.dir, "entry")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	origin.head = snapCommit(t, origin.dir, "replace with symlink")
	s := snapBuild(t, snapOptions(t, origin))
	snapAssertTree(t, s.Dir, s.BaseCommit, snapTree(t, origin.dir, origin.base))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
	info, err := os.Lstat(filepath.Join(s.Dir, "entry"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || snapRead(t, s.Dir, "entry") != "regular base file" {
		t.Error("work tree did not keep the regular base file")
	}
	if s.Changed != 1 {
		t.Errorf("Changed = %d, want 1", s.Changed)
	}
}

func TestSnapshotBuildPreservesGitlinkWithoutFetchingIt(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{}, map[string]string{})
	oid := strings.Repeat("a", 40)
	snapGit(t, origin.dir, "update-index", "--add", "--cacheinfo", "160000,"+oid+",module")
	snapGit(t, origin.dir, "commit", "-qm", "add gitlink without local commit")
	origin.head = strings.TrimSpace(snapGit(t, origin.dir, "rev-parse", "HEAD"))
	s := snapBuild(t, snapOptions(t, origin))
	snapAssertTree(t, s.Dir, s.BaseCommit, map[string]snapEntry{})
	snapAssertTree(t, s.Dir, s.HeadCommit, map[string]snapEntry{"module": {mode: "160000", kind: "commit", oid: oid}})
	for _, object := range snapObjects(t, s.Dir, "") {
		if object == oid {
			t.Error("gitlink commit was fetched")
		}
	}
	if got := snapObjects(t, s.Dir, "blob"); len(got) != 0 {
		t.Errorf("gitlink-only snapshot has blobs: %v", got)
	}
	if s.Changed != 1 {
		t.Errorf("Changed = %d, want 1", s.Changed)
	}
}

func TestSnapshotBuildResolvesBranchBaseAndSHAHead(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"file": "base"}, map[string]string{"file": "head"})
	opts := snapOptions(t, origin)
	opts.Base = "target"
	s := snapBuild(t, opts)
	if s.SourceBase != origin.base || s.SourceHead != origin.head {
		t.Errorf("resolved endpoints = (%s, %s), want (%s, %s)", s.SourceBase, s.SourceHead, origin.base, origin.head)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, snapTree(t, origin.dir, origin.base))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
}

func TestSnapshotBuildEverythingAdded(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{}, map[string]string{"a": "added a", "dir/b": "added b"})
	s := snapBuild(t, snapOptions(t, origin))
	snapAssertTree(t, s.Dir, s.BaseCommit, map[string]snapEntry{})
	snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
	if s.Changed != 2 {
		t.Errorf("Changed = %d, want 2", s.Changed)
	}
	if got, want := strings.TrimSpace(snapGit(t, s.Dir, "rev-parse", s.BaseCommit+"^{tree}")), strings.TrimSpace(snapGit(t, origin.dir, "rev-parse", origin.base+"^{tree}")); got != want {
		t.Errorf("base tree = %s, want empty tree %s", got, want)
	}
	snapAssertAbsent(t, s.Dir, "a")
}

func TestSnapshotBuildEverythingDeleted(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"a": "deleted a", "dir/b": "deleted b"}, map[string]string{})
	s := snapBuild(t, snapOptions(t, origin))
	snapAssertTree(t, s.Dir, s.BaseCommit, snapTree(t, origin.dir, origin.base))
	snapAssertTree(t, s.Dir, s.HeadCommit, map[string]snapEntry{})
	if s.Changed != 2 {
		t.Errorf("Changed = %d, want 2", s.Changed)
	}
	if got := snapRead(t, s.Dir, "a"); got != "deleted a" {
		t.Errorf("base work tree file = %q, want deleted a", got)
	}
}

func TestSnapshotBuildIdenticalEndpoints(t *testing.T) {
	for _, withContext := range []bool{false, true} {
		t.Run(fmt.Sprintf("context=%t", withContext), func(t *testing.T) {
			origin := snapNewOrigin(t, map[string]string{"rule": "context bytes", "private": "private bytes"}, map[string]string{})
			opts := snapOptions(t, origin)
			opts.Head = origin.base
			want := map[string]snapEntry{}
			wantContext := 0
			if withContext {
				opts.Context = []string{"rule"}
				want = snapSubset(t, snapTree(t, origin.dir, origin.base), "rule")
				wantContext = 1
			}
			s := snapBuild(t, opts)
			if s.Changed != 0 || s.ContextFiles != wantContext {
				t.Errorf("counts = (%d, %d), want (0, %d)", s.Changed, s.ContextFiles, wantContext)
			}
			snapAssertTree(t, s.Dir, s.BaseCommit, want)
			snapAssertTree(t, s.Dir, s.HeadCommit, want)
			if got := snapGit(t, s.Dir, "diff", "--name-only", s.BaseCommit, s.HeadCommit); got != "" {
				t.Errorf("identical endpoint diff = %q, want empty", got)
			}
			if got, wantIDs := snapObjects(t, s.Dir, "blob"), snapBlobIDs(t, want); !reflect.DeepEqual(got, wantIDs) {
				t.Errorf("zero-change blobs = %v, want %v", got, wantIDs)
			}
		})
	}
}

func TestSnapshotBuildPreservesUnusualPathBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tabs and newlines in filenames require a Unix filesystem")
	}
	oldPath, newPath := "old space\tand\nnewline.txt", "new space\tand\nnewline.txt"
	common := strings.Repeat("rename content retained\n", 30)
	origin := snapNewOrigin(t, map[string]string{oldPath: common + "old\n", "zz-following": "old following"},
		map[string]string{newPath: common + "new\n", "zz-following": "new following"})
	s := snapBuild(t, snapOptions(t, origin))
	snapAssertTree(t, s.Dir, s.BaseCommit, snapTree(t, origin.dir, origin.base))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
	if s.Changed != 3 {
		t.Errorf("Changed = %d, want 3", s.Changed)
	}
	if got := snapGit(t, s.Dir, "show", s.HeadCommit+":"+newPath); got != common+"new\n" {
		t.Errorf("unusual-path content = %q, want original bytes", got)
	}
}

func TestSnapshotBuildPreservesNonASCIIPaths(t *testing.T) {
	changed, contextPath := "docs/caf\u00e9.md", "context/na\u00efve.md"
	origin := snapNewOrigin(t, map[string]string{changed: "old bytes", contextPath: "context bytes"},
		map[string]string{changed: "new bytes", contextPath: "context bytes"})
	opts := snapOptions(t, origin)
	opts.Context = []string{"context"}
	s := snapBuild(t, opts)
	snapAssertTree(t, s.Dir, s.BaseCommit, snapTree(t, origin.dir, origin.base))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
	if s.Changed != 1 || s.ContextFiles != 1 {
		t.Errorf("counts = (%d, %d), want (1, 1)", s.Changed, s.ContextFiles)
	}
	if got := snapRead(t, s.Dir, changed); got != "old bytes" {
		t.Errorf("non-ASCII work tree file = %q, want old bytes", got)
	}
	if got := snapRead(t, s.Dir, contextPath); got != "context bytes" {
		t.Errorf("non-ASCII context file = %q, want context bytes", got)
	}
}

func TestSnapshotBuildUnreachableRemoteLeavesReusableMarker(t *testing.T) {
	snapSetup(t)
	opts := Options{Dir: filepath.Join(t.TempDir(), "snapshot"), RemoteURL: snapFileURL(t, filepath.Join(t.TempDir(), "missing-origin")),
		Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Token: "snapshot-secret-token", TokenHost: "github.com"}
	_, err := Build(t.Context(), opts)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "fetch") {
		t.Errorf("Build error = %v, want fetch error", err)
	}
	if err != nil && strings.Contains(err.Error(), opts.Token) {
		t.Error("fetch error exposed the token")
	}
	info, markerErr := os.Stat(filepath.Join(opts.Dir, MarkerFile))
	if markerErr != nil {
		t.Fatalf("failed Build did not leave a root marker: %v", markerErr)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("root marker is not a regular file")
	}
	if err := PrepareDir(opts.Dir); err != nil {
		t.Fatalf("failed Build directory is not reusable: %v", err)
	}
}

func TestSnapshotBuildDoesNotTouchInheritedRepository(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"file": "base"}, map[string]string{"file": "head"})
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.Mkdir(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	snapGit(t, sentinel, "-c", "init.defaultBranch=sentinel", "init", "--template=", "-q")
	snapWrite(t, sentinel, "precious", "sentinel contents")
	snapCommit(t, sentinel, "sentinel commit")
	refs := snapGit(t, sentinel, "show-ref")
	objects := snapObjects(t, sentinel, "")
	config := snapRead(t, sentinel, ".git/config")
	t.Setenv("GIT_DIR", filepath.Join(sentinel, ".git"))
	t.Setenv("GIT_WORK_TREE", sentinel)
	s := snapBuild(t, snapOptions(t, origin))
	if got := snapGit(t, sentinel, "show-ref"); got != refs {
		t.Errorf("sentinel refs changed: %q, want %q", got, refs)
	}
	if got := snapObjects(t, sentinel, ""); !reflect.DeepEqual(got, objects) {
		t.Errorf("sentinel objects changed: %v, want %v", got, objects)
	}
	if got := snapRead(t, sentinel, ".git/config"); got != config {
		t.Error("sentinel config changed")
	}
	if got := snapRead(t, sentinel, "precious"); got != "sentinel contents" {
		t.Errorf("sentinel work tree changed: %q", got)
	}
	snapAssertTree(t, s.Dir, s.HeadCommit, snapTree(t, origin.dir, origin.head))
}

func TestSnapshotBuildIgnoresUserHooksTemplatesAndSmudge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture hooks are POSIX shell scripts")
	}
	origin := snapNewOrigin(t, map[string]string{"file": "trusted base bytes"}, map[string]string{"file": "untrusted head bytes"})
	home := t.TempDir()
	marker := filepath.Join(home, "user-code-ran")
	script := "#!/bin/sh\nprintf ran > '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\nprintf corrupted\n"
	for _, name := range []string{"hooks/post-checkout", "hooks/reference-transaction", "template/hooks/post-checkout", "smudge"} {
		snapWrite(t, home, name, script)
		if err := os.Chmod(filepath.Join(home, filepath.FromSlash(name)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	snapWrite(t, home, "attributes", "* filter=hostile\n")
	global := filepath.Join(home, ".gitconfig")
	for key, value := range map[string]string{
		"core.hooksPath": filepath.Join(home, "hooks"), "init.templateDir": filepath.Join(home, "template"),
		"core.attributesFile": filepath.Join(home, "attributes"), "filter.hostile.smudge": filepath.Join(home, "smudge"), "filter.hostile.required": "true",
	} {
		snapGit(t, home, "config", "--file", global, key, value)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	opts := snapOptions(t, origin)
	s := snapBuild(t, opts)
	snapAssertAbsent(t, home, "user-code-ran")
	if got := snapRead(t, s.Dir, "file"); got != "trusted base bytes" {
		t.Errorf("global smudge changed base content: %q", got)
	}
	if got, want := strings.TrimSpace(snapGit(t, s.Dir, "config", "--local", "core.hooksPath")), filepath.Join(opts.Dir, ".git", "ocr-no-hooks"); got != want {
		t.Errorf("hooksPath = %q, want %q", got, want)
	}
	snapAssertAbsent(t, opts.Dir, ".git/ocr-no-hooks")
	snapAssertAbsent(t, opts.Dir, ".git/ocr-empty-xdg")
}

func snapCaptureStderr(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	t.Cleanup(func() {
		os.Stderr = previous
		_ = r.Close()
		_ = w.Close()
	})
	os.Stderr = w
	type snapReadResult struct {
		data []byte
		err  error
	}
	result := make(chan snapReadResult, 1)
	go func() {
		data, err := io.ReadAll(r)
		result <- snapReadResult{data: data, err: err}
	}()
	run()
	os.Stderr = previous
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	read := <-result
	if read.err != nil {
		t.Fatal(read.err)
	}
	return string(read.data)
}

func TestSnapshotBuildWarnsWhenServerIgnoresFiltering(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"file": "old", "private": "unrelated"}, map[string]string{"file": "new", "private": "unrelated"})
	snapGit(t, origin.dir, "config", "uploadpack.allowFilter", "false")
	opts := snapOptions(t, origin)
	var s *Snapshot
	var buildErr error
	output := snapCaptureStderr(t, func() { s, buildErr = Build(t.Context(), opts) })
	if buildErr != nil {
		t.Fatalf("Build should tolerate unsupported filtering: %v", buildErr)
	}
	if s == nil {
		t.Fatal("Build returned nil without an error")
	}
	if lower := strings.ToLower(output); !strings.Contains(lower, "warn") || !strings.Contains(lower, "filter") {
		t.Errorf("stderr = %q, want a filtering warning", output)
	}
	snapAssertTree(t, s.Dir, s.BaseCommit, snapSubset(t, snapTree(t, origin.dir, origin.base), "file"))
	snapAssertTree(t, s.Dir, s.HeadCommit, snapSubset(t, snapTree(t, origin.dir, origin.head), "file"))
}

func TestSnapshotBuildRejectsGitOlderThan229(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git version fixture is a POSIX shell script")
	}
	snapSetup(t)
	bin := t.TempDir()
	snapWrite(t, bin, "git", "#!/bin/sh\nwhile [ \"$1\" = -c ]; do shift 2; done\ncase \"$1\" in\nversion|--version) printf 'git version 2.28.0\\n'; exit 0;;\n*) exit 99;;\nesac\n")
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := Build(t.Context(), Options{Dir: filepath.Join(t.TempDir(), "snapshot"), RemoteURL: "file:///unused", Base: "base", Head: "head"})
	if err == nil || !strings.Contains(err.Error(), "review-pr needs git >= 2.29, found 2.28.0") {
		t.Errorf("old git error = %v, want minimum-version diagnostic", err)
	}
}

func snapEnvMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			t.Fatalf("invalid environment entry %q", entry)
		}
		if _, duplicate := values[key]; duplicate {
			t.Errorf("duplicate environment key %s", key)
		}
		values[key] = value
	}
	return values
}

func TestSnapshotGitEnvDisablesPromptsAndCopiesInput(t *testing.T) {
	base := []string{"PATH=/fixture/bin", "GIT_TERMINAL_PROMPT=1", "CUSTOM=a=b"}
	wantBase := append([]string{}, base...)
	out := GitEnv(base, "https://github.com/o/r", "", "github.com")
	values := snapEnvMap(t, out)
	for key, want := range map[string]string{"PATH": "/fixture/bin", "GIT_TERMINAL_PROMPT": "0", "CUSTOM": "a=b"} {
		if got := values[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if !reflect.DeepEqual(base, wantBase) {
		t.Fatalf("GitEnv mutated base: %v, want %v", base, wantBase)
	}
	if len(out) == 0 {
		t.Fatal("GitEnv returned an empty environment")
	}
	out[0] = "CHANGED=output"
	if !reflect.DeepEqual(base, wantBase) {
		t.Error("returned environment aliases the input slice")
	}
}

func TestSnapshotGitEnvScopesAuthenticationToCloneHost(t *testing.T) {
	token := "snapshot-token-only-in-value"
	for _, tc := range []struct {
		name, remote, host, token string
		auth                      bool
	}{
		{"matching", "https://github.com/o/r", "github.com", token, true},
		{"host-case", "https://github.com/o/r", "GITHUB.COM", token, true},
		{"matching-port", "https://ghe.example:8443/o/r", "ghe.example:8443", token, true},
		{"different-port", "https://ghe.example:8443/o/r", "ghe.example:443", token, false},
		{"api-host", "https://api.github.com/o/r", "github.com", token, false},
		{"other-host", "https://evil.example/o/r", "github.com", token, false},
		{"host-suffix", "https://github.com.evil.example/o/r", "github.com", token, false},
		{"http", "http://github.com/o/r", "github.com", token, false},
		{"file", "file:///tmp/origin", "github.com", token, false},
		{"ssh", "ssh://git@github.com/o/r", "github.com", token, false},
		{"scp", "git@github.com:o/r", "github.com", token, false},
		{"empty-token", "https://github.com/o/r", "github.com", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := snapEnvMap(t, GitEnv([]string{"KEEP=present"}, tc.remote, tc.token, tc.host))
			if values["KEEP"] != "present" || values["GIT_TERMINAL_PROMPT"] != "0" {
				t.Error("GitEnv lost inherited data or did not disable prompting")
			}
			if tc.auth {
				u, err := url.Parse(tc.remote)
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]string{
					"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "http.https://" + u.Host + "/.extraheader",
					"GIT_CONFIG_VALUE_0": "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
					"GIT_CONFIG_KEY_1":   "http.followRedirects", "GIT_CONFIG_VALUE_1": "false",
				}
				for key, value := range want {
					if values[key] != value {
						t.Errorf("%s = %q, want %q", key, values[key], value)
					}
				}
			} else {
				for key := range values {
					if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") || key == "GIT_CONFIG_COUNT" {
						t.Errorf("authentication config unexpectedly added for %s: %s", tc.remote, key)
					}
				}
			}
			for key, value := range values {
				if strings.Contains(key, token) || (strings.HasPrefix(key, "GIT_CONFIG_KEY_") && strings.Contains(value, token)) {
					t.Errorf("token leaked into key %s", key)
				}
			}
		})
	}
}

func TestSnapshotGitEnvAppendsToExistingConfig(t *testing.T) {
	base := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=fixture-helper",
		"GIT_CONFIG_KEY_1=http.sslVerify", "GIT_CONFIG_VALUE_1=true", "GIT_TERMINAL_PROMPT=1"}
	wantBase := append([]string{}, base...)
	values := snapEnvMap(t, GitEnv(base, "https://github.com/o/r", "secret", "github.com"))
	for key, want := range map[string]string{
		"GIT_CONFIG_COUNT": "4", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "fixture-helper",
		"GIT_CONFIG_KEY_1": "http.sslVerify", "GIT_CONFIG_VALUE_1": "true",
		"GIT_CONFIG_KEY_2":   "http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_2": "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:secret")),
		"GIT_CONFIG_KEY_3":   "http.followRedirects", "GIT_CONFIG_VALUE_3": "false",
		"GIT_TERMINAL_PROMPT": "0",
	} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
	if !reflect.DeepEqual(base, wantBase) {
		t.Errorf("base changed: %v, want %v", base, wantBase)
	}
}

func TestSnapshotGitEnvInvalidConfigCountStartsAtZero(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, count := range []string{"missing", "not-a-number", "-1", strconv.Itoa(maxInt), strconv.Itoa(maxInt - 1)} {
		t.Run(count, func(t *testing.T) {
			base := []string{"KEEP=value"}
			if count != "missing" {
				base = append(base, "GIT_CONFIG_COUNT="+count)
			}
			values := snapEnvMap(t, GitEnv(base, "https://github.com/o/r", "tok", "github.com"))
			if values["GIT_CONFIG_COUNT"] != "2" || values["GIT_CONFIG_KEY_0"] != "http.https://github.com/.extraheader" {
				t.Errorf("invalid count did not append at zero: %v", values)
			}
			want := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))
			if values["GIT_CONFIG_VALUE_0"] != want {
				t.Errorf("auth value = %q, want %q", values["GIT_CONFIG_VALUE_0"], want)
			}
		})
	}
}

func TestSnapshotGitEnvDisablesRedirectsWithToken(t *testing.T) {
	for _, tc := range []struct {
		name, remote, token string
		auth                bool
	}{
		{"attached", "https://github.com/o/r", "secret", true},
		{"no-token", "https://github.com/o/r", "", false},
		{"other-host", "https://example.invalid/o/r", "secret", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.followRedirects", "GIT_CONFIG_VALUE_0=true"}
			values := snapEnvMap(t, GitEnv(base, tc.remote, tc.token, "github.com"))
			if values["GIT_CONFIG_KEY_0"] != "http.followRedirects" || values["GIT_CONFIG_VALUE_0"] != "true" {
				t.Error("inherited redirect configuration changed")
			}
			wantCount := 1
			if tc.auth {
				wantCount = 3
				if values["GIT_CONFIG_KEY_2"] != "http.followRedirects" || values["GIT_CONFIG_VALUE_2"] != "false" {
					t.Error("token header attached without http.followRedirects=false")
				}
			} else if _, ok := values["GIT_CONFIG_KEY_1"]; ok {
				t.Error("configuration appended without a token header")
			}
			if values["GIT_CONFIG_COUNT"] != strconv.Itoa(wantCount) {
				t.Errorf("config count = %q, want %d", values["GIT_CONFIG_COUNT"], wantCount)
			}
			for i := range wantCount {
				for _, prefix := range []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"} {
					if _, ok := values[prefix+strconv.Itoa(i)]; !ok {
						t.Errorf("non-contiguous config: missing %s%d", prefix, i)
					}
				}
			}
		})
	}
}

func TestSnapshotGitEnvRemovesRepositoryLocationVariables(t *testing.T) {
	base := []string{"KEEP=value", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=helper"}
	for i, key := range snapRepositoryEnv {
		base = append(base, key+"=/sentinel/"+strconv.Itoa(i))
	}
	values := snapEnvMap(t, GitEnv(base, "file:///origin", "", ""))
	for _, key := range snapRepositoryEnv {
		if _, exists := values[key]; exists {
			t.Errorf("inherited repository variable survived: %s", key)
		}
	}
	for key, want := range map[string]string{"KEEP": "value", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "helper", "GIT_TERMINAL_PROMPT": "0"} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
}

func TestSnapshotBuildKeepsTokenOutOfGlobalEnvironmentAndConfig(t *testing.T) {
	origin := snapNewOrigin(t, map[string]string{"file": "base"}, map[string]string{"file": "head"})
	opts := snapOptions(t, origin)
	opts.Token, opts.TokenHost = "token-never-global-93c7", "github.com"
	before := append([]string{}, os.Environ()...)
	s := snapBuild(t, opts)
	if after := os.Environ(); !reflect.DeepEqual(before, after) {
		t.Error("Build changed the process environment")
	}
	if config := snapRead(t, s.Dir, ".git/config"); strings.Contains(config, opts.Token) || strings.Contains(strings.ToLower(config), "authorization") {
		t.Error("Build persisted authentication in repository config")
	}
}

func TestSnapshotBuildExpandMatchesInitialContext(t *testing.T) {
	origin := snapNewOrigin(t,
		map[string]string{"file": "old", "a": "context A", "b": "context B"},
		map[string]string{"file": "new", "a": "context A", "b": "context B"})
	opts := snapOptions(t, origin)
	opts.Context = []string{"a", "a"}
	calls := 0
	opts.Expand = func(dir string) ([]string, error) {
		calls++
		if got := snapRead(t, dir, "file"); got != "old" {
			t.Errorf("Expand work tree file = %q, want base content", got)
		}
		if got := snapRead(t, dir, "a"); got != "context A" {
			t.Errorf("Expand context a = %q, want context A", got)
		}
		if calls == 1 {
			return []string{"b", "a", "b"}, nil
		}
		if got := snapRead(t, dir, "b"); got != "context B" {
			t.Errorf("Expand context b = %q, want context B", got)
		}
		return nil, nil
	}
	expanded := snapBuild(t, opts)
	opts = snapOptions(t, origin)
	opts.Context = []string{"a", "b"}
	direct := snapBuild(t, opts)
	if expanded.BaseCommit != direct.BaseCommit || expanded.HeadCommit != direct.HeadCommit {
		t.Errorf("expanded commits = (%s, %s), want direct commits (%s, %s)", expanded.BaseCommit, expanded.HeadCommit, direct.BaseCommit, direct.HeadCommit)
	}
	if expanded.ContextFiles != direct.ContextFiles || expanded.ContextFiles != 2 {
		t.Errorf("expanded ContextFiles = %d, direct = %d, want 2", expanded.ContextFiles, direct.ContextFiles)
	}
	if expanded.Changed != 1 || expanded.SourceBase != origin.base || expanded.SourceHead != origin.head {
		t.Errorf("expanded snapshot metadata = %+v, want original endpoints and one changed file", expanded)
	}
	if calls != 2 {
		t.Errorf("Expand calls = %d, want 2", calls)
	}
	if got := snapRead(t, expanded.Dir, "b"); got != "context B" {
		t.Errorf("expanded work tree b = %q, want context B", got)
	}
	for _, ref := range [][2]string{{BaseRef, expanded.BaseCommit}, {HeadRef, expanded.HeadCommit}, {"HEAD", expanded.BaseCommit}} {
		if got := strings.TrimSpace(snapGit(t, expanded.Dir, "rev-parse", ref[0])); got != ref[1] {
			t.Errorf("%s = %s, want %s", ref[0], got, ref[1])
		}
	}
	if got := snapGit(t, expanded.Dir, "for-each-ref", "--format=%(refname)", "refs/ocr/"); got != "" {
		t.Errorf("source refs remain after expansion: %q", got)
	}
}

func TestSnapshotBuildExpandFetchesEndpointsOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git logging fixture is a POSIX shell script")
	}
	for _, tc := range []struct {
		name      string
		filtering bool
		// Git before 2.39.4 ignores GIT_NO_LAZY_FETCH, so any object lookup
		// in the still-promisor repository would fetch blobs one at a time.
		lazyFetchEnv bool
	}{
		{"filtering", true, true},
		{"no filtering", false, true},
		{"git without GIT_NO_LAZY_FETCH", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := snapNewOrigin(t,
				map[string]string{"file": "old", "a": "context A", "b": "context B"},
				map[string]string{"file": "new", "a": "context A", "b": "context B"})
			if !tc.filtering {
				snapGit(t, origin.dir, "config", "uploadpack.allowFilter", "false")
			}
			expandedOID := snapTree(t, origin.dir, origin.base)["b"].oid
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			realGit, err = filepath.Abs(realGit)
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			snapWrite(t, bin, "args", "")
			snapWrite(t, bin, "stdin", "")
			t.Setenv("SNAP_REAL_GIT", realGit)
			t.Setenv("SNAP_GIT_ARGS", filepath.Join(bin, "args"))
			t.Setenv("SNAP_GIT_STDIN", filepath.Join(bin, "stdin"))
			script := "#!/bin/sh\n"
			if !tc.lazyFetchEnv {
				script += "unset GIT_NO_LAZY_FETCH\n"
			}
			snapWrite(t, bin, "git", script+`printf '%s\n' "$*" >> "$SNAP_GIT_ARGS"
for arg do
    if [ "$arg" = --stdin ]; then
        printf 'batch\n' >> "$SNAP_GIT_STDIN"
        tee -a "$SNAP_GIT_STDIN" | "$SNAP_REAL_GIT" "$@"
        exit $?
    fi
done
exec "$SNAP_REAL_GIT" "$@"
`)
			if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			opts := snapOptions(t, origin)
			opts.Context = []string{"a"}
			opts.Expand = func(string) ([]string, error) { return []string{"b"}, nil }
			snapBuild(t, opts)
			endpointFetches := 0
			for line := range strings.SplitSeq(snapRead(t, bin, "args"), "\n") {
				fields := strings.Fields(line)
				if slices.Contains(fields, "fetch") && slices.Contains(fields, "--depth=1") {
					endpointFetches++
				}
			}
			if endpointFetches != 1 {
				t.Errorf("endpoint fetches = %d, want 1", endpointFetches)
			}
			batch, expandedBatch := 0, 0
			seen := make(map[string]bool)
			for _, line := range strings.Fields(snapRead(t, bin, "stdin")) {
				if line == "batch" {
					batch++
					continue
				}
				if seen[line] {
					t.Errorf("blob %s fetched more than once", line)
				}
				seen[line] = true
				if line == expandedOID {
					expandedBatch = batch
				}
			}
			if batch != 2 || expandedBatch != 2 {
				t.Errorf("blob batches = %d, expanded blob batch = %d, want 2 and 2", batch, expandedBatch)
			}
		})
	}
}

func TestSnapshotBuildExpandBoundAndErrors(t *testing.T) {
	for _, mode := range []string{"bound", "already context", "error"} {
		t.Run(mode, func(t *testing.T) {
			origin := snapNewOrigin(t,
				map[string]string{"file": "old", "a": "context A"},
				map[string]string{"file": "new", "a": "context A"})
			opts := snapOptions(t, origin)
			opts.Context = []string{"a"}
			calls := 0
			expandErr := errors.New("expand failed")
			opts.Expand = func(string) ([]string, error) {
				calls++
				switch mode {
				case "bound":
					if calls > 4 {
						return nil, fmt.Errorf("Expand exceeded four calls")
					}
					return []string{fmt.Sprintf("missing-%d", calls)}, nil
				case "already context":
					return []string{"a", "a"}, nil
				default:
					return nil, expandErr
				}
			}
			_, err := Build(t.Context(), opts)
			if mode == "error" {
				if !errors.Is(err, expandErr) {
					t.Errorf("Build error = %v, want wrapped Expand error", err)
				}
			} else if err != nil {
				t.Fatalf("Build: %v", err)
			}
			wantCalls := 1
			if mode == "bound" {
				wantCalls = 4
			}
			if calls != wantCalls {
				t.Errorf("Expand calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}
