// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/alibaba/open-code-review/internal/gitcmd"
)

type snapshotGit struct {
	opts   Options
	dir    string
	gitDir string
}

func sameEnvKey(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func repositoryEnvKey(key string) bool {
	for _, name := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
		"GIT_PREFIX", "GIT_QUARANTINE_PATH",
	} {
		if sameEnvKey(key, name) {
			return true
		}
	}
	return false
}

func setGitEnv(env []string, key, value string) []string {
	result := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !sameEnvKey(name, key) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func configEnvKey(key string) bool {
	if runtime.GOOS == "windows" {
		key = strings.ToUpper(key)
	}
	return key == "GIT_CONFIG_PARAMETERS" || key == "GIT_CONFIG_COUNT" ||
		strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_")
}

func withoutConfigEnv(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); !configEnvKey(key) {
			result = append(result, entry)
		}
	}
	return result
}

// checkRemoteURL refuses a remote URL that carries credentials: git keeps the
// URL in .git/config, which outlives the review, and repeats it in errors. A
// user name alone is an account on ssh, but on http(s) tokens sit there. git
// accepts URLs Go cannot parse, so those are inspected as text.
func checkRemoteURL(remote string) error {
	u, err := url.Parse(remote)
	var scheme string
	var password bool
	if err != nil {
		var authority string
		var ok bool
		scheme, authority, ok = strings.Cut(remote, "://")
		if !ok {
			return nil
		}
		if end := strings.IndexAny(authority, "/?#"); end >= 0 {
			authority = authority[:end]
		}
		at := strings.LastIndex(authority, "@")
		if at < 0 {
			return nil
		}
		password = strings.Contains(authority[:at], ":")
	} else {
		if u.User == nil {
			return nil
		}
		scheme = u.Scheme
		_, password = u.User.Password()
	}
	if scheme := strings.ToLower(scheme); password || scheme == "http" || scheme == "https" {
		return errors.New("the remote URL must not carry credentials; use a git credential helper, or GH_TOKEN for GitHub")
	}
	return nil
}

func (g snapshotGit) command(ctx context.Context, fetch bool, input string, extra []string, args ...string) (string, error) {
	env := GitEnv(os.Environ(), g.opts.RemoteURL, g.opts.Token, g.opts.TokenHost)
	if !fetch {
		// Configuration from the environment outranks the local one, so an
		// inherited hooksPath, fsmonitor or filter would run during checkout.
		env = withoutConfigEnv(env)
		extra = append([]string{
			"GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1",
			"XDG_CONFIG_HOME=" + filepath.Join(g.gitDir, "ocr-empty-xdg"),
		}, extra...)
	}
	for _, entry := range extra {
		key, value, _ := strings.Cut(entry, "=")
		env = setGitEnv(env, key, value)
	}
	// A fetch keeps the inherited configuration for credentials and URL
	// rewrites; a -c option is applied after it, so no hook runs either way.
	gitArgs := append([]string{"-c", "core.hooksPath=" + filepath.Join(g.gitDir, "ocr-no-hooks")}, args...)
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	cmd.Dir, cmd.Env = g.dir, env
	cmd.Stdin = strings.NewReader(input)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if fetch && strings.Contains(stderr.String(), "filtering not recognized by server") {
		fmt.Fprintln(os.Stderr, "[ocr] warning: server ignored blob filtering; full file contents may have been fetched")
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if g.opts.Token != "" {
			detail = strings.ReplaceAll(detail, g.opts.Token, "[redacted]")
			auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + g.opts.Token))
			detail = strings.ReplaceAll(detail, auth, "[redacted]")
		}
		step := args[0]
		if fetch {
			step = "fetch"
		}
		return "", fmt.Errorf("git %s: %w: %s", step, err, detail)
	}
	return string(out), nil
}

func (g snapshotGit) run(ctx context.Context, args ...string) (string, error) {
	out, err := g.command(ctx, false, "", nil, args...)
	return strings.TrimSpace(out), err
}

func (g snapshotGit) checkVersion(ctx context.Context) error {
	out, err := g.run(ctx, "version")
	if err != nil {
		return err
	}
	v, err := gitcmd.ParseGitVersion(out)
	if err != nil {
		return fmt.Errorf("parse git version: %w", err)
	}
	if !v.AtLeast(gitcmd.GitVersion{Major: 2, Minor: 29}) {
		return fmt.Errorf("review-pr needs git >= 2.29, found %s", v)
	}
	return nil
}

func (g snapshotGit) initialize(ctx context.Context) error {
	if err := os.Mkdir(g.gitDir, 0o755); err != nil {
		return fmt.Errorf("create snapshot git directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(g.gitDir, gitMarkerFile), nil, 0o600); err != nil {
		return fmt.Errorf("write snapshot git marker: %w", err)
	}
	if _, err := g.run(ctx, "init", "--template=", "-q"); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"config", "core.hooksPath", filepath.Join(g.gitDir, "ocr-no-hooks")},
		{"remote", "add", "origin", g.opts.RemoteURL},
		{"config", "remote.origin.promisor", "true"},
		{"config", "remote.origin.partialclonefilter", "blob:none"},
	} {
		if _, err := g.run(ctx, args...); err != nil {
			return err
		}
	}
	return nil
}

func (g snapshotGit) detachSource(ctx context.Context) error {
	for _, ref := range []string{"refs/ocr/source-base", "refs/ocr/source-head"} {
		if _, err := g.run(ctx, "update-ref", "-d", ref); err != nil {
			return fmt.Errorf("remove source ref %s: %w", ref, err)
		}
	}
	for _, key := range []string{"remote.origin.promisor", "remote.origin.partialclonefilter", "extensions.partialclone"} {
		if _, err := g.run(ctx, "config", "--local", "--unset-all", key); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 5 {
				return fmt.Errorf("remove source configuration %s: %w", key, err)
			}
		}
	}
	return nil
}

// fetchBlobs fetches, in one request, the blobs of trees that are not in
// fetched yet, and records them there. Presence is tracked here rather than
// asked of git: before 2.39.4 git ignores GIT_NO_LAZY_FETCH, so any lookup of
// a missing blob in the promisor repository would fetch it on its own.
func (g snapshotGit) fetchBlobs(ctx context.Context, fetched map[string]bool, trees ...map[string]treeEntry) error {
	ids := make(map[string]bool)
	for _, tree := range trees {
		for _, entry := range tree {
			if entry.mode != "160000" && strings.Trim(entry.oid, "0") != "" && !fetched[entry.oid] {
				ids[entry.oid] = true
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	if _, err := g.command(ctx, true, strings.Join(ordered, "\n")+"\n", nil,
		"-c", "fetch.negotiationAlgorithm=noop", "fetch", "--no-tags", "--no-write-fetch-head",
		"--recurse-submodules=no", "--filter=blob:none", "--stdin", "origin"); err != nil {
		return err
	}
	for _, id := range ordered {
		fetched[id] = true
	}
	return nil
}

func (g snapshotGit) writeCommit(ctx context.Context, side, source, parent string, entries map[string]treeEntry) (string, error) {
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var input strings.Builder
	for _, path := range paths {
		entry := entries[path]
		fmt.Fprintf(&input, "%s %s\t%s\x00", entry.mode, entry.oid, path)
	}
	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(g.gitDir, "ocr-"+side+".index")}
	if _, err := g.command(ctx, false, input.String(), indexEnv, "update-index", "--add", "-z", "--index-info"); err != nil {
		return "", err
	}
	tree, err := g.command(ctx, false, "", indexEnv, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", strings.TrimSpace(tree)}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	env := []string{
		"GIT_AUTHOR_NAME=ocr", "GIT_AUTHOR_EMAIL=ocr@localhost", "GIT_AUTHOR_DATE=@0 +0000",
		"GIT_COMMITTER_NAME=ocr", "GIT_COMMITTER_EMAIL=ocr@localhost", "GIT_COMMITTER_DATE=@0 +0000",
	}
	commit, err := g.command(ctx, false, "ocr review-pr "+side+" "+source+"\n", env, args...)
	return strings.TrimSpace(commit), err
}
