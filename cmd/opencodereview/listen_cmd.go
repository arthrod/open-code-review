// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/open-code-review/internal/prlisten"
	"github.com/alibaba/open-code-review/internal/prsnapshot"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type listenOptions struct {
	repos         string
	dir           string
	stateDir      string
	paths         string
	baseBranch    string
	format        string
	onReview      string
	githubAPI     string
	interval      time.Duration
	reviewTimeout time.Duration
	once          bool
	includeDrafts bool
	keepWork      bool
	maxAttempts   int
}

var listenOpts listenOptions

// listenReservedFlags are review-pr flags the listener decides for every job,
// or that would skip the review altogether. Extras after "--" are parsed with
// the real review-pr flag set, so every spelling of these is caught.
var listenReservedFlags = []string{
	"output", "workdir", "head", "base", "remote", "cleanup", "format", "audience",
	"repo", "from", "to", "commit", "preview", "resume", "help", "path", "github-api",
	"success-file",
}

var listenCmd = newListenCmd(&listenOpts)

func init() {
	rootCmd.AddCommand(listenCmd)
}

// newListenCmd builds the listen command bound to opts; tests build their own.
func newListenCmd(opts *listenOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "listen --github-repo OWNER/REPO [flags] [-- review-pr flags]",
		Short: "Keep reviewing new pull request commits without cloning",
		Long: "Keep reviewing new pull request commits without cloning.\n\n" +
			"Each poll lists the open pull requests and reviews every head commit not reviewed yet\n" +
			"with `ocr review-pr`. Run several listeners in different folders: they share a state\n" +
			"directory and claim work with OS file locks, so each commit is reviewed once.",
		Example: `  # Three listeners, each in its own folder, sharing the work
  for n in 1 2 3; do
    mkdir -p ~/ocr-listeners/$n
    (cd ~/ocr-listeners/$n && ocr listen --github-repo acme/book &)
  done

  # One chapter per listener, posting a comment after each review
  ocr listen --github-repo acme/book --path chapters/07 --format text \
    --on-review 'gh pr comment "$OCR_LISTEN_PR_URL" --body-file "$OCR_LISTEN_RESULT"' \
    -- --effort high`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := interruptContextWithForcedExit(cmd.Context())
			defer stop()
			return runListen(ctx, cmd, *opts, args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.repos, "github-repo", "", "comma-separated repositories to watch: OWNER/REPO, HOST/OWNER/REPO or a repository URL")
	f.StringVar(&opts.dir, "dir", "", "this listener's folder; results, logs and scratch work go under <dir>/ocr-listen (default: current dir)")
	f.StringVar(&opts.stateDir, "state-dir", "", "state shared by cooperating listeners (default: ~/.opencodereview/listen)")
	f.StringVar(&opts.paths, "path", "", "comma-separated repo-relative paths; passed to review-pr --path")
	f.StringVar(&opts.baseBranch, "base-branch", "", "only review pull requests targeting this branch")
	f.StringVarP(&opts.format, "format", "f", "json", "result format: text, json, or sarif")
	f.StringVar(&opts.onReview, "on-review", "", "shell command run after each successful review, with OCR_LISTEN_* variables set")
	f.StringVar(&opts.githubAPI, "github-api", "", "GitHub REST API root (default: derived from each repository's host)")
	f.DurationVar(&opts.interval, "interval", time.Minute, "time between two polls")
	f.DurationVar(&opts.reviewTimeout, "review-timeout", time.Hour, "timeout for each review and separately for its on-review hook (0 disables the limit)")
	f.BoolVar(&opts.once, "once", false, "poll once and exit")
	f.BoolVar(&opts.includeDrafts, "include-drafts", false, "also review draft pull requests")
	f.BoolVar(&opts.keepWork, "keep-work", false, "keep each review's scratch directory instead of removing it")
	f.IntVar(&opts.maxAttempts, "max-attempts", 3, "failed reviews of one head commit before it is given up")
	cmd.RegisterFlagCompletionFunc("format", completeEnum("text", "json", "sarif"))
	return cmd
}

// Test seams.
var (
	listenExecutable = os.Executable
	newPullLister    = func(baseURL, token string) prlisten.Lister {
		return prsnapshot.NewClient(baseURL, token)
	}
	resolveListenToken = func(ctx context.Context, host string) string {
		return prsnapshot.ResolveToken(ctx, host, nil, nil)
	}
)

// listenChildWaitDelay bounds how long a cancelled review may take to stop
// before it is killed.
const listenChildWaitDelay = 30 * time.Second

// runListen validates the flags, builds a prlisten.Listener whose Review runs
// `<this executable> review-pr` as a subprocess (stderr to the job's log
// file), and runs it.
//
// The extra review-pr arguments are those after "--"; positional arguments
// before it are an error, and so is any reserved flag among the extras.
func runListen(ctx context.Context, cmd *cobra.Command, opts listenOptions, args []string) error {
	extras, err := listenExtras(cmd, args)
	if err != nil {
		return err
	}
	format, err := validateOutputFormat(opts.format)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if opts.reviewTimeout < 0 {
		return fmt.Errorf("listen: --review-timeout must not be negative (0 disables the limit)")
	}
	if opts.interval <= 0 {
		return fmt.Errorf("listen: --interval must be positive")
	}
	if opts.maxAttempts < 1 {
		return fmt.Errorf("listen: --max-attempts must be at least 1")
	}
	repos, err := listenRepos(opts.repos)
	if err != nil {
		return err
	}
	if err := listenCheckAPIHosts(opts.githubAPI, repos); err != nil {
		return err
	}
	dir, err := listenWorkDir(opts.dir)
	if err != nil {
		return err
	}
	stateDir, err := listenStateDir(opts.stateDir)
	if err != nil {
		return err
	}
	exe, err := listenExecutable()
	if err != nil {
		return fmt.Errorf("listen: find the ocr executable: %w", err)
	}

	paths := splitListenPaths(opts.paths)
	runner := &listenRunner{
		exe:           exe,
		format:        format,
		paths:         strings.Join(paths, ","),
		extras:        extras,
		keepWork:      opts.keepWork,
		githubAPI:     opts.githubAPI,
		onReview:      opts.onReview,
		reviewTimeout: opts.reviewTimeout,
	}
	listener, err := prlisten.New(prlisten.Config{
		Repos:         repos,
		Dir:           dir,
		StateDir:      stateDir,
		Interval:      opts.interval,
		Once:          opts.once,
		BaseBranch:    opts.baseBranch,
		IncludeDrafts: opts.includeDrafts,
		MaxAttempts:   opts.maxAttempts,
		ResultExt:     listenResultExt(format),
		Scope:         prlisten.ScopeID(paths, extras, format),
		ListerFor:     listenListerFor(ctx, opts.githubAPI),
		Review:        runner.review,
		OnReviewed:    runner.hook,
		Log:           &listenLogger{},
	})
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return listener.Run(ctx)
}

// listenExtras returns the review-pr flags given after "--". A positional
// argument before the dash, or a flag the listener decides itself, is refused.
func listenExtras(cmd *cobra.Command, args []string) ([]string, error) {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		if len(args) > 0 {
			return nil, listenUnexpectedArgument(args[0])
		}
		return nil, nil
	}
	if dash > 0 {
		return nil, listenUnexpectedArgument(args[0])
	}
	extras := args[dash:]
	if err := validateListenExtras(extras); err != nil {
		return nil, err
	}
	return extras, nil
}

// listenUnexpectedArgument reports a positional argument the listener has no
// place for.
func listenUnexpectedArgument(arg string) error {
	return fmt.Errorf("listen: unexpected positional argument %q; pass the review-pr flags after \"--\"", arg)
}

// validateListenExtras parses the extras with the real review-pr flag set, so
// every spelling of a reserved flag is caught, and so an unusable argument is
// refused before a review starts.
func validateListenExtras(extras []string) error {
	if len(extras) == 0 {
		return nil
	}
	var opts reviewPROptions
	cmd := newReviewPRCmd(&opts)
	if err := cmd.Flags().Parse(extras); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			// pflag reports an undefined help flag as a help request, and
			// cobra would answer that with its help text instead of an error.
			return fmt.Errorf("listen: the review-pr flag --help is reserved; the listener decides it for every job")
		}
		return fmt.Errorf("listen: invalid review-pr flags after \"--\": %w", err)
	}
	if rest := cmd.Flags().Args(); len(rest) > 0 {
		return listenUnexpectedArgument(rest[0])
	}
	for _, name := range listenReservedFlags {
		flag := cmd.Flags().Lookup(name)
		if flag != nil && flag.Changed {
			return fmt.Errorf("listen: the review-pr flag --%s is reserved; the listener decides it for every job", name)
		}
	}
	return nil
}

// listenRepos parses the comma-separated repositories to watch.
func listenRepos(value string) ([]prsnapshot.RepoRef, error) {
	var repos []prsnapshot.RepoRef
	for _, part := range strings.Split(value, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		repo, err := prsnapshot.ParseRepoRef(name)
		if err != nil {
			return nil, fmt.Errorf("listen: invalid --github-repo value %q: %w", name, err)
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("listen: --github-repo is required")
	}
	return repos, nil
}

// listenCheckAPIHosts refuses an explicit API root for repositories on
// several hosts: one root cannot serve them all.
func listenCheckAPIHosts(apiBase string, repos []prsnapshot.RepoRef) error {
	if apiBase == "" {
		return nil
	}
	var hosts []string
	for _, repo := range repos {
		if !slices.Contains(hosts, repo.Host) {
			hosts = append(hosts, repo.Host)
		}
	}
	if len(hosts) > 1 {
		return fmt.Errorf("listen: --github-api serves one host, but the repositories use %s", strings.Join(hosts, ", "))
	}
	return nil
}

// listenWorkDir is the listener's own folder: everything it writes lives
// below it.
func listenWorkDir(dir string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("listen: find the current directory: %w", err)
		}
		dir = cwd
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("listen: resolve %s: %w", dir, err)
	}
	return absolute, nil
}

// listenStateDir is the state the cooperating listeners share.
func listenStateDir(stateDir string) (string, error) {
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("listen: find the home directory: %w", err)
		}
		stateDir = filepath.Join(home, ".opencodereview", "listen")
	}
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("listen: resolve %s: %w", stateDir, err)
	}
	return absolute, nil
}

// splitListenPaths splits the comma-separated path filter.
func splitListenPaths(value string) []string {
	var paths []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}

// listenResultExt is the extension of the result file of one format.
func listenResultExt(format string) string {
	switch format {
	case "text":
		return "txt"
	case "sarif":
		return "sarif"
	default:
		return "json"
	}
}

// listenListerFor returns the factory of the listers, one per host, so the
// token of a host is resolved once.
func listenListerFor(ctx context.Context, apiBase string) func(prsnapshot.RepoRef) prlisten.Lister {
	listers := map[string]prlisten.Lister{}
	return func(repo prsnapshot.RepoRef) prlisten.Lister {
		if lister, ok := listers[repo.Host]; ok {
			return lister
		}
		base := apiBase
		if base == "" {
			base = prsnapshot.APIBaseForHost(repo.Host)
		}
		token := resolveListenToken(ctx, repo.Host)
		if token == "" {
			fmt.Fprintf(os.Stderr, "[ocr listen] no GitHub token for %s: the anonymous API allows 60 requests per hour\n", repo.Host)
		}
		lister := newPullLister(base, token)
		listers[repo.Host] = lister
		return lister
	}
}

// listenRunner reviews one job: it runs the review-pr command of this binary
// and, after a successful review, the on-review hook.
type listenRunner struct {
	exe           string
	format        string
	paths         string
	extras        []string
	keepWork      bool
	githubAPI     string
	onReview      string
	reviewTimeout time.Duration
}

// review runs `ocr review-pr` for the job. The output of the child goes to
// the job's log file, so a failed review can be read after the fact.
func (r *listenRunner) review(ctx context.Context, job prlisten.Job) error {
	if r.reviewTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.reviewTimeout)
		defer cancel()
	}
	logFile, err := os.OpenFile(job.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open the log file %s: %w", job.LogPath, err)
	}
	defer logFile.Close()
	if _, err := fmt.Fprintf(logFile, "=== ocr listen: review-pr %s#%d @%.12s at %s ===\n",
		job.Repo, job.Pull.Number, job.Pull.HeadSHA, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write the log header to %s: %w", job.LogPath, err)
	}

	cmd := exec.CommandContext(ctx, r.exe, r.reviewArgs(job)...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.WaitDelay = listenChildWaitDelay
	var claim *os.File
	if job.Claim != nil {
		claim = job.Claim.File()
	}
	configureListenChild(cmd, claim)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("review-pr of %s#%d failed: %w", job.Repo, job.Pull.Number, err)
	}
	return nil
}

// reviewArgs are the arguments of the review-pr command of one job.
func (r *listenRunner) reviewArgs(job prlisten.Job) []string {
	args := []string{
		"review-pr",
		fmt.Sprintf("%s#%d", job.Repo, job.Pull.Number),
		"--head", job.Pull.HeadSHA,
		"--workdir", job.WorkDir,
		"--output", job.ResultPath,
		"--success-file", job.SuccessPath,
		"--format", r.format,
		"--audience", "agent",
	}
	if !r.keepWork {
		args = append(args, "--cleanup")
	}
	if r.paths != "" {
		args = append(args, "--path", r.paths)
	}
	if r.githubAPI != "" {
		args = append(args, "--github-api", r.githubAPI)
	}
	return append(args, r.extras...)
}

// hook runs the on-review command with the facts of the job in its
// environment. Its failure is a warning, never a lost review.
func (r *listenRunner) hook(ctx context.Context, job prlisten.Job) error {
	if r.onReview == "" {
		return nil
	}
	if r.reviewTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.reviewTimeout)
		defer cancel()
	}
	logFile, err := os.OpenFile(job.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open the log file %s: %w", job.LogPath, err)
	}
	defer logFile.Close()

	cmd := shellCommand(ctx, r.onReview)
	configureListenHook(cmd)
	cmd.WaitDelay = listenChildWaitDelay
	cmd.Env = listenHookEnv(job, r.format)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the on-review hook failed: %w", err)
	}
	return nil
}

// listenHookEnv returns the environment of the hook: the environment of the
// listener plus the facts of the job. The facts are passed as variables, not
// as a template, so a pull request title cannot inject shell code.
func listenHookEnv(job prlisten.Job, format string) []string {
	env := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "OCR_LISTEN_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"OCR_LISTEN_REPO="+job.Repo.String(),
		"OCR_LISTEN_PR="+strconv.Itoa(job.Pull.Number),
		"OCR_LISTEN_PR_URL="+job.Pull.HTMLURL,
		"OCR_LISTEN_HEAD_SHA="+job.Pull.HeadSHA,
		"OCR_LISTEN_RESULT="+job.ResultPath,
		"OCR_LISTEN_FORMAT="+format,
		"OCR_LISTEN_LOG="+job.LogPath,
	)
}

// listenLogger writes the events of the listener to stderr, one prefixed
// line at a time, so several listeners can share one terminal.
type listenLogger struct {
	mu  sync.Mutex
	buf string
}

// Write splits the events into lines and writes them to stderr.
func (l *listenLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf += string(p)
	for {
		index := strings.IndexByte(l.buf, '\n')
		if index < 0 {
			break
		}
		line := l.buf[:index]
		l.buf = l.buf[index+1:]
		fmt.Fprintf(os.Stderr, "[ocr listen] %s\n", line)
	}
	return len(p), nil
}
