// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package prlisten keeps reviewing the open pull requests of a set of
// repositories, one new head commit at a time.
//
// Several listeners, each in its own folder, may run on one machine. They
// share a state directory and claim each unit of work with an OS advisory
// lock, so a unit is reviewed once whichever listener gets to it first. On Unix
// the review child keeps the claim when its listener dies. The next
// listener adopts the finished result.
package prlisten

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alibaba/open-code-review/internal/flock"
	"github.com/alibaba/open-code-review/internal/prsnapshot"
)

// Defaults for the configuration a caller may leave empty.
const (
	defaultInterval    = 60 * time.Second
	defaultMaxAttempts = 3
	defaultResultExt   = "json"
)

// headSHAPattern matches a full git object name. The head commit ends up in
// file names, so a value that is not a hex object name is refused before it
// reaches a path.
var headSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// fileNamePartPattern bounds Scope and ResultExt, which end up in state keys
// and file names, to characters that can never form a path.
var fileNamePartPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Lister lists the open pull requests of one repository.
type Lister interface {
	ListOpenPulls(ctx context.Context, owner, repo string) ([]prsnapshot.Pull, error)
}

// Job is one unit of work: one head commit of one pull request, for one
// scope. The paths are created by the listener's parent directories before
// Review runs; Review owns the files themselves.
type Job struct {
	Repo prsnapshot.RepoRef
	Pull prsnapshot.Pull
	// Key identifies the unit in the shared state: "pr-<N>-<headsha>-<scope>".
	Key string
	// Claim is the held lock for Key. Review may hand Claim.File() to a child
	// process so the claim outlives a listener killed mid-review.
	Claim      *flock.Lock
	ResultPath string
	LogPath    string
	WorkDir    string
	// SuccessPath is created by Review once ResultPath is complete and the
	// review succeeded. A listener that finds the inflight record of a review
	// whose own listener died adopts the result only when this file exists:
	// a failed review may still leave a non-empty report behind.
	SuccessPath string
}

// Config configures a Listener.
type Config struct {
	Repos []prsnapshot.RepoRef
	// Dir is the listener's own folder; everything it writes lives under
	// Dir/ocr-listen/{reviews,logs,work}/<host>/<owner>/<repo>/.
	Dir string
	// StateDir is shared by cooperating listeners; claims and outcomes live
	// under StateDir/<host>/<owner>/<repo>/{locks,done,attempts,inflight}/<key>.
	StateDir string
	// Interval separates the start of two polls. Once stops after one poll.
	Interval time.Duration
	Once     bool
	// BaseBranch, when set, skips pull requests targeting another branch.
	BaseBranch    string
	IncludeDrafts bool
	// MaxAttempts bounds failed reviews of one unit across all listeners.
	MaxAttempts int
	// ResultExt is the result file extension without the dot.
	ResultExt string
	// Scope distinguishes listeners that review the same pull request
	// differently (see ScopeID); it is part of every Key.
	Scope string
	// ListerFor returns the Lister serving a repository's host.
	ListerFor func(prsnapshot.RepoRef) Lister
	// Review performs the review of job, writing job.ResultPath. A nil error
	// with a non-empty result file marks the unit done.
	Review func(ctx context.Context, job Job) error
	// OnReviewed, when set, runs after a successful review. Its error is
	// logged and never undoes the outcome.
	OnReviewed func(ctx context.Context, job Job) error
	// Log receives one line per event; nil discards.
	Log io.Writer
}

// Stats summarizes one poll.
type Stats struct {
	Seen     int // open pull requests listed
	Reviewed int // reviews that succeeded
	Failed   int // reviews that failed, or units the shared state kept from reviewing
	Skipped  int // done, out of attempts, draft or other base branch
	Busy     int // claimed by another listener right now
	Errors   int // repositories whose listing failed
}

// Listener polls and reviews.
type Listener struct {
	cfg Config
	// abandoned holds units whose failed attempt could not be counted: without
	// a counter, --max-attempts could never stop them, so this listener stops.
	abandoned map[string]bool
}

// New validates cfg and fills defaults: Interval 60s, MaxAttempts 3,
// ResultExt "json".
func New(cfg Config) (*Listener, error) {
	switch {
	case len(cfg.Repos) == 0:
		return nil, errors.New("prlisten: at least one repository is required")
	case cfg.Dir == "":
		return nil, errors.New("prlisten: a listener directory is required")
	case cfg.StateDir == "":
		return nil, errors.New("prlisten: a state directory is required")
	case cfg.Review == nil:
		return nil, errors.New("prlisten: a review function is required")
	case cfg.ListerFor == nil:
		return nil, errors.New("prlisten: a lister factory is required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultMaxAttempts
	}
	if cfg.ResultExt == "" {
		cfg.ResultExt = defaultResultExt
	}
	if cfg.Scope == "" {
		cfg.Scope = ScopeID(nil, nil, "")
	}
	if !fileNamePartPattern.MatchString(cfg.Scope) || !fileNamePartPattern.MatchString(cfg.ResultExt) {
		return nil, fmt.Errorf("prlisten: scope %q and result extension %q may only hold letters, digits, '-' and '_'", cfg.Scope, cfg.ResultExt)
	}
	return &Listener{cfg: cfg}, nil
}

// Poll makes one pass over every repository. A listing failure is logged and
// counted in Stats.Errors and the pass moves on to the next repository; the
// returned error is non-nil only when ctx ends the pass early.
func (l *Listener) Poll(ctx context.Context) (Stats, error) {
	var stats Stats
	for _, repo := range l.cfg.Repos {
		lister := l.cfg.ListerFor(repo)
		if lister == nil {
			stats.Errors++
			l.logf("listing %s failed: no lister serves its host", repo)
			continue
		}
		pulls, err := lister.ListOpenPulls(ctx, repo.Owner, repo.Repo)
		if err != nil {
			stats.Errors++
			l.logf("listing %s failed: %v", repo, err)
			continue
		}
		stats.Seen += len(pulls)
		for _, pull := range pulls {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			l.reviewPull(ctx, repo, pull, &stats)
		}
	}
	return stats, ctx.Err()
}

// Run polls until ctx is done (it then returns nil), or once when Once is
// set. A single poll with failed reviews or listing errors returns an error.
func (l *Listener) Run(ctx context.Context) error {
	for {
		start := time.Now()
		stats, err := l.Poll(ctx)
		l.logf("poll: %d open, %d reviewed, %d failed, %d skipped, %d busy, %d listing errors", stats.Seen, stats.Reviewed, stats.Failed, stats.Skipped, stats.Busy, stats.Errors)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("prlisten: poll: %w", err)
		}
		if l.cfg.Once {
			if stats.Failed > 0 || stats.Errors > 0 {
				return fmt.Errorf("prlisten: poll: %d failed reviews, %d listing errors", stats.Failed, stats.Errors)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(l.cfg.Interval - time.Since(start)):
		}
	}
}

// ScopeID fingerprints what makes two reviews of one head commit differ: the
// path filter (order-insensitive), the extra review arguments (order kept)
// and the output format. It returns 12 lowercase hex characters.
func ScopeID(paths, extraArgs []string, format string) string {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	fingerprint := "paths=" + strings.Join(sorted, "\x00") +
		"\x01args=" + strings.Join(extraArgs, "\x00") +
		"\x01format=" + format
	sum := sha256.Sum256([]byte(fingerprint))
	return hex.EncodeToString(sum[:])[:12]
}

// reviewPull reviews one pull request head, unless the head was filtered
// out, another listener holds its claim, it was reviewed already or it failed
// too often.
func (l *Listener) reviewPull(ctx context.Context, repo prsnapshot.RepoRef, pull prsnapshot.Pull, stats *Stats) {
	if pull.Draft && !l.cfg.IncludeDrafts {
		stats.Skipped++
		return
	}
	if l.cfg.BaseBranch != "" && pull.BaseRef != l.cfg.BaseBranch {
		stats.Skipped++
		return
	}
	if !headSHAPattern.MatchString(pull.HeadSHA) {
		stats.Skipped++
		l.logf("%s#%d has an unusable head commit %q", repo, pull.Number, pull.HeadSHA)
		return
	}

	store := newStateStore(l.cfg.StateDir)
	components := repo.PathComponents()
	key := fmt.Sprintf("pr-%d-%s-%s", pull.Number, pull.HeadSHA, l.cfg.Scope)
	if l.abandoned[key] {
		stats.Skipped++
		return
	}
	claim, acquired, err := flock.TryLock(store.lockPath(components, key))
	if err != nil {
		stats.Failed++
		l.logf("claiming %s#%d failed: %v", repo, pull.Number, err)
		return
	}
	if !acquired {
		stats.Busy++
		l.logf("%s#%d is claimed by another listener", repo, pull.Number)
		return
	}
	defer claim.Unlock()

	if _, err := os.Stat(store.donePath(components, key)); err == nil {
		stats.Skipped++
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		stats.Failed++
		l.logf("reading the outcome of %s#%d failed: %v", repo, pull.Number, err)
		return
	}

	attempts, ok := store.attempts(components, key)
	if !ok {
		stats.Skipped++
		l.logf("the attempt counter of %s#%d is unusable; giving the head up", repo, pull.Number)
		return
	}
	job := l.newJob(repo, pull, key, claim)
	data, err := os.ReadFile(store.inflightPath(components, key))
	if err == nil {
		var record inflightRecord
		decodeErr := json.Unmarshal(data, &record)
		started, timeErr := time.Parse(time.RFC3339Nano, record.StartedAt)
		if decodeErr == nil && timeErr == nil && finishedSince(record, started) {
			job.ResultPath, job.LogPath = record.Result, record.Log
			l.finishReview(ctx, store, job, record.ListenerDir, attempts, stats)
			return
		}
		if err := store.clearInflight(components, key); err != nil {
			stats.Failed++
			l.logf("clearing the inflight review of %s#%d failed: %v", repo, pull.Number, err)
			return
		}
		stats.Failed++
		// Saturation keeps an exhausted counter from wrapping into retries.
		if attempts < math.MaxInt {
			attempts++
		}
		if !l.recordAttempt(store, job, attempts) {
			stats.Skipped++
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		stats.Failed++
		l.logf("reading the inflight review of %s#%d failed: %v", repo, pull.Number, err)
		return
	}
	if attempts >= l.cfg.MaxAttempts {
		stats.Skipped++
		return
	}

	for _, dir := range []string{filepath.Dir(job.ResultPath), filepath.Dir(job.LogPath), filepath.Dir(job.WorkDir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			stats.Failed++
			l.logf("creating %s failed: %v", dir, err)
			l.recordAttempt(store, job, attempts+1)
			return
		}
	}

	if err := os.Remove(job.SuccessPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		stats.Failed++
		l.logf("removing the success marker %s failed: %v", job.SuccessPath, err)
		l.recordAttempt(store, job, attempts+1)
		return
	}

	l.logf("reviewing %s#%d @%s", repo, pull.Number, pull.HeadSHA[:12])
	if err := store.writeInflight(components, key, inflightRecord{
		Result: job.ResultPath, Success: job.SuccessPath, Log: job.LogPath, ListenerDir: l.cfg.Dir,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		stats.Failed++
		l.logf("recording the inflight review of %s#%d failed: %v", repo, pull.Number, err)
		l.recordAttempt(store, job, attempts+1)
		return
	}
	reviewErr := l.cfg.Review(ctx, job)
	// A failed review's record goes first, so that no crash can let it be
	// adopted; a successful one keeps it until finishReview records done.
	clearInflight := func() {
		if err := store.clearInflight(components, key); err != nil {
			l.logf("clearing the inflight review of %s#%d failed: %v", repo, pull.Number, err)
		}
	}
	if err := reviewErr; err != nil {
		clearInflight()
		if ctx.Err() != nil {
			// A review cut short by shutdown says nothing about the head, so it
			// must not use up one of its attempts.
			l.logf("reviewing %s#%d was interrupted", repo, pull.Number)
			return
		}
		stats.Failed++
		l.logf("reviewing %s#%d failed: %v", repo, pull.Number, err)
		l.recordAttempt(store, job, attempts+1)
		return
	}
	if err := usableResult(job.ResultPath); err != nil {
		clearInflight()
		stats.Failed++
		l.logf("review produced no result file at %s: %v", job.ResultPath, err)
		l.recordAttempt(store, job, attempts+1)
		return
	}

	l.finishReview(ctx, store, job, l.cfg.Dir, attempts, stats)
}

func (l *Listener) finishReview(ctx context.Context, store stateStore, job Job, listenerDir string, attempts int, stats *Stats) {
	repo, pull, key := job.Repo, job.Pull, job.Key
	components := repo.PathComponents()
	record := doneRecord{
		Pull:        pull.Number,
		HeadSHA:     pull.HeadSHA,
		Result:      job.ResultPath,
		Log:         job.LogPath,
		ListenerDir: listenerDir,
		FinishedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if err := store.writeDone(components, key, record); err != nil {
		stats.Failed++
		l.logf("recording the outcome of %s#%d failed: %v", repo, pull.Number, err)
		l.recordAttempt(store, job, attempts+1)
		return
	}
	if err := store.clearInflight(components, key); err != nil {
		l.logf("clearing the inflight review of %s#%d failed: %v", repo, pull.Number, err)
	}
	stats.Reviewed++
	store.clearAttempts(components, key)
	if l.cfg.OnReviewed != nil {
		// Done is already recorded, so the hook would never run again: a
		// shutdown must not cancel it.
		if err := l.cfg.OnReviewed(context.WithoutCancel(ctx), job); err != nil {
			l.logf("the on-review hook of %s#%d failed: %v", repo, pull.Number, err)
		}
	}
}

// newJob describes the unit of work: the files of one head commit of one
// pull request, below the listener's own folder.
func (l *Listener) newJob(repo prsnapshot.RepoRef, pull prsnapshot.Pull, key string, claim *flock.Lock) Job {
	components := repo.PathComponents()
	stem := fmt.Sprintf("pr-%d-%s-%s", pull.Number, pull.HeadSHA[:12], l.cfg.Scope)
	return Job{
		Repo:        repo,
		Pull:        pull,
		Key:         key,
		Claim:       claim,
		ResultPath:  filepath.Join(l.outputDir("reviews", components), stem+"."+l.cfg.ResultExt),
		LogPath:     filepath.Join(l.outputDir("logs", components), stem+".log"),
		WorkDir:     filepath.Join(l.outputDir("work", components), stem),
		SuccessPath: filepath.Join(l.outputDir("work", components), stem+".ok"),
	}
}

// outputDir is the folder of one kind of output of one repository.
func (l *Listener) outputDir(kind string, components []string) string {
	parts := make([]string, 0, len(components)+3)
	parts = append(parts, l.cfg.Dir, "ocr-listen", kind)
	parts = append(parts, components...)
	return filepath.Join(parts...)
}

// recordAttempt counts a failed review of one unit.
func (l *Listener) recordAttempt(store stateStore, job Job, attempts int) bool {
	components, key := job.Repo.PathComponents(), job.Key
	if err := store.writeAttempts(components, key, attempts); err != nil {
		l.logf("recording the attempt of %s failed: %v; this listener gives the head up", key, err)
		if l.abandoned == nil {
			l.abandoned = make(map[string]bool)
		}
		l.abandoned[key] = true
		return false
	}
	if attempts == l.cfg.MaxAttempts {
		l.logf("giving up on %s#%d @%s after %d failed attempts; delete %s to retry", job.Repo, job.Pull.Number, job.Pull.HeadSHA[:12], attempts, store.attemptsPath(components, key))
	}
	return true
}

// logf writes one line to the configured log.
func (l *Listener) logf(format string, args ...any) {
	if l.cfg.Log == nil {
		return
	}
	fmt.Fprintf(l.cfg.Log, format+"\n", args...)
}

// usableResult reports whether the review left a result worth keeping.
func usableResult(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("the result is not a regular file")
	}
	if info.Size() == 0 {
		return errors.New("the result file is empty")
	}
	return nil
}

// finishedSince reports whether the review of record succeeded after started:
// its success marker and a usable result were both written since then.
func finishedSince(record inflightRecord, started time.Time) bool {
	if usableResult(record.Result) != nil || record.Success == "" {
		return false
	}
	for _, path := range []string{record.Result, record.Success} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(started) {
			return false
		}
	}
	return true
}
