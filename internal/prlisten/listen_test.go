// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prlisten

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/flock"
	"github.com/alibaba/open-code-review/internal/prsnapshot"
)

const listenTestSHA = "0123456789abcdef0123456789abcdef01234567"

type listenTestListing struct {
	pulls []prsnapshot.Pull
	err   error
}

type listenTestLister struct {
	repos  map[string]listenTestListing
	calls  []string
	onList func()
}

func (f *listenTestLister) ListOpenPulls(_ context.Context, owner, repo string) ([]prsnapshot.Pull, error) {
	key := owner + "/" + repo
	f.calls = append(f.calls, key)
	if f.onList != nil {
		f.onList()
	}
	listing, ok := f.repos[key]
	if !ok {
		return nil, fmt.Errorf("unexpected repository %s", key)
	}
	return append([]prsnapshot.Pull(nil), listing.pulls...), listing.err
}

func listenTestConfig(t *testing.T) (Config, *listenTestLister) {
	t.Helper()
	pull := prsnapshot.Pull{
		Number: 7, Title: "Improve review", Body: "Review body", State: "open",
		HTMLURL: "https://github.com/o/r/pull/7", HeadSHA: listenTestSHA, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), CloneURL: "https://github.com/o/r.git",
	}
	fake := &listenTestLister{repos: map[string]listenTestListing{"o/r": {pulls: []prsnapshot.Pull{pull}}}}
	cfg := Config{
		Repos: []prsnapshot.RepoRef{{Host: "github.com", Owner: "o", Repo: "r"}},
		Dir:   t.TempDir(), StateDir: t.TempDir(), Interval: time.Millisecond,
		MaxAttempts: 3, ResultExt: "json", Scope: "012345abcdef",
		ListerFor: func(prsnapshot.RepoRef) Lister { return fake },
		Review:    listenTestWriteResult,
	}
	return cfg, fake
}

func listenTestWriteResult(_ context.Context, job Job) error {
	if err := os.WriteFile(job.ResultPath, []byte(`{"review":"complete"}`), 0o600); err != nil {
		return err
	}
	return os.WriteFile(job.SuccessPath, nil, 0o600)
}

func listenTestKey(cfg Config, pull prsnapshot.Pull) string {
	return fmt.Sprintf("pr-%d-%s-%s", pull.Number, pull.HeadSHA, cfg.Scope)
}

func listenTestStatePath(cfg Config, kind string, pull prsnapshot.Pull) string {
	return filepath.Join(cfg.StateDir, "github.com", "o", "r", kind, listenTestKey(cfg, pull))
}

func listenTestPoll(t *testing.T, listener *Listener, want Stats) {
	t.Helper()
	got, err := listener.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got != want {
		t.Fatalf("Poll stats = %+v, want %+v", got, want)
	}
}

func listenTestRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func listenTestWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listenTestAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should not exist; stat error = %v", path, err)
	}
}

func listenTestAttempts(t *testing.T, cfg Config, pull prsnapshot.Pull, want int) {
	t.Helper()
	data := listenTestRead(t, listenTestStatePath(cfg, "attempts", pull))
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || got != want {
		t.Fatalf("attempts = %q, want %d (parse error %v)", data, want, err)
	}
}

func listenTestLockAvailable(t *testing.T, path string) {
	t.Helper()
	lock, acquired, err := flock.TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after Poll: %v", err)
	}
	if lock != nil {
		t.Cleanup(func() { _ = lock.Unlock() })
	}
	if !acquired || lock == nil {
		t.Fatal("claim was not released after Poll")
	}
}

func TestPollWritesDoneAndSkipsReviewedHead(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	calls := 0
	var reviewedJob Job
	cfg.Review = func(ctx context.Context, job Job) error {
		calls++
		reviewedJob = job
		if job.Repo != cfg.Repos[0] || !reflect.DeepEqual(job.Pull, pull) || job.Key != listenTestKey(cfg, pull) {
			t.Fatalf("unexpected job identity: %+v", job)
		}
		stem := "pr-7-" + listenTestSHA[:12] + "-" + cfg.Scope
		for name, pair := range map[string][2]string{
			"result": {job.ResultPath, filepath.Join(cfg.Dir, "ocr-listen", "reviews", "github.com", "o", "r", stem+".json")},
			"log":    {job.LogPath, filepath.Join(cfg.Dir, "ocr-listen", "logs", "github.com", "o", "r", stem+".log")},
			"work":   {job.WorkDir, filepath.Join(cfg.Dir, "ocr-listen", "work", "github.com", "o", "r", stem)},
		} {
			if pair[0] != pair[1] {
				t.Errorf("%s path = %q, want %q", name, pair[0], pair[1])
			}
		}
		for _, path := range []string{job.ResultPath, job.LogPath} {
			info, err := os.Stat(filepath.Dir(path))
			if err != nil {
				t.Fatalf("job parent missing: %v", err)
			}
			if !info.IsDir() {
				t.Fatalf("job parent is not a directory: %s", path)
			}
		}
		listenTestAbsent(t, job.WorkDir)
		if job.Claim == nil || job.Claim.File() == nil {
			t.Fatal("Review must receive the held claim and its open file")
		}
		other, acquired, err := flock.TryLock(listenTestStatePath(cfg, "locks", pull) + ".lock")
		if other != nil {
			t.Cleanup(func() { _ = other.Unlock() })
		}
		if err != nil || acquired {
			t.Fatalf("claim is not held during Review: acquired=%v, err=%v", acquired, err)
		}
		return listenTestWriteResult(ctx, job)
	}
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	if calls != 1 {
		t.Fatalf("Review calls = %d, want 1", calls)
	}
	var done struct {
		Pull        int    `json:"pull"`
		HeadSHA     string `json:"head_sha"`
		Result      string `json:"result"`
		Log         string `json:"log"`
		ListenerDir string `json:"listener_dir"`
		FinishedAt  string `json:"finished_at"`
	}
	donePath := listenTestStatePath(cfg, "done", pull)
	data := listenTestRead(t, donePath)
	if err := json.Unmarshal(data, &done); err != nil {
		t.Fatalf("done JSON: %v", err)
	}
	if done.Pull != 7 || done.HeadSHA != pull.HeadSHA || done.Result != reviewedJob.ResultPath || done.Log != reviewedJob.LogPath || done.ListenerDir != cfg.Dir {
		t.Errorf("unexpected done record: %+v", done)
	}
	if finished, err := time.Parse(time.RFC3339, done.FinishedAt); err != nil || finished.IsZero() {
		t.Errorf("finished_at = %q, want RFC3339 timestamp", done.FinishedAt)
	}
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	if calls != 1 || !bytes.Equal(data, listenTestRead(t, donePath)) {
		t.Error("second Poll repeated the review or rewrote its done record")
	}
	listenTestLockAvailable(t, listenTestStatePath(cfg, "locks", pull)+".lock")
}

func TestPollFiltersDraftsAndBaseBranches(t *testing.T) {
	for _, tc := range []struct {
		name           string
		draft, include bool
		base, filter   string
		reviewed       int
	}{
		{"draft excluded", true, false, "main", "", 0},
		{"draft included", true, true, "main", "", 1},
		{"matching base", false, false, "main", "main", 1},
		{"other base", false, false, "release", "main", 0},
		{"included draft still filtered by base", true, true, "release", "main", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			pull.Draft, pull.BaseRef = tc.draft, tc.base
			fake.repos["o/r"] = listenTestListing{pulls: []prsnapshot.Pull{pull}}
			cfg.IncludeDrafts, cfg.BaseBranch = tc.include, tc.filter
			calls := 0
			cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
			listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Reviewed: tc.reviewed, Skipped: 1 - tc.reviewed})
			if calls != tc.reviewed {
				t.Errorf("Review calls = %d, want %d", calls, tc.reviewed)
			}
		})
	}
}

func TestPollStopsAfterMaxAttempts(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	cfg.MaxAttempts = 2
	pull := fake.repos["o/r"].pulls[0]
	calls := 0
	cfg.Review = func(context.Context, Job) error { calls++; return errors.New("review failed") }
	listener := &Listener{cfg: cfg}
	for attempt := 1; attempt <= 2; attempt++ {
		listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1})
		listenTestAttempts(t, cfg, pull, attempt)
		listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
	}
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	listenTestAttempts(t, cfg, pull, 2)
	if calls != 2 {
		t.Errorf("Review calls = %d, want 2", calls)
	}
	listenTestLockAvailable(t, listenTestStatePath(cfg, "locks", pull)+".lock")
}

func TestPollSucceedsAfterOneFailure(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error {
		calls++
		if calls == 1 {
			return errors.New("temporary failure")
		}
		return listenTestWriteResult(ctx, job)
	}
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1})
	listenTestAttempts(t, cfg, pull, 1)
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	listenTestRead(t, listenTestStatePath(cfg, "done", pull))
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	if calls != 2 {
		t.Errorf("Review calls = %d, want 2", calls)
	}
}

func TestPollCoordinatesListenersWithHeldClaim(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	entered := make(chan Job, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	cfg.Review = func(ctx context.Context, job Job) error {
		entered <- job
		select {
		case <-release:
			return listenTestWriteResult(ctx, job)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	secondCfg := cfg
	secondCfg.Dir = t.TempDir()
	// Each listener has its own transport so concurrent polls share only the OS claim.
	secondFake := &listenTestLister{repos: map[string]listenTestListing{"o/r": {pulls: []prsnapshot.Pull{pull}}}}
	secondCfg.ListerFor = func(prsnapshot.RepoRef) Lister { return secondFake }
	secondCalls := 0
	secondCfg.Review = func(ctx context.Context, job Job) error { secondCalls++; return listenTestWriteResult(ctx, job) }
	first, second := &Listener{cfg: cfg}, &Listener{cfg: secondCfg}
	type outcome struct {
		stats Stats
		err   error
	}
	finished := make(chan outcome, 1)
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		unblock()
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("first Poll did not stop during cleanup")
		}
	})
	go func() {
		defer close(stopped)
		stats, err := first.Poll(ctx)
		finished <- outcome{stats, err}
	}()
	select {
	case job := <-entered:
		if job.Claim == nil || job.Claim.File() == nil {
			t.Fatal("first Review did not receive an open claim")
		}
	case result := <-finished:
		t.Fatalf("first Poll ended before Review blocked: %+v, %v", result.stats, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("first Review did not start")
	}
	listenTestPoll(t, second, Stats{Seen: 1, Busy: 1})
	if secondCalls != 0 {
		t.Fatal("second listener reviewed an already claimed head")
	}
	unblock()
	select {
	case result := <-finished:
		if result.err != nil || result.stats != (Stats{Seen: 1, Reviewed: 1}) {
			t.Fatalf("first Poll = %+v, %v", result.stats, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Poll did not finish after Review returned")
	}
	listenTestPoll(t, second, Stats{Seen: 1, Skipped: 1})
	if secondCalls != 0 {
		t.Fatal("second listener repeated the completed review")
	}
	listenTestLockAvailable(t, listenTestStatePath(cfg, "locks", pull)+".lock")
}

func TestPollReviewsDifferentScopesIndependently(t *testing.T) {
	cfg, _ := listenTestConfig(t)
	var jobs []Job
	cfg.Review = func(ctx context.Context, job Job) error {
		jobs = append(jobs, job)
		return listenTestWriteResult(ctx, job)
	}
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Reviewed: 1})
	other := cfg
	other.Scope = "abcdef012345"
	listenTestPoll(t, &Listener{cfg: other}, Stats{Seen: 1, Reviewed: 1})
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	if jobs[0].Key == jobs[1].Key || jobs[0].WorkDir == jobs[1].WorkDir || jobs[0].ResultPath == jobs[1].ResultPath || jobs[0].LogPath == jobs[1].LogPath {
		t.Errorf("scopes must have distinct keys and paths: %+v", jobs)
	}
	for i, current := range []Config{cfg, other} {
		listenTestRead(t, listenTestStatePath(current, "done", jobs[i].Pull))
	}
}

func TestPollContinuesAfterRepositoryListingError(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	bad := prsnapshot.RepoRef{Host: "github.com", Owner: "o", Repo: "bad"}
	cfg.Repos = append([]prsnapshot.RepoRef{bad}, cfg.Repos...)
	fake.repos["o/bad"] = listenTestListing{err: errors.New("listing unavailable")}
	var logs bytes.Buffer
	cfg.Log = &logs
	var reviewed []prsnapshot.RepoRef
	cfg.Review = func(ctx context.Context, job Job) error {
		reviewed = append(reviewed, job.Repo)
		return listenTestWriteResult(ctx, job)
	}
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Reviewed: 1, Errors: 1})
	if !reflect.DeepEqual(fake.calls, []string{"o/bad", "o/r"}) || !reflect.DeepEqual(reviewed, cfg.Repos[1:]) {
		t.Errorf("listing calls = %v; reviewed = %+v", fake.calls, reviewed)
	}
	if !strings.Contains(logs.String(), "listing unavailable") {
		t.Errorf("listing error was not logged: %q", logs.String())
	}
}

func TestPollReviewsNewHeadWithDistinctWorkDirectory(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	var jobs []Job
	cfg.Review = func(ctx context.Context, job Job) error {
		jobs = append(jobs, job)
		return listenTestWriteResult(ctx, job)
	}
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	pull := fake.repos["o/r"].pulls[0]
	pull.HeadSHA = strings.Repeat("b", 40)
	fake.repos["o/r"] = listenTestListing{pulls: []prsnapshot.Pull{pull}}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	if len(jobs) != 2 {
		t.Fatalf("Review calls = %d, want 2", len(jobs))
	}
	if jobs[0].Key == jobs[1].Key || jobs[0].WorkDir == jobs[1].WorkDir || jobs[0].ResultPath == jobs[1].ResultPath || jobs[0].LogPath == jobs[1].LogPath {
		t.Errorf("different heads must use distinct keys and paths: %+v", jobs)
	}
	wantWork := filepath.Join(cfg.Dir, "ocr-listen", "work", "github.com", "o", "r", "pr-7-"+pull.HeadSHA[:12]+"-"+cfg.Scope)
	if jobs[1].WorkDir != wantWork {
		t.Errorf("new head WorkDir = %q, want %q", jobs[1].WorkDir, wantWork)
	}
	for _, job := range jobs {
		listenTestRead(t, listenTestStatePath(cfg, "done", job.Pull))
	}
}

func TestPollCommitsDoneBeforeHookAndKeepsHookFailureSuccessful(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	var logs bytes.Buffer
	cfg.Log = &logs
	hooks := 0
	cfg.OnReviewed = func(_ context.Context, job Job) error {
		hooks++
		var done map[string]any
		if err := json.Unmarshal(listenTestRead(t, listenTestStatePath(cfg, "done", pull)), &done); err != nil {
			t.Fatalf("hook observed incomplete done JSON: %v", err)
		}
		if done["result"] != job.ResultPath {
			t.Errorf("hook observed wrong result: %v", done)
		}
		return errors.New("hook failed")
	}
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	if hooks != 1 || !strings.Contains(logs.String(), "hook failed") {
		t.Errorf("hooks = %d; log = %q", hooks, logs.String())
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "attempts", pull))
}

func TestPollDoesNotCallHookOnReviewFailure(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	cfg.Review = func(context.Context, Job) error { return errors.New("review failed") }
	hooks := 0
	cfg.OnReviewed = func(context.Context, Job) error { hooks++; return nil }
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1})
	if hooks != 0 {
		t.Errorf("hooks = %d, want 0", hooks)
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "done", fake.repos["o/r"].pulls[0]))
}

func TestRunOncePollsExactlyOnce(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	cfg.Once = true
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
	if err := (&Listener{cfg: cfg}).Run(context.Background()); err != nil {
		t.Fatalf("Run once: %v", err)
	}
	if calls != 1 || len(fake.calls) != 1 {
		t.Errorf("Review calls = %d; listing calls = %v, want one each", calls, fake.calls)
	}
}

func TestRunStopsWhenCancelledBetweenPolls(t *testing.T) {
	cfg, _ := listenTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error {
		calls++
		err := listenTestWriteResult(ctx, job)
		cancel()
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- (&Listener{cfg: cfg}).Run(ctx) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("Run on cancellation: %v", err)
		}
		if calls != 1 {
			t.Errorf("Review calls = %d, want 1", calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunRepeatsPollsUntilCancellation(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fake.onList = func() {
		if len(fake.calls) == 2 {
			cancel()
		}
	}
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error {
		calls++
		return listenTestWriteResult(ctx, job)
	}
	finished := make(chan error, 1)
	go func() { finished <- (&Listener{cfg: cfg}).Run(ctx) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("Run on cancellation during a later poll: %v", err)
		}
		if len(fake.calls) != 2 || calls != 1 {
			t.Errorf("listing calls = %v; Review calls = %d, want two polls and one review", fake.calls, calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not poll again and stop on cancellation")
	}
}

func TestPollDoesNotCountInterruptedReviewAsAttempt(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg.Review = func(ctx context.Context, _ Job) error {
		cancel()
		return fmt.Errorf("review-pr interrupted: %w", ctx.Err())
	}
	got, err := (&Listener{cfg: cfg}).Poll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll error = %v, want context.Canceled", err)
	}
	if got != (Stats{Seen: 1}) {
		t.Errorf("Poll stats = %+v, want only Seen: 1", got)
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "attempts", pull))
	listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
	listenTestLockAvailable(t, filepath.Join(cfg.StateDir, "github.com", "o", "r", "locks", listenTestKey(cfg, pull)+".lock"))
}

func TestRunSpacesPollStartsByInterval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		review time.Duration
		check  func(gap, interval time.Duration) bool
		want   string
	}{
		{"fast pass waits out the interval", 0,
			func(gap, interval time.Duration) bool { return gap >= interval },
			"at least the interval"},
		{"slow pass polls again at once", 600 * time.Millisecond,
			func(gap, interval time.Duration) bool { return gap < 600*time.Millisecond+interval*3/4 },
			"right after the pass, not a full interval later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			cfg.Interval = 300 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			var starts []time.Time
			fake.onList = func() {
				starts = append(starts, time.Now())
				if len(starts) == 2 {
					cancel()
				}
			}
			cfg.Review = func(ctx context.Context, job Job) error {
				time.Sleep(tc.review)
				return listenTestWriteResult(ctx, job)
			}
			finished := make(chan error, 1)
			go func() { finished <- (&Listener{cfg: cfg}).Run(ctx) }()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not poll twice")
			}
			if len(starts) != 2 {
				t.Fatalf("polls = %d, want 2", len(starts))
			}
			if gap := starts[1].Sub(starts[0]); !tc.check(gap, cfg.Interval) {
				t.Errorf("second poll started %v after the first, want %s", gap, tc.want)
			}
		})
	}
}

func TestPollChecksCancellationBetweenPulls(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	first := fake.repos["o/r"].pulls[0]
	second := first
	second.Number = 8
	fake.repos["o/r"] = listenTestListing{pulls: []prsnapshot.Pull{first, second}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error {
		calls++
		err := listenTestWriteResult(ctx, job)
		cancel()
		return err
	}
	stats, err := (&Listener{cfg: cfg}).Poll(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll error = %v, want context.Canceled", err)
	}
	if calls != 1 || stats.Reviewed != 1 {
		t.Errorf("Review calls = %d, stats = %+v", calls, stats)
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "done", second))
}

func TestScopeIDMatchesFingerprintContract(t *testing.T) {
	paths := []string{"b", "a"}
	args := []string{"--effort", "high"}
	got := ScopeID(paths, args, "json")
	if got != "bf59394a5cea" || !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(got) {
		t.Errorf("ScopeID = %q, want bf59394a5cea", got)
	}
	if ScopeID(nil, nil, "") != "96e422caf0d6" {
		t.Error("empty scope fingerprint does not match contract")
	}
	if !reflect.DeepEqual(paths, []string{"b", "a"}) || !reflect.DeepEqual(args, []string{"--effort", "high"}) {
		t.Error("ScopeID modified caller inputs")
	}
}

func TestScopeIDSortsPathsButPreservesArgumentsAndFormat(t *testing.T) {
	got := ScopeID([]string{"b", "a"}, []string{"one", "two"}, "json")
	if got != ScopeID([]string{"a", "b"}, []string{"one", "two"}, "json") {
		t.Error("path order changed scope")
	}
	for name, other := range map[string]string{
		"argument order": ScopeID([]string{"a", "b"}, []string{"two", "one"}, "json"),
		"format":         ScopeID([]string{"a", "b"}, []string{"one", "two"}, "text"),
		"paths":          ScopeID([]string{"a", "c"}, []string{"one", "two"}, "json"),
		"boundaries":     ScopeID([]string{"a"}, []string{"b", "one", "two"}, "json"),
	} {
		if other == got {
			t.Errorf("changing %s did not change scope", name)
		}
	}
}

func TestNewRejectsMissingRequiredConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"Repos", func(c *Config) { c.Repos = nil }},
		{"empty Repos", func(c *Config) { c.Repos = []prsnapshot.RepoRef{} }},
		{"Dir", func(c *Config) { c.Dir = "" }},
		{"StateDir", func(c *Config) { c.StateDir = "" }},
		{"Review", func(c *Config) { c.Review = nil }},
		{"ListerFor", func(c *Config) { c.ListerFor = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := listenTestConfig(t)
			valid, err := New(cfg)
			if err != nil {
				t.Fatalf("New rejected valid control configuration: %v", err)
			}
			if valid == nil {
				t.Fatal("New returned nil for valid control configuration")
			}
			tc.edit(&cfg)
			listener, err := New(cfg)
			if err == nil {
				t.Fatalf("New accepted missing %s: %+v", tc.name, listener)
			}
		})
	}
}

func TestNewFillsDefaults(t *testing.T) {
	for _, nonPositive := range []int{0, -1} {
		t.Run(strconv.Itoa(nonPositive), func(t *testing.T) {
			cfg, _ := listenTestConfig(t)
			cfg.Interval, cfg.MaxAttempts = time.Duration(nonPositive), nonPositive
			cfg.ResultExt, cfg.Scope = "", ""
			listener, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if listener == nil {
				t.Fatal("New returned nil without an error")
			}
			if listener.cfg.Interval != 60*time.Second || listener.cfg.MaxAttempts != 3 || listener.cfg.ResultExt != "json" || listener.cfg.Scope != "96e422caf0d6" {
				t.Errorf("unexpected defaults: %+v", listener.cfg)
			}
			listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
		})
	}
}

func TestNewPreservesExplicitConfiguration(t *testing.T) {
	cfg, _ := listenTestConfig(t)
	cfg.ResultExt, cfg.Interval, cfg.MaxAttempts = "sarif", 2*time.Second, 5
	cfg.Once, cfg.IncludeDrafts, cfg.BaseBranch = true, true, "main"
	listener, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if listener == nil {
		t.Fatal("New returned nil without an error")
	}
	got := listener.cfg
	if got.Interval != cfg.Interval || got.MaxAttempts != cfg.MaxAttempts || got.ResultExt != cfg.ResultExt || got.Scope != cfg.Scope || got.Dir != cfg.Dir || got.StateDir != cfg.StateDir || got.Once != cfg.Once || got.IncludeDrafts != cfg.IncludeDrafts || got.BaseBranch != cfg.BaseBranch || !reflect.DeepEqual(got.Repos, cfg.Repos) {
		t.Errorf("New changed explicit configuration: %+v", got)
	}
	listener.cfg.Review = func(ctx context.Context, job Job) error {
		if filepath.Ext(job.ResultPath) != ".sarif" {
			t.Errorf("result extension = %q, want .sarif", filepath.Ext(job.ResultPath))
		}
		return listenTestWriteResult(ctx, job)
	}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
}

func TestPollRejectsInvalidHeadBeforeCreatingPaths(t *testing.T) {
	for _, sha := range []string{"../x", "", strings.Repeat("a", 39), strings.Repeat("a", 65), strings.Repeat("A", 40), strings.Repeat("g", 40)} {
		t.Run(fmt.Sprintf("sha_%q", sha), func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			pull.HeadSHA = sha
			fake.repos["o/r"] = listenTestListing{pulls: []prsnapshot.Pull{pull}}
			var logs bytes.Buffer
			cfg.Log = &logs
			calls := 0
			cfg.Review = func(context.Context, Job) error { calls++; return errors.New("invalid head reached Review") }
			listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Skipped: 1})
			if calls != 0 || logs.Len() == 0 {
				t.Errorf("Review calls = %d, log = %q", calls, logs.String())
			}
			for _, root := range []string{cfg.Dir, cfg.StateDir} {
				if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !entry.IsDir() {
						t.Errorf("invalid head created file %s", path)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPollFailsClosedOnMalformedAttempts(t *testing.T) {
	for _, value := range []string{"", "not-a-counter", "-1", "1.5", "99999999999999999999999999999999999999"} {
		t.Run(fmt.Sprintf("counter_%q", value), func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			path := listenTestStatePath(cfg, "attempts", pull)
			listenTestWrite(t, path, value)
			var logs bytes.Buffer
			cfg.Log = &logs
			calls := 0
			cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
			listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Skipped: 1})
			if calls != 0 || logs.Len() == 0 || string(listenTestRead(t, path)) != value {
				t.Errorf("malformed attempts did not fail closed: calls=%d, log=%q", calls, logs.String())
			}
			listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
		})
	}
}

func listenTestRequireUnixPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission denial requires Unix permissions and a non-root user")
	}
}

func TestPollFailsClosedOnUnreadableAttempts(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	path := listenTestStatePath(cfg, "attempts", pull)
	listenTestWrite(t, path, "0")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	var logs bytes.Buffer
	cfg.Log = &logs
	calls := 0
	cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Skipped: 1})
	if calls != 0 || logs.Len() == 0 {
		t.Errorf("unreadable attempts did not fail closed: calls=%d, log=%q", calls, logs.String())
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
}

func TestPollCountsDoneWriteErrorAsFailedAttempt(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	donePath := listenTestStatePath(cfg, "done", pull)
	doneDir := filepath.Dir(donePath)
	if err := os.MkdirAll(doneDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(doneDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(doneDir, 0o700) })
	var logs bytes.Buffer
	cfg.Log = &logs
	calls, hooks := 0, 0
	cfg.Review = func(ctx context.Context, job Job) error { calls++; return listenTestWriteResult(ctx, job) }
	cfg.OnReviewed = func(context.Context, Job) error { hooks++; return nil }
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1})
	if calls != 1 || hooks != 0 || logs.Len() == 0 {
		t.Errorf("done write failure: reviews=%d, hooks=%d, log=%q", calls, hooks, logs.String())
	}
	listenTestAttempts(t, cfg, pull, 1)
	listenTestAbsent(t, donePath)
	// The finished review stays recoverable until its outcome is recorded.
	listenTestRead(t, listenTestStatePath(cfg, "inflight", pull))
	if err := os.Chmod(doneDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	if calls != 1 || hooks != 1 {
		t.Errorf("after the done failure: reviews=%d, hooks=%d; want the result adopted", calls, hooks)
	}
	listenTestRead(t, donePath)
	listenTestAbsent(t, listenTestStatePath(cfg, "inflight", pull))
}

func TestPollRejectsMissingOrEmptyReviewResult(t *testing.T) {
	for _, emptyFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty_file_%v", emptyFile), func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			var logs bytes.Buffer
			cfg.Log = &logs
			hooks, calls := 0, 0
			cfg.OnReviewed = func(context.Context, Job) error { hooks++; return nil }
			cfg.Review = func(_ context.Context, job Job) error {
				calls++
				if emptyFile {
					return os.WriteFile(job.ResultPath, nil, 0o600)
				}
				return nil
			}
			listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1})
			if calls != 1 || hooks != 0 || !strings.Contains(logs.String(), "review produced no result file") {
				t.Errorf("invalid result: calls=%d, hooks=%d, log=%q", calls, hooks, logs.String())
			}
			listenTestAttempts(t, cfg, pull, 1)
			listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
		})
	}
}

func listenTestInflight(t *testing.T, cfg Config, pull prsnapshot.Pull, result, success, log, listenerDir, startedAt string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{
		"result": result, "success": success, "log": log, "listener_dir": listenerDir, "started_at": startedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	listenTestWrite(t, listenTestStatePath(cfg, "inflight", pull), string(data))
}

func TestPollAdoptsOrphanedResult(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	orphanDir := t.TempDir()
	result, log := filepath.Join(orphanDir, "result.json"), filepath.Join(orphanDir, "review.log")
	success := filepath.Join(orphanDir, "review.ok")
	listenTestWrite(t, result, `{"review":"orphan"}`)
	listenTestWrite(t, success, "")
	info, err := os.Stat(result)
	if err != nil {
		t.Fatal(err)
	}
	listenTestInflight(t, cfg, pull, result, success, log, orphanDir, info.ModTime().UTC().Format(time.RFC3339Nano))
	listenTestWrite(t, listenTestStatePath(cfg, "attempts", pull), strconv.Itoa(cfg.MaxAttempts))
	calls, hooks := 0, 0
	cfg.Review = func(context.Context, Job) error { calls++; return errors.New("orphan reviewed again") }
	cfg.OnReviewed = func(_ context.Context, job Job) error {
		hooks++
		if job.ResultPath != result || job.LogPath != log || job.Key != listenTestKey(cfg, pull) || job.Claim == nil {
			t.Errorf("adopted job = %+v", job)
		}
		listenTestRead(t, listenTestStatePath(cfg, "done", pull))
		listenTestAbsent(t, listenTestStatePath(cfg, "inflight", pull))
		return nil
	}
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	var done doneRecord
	if err := json.Unmarshal(listenTestRead(t, listenTestStatePath(cfg, "done", pull)), &done); err != nil {
		t.Fatal(err)
	}
	if done.Result != result || done.Log != log || done.ListenerDir != orphanDir || done.Pull != pull.Number || done.HeadSHA != pull.HeadSHA {
		t.Errorf("adopted done record = %+v", done)
	}
	listenTestAbsent(t, listenTestStatePath(cfg, "attempts", pull))
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	if calls != 0 || hooks != 1 {
		t.Errorf("Review calls = %d, hooks = %d; want 0, 1", calls, hooks)
	}
}

func TestPollCountsStaleInflightBeforeRetry(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "old", "invalid timestamp", "invalid JSON", "no success marker", "old success marker", "success marker is a directory"} {
		t.Run(kind, func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			orphanDir := t.TempDir()
			result, success := filepath.Join(orphanDir, "orphan.json"), filepath.Join(orphanDir, "orphan.ok")
			started := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
			successTime := started.Add(time.Second)
			switch kind {
			case "no success marker":
			case "success marker is a directory":
				if err := os.Mkdir(success, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "old success marker" {
					successTime = started.Add(-time.Second)
				}
				listenTestWrite(t, success, "")
				if err := os.Chtimes(success, successTime, successTime); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "missing" {
				content := "result"
				if kind == "empty" {
					content = ""
				}
				listenTestWrite(t, result, content)
				mtime := started.Add(time.Second)
				if kind == "old" {
					mtime = started.Add(-time.Second)
				}
				if err := os.Chtimes(result, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}
			stamp := started.Format(time.RFC3339Nano)
			if kind == "invalid timestamp" {
				stamp = "invalid"
			}
			listenTestInflight(t, cfg, pull, result, success, "orphan.log", "orphan", stamp)
			if kind == "invalid JSON" {
				listenTestWrite(t, listenTestStatePath(cfg, "inflight", pull), "{")
			}
			calls := 0
			cfg.Review = func(ctx context.Context, job Job) error {
				calls++
				listenTestAttempts(t, cfg, pull, 1)
				return listenTestWriteResult(ctx, job)
			}
			listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1, Reviewed: 1})
			if calls != 1 {
				t.Errorf("Review calls = %d, want 1", calls)
			}
			listenTestAbsent(t, listenTestStatePath(cfg, "inflight", pull))
		})
	}
}

func TestPollStaleInflightExhaustsAttempts(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	listenTestInflight(t, cfg, pull, filepath.Join(t.TempDir(), "missing"), "", "log", "orphan", "2026-01-02T03:04:05Z")
	listenTestWrite(t, listenTestStatePath(cfg, "attempts", pull), strconv.Itoa(cfg.MaxAttempts-1))
	calls := 0
	cfg.Review = func(context.Context, Job) error { calls++; return errors.New("exhausted head reviewed") }
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1, Skipped: 1})
	listenTestAttempts(t, cfg, pull, cfg.MaxAttempts)
	listenTestAbsent(t, listenTestStatePath(cfg, "inflight", pull))
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	if calls != 0 {
		t.Errorf("Review calls = %d, want 0", calls)
	}
}

func TestPollRemovesInflightAfterReview(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "interrupted", "missing result"} {
		t.Run(outcome, func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			pull := fake.repos["o/r"].pulls[0]
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			calls := 0
			cfg.Review = func(ctx context.Context, job Job) error {
				calls++
				var record map[string]string
				if err := json.Unmarshal(listenTestRead(t, listenTestStatePath(cfg, "inflight", pull)), &record); err != nil {
					t.Fatal(err)
				}
				if record["result"] != job.ResultPath || record["log"] != job.LogPath || record["listener_dir"] != cfg.Dir {
					t.Errorf("inflight record = %v", record)
				}
				if started, err := time.Parse(time.RFC3339Nano, record["started_at"]); err != nil || started.IsZero() {
					t.Errorf("started_at = %q, want RFC3339Nano", record["started_at"])
				}
				switch outcome {
				case "failure":
					return errors.New("review failed")
				case "interrupted":
					cancel()
					return ctx.Err()
				case "missing result":
					return nil
				default:
					return listenTestWriteResult(ctx, job)
				}
			}
			got, err := (&Listener{cfg: cfg}).Poll(ctx)
			want := Stats{Seen: 1, Failed: 1}
			if outcome == "success" {
				want = Stats{Seen: 1, Reviewed: 1}
			} else if outcome == "interrupted" {
				want = Stats{Seen: 1}
			}
			if got != want || !errors.Is(err, ctx.Err()) || calls != 1 {
				t.Errorf("Poll = %+v, %v, calls=%d; want %+v, %v, 1", got, err, calls, want, ctx.Err())
			}
			listenTestAbsent(t, listenTestStatePath(cfg, "inflight", pull))
		})
	}
}

func TestPollLogsWhenAttemptsRunOut(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale_%v", stale), func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			cfg.MaxAttempts = 2
			pull := fake.repos["o/r"].pulls[0]
			var logs bytes.Buffer
			cfg.Log = &logs
			cfg.Review = func(context.Context, Job) error { return errors.New("review failed") }
			listener := &Listener{cfg: cfg}
			listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1})
			if strings.Contains(logs.String(), "giving up on") {
				t.Fatalf("gave up before the limit: %q", logs.String())
			}
			want := Stats{Seen: 1, Failed: 1}
			if stale {
				listenTestInflight(t, cfg, pull, filepath.Join(t.TempDir(), "missing"), "", "log", "orphan", "2026-01-02T03:04:05Z")
				want.Skipped = 1
			}
			listenTestPoll(t, listener, want)
			listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
			line := fmt.Sprintf("giving up on %s#%d @%s after %d failed attempts; delete %s to retry\n", cfg.Repos[0], pull.Number, pull.HeadSHA[:12], cfg.MaxAttempts, listenTestStatePath(cfg, "attempts", pull))
			if got := strings.Count(logs.String(), line); got != 1 {
				t.Errorf("giving-up line count = %d, want 1; log = %q", got, logs.String())
			}
		})
	}
}

func TestRunLogsEveryPoll(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	var logs bytes.Buffer
	cfg.Log = &logs
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fake.onList = func() {
		if len(fake.calls) == 2 {
			cancel()
		}
	}
	if err := (&Listener{cfg: cfg}).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var summaries []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.HasPrefix(line, "poll: ") {
			summaries = append(summaries, line)
		}
	}
	want := []string{
		"poll: 1 open, 1 reviewed, 0 failed, 0 skipped, 0 busy, 0 listing errors",
		"poll: 1 open, 0 reviewed, 0 failed, 0 skipped, 0 busy, 0 listing errors",
	}
	if !reflect.DeepEqual(summaries, want) {
		t.Errorf("poll summaries = %q, want %q", summaries, want)
	}
}

func TestRunOnceReportsPollFailures(t *testing.T) {
	for _, tc := range []struct{ failed, listingErrors int }{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		t.Run(fmt.Sprintf("failed_%d_errors_%d", tc.failed, tc.listingErrors), func(t *testing.T) {
			cfg, fake := listenTestConfig(t)
			cfg.Once = true
			if tc.failed != 0 {
				cfg.Review = func(context.Context, Job) error { return errors.New("review failed") }
			}
			if tc.listingErrors != 0 {
				bad := prsnapshot.RepoRef{Host: "github.com", Owner: "o", Repo: "bad"}
				cfg.Repos = append(cfg.Repos, bad)
				fake.repos["o/bad"] = listenTestListing{err: errors.New("listing failed")}
			}
			err := (&Listener{cfg: cfg}).Run(context.Background())
			if tc.failed+tc.listingErrors == 0 {
				if err != nil {
					t.Errorf("successful Run once = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d failed", tc.failed)) || !strings.Contains(err.Error(), fmt.Sprintf("%d listing errors", tc.listingErrors)) {
				t.Errorf("Run once = %v, want error mentioning %d failed and %d listing errors", err, tc.failed, tc.listingErrors)
			}
		})
	}
}

func TestRunLogsMixedPollSummary(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	cfg.Once = true
	pull := fake.repos["o/r"].pulls[0]
	failed, skipped, busy := pull, pull, pull
	failed.Number, skipped.Number, busy.Number = 8, 9, 10
	skipped.Draft = true
	fake.repos["o/r"] = listenTestListing{pulls: []prsnapshot.Pull{pull, failed, skipped, busy}}
	bad := prsnapshot.RepoRef{Host: "github.com", Owner: "o", Repo: "bad"}
	cfg.Repos = append(cfg.Repos, bad)
	fake.repos["o/bad"] = listenTestListing{err: errors.New("listing failed")}
	claim, acquired, err := flock.TryLock(listenTestStatePath(cfg, "locks", busy) + ".lock")
	if err != nil || !acquired {
		t.Fatalf("hold busy claim: acquired=%v, err=%v", acquired, err)
	}
	t.Cleanup(func() { _ = claim.Unlock() })
	cfg.Review = func(ctx context.Context, job Job) error {
		if job.Pull.Number == failed.Number {
			return errors.New("review failed")
		}
		return listenTestWriteResult(ctx, job)
	}
	var logs bytes.Buffer
	cfg.Log = &logs
	_ = (&Listener{cfg: cfg}).Run(context.Background())
	line := "poll: 4 open, 1 reviewed, 1 failed, 1 skipped, 1 busy, 1 listing errors\n"
	if strings.Count(logs.String(), line) != 1 {
		t.Errorf("missing mixed poll summary %q; log = %q", line, logs.String())
	}
}

func TestRunOnceCancellationOverridesPollFailures(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	cfg.Once = true
	bad := prsnapshot.RepoRef{Host: "github.com", Owner: "o", Repo: "bad"}
	cfg.Repos = append([]prsnapshot.RepoRef{bad}, cfg.Repos...)
	fake.repos["o/bad"] = listenTestListing{err: errors.New("listing failed")}
	pull := fake.repos["o/r"].pulls[0]
	second := pull
	second.Number++
	fake.repos["o/r"] = listenTestListing{pulls: []prsnapshot.Pull{pull, second}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg.Review = func(ctx context.Context, job Job) error {
		if job.Pull.Number == second.Number {
			cancel()
			return ctx.Err()
		}
		return errors.New("review failed")
	}
	var logs bytes.Buffer
	cfg.Log = &logs
	if err := (&Listener{cfg: cfg}).Run(ctx); err != nil {
		t.Errorf("cancelled Run once = %v, want nil", err)
	}
	line := "poll: 2 open, 0 reviewed, 1 failed, 0 skipped, 0 busy, 1 listing errors\n"
	if strings.Count(logs.String(), line) != 1 {
		t.Errorf("missing cancelled poll summary %q; log = %q", line, logs.String())
	}
}

func TestPollStaleInflightCountsAttemptBeyondLimit(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	listenTestInflight(t, cfg, pull, filepath.Join(t.TempDir(), "missing"), "", "log", "orphan", "2026-01-02T03:04:05Z")
	listenTestWrite(t, listenTestStatePath(cfg, "attempts", pull), strconv.Itoa(cfg.MaxAttempts))
	cfg.Review = func(context.Context, Job) error { t.Fatal("exhausted head reviewed"); return nil }
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1, Skipped: 1})
	listenTestAttempts(t, cfg, pull, cfg.MaxAttempts+1)
	listenTestAbsent(t, listenTestStatePath(cfg, "inflight", pull))
}

func TestPollGivesUpWhenAttemptsCannotBeRecorded(t *testing.T) {
	listenTestRequireUnixPermissions(t)
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	attemptsDir := filepath.Dir(listenTestStatePath(cfg, "attempts", pull))
	if err := os.MkdirAll(attemptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(attemptsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(attemptsDir, 0o700) })
	var logs bytes.Buffer
	cfg.Log = &logs
	calls := 0
	cfg.Review = func(context.Context, Job) error { calls++; return errors.New("review failed") }
	listener := &Listener{cfg: cfg}
	listenTestPoll(t, listener, Stats{Seen: 1, Failed: 1})
	listenTestPoll(t, listener, Stats{Seen: 1, Skipped: 1})
	if calls != 1 {
		t.Errorf("Review calls = %d; an uncountable failure must not be retried forever", calls)
	}
}

func TestNewRejectsUnsafeScopeAndResultExtension(t *testing.T) {
	for _, tc := range []struct{ scope, ext string }{
		{"../escape", "json"}, {"a/b", "json"}, {`a\b`, "json"}, {"..", "json"},
		{"012345abcdef", "../x"}, {"012345abcdef", "j/son"}, {"012345abcdef", "."},
	} {
		t.Run(tc.scope+"/"+tc.ext, func(t *testing.T) {
			cfg, _ := listenTestConfig(t)
			cfg.Scope, cfg.ResultExt = tc.scope, tc.ext
			if _, err := New(cfg); err == nil {
				t.Errorf("New accepted Scope %q and ResultExt %q", tc.scope, tc.ext)
			}
		})
	}
}

func TestPollCountsNilListerAsListingError(t *testing.T) {
	cfg, _ := listenTestConfig(t)
	var logs bytes.Buffer
	cfg.Log = &logs
	cfg.ListerFor = func(prsnapshot.RepoRef) Lister { return nil }
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Errors: 1})
	if !strings.Contains(logs.String(), "o/r") {
		t.Errorf("log = %q, want the repository named", logs.String())
	}
}

func TestPollRejectsNonRegularReviewResult(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	cfg.Review = func(_ context.Context, job Job) error {
		if err := os.MkdirAll(filepath.Join(job.ResultPath, "inside"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(job.SuccessPath, nil, 0o600)
	}
	listenTestPoll(t, &Listener{cfg: cfg}, Stats{Seen: 1, Failed: 1})
	listenTestAbsent(t, listenTestStatePath(cfg, "done", pull))
}

func TestPollRunsHookWithAContextShutdownCannotCancel(t *testing.T) {
	cfg, _ := listenTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.Review = func(ctx context.Context, job Job) error {
		cancel()
		return listenTestWriteResult(ctx, job)
	}
	var hookErr error
	hooks := 0
	cfg.OnReviewed = func(ctx context.Context, _ Job) error {
		hooks++
		hookErr = ctx.Err()
		return nil
	}
	_, _ = (&Listener{cfg: cfg}).Poll(ctx)
	if hooks != 1 || hookErr != nil {
		t.Errorf("hooks = %d, hook context error = %v; want one hook with a live context", hooks, hookErr)
	}
}

func TestPollClearsStaleSuccessMarkerBeforeReviewing(t *testing.T) {
	cfg, fake := listenTestConfig(t)
	pull := fake.repos["o/r"].pulls[0]
	seen := false
	cfg.Review = func(ctx context.Context, job Job) error {
		if _, err := os.Stat(job.SuccessPath); err == nil {
			seen = true
		}
		return listenTestWriteResult(ctx, job)
	}
	listener := &Listener{cfg: cfg}
	job := listener.newJob(cfg.Repos[0], pull, listenTestKey(cfg, pull), nil)
	listenTestWrite(t, job.SuccessPath, "")
	listenTestPoll(t, listener, Stats{Seen: 1, Reviewed: 1})
	if seen {
		t.Error("a success marker from an earlier attempt was still present during the review")
	}
}
