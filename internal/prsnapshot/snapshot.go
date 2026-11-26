// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package prsnapshot materializes a pull request as a tiny local repository
// without cloning the repository it belongs to.
//
// Only the two endpoint commits are fetched, shallow and without blobs; then
// exactly the blobs of the changed files are fetched in one request. From
// those a synthetic two-commit history is written: a base commit holding the
// old side of every changed file and a head commit holding the new side. The
// regular range review then runs on base..head unchanged, and every tool it
// uses (git show, git ls-tree, git grep) can only see the pull request's own
// files.
package prsnapshot

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MarkerFile is created at the root of the scratch directory before anything
// else is written there. A directory is only ever wiped or removed when it
// carries this regular file and, once initialized, .git/ocr-review-pr.
const MarkerFile = ".ocr-review-pr"

// gitMarkerFile is written inside .git, so a tracked MarkerFile in an
// ordinary checkout cannot prove ownership.
const gitMarkerFile = "ocr-review-pr"

const maxExpansionRounds = 4

// Synthetic refs written into the scratch repository.
const (
	BaseRef = "refs/heads/ocr-base"
	HeadRef = "refs/heads/ocr-head"
)

// Options configures Build.
type Options struct {
	// Dir is the scratch repository. It must be missing, empty, or a previous
	// snapshot (it carries MarkerFile); a previous snapshot is wiped first.
	Dir string
	// RemoteURL is fetched from; any URL git accepts.
	RemoteURL string
	// Base and Head are the endpoint revisions: commit ids or ref names the
	// remote advertises. Base is used as the base commit as is, so for a pull
	// request it must already be the merge base.
	Base string
	Head string
	// Paths limits the changed files to these repository-relative paths
	// (a pathspec for git diff-tree). Empty means every changed file.
	Paths []string
	// Context lists unchanged repository-relative files or directories, read
	// at Base, that are added identically to both commits so the review can
	// read them without them appearing in the diff. A changed path keeps its
	// changed version. Missing paths are ignored.
	Context []string
	// Expand is optional. It is called with the snapshot directory after the
	// work tree holds the base commit. Returned paths are added like Context,
	// rewriting both synthetic commits from the fetched endpoints. Expansion
	// stops when no new context paths are returned or after four calls.
	Expand func(dir string) ([]string, error)
	// Token, when non-empty and RemoteURL is https on TokenHost, authenticates
	// git through the environment only, never through argv.
	Token     string
	TokenHost string
}

// Snapshot describes a materialized pull request.
type Snapshot struct {
	Dir string
	// BaseCommit and HeadCommit are the synthetic commits to review
	// (--from BaseCommit --to HeadCommit). They are deterministic: the same
	// endpoint contents always produce the same ids.
	BaseCommit string
	HeadCommit string
	// SourceBase and SourceHead are the fetched endpoint commit ids.
	SourceBase string
	SourceHead string
	// Changed counts changed entries; ContextFiles counts context files added.
	Changed      int
	ContextFiles int
}

// Build materializes the snapshot described by opts. The work tree holds the
// synthetic base commit, never the head: tools read the head through git,
// while anything that reads the work tree (project rules, .gitignore, MCP
// servers and their setup commands) only ever sees target-branch content.
func Build(ctx context.Context, opts Options) (*Snapshot, error) {
	if err := checkRemoteURL(opts.RemoteURL); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot directory: %w", err)
	}
	g := snapshotGit{opts: opts, gitDir: filepath.Join(dir, ".git")}
	if err := g.checkVersion(ctx); err != nil {
		return nil, err
	}
	if err := PrepareDir(dir); err != nil {
		return nil, err
	}
	g.dir = dir
	if err := g.initialize(ctx); err != nil {
		return nil, err
	}
	if _, err := g.command(ctx, true, "", nil, "fetch", "--no-tags", "--depth=1", "--filter=blob:none",
		"--recurse-submodules=no", "origin", "+"+opts.Base+":refs/ocr/source-base", "+"+opts.Head+":refs/ocr/source-head"); err != nil {
		return nil, err
	}
	s := &Snapshot{Dir: dir}
	if s.SourceBase, err = g.run(ctx, "rev-parse", "refs/ocr/source-base^{commit}"); err != nil {
		return nil, err
	}
	if s.SourceHead, err = g.run(ctx, "rev-parse", "refs/ocr/source-head^{commit}"); err != nil {
		return nil, err
	}
	changedBase, changedHead, changed, err := g.changedEntries(ctx, s.SourceBase, s.SourceHead, opts.Paths)
	if err != nil {
		return nil, err
	}
	s.Changed = changed
	var contextPaths []string
	inContext := make(map[string]bool)
	addPaths := func(paths []string) bool {
		added := false
		for _, path := range paths {
			if !inContext[path] {
				inContext[path] = true
				contextPaths = append(contextPaths, path)
				added = true
			}
		}
		return added
	}
	addPaths(opts.Context)
	fetched := make(map[string]bool)
	for round := 0; ; round++ {
		base, head := maps.Clone(changedBase), maps.Clone(changedHead)
		if s.ContextFiles, err = g.addContext(ctx, s.SourceBase, contextPaths, base, head); err != nil {
			return nil, err
		}
		if err := g.fetchBlobs(ctx, fetched, base, head); err != nil {
			return nil, err
		}
		if s.BaseCommit, err = g.writeCommit(ctx, "base", s.SourceBase, "", base); err != nil {
			return nil, err
		}
		if s.HeadCommit, err = g.writeCommit(ctx, "head", s.SourceHead, s.BaseCommit, head); err != nil {
			return nil, err
		}
		for _, ref := range [][2]string{{BaseRef, s.BaseCommit}, {HeadRef, s.HeadCommit}} {
			if _, err := g.run(ctx, "update-ref", ref[0], ref[1]); err != nil {
				return nil, err
			}
		}
		args := []string{"checkout", "-q", "ocr-base"}
		if round > 0 {
			args = []string{"reset", "-q", "--hard", "ocr-base"}
		}
		if _, err := g.run(ctx, args...); err != nil {
			return nil, err
		}
		if opts.Expand == nil || round == maxExpansionRounds {
			break
		}
		paths, err := opts.Expand(dir)
		if err != nil {
			return nil, fmt.Errorf("expand snapshot context: %w", err)
		}
		if !addPaths(paths) {
			break
		}
	}
	if err := g.detachSource(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// PrepareDir makes dir ready for Build: a missing dir is created, an empty
// one is used as is, a previous snapshot is emptied, and anything else is
// refused with an error so a mistyped --workdir can never delete user data.
func PrepareDir(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create snapshot directory %s: %w", dir, err)
		}
	case err != nil:
		return fmt.Errorf("inspect snapshot directory %s: %w", dir, err)
	case !info.IsDir():
		return fmt.Errorf("snapshot path %s is not a directory", dir)
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read snapshot directory %s: %w", dir, err)
		}
		if len(entries) > 0 {
			if err := requireMarker(dir); err != nil {
				return fmt.Errorf("refusing to use non-empty directory %s not created by ocr: %w", dir, err)
			}
			if err := emptyDir(dir); err != nil {
				return fmt.Errorf("empty snapshot directory %s: %w", dir, err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, MarkerFile), nil, 0o600); err != nil {
		return fmt.Errorf("write snapshot marker in %s: %w", dir, err)
	}
	return nil
}

// RemoveDir deletes dir when requireMarker proves snapshot ownership and
// does nothing when dir does not exist. Ownership markers are preserved until
// the work tree and git data are removed. Any other directory is refused.
func RemoveDir(dir string) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect snapshot directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to remove %s: not a snapshot directory", dir)
	}
	if err := requireMarker(dir); err != nil {
		return fmt.Errorf("refusing to remove directory %s not created by ocr: %w", dir, err)
	}
	if err := emptyDir(dir); err != nil {
		return fmt.Errorf("remove snapshot directory %s: %w", dir, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove snapshot directory %s: %w", dir, err)
	}
	return nil
}

// emptyDir removes everything in a snapshot but its root marker.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != MarkerFile && entry.Name() != ".git" {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	gitDir := filepath.Join(dir, ".git")
	entries, err = os.ReadDir(gitDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Keep ownership provable until both the work tree and git data are gone.
	for _, entry := range entries {
		if entry.Name() != gitMarkerFile {
			if err := os.RemoveAll(filepath.Join(gitDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(filepath.Join(gitDir, gitMarkerFile)); err != nil {
		return err
	}
	return os.Remove(gitDir)
}

// requireMarker proves that dir is a snapshot: a regular root marker, and
// either a .git directory carrying a regular git marker or no data beyond an empty .git.
func requireMarker(dir string) error {
	info, err := os.Lstat(filepath.Join(dir, MarkerFile))
	if err != nil {
		return fmt.Errorf("inspect snapshot marker: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot marker is not a regular file")
	}
	gitDir := filepath.Join(dir, ".git")
	info, err = os.Lstat(gitDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect snapshot git directory: %w", err)
	}
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("snapshot git path is not a directory")
		}
		if info, err := os.Lstat(filepath.Join(gitDir, gitMarkerFile)); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("snapshot git marker is not a regular file")
			}
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect snapshot git marker: %w", err)
		}
		entries, err := os.ReadDir(gitDir)
		if err != nil {
			return fmt.Errorf("read snapshot git directory: %w", err)
		}
		if len(entries) != 0 {
			return fmt.Errorf("git data without the snapshot git marker")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read snapshot directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != MarkerFile && entry.Name() != ".git" {
			return fmt.Errorf("data without the snapshot git marker")
		}
	}
	return nil
}

// GitEnv returns base extended with the variables every git command of a
// snapshot runs with: GIT_TERMINAL_PROMPT=0 so a missing credential fails
// instead of hanging, and, when token is non-empty and remoteURL is
// https://tokenHost/..., an http extraheader carrying the token passed through
// GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n, together with
// http.followRedirects=false so redirects cannot carry the token elsewhere.
// Existing GIT_CONFIG_* entries in base are preserved and new ones follow them.
// Inherited repository-location variables are removed so commands cannot act
// on a repository outside the snapshot.
func GitEnv(base []string, remoteURL, token, tokenHost string) []string {
	env := make([]string, 0, len(base)+4)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if !repositoryEnvKey(key) {
			env = append(env, entry)
		}
	}
	env = setGitEnv(env, "GIT_TERMINAL_PROMPT", "0")
	u, err := url.Parse(remoteURL)
	if token == "" || err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || !strings.EqualFold(u.Host, tokenHost) {
		return env
	}
	n := 0
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if sameEnvKey(key, "GIT_CONFIG_COUNT") {
			n, err = strconv.Atoi(value)
			if err != nil || n < 0 || n > int(^uint(0)>>1)-2 {
				n = 0
			}
		}
	}
	index := strconv.Itoa(n)
	env = setGitEnv(env, "GIT_CONFIG_KEY_"+index, "http.https://"+u.Host+"/.extraheader")
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	env = setGitEnv(env, "GIT_CONFIG_VALUE_"+index, "AUTHORIZATION: basic "+auth)
	index = strconv.Itoa(n + 1)
	env = setGitEnv(env, "GIT_CONFIG_KEY_"+index, "http.followRedirects")
	env = setGitEnv(env, "GIT_CONFIG_VALUE_"+index, "false")
	return setGitEnv(env, "GIT_CONFIG_COUNT", strconv.Itoa(n+2))
}
