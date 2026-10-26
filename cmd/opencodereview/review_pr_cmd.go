// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alibaba/open-code-review/internal/config/rules"
	"github.com/alibaba/open-code-review/internal/flock"
	"github.com/alibaba/open-code-review/internal/pathutil"
	"github.com/alibaba/open-code-review/internal/prsnapshot"
	"github.com/spf13/cobra"
)

type reviewPROptions struct {
	review    reviewOptions
	remote    string
	base      string
	head      string
	paths     string
	context   string
	workdir   string
	cleanup   bool
	githubAPI string
	// successFile is created once the review succeeded; ocr listen adopts the
	// result of a review whose listener died only when it exists.
	successFile string
}

var reviewPROpts reviewPROptions

// reviewPROwnedFlags are review flags review-pr derives from the snapshot it
// builds. They stay registered so every other review flag is inherited
// without a copy, but are hidden and rejected when set.
var reviewPROwnedFlags = []string{"repo", "from", "to", "commit"}

// reviewPRContextFiles are always offered as context because the engine reads
// them from the work tree: the rules directory (rule.json and the rule files it
// may reference), the root .gitignore and .gitattributes. They come from the
// base side, so a pull request cannot change the rules it is reviewed under.
var reviewPRContextFiles = []string{".opencodereview", ".gitignore", ".gitattributes"}

// reviewPRRuleExts mirrors the unexported allowedRuleExts of
// internal/config/rules: a rule file the engine would read must be fetched
// into the snapshot. A test compares the two sets.
var reviewPRRuleExts = map[string]bool{".md": true, ".txt": true, ".markdown": true}

// maxPRBackgroundRunes caps the PR title and body used as default background.
const maxPRBackgroundRunes = 8000

var reviewPRCmd = newReviewPRCmd(&reviewPROpts)

func init() {
	rootCmd.AddCommand(reviewPRCmd)
}

// newReviewPRCmd builds the review-pr command bound to opts. Tests build their
// own instead of reusing the package-level one, whose flag state would leak
// from one test to the next.
func newReviewPRCmd(opts *reviewPROptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review-pr <pr> [flags]",
		Short: "Review a pull request without cloning its repository",
		Long: "Review a pull request without cloning its repository.\n\n" +
			"Only the merge-base and head commits are fetched (shallow, without blobs), then only\n" +
			"the blobs of the changed files. The review runs on a synthetic two-commit repository\n" +
			"holding just those files, so it never depends on the rest of the repository.\n\n" +
			"<pr> is OWNER/REPO#N, HOST/OWNER/REPO#N or a pull request URL. Without it, --remote,\n" +
			"--base and --head name the commits directly, on any git host.",
		Args: maximumArgs(1),
		Example: `  # Review a GitHub pull request
  ocr review-pr alibaba/open-code-review#1773
  ocr review-pr https://github.com/alibaba/open-code-review/pull/1773

  # Only the changed files of one chapter, with its neighbours as context
  ocr review-pr acme/book#42 --path chapters/07 --context chapters/07

  # Any git host, no API: --base is used as the base commit
  ocr review-pr --remote https://git.example.com/acme/book.git --base 1a2b3c --head feature/ch7`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := interruptContextWithForcedExit(cmd.Context())
			defer stop()
			if err := runReviewPR(ctx, cmd, *opts, args); err != nil {
				return err
			}
			if opts.successFile != "" {
				if err := os.WriteFile(opts.successFile, nil, 0o644); err != nil {
					return fmt.Errorf("write the success file: %w", err)
				}
			}
			return nil
		},
	}
	registerReviewFlags(cmd, &opts.review)
	for _, name := range reviewPROwnedFlags {
		_ = cmd.Flags().MarkHidden(name)
	}
	f := cmd.Flags()
	f.StringVar(&opts.remote, "remote", "", "git URL to fetch from (PR mode: overrides the API clone URL, e.g. an SSH URL)")
	f.StringVar(&opts.base, "base", "", "base commit or ref (PR mode: overrides the merge base; required without a PR)")
	f.StringVar(&opts.head, "head", "", "head commit or ref (PR mode: pins the reviewed head; required without a PR)")
	f.StringVar(&opts.paths, "path", "", "comma-separated repo-relative paths; only changed files under them are reviewed")
	f.StringVar(&opts.context, "context", "", "comma-separated unchanged repo-relative files or directories to include as read-only context")
	f.StringVar(&opts.workdir, "workdir", "", "scratch directory for the snapshot (default: ~/.opencodereview/pr/<host>/<owner>/<repo>/<number>)")
	f.BoolVar(&opts.cleanup, "cleanup", false, "remove the scratch directory after the review")
	f.StringVar(&opts.githubAPI, "github-api", "", "GitHub REST API root (default: derived from the PR host; GHES: https://HOST/api/v3)")
	f.StringVar(&opts.successFile, "success-file", "", "file to create once the review succeeded (used by ocr listen)")
	_ = f.MarkHidden("success-file")
	return cmd
}

// pullSource is the part of the GitHub client review-pr uses.
type pullSource interface {
	GetPull(ctx context.Context, owner, repo string, number int) (*prsnapshot.Pull, error)
	MergeBase(ctx context.Context, owner, repo, base, head string) (string, error)
}

// Test seams.
var (
	newPullSource = func(baseURL, token string) pullSource {
		return prsnapshot.NewClient(baseURL, token)
	}
	resolveGitHubToken = func(ctx context.Context, host string) string {
		return prsnapshot.ResolveToken(ctx, host, nil, nil)
	}
	runReviewEngine = executeReviewContext
)

// runReviewPR resolves the source, builds the snapshot under an exclusive
// lock on its workdir, then runs the regular review on base..head.
//
// Every relative path flag (--rule, --tools, --background-file, --output) is
// made absolute against the caller's working directory first, because the
// engine resolves some of them against the repository, which here is the
// scratch directory.
func runReviewPR(ctx context.Context, cmd *cobra.Command, opts reviewPROptions, args []string) (retErr error) {
	for _, name := range reviewPROwnedFlags {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s cannot be used with review-pr: the snapshot decides it", name)
		}
	}
	if err := validateReviewOptions(&opts.review); err != nil {
		return err
	}
	input, err := resolveReviewPRInput(ctx, opts, args)
	if err != nil {
		return err
	}
	workdir, err := reviewPRWorkdir(opts.workdir, input)
	if err != nil {
		return err
	}
	lock, acquired, err := flock.TryLock(workdir + ".lock")
	if err != nil {
		return fmt.Errorf("lock review-pr workdir %s: %w", workdir, err)
	}
	if !acquired {
		return fmt.Errorf("workdir %s is in use by another review-pr", workdir)
	}
	defer func() { retErr = errors.Join(retErr, lock.Unlock()) }()

	input.snapshot.Dir = workdir
	input.snapshot.Paths = splitPaths(opts.paths)
	input.snapshot.Context = append(append([]string(nil), reviewPRContextFiles...), splitPaths(opts.context)...)
	if opts.cleanup {
		defer func() {
			// Without the marker the directory was never ours (Build refused it),
			// and removing it is not cleanup.
			if _, err := os.Lstat(filepath.Join(workdir, prsnapshot.MarkerFile)); err == nil {
				retErr = errors.Join(retErr, prsnapshot.RemoveDir(workdir))
			}
		}()
	}
	snap, err := buildReviewPRSnapshot(ctx, input.snapshot)
	if err != nil {
		return fmt.Errorf("build review-pr snapshot: %w", err)
	}
	for _, path := range []*string{&opts.review.rulePath, &opts.review.toolConfigPath, &opts.review.backgroundFile, &opts.review.outputPath} {
		if *path == "" || *path == "-" {
			continue
		}
		abs, err := filepath.Abs(*path)
		if err != nil {
			return fmt.Errorf("resolve review-pr path %q: %w", *path, err)
		}
		*path = abs
	}
	if opts.review.background == "" && opts.review.backgroundFile == "" {
		opts.review.background = input.background
	}
	opts.review.repoDir, opts.review.from, opts.review.to = snap.Dir, snap.BaseCommit, snap.HeadCommit
	if opts.review.audience != "agent" {
		fmt.Fprintf(os.Stderr, "[ocr] review-pr: %s %.12s..%.12s: %d changed file(s), %d context file(s) in %s\n",
			input.label, snap.SourceBase, snap.SourceHead, snap.Changed, snap.ContextFiles, workdir)
	}
	return runReviewPRIsolated(ctx, opts.review)
}

type reviewPRInput struct {
	snapshot   prsnapshot.Options
	ref        *prsnapshot.PullRef
	label      string
	background string
}

func resolveReviewPRInput(ctx context.Context, opts reviewPROptions, args []string) (reviewPRInput, error) {
	input := reviewPRInput{
		snapshot: prsnapshot.Options{RemoteURL: opts.remote, Base: opts.base, Head: opts.head},
		label:    reviewPRRemoteLabel(opts.remote),
	}
	if len(args) == 0 {
		if opts.remote == "" || opts.base == "" || opts.head == "" {
			return input, errors.New("review-pr needs a pull request, or --remote with --base and --head")
		}
		return input, nil
	}
	ref, err := prsnapshot.ParsePullRef(args[0])
	if err != nil {
		return input, fmt.Errorf("parse pull request: %w", err)
	}
	input.ref, input.label = &ref, ref.String()
	apiBase := opts.githubAPI
	if apiBase == "" {
		apiBase = prsnapshot.APIBaseForHost(ref.Host)
	}
	input.snapshot.TokenHost = ref.Host
	input.snapshot.Token = resolveGitHubToken(ctx, ref.Host)
	source := newPullSource(apiBase, input.snapshot.Token)
	pull, err := source.GetPull(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return input, reviewPRAPIError(fmt.Sprintf("get pull request %s", ref.String()), ref.Host, input.snapshot.Token, err)
	}
	if input.snapshot.Head == "" {
		input.snapshot.Head = pull.HeadSHA
	}
	if input.snapshot.Base == "" {
		input.snapshot.Base, err = source.MergeBase(ctx, ref.Owner, ref.Repo, pull.BaseRef, input.snapshot.Head)
		if err != nil {
			return input, reviewPRAPIError(fmt.Sprintf("find merge base for %s", ref.String()), ref.Host, input.snapshot.Token, err)
		}
	}
	if input.snapshot.RemoteURL == "" {
		input.snapshot.RemoteURL = pull.CloneURL
	}
	if pull.State != "open" {
		fmt.Fprintf(os.Stderr, "[ocr] review-pr: %s is %s; reviewing anyway\n", ref.String(), pull.State)
	}
	if opts.review.background == "" && opts.review.backgroundFile == "" {
		input.background = reviewPRBackground(ref.Number, pull.Title, pull.Body)
	}
	return input, nil
}

// reviewPRAPIError wraps a failed GitHub API call. Without a token it also says
// how to provide one when a token could change the answer: GitHub answers 404,
// not 401, for a private repository read anonymously.
func reviewPRAPIError(action, host, token string, err error) error {
	var apiErr *prsnapshot.APIError
	if token == "" && errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403 || apiErr.StatusCode == 404) {
		return fmt.Errorf("%s: no GitHub token found (set %s or run gh auth login --hostname %s): %w", action, prsnapshot.TokenEnvVars(host)[0], host, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func reviewPRWorkdir(dir string, input reviewPRInput) (string, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve review-pr home directory: %w", err)
		}
		parts := []string{home, ".opencodereview", "pr"}
		if input.ref != nil {
			parts = append(parts, input.ref.PathComponents()...)
			parts = append(parts, strconv.Itoa(input.ref.Number))
		} else {
			sum := sha256.Sum256([]byte(input.snapshot.RemoteURL))
			parts = append(parts, "remote", fmt.Sprintf("%x", sum[:8]))
		}
		dir = filepath.Join(parts...)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve review-pr workdir: %w", err)
	}
	return abs, nil
}

func reviewPRBackground(number int, title, body string) string {
	cleaned := sanitizeMarkdown(fmt.Sprintf("Pull request #%d: %s\n\n%s", number, title, body))
	// Removing a delimiter can join the text around it into another one, so
	// strip until nothing changes.
	for {
		stripped := strings.ReplaceAll(strings.ReplaceAll(cleaned, backgroundOpenTag, ""), backgroundCloseTag, "")
		if stripped == cleaned {
			break
		}
		cleaned = stripped
	}
	if cleaned == "" {
		return ""
	}
	if runes := []rune(cleaned); len(runes) > maxPRBackgroundRunes {
		cleaned = string(runes[:maxPRBackgroundRunes]) + "\n[truncated]"
	}
	return backgroundOpenTag + "\n" + cleaned + "\n" + backgroundCloseTag
}

func buildReviewPRSnapshot(ctx context.Context, opts prsnapshot.Options) (*prsnapshot.Snapshot, error) {
	inContext := make(map[string]bool)
	for _, path := range opts.Context {
		inContext[path] = true
	}
	opts.Expand = func(dir string) ([]string, error) {
		missing, err := missingReviewPRRules(dir)
		if err != nil {
			return nil, err
		}
		var added []string
		for _, path := range missing {
			if !inContext[path] {
				inContext[path] = true
				added = append(added, path)
			}
		}
		return added, nil
	}
	return prsnapshot.Build(ctx, opts)
}

// missingReviewPRPath follows every component of rel through in-repository
// symlinks and returns the repository-relative path, links substituted, that
// the work tree lacks (rel itself when no link was involved), or "" when the
// chain resolves. Absolute or escaping targets, a file in the middle of the
// path and chains of more than 8 links are left to the engine's own
// confinement.
func missingReviewPRPath(dir, rel string) (string, error) {
	links := 0
	for {
		parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
		prefix := ""
		restart := false
		for i, part := range parts {
			prefix = filepath.Join(prefix, part)
			info, err := os.Lstat(filepath.Join(dir, prefix))
			if errors.Is(err, os.ErrNotExist) {
				return filepath.ToSlash(rel), nil
			}
			if err != nil {
				return "", fmt.Errorf("inspect review-pr rule %q: %w", prefix, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if links == 8 {
					return "", nil
				}
				link, err := os.Readlink(filepath.Join(dir, prefix))
				if err != nil {
					return "", fmt.Errorf("read review-pr rule link %q: %w", prefix, err)
				}
				target := filepath.Join(filepath.Dir(prefix), link)
				if filepath.IsAbs(link) || !filepath.IsLocal(target) {
					return "", nil
				}
				rel = filepath.Join(append([]string{target}, parts[i+1:]...)...)
				links++
				restart = true
				break
			}
			if i < len(parts)-1 && !info.IsDir() {
				return "", nil
			}
		}
		if !restart {
			return "", nil
		}
	}
}

func missingReviewPRRules(dir string) ([]string, error) {
	root, err := pathutil.CanonicalPath(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve review-pr rule root: %w", err)
	}
	ruleFile := filepath.Join(".opencodereview", "rule.json")
	missing, err := missingReviewPRPath(dir, ruleFile)
	if err != nil {
		return nil, err
	}
	if missing == filepath.ToSlash(ruleFile) {
		return nil, nil
	}
	if missing != "" {
		return []string{missing}, nil
	}
	path, err := filepath.EvalSymlinks(filepath.Join(dir, ruleFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve review-pr project rules: %w", err)
	}
	if !pathutil.WithinBase(root, path) {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read review-pr project rules: %w", err)
	}
	var project rules.ProjectRule
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, fmt.Errorf("decode review-pr project rules: %w", err)
	}
	var absent []string
	seen := make(map[string]bool)
	for _, entry := range project.Rules {
		if strings.ContainsAny(entry.Rule, "\n ") || !filepath.IsLocal(entry.Rule) {
			continue
		}
		if !reviewPRRuleExts[strings.ToLower(filepath.Ext(entry.Rule))] {
			continue
		}
		rel := filepath.Clean(entry.Rule)
		if seen[rel] {
			continue
		}
		seen[rel] = true
		path, err := missingReviewPRPath(dir, rel)
		if err != nil {
			return nil, err
		}
		if path != "" {
			absent = append(absent, path)
		}
	}
	return absent, nil
}

func runReviewPRIsolated(ctx context.Context, opts reviewOptions) (retErr error) {
	type envValue struct {
		name, value string
		set         bool
	}
	var saved []envValue
	for _, name := range []string{
		"GIT_NO_LAZY_FETCH", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
		"GIT_PREFIX", "GIT_QUARANTINE_PATH",
	} {
		value, set := os.LookupEnv(name)
		saved = append(saved, envValue{name: name, value: value, set: set})
	}
	defer func() {
		for _, old := range saved {
			var err error
			if old.set {
				err = os.Setenv(old.name, old.value)
			} else {
				err = os.Unsetenv(old.name)
			}
			if err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("restore %s: %w", old.name, err))
			}
		}
	}()
	for _, old := range saved {
		var err error
		if old.name == "GIT_NO_LAZY_FETCH" {
			err = os.Setenv(old.name, "1")
		} else {
			err = os.Unsetenv(old.name)
		}
		if err != nil {
			return fmt.Errorf("isolate review-pr environment %s: %w", old.name, err)
		}
	}
	return runReviewEngine(ctx, opts)
}

// reviewPRRemoteLabel masks the whole user information of the remote URL: a
// token may sit in the password or, as GitHub tokens often do, in the user
// name, and the label is printed to stderr, which listeners keep in their logs.
func reviewPRRemoteLabel(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.User == nil {
		return remote
	}
	u.User = url.User("xxxxx")
	return u.String()
}
