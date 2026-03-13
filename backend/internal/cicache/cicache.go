// Package cicache provides a persistent cache for CI check-run results from
// code hosting forges (GitHub, GitLab, etc.). Only terminal results (all checks
// completed) are stored. The cache is backed by a single JSON file and is safe
// for concurrent use.
package cicache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status is the outcome of a completed set of CI check-runs.
type Status string

// Terminal CI status values. Pending is not cached — only terminal statuses are stored.
const (
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
	StatusPending Status = "pending"
)

// CheckConclusion is the conclusion of a completed CI check run.
type CheckConclusion string

// CI check-run conclusion values.
const (
	CheckConclusionSuccess        CheckConclusion = "success"
	CheckConclusionFailure        CheckConclusion = "failure"
	CheckConclusionNeutral        CheckConclusion = "neutral"
	CheckConclusionSkipped        CheckConclusion = "skipped"
	CheckConclusionCancelled      CheckConclusion = "cancelled"
	CheckConclusionTimedOut       CheckConclusion = "timed_out"
	CheckConclusionActionRequired CheckConclusion = "action_required"
	CheckConclusionStale          CheckConclusion = "stale"
)

// ForgeCheck is a CI check run with its status, conclusion, and timing.
type ForgeCheck struct {
	Name        string          `json:"name"`
	Owner       string          `json:"owner"`
	Repo        string          `json:"repo"`
	RunID       int64           `json:"runID"`  // Pipeline/workflow run ID.
	JobID       int64           `json:"jobID"`  // Check run / job ID.
	Status      string          `json:"status"` // queued, in_progress, completed.
	Conclusion  CheckConclusion `json:"conclusion"`
	QueuedAt    time.Time       `json:"queuedAt,omitzero"`    // When the check was created/queued.
	StartedAt   time.Time       `json:"startedAt,omitzero"`   // When execution began.
	CompletedAt time.Time       `json:"completedAt,omitzero"` // When execution finished.
}

// Result is the cached outcome for a commit SHA.
// Only written once all check-runs for that SHA have completed.
type Result struct {
	Status   Status       `json:"status"`
	Checks   []ForgeCheck `json:"checks,omitempty"`
	CachedAt time.Time    `json:"cachedAt"`
}

// Cache is a thread-safe persistent store of terminal CI results keyed by
// "owner/repo/sha". Pending states are never cached.
type Cache struct {
	mu   sync.Mutex
	path string // empty → in-memory only
	data map[string]Result
}

// fileData is the on-disk format.
type fileData struct {
	Results map[string]Result `json:"results"`
}

// Open loads or creates a Cache backed by path. If path is empty, the cache
// operates in-memory only (no persistence). Returns a functional empty cache
// if the file does not exist or cannot be parsed.
func Open(path string) (*Cache, error) {
	c := &Cache{path: path, data: make(map[string]Result)}
	if path == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from os.UserCacheDir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("cicache open %s: %w", path, err)
	}
	var f fileData
	if err := json.Unmarshal(raw, &f); err != nil {
		// Corrupted — start fresh rather than failing startup.
		return c, nil //nolint:nilerr // intentional: treat corrupt cache as empty
	}
	if f.Results != nil {
		c.data = f.Results
	}
	return c, nil
}

// Get returns the cached Result for (owner, repo, sha), or (Result{}, false)
// on a cache miss.
func (c *Cache) Get(owner, repo, sha string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.data[cacheKey(owner, repo, sha)]
	return r, ok
}

// Put stores a terminal Result for (owner, repo, sha) and persists to disk.
func (c *Cache) Put(owner, repo, sha string, r Result) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[cacheKey(owner, repo, sha)] = r
	if c.path == "" {
		return nil
	}
	return c.save()
}

func cacheKey(owner, repo, sha string) string {
	return owner + "/" + repo + "/" + sha
}

// save writes the cache to disk atomically. Must be called with c.mu held.
func (c *Cache) save() error {
	raw, err := json.MarshalIndent(fileData{Results: c.data}, "", "  ")
	if err != nil {
		return fmt.Errorf("cicache marshal: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("cicache mkdir: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("cicache write: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cicache rename: %w", err)
	}
	return nil
}
