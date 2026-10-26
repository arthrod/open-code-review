// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/flock"
	"github.com/alibaba/open-code-review/internal/prlisten"
	"github.com/alibaba/open-code-review/internal/prsnapshot"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	lsnPRNumber  = 7
	lsnPRRepo    = "acme/book"
	lsnPRHeadSHA = "0123456789abcdef0123456789abcdef01234567"
	lsnPRURL     = "https://github.com/acme/book/pull/7"
	// lsnPC is the path components of the watched repository.
	lsnPC = "github.com/acme/book"
)

// lsnChild describes the fake executable that stands in for the ocr binary.
type lsnChild struct {
	argvPath   string
	runsPath   string
	started    string
	release    string
	pgidPath   string
	stderr     string
	skipOutput bool
	exitCode   int
}

// lsnPull is the pull request the fake lister serves.
func lsnPull() prsnapshot.Pull {
	return prsnapshot.Pull{
		Number:   lsnPRNumber,
		Title:    "Add a chapter",
		Body:     "The body of the pull request.",
		HTMLURL:  lsnPRURL,
		State:    "open",
		HeadSHA:  lsnPRHeadSHA,
		BaseSHA:  strings.Repeat("b", 40),
		BaseRef:  "main",
		CloneURL: "https://github.com/acme/book.git",
	}
}

// lsnFakeLister serves a fixed list of pull requests for every repository.
type lsnFakeLister struct {
	pulls []prsnapshot.Pull
}

func (l lsnFakeLister) ListOpenPulls(_ context.Context, _, _ string) ([]prsnapshot.Pull, error) {
	return l.pulls, nil
}

// lsnSkipOnWindows skips the tests that need a POSIX shell script.
func lsnSkipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake child is a POSIX shell script")
	}
}

// lsnShellQuote quotes s so a POSIX shell treats it as one literal word.
func lsnShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// lsnFakeChild writes the shell script that stands in for the ocr binary: it
// records its arguments, waits for a release file when one is given, writes
// an empty JSON result to the path given after --output and exits.
func lsnFakeChild(t *testing.T, dir string, child lsnChild) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if child.runsPath != "" {
		b.WriteString("printf 'run\\n' >> " + lsnShellQuote(child.runsPath) + "\n")
	}
	if child.argvPath != "" {
		b.WriteString(": > " + lsnShellQuote(child.argvPath) + "\n")
		b.WriteString("for arg in \"$@\"; do printf '%s\\n' \"$arg\" >> " + lsnShellQuote(child.argvPath) + "; done\n")
	}
	b.WriteString("prev=''\nout=''\nsuccess=''\n")
	b.WriteString("for arg in \"$@\"; do\n")
	b.WriteString("  if [ \"$prev\" = '--output' ]; then out=\"$arg\"; fi\n")
	b.WriteString("  if [ \"$prev\" = '--success-file' ]; then success=\"$arg\"; fi\n")
	b.WriteString("  prev=\"$arg\"\n")
	b.WriteString("done\n")
	if child.pgidPath != "" {
		b.WriteString("printf '%s ' \"$$\" > " + lsnShellQuote(child.pgidPath) + "\n")
		b.WriteString("OCR_LSN_FAKE_PGID=1 GORACE=atexit_sleep_ms=0 " + lsnShellQuote(os.Args[0]) + " >> " + lsnShellQuote(child.pgidPath) + "\n")
	}
	if child.started != "" {
		b.WriteString(": > " + lsnShellQuote(child.started) + "\n")
	}
	if child.release != "" {
		b.WriteString("while [ ! -f " + lsnShellQuote(child.release) + " ]; do sleep 0.05; done\n")
	}
	if child.stderr != "" {
		b.WriteString("printf '%s\\n' " + lsnShellQuote(child.stderr) + " >&2\n")
	}
	if !child.skipOutput {
		b.WriteString("if [ -n \"$out\" ]; then\n")
		b.WriteString("  if ! printf '{}' > \"$out\"; then\n")
		b.WriteString("    printf 'fake child: cannot write %s\\n' \"$out\" >&2\n")
		b.WriteString("    exit 3\n")
		b.WriteString("  fi\n")
		b.WriteString("fi\n")
		if child.exitCode == 0 {
			b.WriteString("if [ -n \"$success\" ]; then : > \"$success\"; fi\n")
		}
	}
	b.WriteString("exit " + strconv.Itoa(child.exitCode) + "\n")
	path := filepath.Join(dir, "fake-ocr")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write the fake child: %v", err)
	}
	return path
}

// lsnInstallSeams points the listener at the fake child and the fake lister
// and restores the package level seams when the test ends.
func lsnInstallSeams(t *testing.T, exe, token string, pulls []prsnapshot.Pull) {
	t.Helper()
	oldExecutable, oldLister, oldToken := listenExecutable, newPullLister, resolveListenToken
	t.Cleanup(func() {
		listenExecutable, newPullLister, resolveListenToken = oldExecutable, oldLister, oldToken
	})
	listenExecutable = func() (string, error) { return exe, nil }
	newPullLister = func(_, _ string) prlisten.Lister { return lsnFakeLister{pulls: pulls} }
	resolveListenToken = func(context.Context, string) string { return token }
}

// lsnListenCmd builds a listen command with fresh options.
func lsnListenCmd(args ...string) *cobra.Command {
	opts := listenOptions{}
	cmd := newListenCmd(&opts)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}

// lsnRunListen runs the listen command with the given arguments.
func lsnRunListen(ctx context.Context, t *testing.T, args ...string) error {
	t.Helper()
	return lsnListenCmd(args...).ExecuteContext(ctx)
}

// lsnRequireError fails unless err is a real error mentioning one of the
// fragments. The placeholder error of the stub must not pass as an error.
func lsnRequireError(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got none")
	}
	if strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("the command still returns the placeholder error: %v", err)
	}
	for _, fragment := range fragments {
		if strings.Contains(err.Error(), fragment) {
			return
		}
	}
	t.Fatalf("error %q mentions none of %v", err.Error(), fragments)
}

// lsnRequireSuccess fails unless the command succeeded.
func lsnRequireSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the command failed: %v", err)
	}
}

// lsnReadArgs returns the arguments the fake child recorded, one per line.
func lsnReadArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the recorded arguments: %v", err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// lsnCountRuns returns how often the fake child started.
func lsnCountRuns(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the run counter: %v", err)
	}
	return len(strings.Fields(string(data)))
}

// lsnParseReviewPR parses args with a fresh review-pr command and returns the
// options it bound plus the names of the flags that were set.
func lsnParseReviewPR(t *testing.T, args []string) (reviewPROptions, []string) {
	t.Helper()
	opts := reviewPROptions{}
	cmd := newReviewPRCmd(&opts)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("the listener's arguments do not parse with review-pr: %v (%v)", err, args)
	}
	var changed []string
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		if flag.Changed {
			changed = append(changed, flag.Name)
		}
	})
	return opts, changed
}

// lsnResultExt maps an output format to the extension of the result file.
func lsnResultExt(format string) string {
	switch format {
	case "text":
		return ".txt"
	case "sarif":
		return ".sarif"
	default:
		return ".json"
	}
}

// lsnJobScope checks the work directory and the result path against the
// documented layout.
func lsnJobScope(t *testing.T, dir string, opts reviewPROptions, format string) {
	t.Helper()
	work := opts.workdir
	if work == "" {
		t.Fatal("the listener did not pass a work directory")
	}
	wantParent := filepath.Join(dir, "ocr-listen", "work", filepath.FromSlash(lsnPC))
	if filepath.Dir(work) != wantParent {
		t.Errorf("work directory %q is not below %q", work, wantParent)
	}
	matches := regexp.MustCompile(`^pr-7-([0-9a-f]{12})-([0-9a-f]{12})$`).FindStringSubmatch(filepath.Base(work))
	if matches == nil {
		t.Fatalf("work directory %q does not name the pull request, the head and the scope", work)
	}
	if matches[1] != lsnPRHeadSHA[:12] {
		t.Errorf("work directory %q does not carry the first 12 characters of the head commit", work)
	}
	scope := matches[2]
	wantResult := filepath.Join(dir, "ocr-listen", "reviews", filepath.FromSlash(lsnPC),
		"pr-7-"+matches[1]+"-"+scope+lsnResultExt(format))
	if opts.review.outputPath != wantResult {
		t.Errorf("result path %q, want %q", opts.review.outputPath, wantResult)
	}
}

// lsnFindStateFile returns the path of the first file below root whose parent
// directory is named parent and whose name starts with prefix.
func lsnFindStateFile(t *testing.T, root, parent, prefix string) string {
	t.Helper()
	found := lsnSearchStateFile(t, root, parent, prefix)
	if found == "" {
		t.Fatalf("no %s file starting with %q below %s", parent, prefix, root)
	}
	return found
}

// lsnSearchStateFile looks for a state file; a missing root is a miss, so
// absence can be asserted, but any other walk error fails the test.
func lsnSearchStateFile(t *testing.T, root, parent, prefix string) string {
	t.Helper()
	return lsnWalkFirst(t, root, func(path string) bool {
		return filepath.Base(filepath.Dir(path)) == parent && strings.HasPrefix(filepath.Base(path), prefix)
	})
}

// lsnFindFile returns the path of the first regular file below root.
func lsnFindFile(t *testing.T, root string) string {
	t.Helper()
	found := lsnWalkFirst(t, root, func(string) bool { return true })
	if found == "" {
		t.Fatalf("no file below %s", root)
	}
	return found
}

func lsnWalkFirst(t *testing.T, root string, match func(path string) bool) string {
	t.Helper()
	found := ""
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if entry.IsDir() || !match(path) {
			return nil
		}
		found = path
		return fs.SkipAll
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// lsnWaitForStart waits until the fake child started or the helper listener
// died, whichever comes first.
func lsnWaitForStart(path string, done <-chan struct{}, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-done:
			_, err := os.Stat(path)
			return err == nil
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// lsnCaptureStderr redirects os.Stderr into a file while fn runs and returns
// everything that was written to it.
func lsnCaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create the stderr file: %v", err)
	}
	original := os.Stderr
	os.Stderr = file
	defer func() { os.Stderr = original }()
	fn()
	os.Stderr = original
	if err := file.Close(); err != nil {
		t.Fatalf("close the stderr file: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the stderr file: %v", err)
	}
	return string(data)
}

// lsnParseEnv turns the output of "env" into a map.
func lsnParseEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the hook environment: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("the hook environment has the malformed line %q", line)
		}
		env[name] = value
	}
	return env
}

func TestLsnRejectsPositionalArgumentBeforeDash(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once",
		"stray",
		"--", "--effort", "high")
	lsnRequireError(t, err, "stray", "positional", "argument")
	if _, statErr := os.Stat(argvPath); statErr == nil {
		t.Error("the child ran although the positional argument was rejected")
	}
}

func TestLsnRejectsPositionalArgumentWithoutDash(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once",
		"stray")
	lsnRequireError(t, err, "stray", "positional", "argument")
	if _, statErr := os.Stat(argvPath); statErr == nil {
		t.Error("the child ran although the positional argument was rejected")
	}
}

func TestLsnAcceptsEmptyDash(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath, runsPath: filepath.Join(tmp, "runs.txt")})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once",
		"--")
	lsnRequireSuccess(t, err)
	if got := lsnCountRuns(t, filepath.Join(tmp, "runs.txt")); got != 1 {
		t.Errorf("the child ran %d times, want 1", got)
	}
}

func TestLsnRejectsReservedFlagsAfterDash(t *testing.T) {
	lsnSkipOnWindows(t)
	cases := []struct {
		name      string
		extra     []string
		fragments []string
	}{
		{"output", []string{"--output", "out.json"}, []string{"output"}},
		{"output-equals", []string{"--output=out.json"}, []string{"output"}},
		{"output-shorthand", []string{"-o", "out.json"}, []string{"output"}},
		{"output-shorthand-attached", []string{"-oother.json"}, []string{"output"}},
		{"workdir", []string{"--workdir", "wd"}, []string{"workdir"}},
		{"head", []string{"--head", "abc"}, []string{"head"}},
		{"base", []string{"--base", "abc"}, []string{"base"}},
		{"remote", []string{"--remote", "https://example.invalid/x.git"}, []string{"remote"}},
		{"cleanup", []string{"--cleanup"}, []string{"cleanup"}},
		{"format", []string{"--format", "text"}, []string{"format"}},
		{"format-equals", []string{"--format=json"}, []string{"format"}},
		{"format-shorthand", []string{"-f", "text"}, []string{"format"}},
		{"format-shorthand-attached", []string{"-ftext"}, []string{"format"}},
		{"audience", []string{"--audience", "human"}, []string{"audience"}},
		{"repo", []string{"--repo", "."}, []string{"repo"}},
		{"from", []string{"--from", "main"}, []string{"from"}},
		{"to", []string{"--to", "head"}, []string{"to"}},
		{"commit", []string{"--commit", "abc"}, []string{"commit"}},
		{"commit-shorthand", []string{"-c", "abc"}, []string{"commit"}},
		{"preview", []string{"--preview"}, []string{"preview"}},
		{"preview-shorthand", []string{"-p"}, []string{"preview"}},
		{"preview-shorthand-attached", []string{"-poother.json"}, []string{"output", "preview"}},
		{"resume", []string{"--resume", "session"}, []string{"resume"}},
		{"help", []string{"--help"}, []string{"help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			argvPath := filepath.Join(tmp, "argv.txt")
			exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
			lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

			args := append([]string{
				"--github-repo", lsnPRRepo,
				"--dir", filepath.Join(tmp, "dir"),
				"--state-dir", filepath.Join(tmp, "state"),
				"--once",
				"--",
			}, tc.extra...)
			err := lsnRunListen(context.Background(), t, args...)
			lsnRequireError(t, err, tc.fragments...)
			if _, statErr := os.Stat(argvPath); statErr == nil {
				t.Error("the child ran although the reserved flag was rejected")
			}
		})
	}
}

func TestLsnRejectsPositionalExtraAfterDash(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once",
		"--", "stray")
	lsnRequireError(t, err, "stray", "positional", "argument")
	if _, statErr := os.Stat(argvPath); statErr == nil {
		t.Error("the child ran although the positional extra was rejected")
	}
}

func TestLsnAcceptsExtrasReviewPrAccepts(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once",
		"--", "--effort", "high", "--provider", "p")
	lsnRequireSuccess(t, err)

	args := lsnReadArgs(t, argvPath)
	if len(args) < 2 || args[0] != "review-pr" {
		t.Fatalf("the child was called with %v, want it to start with review-pr", args)
	}
	opts, changed := lsnParseReviewPR(t, args[1:])
	if opts.review.effort != "high" {
		t.Errorf("the listener did not pass --effort high on: %v", args)
	}
	if opts.review.provider != "p" {
		t.Errorf("the listener did not pass --provider p on: %v", args)
	}
	if !lsnHasFlag(changed, "effort") || !lsnHasFlag(changed, "provider") {
		t.Errorf("the listener set the flags %v, want effort and provider among them", changed)
	}
}

// lsnHasFlag reports whether the changed flag names contain name.
func lsnHasFlag(changed []string, name string) bool {
	for _, flag := range changed {
		if flag == name {
			return true
		}
	}
	return false
}

func TestLsnRejectsInvalidFormat(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: filepath.Join(tmp, "argv.txt")})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--format", "xml",
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireError(t, err, "format")
}

func TestLsnRequiresGithubRepo(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: filepath.Join(tmp, "argv.txt")})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireError(t, err, "github-repo")
}

func TestLsnRejectsInvalidRepoRef(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: filepath.Join(tmp, "argv.txt")})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", "acme",
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireError(t, err, "acme", "github-repo")
}

func TestLsnRejectsGithubApiWithSeveralHosts(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: filepath.Join(tmp, "argv.txt")})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo+",ghe.example.com/other/repo",
		"--github-api", "https://api.example.invalid",
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireError(t, err, "github-api", "host")
}

func TestLsnReviewsOpenPullRequestOnce(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	runsPath := filepath.Join(tmp, "runs.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath, runsPath: runsPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	dir := filepath.Join(tmp, "dir")
	stateDir := filepath.Join(tmp, "state")

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", dir,
		"--state-dir", stateDir,
		"--once")
	lsnRequireSuccess(t, err)

	if got := lsnCountRuns(t, runsPath); got != 1 {
		t.Errorf("the child ran %d times, want 1", got)
	}
	args := lsnReadArgs(t, argvPath)
	if len(args) < 2 {
		t.Fatalf("the child was called with %v, want the pull request reference", args)
	}
	if args[0] != "review-pr" {
		t.Errorf("the child was called with %q first, want review-pr", args[0])
	}
	if args[1] != "acme/book#7" {
		t.Errorf("the child was called with %q, want acme/book#7", args[1])
	}
	opts, changed := lsnParseReviewPR(t, args[1:])
	if opts.head != lsnPRHeadSHA {
		t.Errorf("the head commit %q, want %q", opts.head, lsnPRHeadSHA)
	}
	if opts.review.audience != "agent" {
		t.Errorf("the audience %q, want agent", opts.review.audience)
	}
	if opts.review.outputFormat != "json" {
		t.Errorf("the output format %q, want json", opts.review.outputFormat)
	}
	if !opts.cleanup {
		t.Error("the listener did not ask for --cleanup")
	}
	want := []string{"audience", "cleanup", "format", "head", "output", "success-file", "workdir"}
	if strings.Join(changed, ",") != strings.Join(want, ",") {
		t.Errorf("the listener set the flags %v, want %v", changed, want)
	}
	if opts.successFile != opts.workdir+".ok" {
		t.Errorf("the success file %q, want %q", opts.successFile, opts.workdir+".ok")
	}
	lsnJobScope(t, dir, opts, "json")

	result, err := os.ReadFile(opts.review.outputPath)
	if err != nil {
		t.Fatalf("read the result file: %v", err)
	}
	if len(result) == 0 {
		t.Error("the result file is empty")
	}
	done := lsnFindStateFile(t, stateDir, "done", "pr-7-")
	if !strings.Contains(filepath.Base(done), lsnPRHeadSHA) {
		t.Errorf("the done marker %q does not name the head commit %q", done, lsnPRHeadSHA)
	}
}

func TestLsnChildArgumentsParseWithReviewPr(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	dir := filepath.Join(tmp, "dir")

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", dir,
		"--state-dir", filepath.Join(tmp, "state"),
		"--path", "chapters/07",
		"--format", "sarif",
		"--once")
	lsnRequireSuccess(t, err)

	args := lsnReadArgs(t, argvPath)
	if len(args) < 2 {
		t.Fatalf("the child was called with %v, want the pull request reference", args)
	}
	opts, changed := lsnParseReviewPR(t, args[1:])
	if opts.head != lsnPRHeadSHA {
		t.Errorf("the head commit %q, want %q", opts.head, lsnPRHeadSHA)
	}
	if opts.review.outputFormat != "sarif" {
		t.Errorf("the output format %q, want sarif", opts.review.outputFormat)
	}
	if opts.paths != "chapters/07" {
		t.Errorf("the path filter %q, want chapters/07", opts.paths)
	}
	if opts.review.outputPath == "" {
		t.Error("the listener did not pass an output path")
	}
	lsnJobScope(t, dir, opts, "sarif")
	if !lsnHasFlag(changed, "path") {
		t.Errorf("the listener set the flags %v, want path among them", changed)
	}
	if _, statErr := os.Stat(opts.review.outputPath); statErr != nil {
		t.Errorf("the result file %q was not written: %v", opts.review.outputPath, statErr)
	}
}

func TestLsnKeepWorkOmitsCleanup(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--keep-work",
		"--once")
	lsnRequireSuccess(t, err)

	args := lsnReadArgs(t, argvPath)
	opts, changed := lsnParseReviewPR(t, args[1:])
	if opts.cleanup {
		t.Error("the listener asked for --cleanup although --keep-work was set")
	}
	if lsnHasFlag(changed, "cleanup") {
		t.Errorf("the listener set the flags %v, want no cleanup among them", changed)
	}
}

func TestLsnOnReviewHookSeesJobEnvironment(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	hookEnv := filepath.Join(tmp, "hook-env.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	dir := filepath.Join(tmp, "dir")

	hook := "env | grep '^OCR_LISTEN_' | sort > " + lsnShellQuote(hookEnv) + " || true"
	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", dir,
		"--state-dir", filepath.Join(tmp, "state"),
		"--on-review", hook,
		"--once")
	lsnRequireSuccess(t, err)

	env := lsnParseEnv(t, hookEnv)
	want := map[string]string{
		"OCR_LISTEN_REPO":     "acme/book",
		"OCR_LISTEN_PR":       "7",
		"OCR_LISTEN_PR_URL":   lsnPRURL,
		"OCR_LISTEN_HEAD_SHA": lsnPRHeadSHA,
		"OCR_LISTEN_FORMAT":   "json",
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("%s = %q, want %q", name, env[name], value)
		}
	}
	args := lsnReadArgs(t, argvPath)
	opts, _ := lsnParseReviewPR(t, args[1:])
	if env["OCR_LISTEN_RESULT"] != opts.review.outputPath {
		t.Errorf("OCR_LISTEN_RESULT = %q, want %q", env["OCR_LISTEN_RESULT"], opts.review.outputPath)
	}
	if env["OCR_LISTEN_LOG"] == "" {
		t.Error("OCR_LISTEN_LOG is empty, want the log file of the job")
	}
	if _, statErr := os.Stat(env["OCR_LISTEN_LOG"]); statErr != nil {
		t.Errorf("the log file %q does not exist: %v", env["OCR_LISTEN_LOG"], statErr)
	}
}

func TestLsnFailingChildRecordsAttemptsAndLog(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{
		argvPath: argvPath,
		stderr:   "child failed: boom",
		exitCode: 1,
	})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	dir := filepath.Join(tmp, "dir")
	stateDir := filepath.Join(tmp, "state")

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", dir,
		"--state-dir", stateDir,
		"--once")
	// Cron and CI read the exit status, so a failed review must fail --once.
	lsnRequireError(t, err, "1 failed reviews")

	attempts := lsnFindStateFile(t, stateDir, "attempts", "pr-7-")
	data, readErr := os.ReadFile(attempts)
	if readErr != nil {
		t.Fatalf("read the attempts counter: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != "1" {
		t.Errorf("the attempts counter holds %q, want 1", strings.TrimSpace(string(data)))
	}
	if done := lsnSearchStateFile(t, stateDir, "done", "pr-7-"); done != "" {
		t.Errorf("the failed review left the done marker %q behind", done)
	}
	logPath := lsnFindFile(t, filepath.Join(dir, "ocr-listen", "logs"))
	log, logErr := os.ReadFile(logPath)
	if logErr != nil {
		t.Fatalf("read the log file: %v", logErr)
	}
	if !strings.Contains(string(log), "child failed: boom") {
		t.Errorf("the log file %q does not hold the stderr of the child:\n%s", logPath, log)
	}
}

func TestLsnChildWithoutResultFileCountsAsFailed(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath, skipOutput: true})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	stateDir := filepath.Join(tmp, "state")

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", stateDir,
		"--once")
	if err != nil && strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("the command still returns the placeholder error: %v", err)
	}

	attempts := lsnFindStateFile(t, stateDir, "attempts", "pr-7-")
	data, readErr := os.ReadFile(attempts)
	if readErr != nil {
		t.Fatalf("read the attempts counter: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != "1" {
		t.Errorf("the attempts counter holds %q, want 1", strings.TrimSpace(string(data)))
	}
	if done := lsnSearchStateFile(t, stateDir, "done", "pr-7-"); done != "" {
		t.Errorf("the review without a result file left the done marker %q behind", done)
	}
}

func TestLsnPassesGithubApiToTheChild(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--github-api", "https://api.example.invalid",
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireSuccess(t, err)

	args := lsnReadArgs(t, argvPath)
	opts, changed := lsnParseReviewPR(t, args[1:])
	if opts.githubAPI != "https://api.example.invalid" {
		t.Errorf("the child got the api root %q, want https://api.example.invalid", opts.githubAPI)
	}
	if !lsnHasFlag(changed, "github-api") {
		t.Errorf("the listener set the flags %v, want github-api among them", changed)
	}
}

func TestLsnListenCommandIsRegistered(t *testing.T) {
	found, _, err := rootCmd.Find([]string{"listen"})
	if err != nil {
		t.Fatalf("find the listen command: %v", err)
	}
	if found.Name() != "listen" {
		t.Fatalf("the registered command is %q, want listen", found.Name())
	}
	if found.Hidden {
		t.Error("the listen command is hidden")
	}
}

func TestLsnWarnsWithoutToken(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: filepath.Join(tmp, "argv.txt")})
	lsnInstallSeams(t, exe, "", []prsnapshot.Pull{lsnPull()})

	var runErr error
	stderr := lsnCaptureStderr(t, func() {
		runErr = lsnRunListen(context.Background(), t,
			"--github-repo", lsnPRRepo,
			"--dir", filepath.Join(tmp, "dir"),
			"--state-dir", filepath.Join(tmp, "state"),
			"--once")
	})
	lsnRequireSuccess(t, runErr)
	if !strings.Contains(strings.ToLower(stderr), "token") {
		t.Errorf("no warning about the missing token on stderr:\n%s", stderr)
	}
}

func TestLsnChildRunsInItsOwnProcessGroup(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	pgidPath := filepath.Join(tmp, "pgid.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath, pgidPath: pgidPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireSuccess(t, err)

	data, readErr := os.ReadFile(pgidPath)
	if readErr != nil {
		t.Fatalf("read the process group of the child: %v", readErr)
	}
	matches := regexp.MustCompile(`(\d+)\s+(\d+)`).FindStringSubmatch(string(data))
	if matches == nil {
		t.Fatalf("the child did not report its process group: %q", string(data))
	}
	pid, _ := strconv.Atoi(matches[1])
	pgid, _ := strconv.Atoi(matches[2])
	if pid != pgid {
		t.Errorf("the child runs in process group %d, want its own group %d", pgid, pid)
	}
}

func TestLsnCancelInterruptsTheChildOnceAndKeepsItsAttempts(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	started, signals := filepath.Join(tmp, "started"), filepath.Join(tmp, "signals.txt")
	// The child sits in its own process group, so the only interrupt it can
	// receive is the one the listener sends when it is cancelled.
	script := "#!/bin/sh\n" +
		"trap 'printf \"int\\n\" >> " + lsnShellQuote(signals) + "; exit 130' INT\n" +
		": > " + lsnShellQuote(started) + "\n" +
		"while :; do sleep 0.05; done\n"
	exe := filepath.Join(tmp, "fake-ocr")
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake child: %v", err)
	}
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	stateDir := filepath.Join(tmp, "state")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	finished := make(chan error, 1)
	go func() {
		finished <- lsnRunListen(ctx, t, "--github-repo", lsnPRRepo, "--dir", filepath.Join(tmp, "dir"), "--state-dir", stateDir)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the review child never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-finished:
		lsnRequireSuccess(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the listener did not stop after cancellation")
	}
	data, err := os.ReadFile(signals)
	if err != nil {
		t.Fatalf("the child was never interrupted: %v", err)
	}
	if got := strings.Count(string(data), "int"); got != 1 {
		t.Errorf("the child received %d interrupts, want exactly 1", got)
	}
	attempts, err := filepath.Glob(filepath.Join(stateDir, "*", "*", "*", "attempts", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Errorf("an interrupted review used up an attempt: %v", attempts)
	}
}

func TestLsnDoesNotReviewTheSameHeadTwice(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	runsPath := filepath.Join(tmp, "runs.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath, runsPath: runsPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	stateDir := filepath.Join(tmp, "state")
	args := []string{
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", stateDir,
		"--once",
	}

	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, args...))
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, args...))
	if got := lsnCountRuns(t, runsPath); got != 1 {
		t.Errorf("the child ran %d times, want 1: a reviewed head commit is not reviewed again", got)
	}
}

func TestLsnDefaultStateDirIsUnderHome(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create the home directory: %v", err)
	}
	setTestHome(t, home)
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: filepath.Join(tmp, "argv.txt")})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--once")
	lsnRequireSuccess(t, err)

	defaultState := filepath.Join(home, ".opencodereview", "listen")
	done := lsnFindStateFile(t, defaultState, "done", "pr-7-")
	if done == "" {
		t.Fatalf("no done marker below %s", defaultState)
	}
}

// lsnHelperEnv makes the test binary act as the listener of the claim lock
// test, so that test can kill it while its child is still running.
const lsnHelperEnv = "OCR_LSN_LISTENER_HELPER"

// TestLsnHelperListener runs a listener in a separate process. It is skipped
// unless the claim lock test starts it as a helper.
func TestLsnHelperListener(t *testing.T) {
	if os.Getenv(lsnHelperEnv) != "1" {
		t.Skip("helper process of the claim lock test")
	}
	exe := os.Getenv("OCR_LSN_EXE")
	if exe == "" {
		t.Fatal("the helper was started without OCR_LSN_EXE")
	}
	lsnInstallSeams(t, exe, os.Getenv("OCR_LSN_TOKEN"), []prsnapshot.Pull{lsnPull()})
	args := []string{}
	for _, arg := range strings.Split(os.Getenv("OCR_LSN_ARGS"), "\x1f") {
		if arg != "" {
			args = append(args, arg)
		}
	}
	if err := lsnRunListen(context.Background(), t, args...); err != nil {
		t.Fatalf("the helper listener failed: %v", err)
	}
}

func TestLsnClaimLockSurvivesTheDeathOfTheListener(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	started := filepath.Join(tmp, "started")
	release := filepath.Join(tmp, "release")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath, started: started, release: release})
	stateDir := filepath.Join(tmp, "state")
	args := []string{
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", stateDir,
		"--once",
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestLsnHelperListener$")
	helper.Env = append(os.Environ(),
		lsnHelperEnv+"=1",
		"OCR_LSN_EXE="+exe,
		"OCR_LSN_TOKEN=token",
		"OCR_LSN_ARGS="+strings.Join(args, "\x1f"),
		"HOME="+tmp,
		"USERPROFILE="+tmp,
	)
	var output bytes.Buffer
	helper.Stdout = &output
	helper.Stderr = &output
	if err := helper.Start(); err != nil {
		t.Fatalf("start the helper listener: %v", err)
	}
	helperDone := make(chan struct{})
	go func() {
		_ = helper.Wait()
		close(helperDone)
	}()

	if !lsnWaitForStart(started, helperDone, 30*time.Second) {
		t.Fatalf("the child never started; the helper listener wrote:\n%s", output.String())
	}

	lockPath := lsnFindStateFile(t, stateDir, "locks", "pr-7-")
	if _, acquired, err := flock.TryLock(lockPath); err != nil {
		t.Fatalf("try to lock %q: %v", lockPath, err)
	} else if acquired {
		t.Fatalf("the claim %q was not held while the review ran", lockPath)
	}

	// The listener dies without unlocking; the child keeps the claim, because
	// the lock belongs to the file description the child inherited.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill the helper listener: %v", err)
	}
	<-helperDone

	if _, acquired, err := flock.TryLock(lockPath); err != nil {
		t.Fatalf("try to lock %q: %v", lockPath, err)
	} else if acquired {
		t.Fatalf("the claim %q was released when the listener died", lockPath)
	}

	if err := os.WriteFile(release, []byte("go\n"), 0o644); err != nil {
		t.Fatalf("release the child: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, acquired, err := flock.TryLock(lockPath)
		if err != nil {
			t.Fatalf("try to lock %q: %v", lockPath, err)
		}
		if acquired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the claim %q was not released after the child exited", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLsnReviewJobUsesThePullRequestReference(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})

	err := lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"),
		"--state-dir", filepath.Join(tmp, "state"),
		"--once")
	lsnRequireSuccess(t, err)

	args := lsnReadArgs(t, argvPath)
	if len(args) < 2 {
		t.Fatalf("the child was called with %v, want the pull request reference", args)
	}
	if args[1] != fmt.Sprintf("%s#%d", lsnPRRepo, lsnPRNumber) {
		t.Errorf("the child was called with %q, want %s#%d", args[1], lsnPRRepo, lsnPRNumber)
	}
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == "--lsn-hook" && os.Getenv("OCR_LSN_FAKE_CHILD") == "1" {
		if path := os.Getenv("OCR_LSN_HOOK_STARTED"); path != "" {
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
		}
		delay, _ := time.ParseDuration(os.Getenv("OCR_LSN_HOOK_SLEEP"))
		time.Sleep(delay)
		if path := os.Getenv("OCR_LSN_HOOK_RESULT"); path != "" {
			if err := os.WriteFile(path, []byte("hook finished"), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	if os.Getenv("OCR_LSN_FAKE_CHILD") != "1" {
		return
	}
	if path := os.Getenv("OCR_LSN_CHILD_STARTED"); path != "" {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	delay, _ := time.ParseDuration(os.Getenv("OCR_LSN_CHILD_SLEEP"))
	time.Sleep(delay)
	if message := os.Getenv("OCR_LSN_CHILD_STDERR"); message != "" {
		fmt.Fprintln(os.Stderr, message)
	}
	if code, _ := strconv.Atoi(os.Getenv("OCR_LSN_CHILD_EXIT")); code != 0 {
		os.Exit(code)
	}
	output, success := "", ""
	for i := 1; i+1 < len(os.Args); i++ {
		switch os.Args[i] {
		case "--output":
			output = os.Args[i+1]
		case "--success-file":
			success = os.Args[i+1]
		}
	}
	if output == "" {
		os.Exit(2)
	}
	for path, content := range map[string]string{output: "{}", success: ""} {
		if path == "" {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func TestLsnChildClaimConfiguration(t *testing.T) {
	claim, err := os.CreateTemp(t.TempDir(), "claim")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	cmd := exec.CommandContext(context.Background(), os.Args[0])
	configureListenChild(cmd, claim)
	if runtime.GOOS == "windows" {
		if len(cmd.ExtraFiles) != 0 {
			t.Fatal("Windows child must not inherit unsupported ExtraFiles")
		}
	} else if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != claim {
		t.Fatal("Unix child must inherit the claim")
	}
	if cmd.Cancel == nil {
		t.Fatal("child configuration must install cancellation")
	}
}

func TestLsnReexecutedChildWritesResultAndDone(t *testing.T) {
	lsnInstallReexecutedChild(t)
	tmp := t.TempDir()
	dir, stateDir := filepath.Join(tmp, "dir"), filepath.Join(tmp, "state")
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t,
		"--github-repo", lsnPRRepo, "--dir", dir, "--state-dir", stateDir, "--once"))
	result := lsnFindFile(t, filepath.Join(dir, "ocr-listen", "reviews"))
	data, err := os.ReadFile(result)
	if err != nil || string(data) != "{}" {
		t.Fatalf("review result = %q, %v; want {}", data, err)
	}
	lsnFindStateFile(t, stateDir, "done", "pr-7-")
}

func lsnInstallReexecutedChild(t *testing.T) {
	t.Helper()
	t.Setenv("OCR_LSN_FAKE_CHILD", "1")
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	lsnInstallSeams(t, os.Args[0], "token", []prsnapshot.Pull{lsnPull()})
}

func lsnReexecutedHook() string {
	if runtime.GOOS == "windows" {
		return `"` + os.Args[0] + `" --lsn-hook`
	}
	return "exec " + lsnShellQuote(os.Args[0]) + " --lsn-hook"
}

func TestLsnReviewTimeoutDefault(t *testing.T) {
	cmd := lsnListenCmd()
	timeout, err := cmd.Flags().GetDuration("review-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if timeout != time.Hour {
		t.Fatalf("review timeout = %s, want 1h", timeout)
	}
}

func TestLsnReviewTimeoutRecordsFailedAttempt(t *testing.T) {
	lsnInstallReexecutedChild(t)
	t.Setenv("OCR_LSN_CHILD_SLEEP", "4s")
	tmp := t.TempDir()
	started := filepath.Join(tmp, "child-started")
	t.Setenv("OCR_LSN_CHILD_STARTED", started)
	dir, stateDir := filepath.Join(tmp, "dir"), filepath.Join(tmp, "state")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	_ = lsnRunListen(ctx, t, "--github-repo", lsnPRRepo,
		"--dir", dir, "--state-dir", stateDir, "--review-timeout", "300ms", "--once")
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("timed-out review took %s, want under 2s for a 4s child", elapsed)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("review child never started: %v", err)
	}
	attempts := lsnFindStateFile(t, stateDir, "attempts", "pr-7-")
	data, err := os.ReadFile(attempts)
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("attempts = %q, %v; want 1", data, err)
	}
	if done := lsnSearchStateFile(t, stateDir, "done", "pr-7-"); done != "" {
		t.Fatalf("timed-out review left a done record: %s", done)
	}
}

func TestLsnHookHasItsOwnReviewTimeout(t *testing.T) {
	lsnInstallReexecutedChild(t)
	t.Setenv("OCR_LSN_CHILD_SLEEP", "100ms")
	t.Setenv("OCR_LSN_HOOK_SLEEP", "4s")
	tmp := t.TempDir()
	dir, stateDir := filepath.Join(tmp, "dir"), filepath.Join(tmp, "state")
	started, result := filepath.Join(tmp, "hook-started"), filepath.Join(tmp, "hook-result")
	t.Setenv("OCR_LSN_HOOK_STARTED", started)
	t.Setenv("OCR_LSN_HOOK_RESULT", result)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	lsnRequireSuccess(t, lsnRunListen(ctx, t, "--github-repo", lsnPRRepo,
		"--dir", dir, "--state-dir", stateDir, "--review-timeout", "300ms",
		"--on-review", lsnReexecutedHook(), "--once"))
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Errorf("timed-out hook took %s, want under 2s for a 4s hook", elapsed)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("hook never started: %v", err)
	}
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatalf("timed-out hook wrote a result, stat error = %v", err)
	}
	lsnFindStateFile(t, stateDir, "done", "pr-7-")
}

func TestLsnReviewTimeoutZeroAllowsCompletion(t *testing.T) {
	lsnInstallReexecutedChild(t)
	t.Setenv("OCR_LSN_CHILD_SLEEP", "400ms")
	t.Setenv("OCR_LSN_HOOK_SLEEP", "400ms")
	tmp := t.TempDir()
	stateDir, result := filepath.Join(tmp, "state"), filepath.Join(tmp, "hook-result")
	t.Setenv("OCR_LSN_HOOK_RESULT", result)
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"), "--state-dir", stateDir, "--review-timeout", "0",
		"--on-review", lsnReexecutedHook(), "--once"))
	lsnFindStateFile(t, stateDir, "done", "pr-7-")
	if _, err := os.Stat(result); err != nil {
		t.Fatalf("unlimited hook did not finish: %v", err)
	}
}

func TestLsnReviewAndHookHaveSeparateTimeoutBudgets(t *testing.T) {
	lsnInstallReexecutedChild(t)
	t.Setenv("OCR_LSN_CHILD_SLEEP", "400ms")
	t.Setenv("OCR_LSN_HOOK_SLEEP", "400ms")
	tmp := t.TempDir()
	result := filepath.Join(tmp, "hook-result")
	t.Setenv("OCR_LSN_HOOK_RESULT", result)
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"), "--state-dir", filepath.Join(tmp, "state"),
		"--review-timeout", "700ms", "--on-review", lsnReexecutedHook(), "--once"))
	if _, err := os.Stat(result); err != nil {
		t.Fatalf("hook did not get its own timeout budget: %v", err)
	}
}

func TestLsnHookSurvivesListenerCancellation(t *testing.T) {
	lsnInstallReexecutedChild(t)
	t.Setenv("OCR_LSN_HOOK_SLEEP", "400ms")
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	started, result := filepath.Join(tmp, "hook-started"), filepath.Join(tmp, "hook-result")
	t.Setenv("OCR_LSN_HOOK_STARTED", started)
	t.Setenv("OCR_LSN_HOOK_RESULT", result)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	finished, stopped := make(chan error, 1), make(chan struct{})
	go func() {
		finished <- lsnRunListen(ctx, t, "--github-repo", lsnPRRepo,
			"--dir", filepath.Join(tmp, "dir"), "--state-dir", stateDir,
			"--review-timeout", "2s", "--on-review", lsnReexecutedHook(), "--once")
		close(stopped)
	}()
	if !lsnWaitForStart(started, stopped, 5*time.Second) {
		cancel()
		<-finished
		t.Fatal("hook never started")
	}
	done := lsnSearchStateFile(t, stateDir, "done", "pr-7-")
	cancel()
	select {
	case err := <-finished:
		lsnRequireSuccess(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not return after the hook")
	}
	if done == "" {
		t.Error("done record was not written before the hook started")
	}
	data, err := os.ReadFile(result)
	if err != nil || string(data) != "hook finished" {
		t.Fatalf("hook lost after listener cancellation: result = %q, %v", data, err)
	}
}

func TestLsnRetryLogAppendsHeadersAndStderr(t *testing.T) {
	lsnInstallReexecutedChild(t)
	t.Setenv("OCR_LSN_CHILD_EXIT", "1")
	tmp := t.TempDir()
	dir, stateDir := filepath.Join(tmp, "dir"), filepath.Join(tmp, "state")
	args := []string{"--github-repo", lsnPRRepo, "--dir", dir, "--state-dir", stateDir, "--once"}
	for _, message := range []string{"first attempt failed", "second attempt failed"} {
		t.Setenv("OCR_LSN_CHILD_STDERR", message)
		_ = lsnRunListen(context.Background(), t, args...)
	}
	attempts := lsnFindStateFile(t, stateDir, "attempts", "pr-7-")
	data, err := os.ReadFile(attempts)
	if err != nil || strings.TrimSpace(string(data)) != "2" {
		t.Fatalf("attempts = %q, %v; want 2", data, err)
	}
	logPath := lsnFindFile(t, filepath.Join(dir, "ocr-listen", "logs"))
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "=== ocr listen: review-pr " + lsnPRRepo + "#7 @" + lsnPRHeadSHA[:12] + " at "
	headers := regexp.MustCompile(`(?m)^`+regexp.QuoteMeta(prefix)+`([^\n]+) ===$`).FindAllStringSubmatch(string(log), -1)
	if len(headers) != 2 {
		t.Errorf("log has %d attempt headers, want 2:\n%s", len(headers), log)
	}
	for _, header := range headers {
		if _, err := time.Parse(time.RFC3339, header[1]); err != nil {
			t.Errorf("attempt header timestamp %q is not RFC3339: %v", header[1], err)
		}
	}
	for _, message := range []string{"first attempt failed", "second attempt failed"} {
		if strings.Count(string(log), message) != 1 {
			t.Errorf("log must retain stderr %q exactly once:\n%s", message, log)
		}
	}
}

func TestLsnRejectsPathAndGithubAPIAfterDash(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		flag  string
	}{
		{"path", []string{"--path", "x"}, "--path"},
		{"path-equals", []string{"--path=x"}, "--path"},
		{"github-api", []string{"--github-api", "https://example.invalid"}, "--github-api"},
		{"github-api-equals", []string{"--github-api=https://example.invalid"}, "--github-api"},
		{"success-file", []string{"--success-file", "x"}, "--success-file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lsnInstallSeams(t, os.Args[0], "token", nil)
			tmp := t.TempDir()
			args := []string{"--github-repo", lsnPRRepo, "--dir", filepath.Join(tmp, "dir"),
				"--state-dir", filepath.Join(tmp, "state"), "--once", "--"}
			err := lsnRunListen(context.Background(), t, append(args, tc.extra...)...)
			if err == nil {
				t.Fatalf("accepted reserved review-pr flag %s", tc.flag)
			}
			if !strings.Contains(err.Error(), tc.flag) || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("error %q must name reserved flag %s", err, tc.flag)
			}
		})
	}
}

func TestLsnRejectsNegativeReviewTimeout(t *testing.T) {
	lsnInstallReexecutedChild(t)
	tmp := t.TempDir()
	err := lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
		"--dir", filepath.Join(tmp, "dir"), "--state-dir", filepath.Join(tmp, "state"),
		"--review-timeout", "-1s", "--once")
	lsnRequireError(t, err, "--review-timeout")
	if _, err := os.Stat(filepath.Join(tmp, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rejected configuration touched the state directory: %v", err)
	}
}

func TestLsnRejectsNonPositiveIntervalAndMaxAttempts(t *testing.T) {
	lsnInstallReexecutedChild(t)
	for _, tc := range []struct {
		name    string
		flag    string
		value   string
		message string
	}{
		{"interval-zero", "--interval", "0", "--interval must be positive"},
		{"interval-negative", "--interval", "-1s", "--interval must be positive"},
		{"max-attempts-zero", "--max-attempts", "0", "--max-attempts must be at least 1"},
		{"max-attempts-negative", "--max-attempts", "-2", "--max-attempts must be at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			err := lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
				"--dir", filepath.Join(tmp, "dir"), "--state-dir", filepath.Join(tmp, "state"),
				"--once", tc.flag, tc.value)
			lsnRequireError(t, err, tc.message)
			if _, err := os.Stat(filepath.Join(tmp, "state")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a rejected configuration touched the state directory: %v", err)
			}
		})
	}
}

func TestLsnPassesASuccessFileBesideTheWorkDirectory(t *testing.T) {
	lsnSkipOnWindows(t)
	tmp := t.TempDir()
	argvPath := filepath.Join(tmp, "argv.txt")
	exe := lsnFakeChild(t, tmp, lsnChild{argvPath: argvPath})
	lsnInstallSeams(t, exe, "token", []prsnapshot.Pull{lsnPull()})
	dir := filepath.Join(tmp, "dir")
	lsnRequireSuccess(t, lsnRunListen(context.Background(), t, "--github-repo", lsnPRRepo,
		"--dir", dir, "--state-dir", filepath.Join(tmp, "state"), "--once"))
	args := lsnReadArgs(t, argvPath)
	success, workdir := "", ""
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--success-file":
			success = args[i+1]
		case "--workdir":
			workdir = args[i+1]
		}
	}
	if success == "" || success != workdir+".ok" {
		t.Fatalf("--success-file = %q, want %q", success, workdir+".ok")
	}
	if _, err := os.Stat(success); err != nil {
		t.Errorf("the success file of the finished review is missing: %v", err)
	}
}
