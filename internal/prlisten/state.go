// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prlisten

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// doneRecord describes a reviewed unit: where its result and log are, and
// which listener produced them.
type doneRecord struct {
	Pull        int    `json:"pull"`
	HeadSHA     string `json:"head_sha"`
	Result      string `json:"result"`
	Log         string `json:"log"`
	ListenerDir string `json:"listener_dir"`
	FinishedAt  string `json:"finished_at"`
}

type inflightRecord struct {
	Result      string `json:"result"`
	Success     string `json:"success"`
	Log         string `json:"log"`
	ListenerDir string `json:"listener_dir"`
	StartedAt   string `json:"started_at"`
}

// stateStore keeps what the cooperating listeners agree on: the claim locks,
// the outcomes of the reviewed units and their attempt counters.
type stateStore struct {
	root string
}

func newStateStore(root string) stateStore {
	return stateStore{root: root}
}

// lockPath is the claim file of one unit.
func (s stateStore) lockPath(components []string, key string) string {
	return s.path(components, "locks", key+".lock")
}

// donePath is the file marking a reviewed unit.
func (s stateStore) donePath(components []string, key string) string {
	return s.path(components, "done", key)
}

// attemptsPath is the file counting the failed reviews of one unit.
func (s stateStore) attemptsPath(components []string, key string) string {
	return s.path(components, "attempts", key)
}

func (s stateStore) inflightPath(components []string, key string) string {
	return s.path(components, "inflight", key)
}

func (s stateStore) writeInflight(components []string, key string, record inflightRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode the inflight review: %w", err)
	}
	return writeFileAtomic(s.inflightPath(components, key), data)
}

func (s stateStore) clearInflight(components []string, key string) error {
	if err := os.Remove(s.inflightPath(components, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the inflight review: %w", err)
	}
	return nil
}

func (s stateStore) path(components []string, kind, name string) string {
	parts := make([]string, 0, len(components)+3)
	parts = append(parts, s.root)
	parts = append(parts, components...)
	parts = append(parts, kind, name)
	return filepath.Join(parts...)
}

// attempts returns how often the review of a unit failed. The second result
// is false when the counter is unusable, which makes the caller give the unit
// up instead of retrying it forever.
func (s stateStore) attempts(components []string, key string) (int, bool) {
	data, err := os.ReadFile(s.attemptsPath(components, key))
	if errors.Is(err, os.ErrNotExist) {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || count < 0 {
		return 0, false
	}
	return count, true
}

// writeAttempts records the number of failed reviews of a unit.
func (s stateStore) writeAttempts(components []string, key string, attempts int) error {
	return writeFileAtomic(s.attemptsPath(components, key), []byte(strconv.Itoa(attempts)))
}

// clearAttempts forgets the failed attempts of a unit that is done. A failed
// removal is harmless: the done record keeps the unit from being reviewed
// again.
func (s stateStore) clearAttempts(components []string, key string) {
	os.Remove(s.attemptsPath(components, key))
}

// writeDone marks a unit as reviewed.
func (s stateStore) writeDone(components []string, key string, record doneRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode the outcome: %w", err)
	}
	return writeFileAtomic(s.donePath(components, key), data)
}

type directorySyncer interface {
	Sync() error
	Close() error
}

// writeFileAtomic writes data through a temporary file in the same directory
// and renames it over path, so another listener never reads half a file.
func writeFileAtomic(path string, data []byte) error {
	return writeFileAtomicWithDirectory(path, data, func(dir string) (directorySyncer, error) {
		return os.Open(dir)
	})
}

func writeFileAtomicWithDirectory(path string, data []byte, openDirectory func(string) (directorySyncer, error)) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, path, err)
	}
	// Directory sync persists the rename; some platforms do not support it.
	if parent, err := openDirectory(dir); err == nil {
		_ = parent.Sync()
		_ = parent.Close()
	}
	return nil
}
