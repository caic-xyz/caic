// Package forgecache provides a persistent cache for CI check-run results from
// code hosting forges (GitHub, GitLab, etc.). Only terminal results (all checks
// completed) are stored. The cache is backed by a single JSON file and is safe
// for concurrent use.
package forgecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache/data"
)

// Result is the cached outcome for a commit SHA.
// Only written once all check-runs for that SHA have completed.
type Result struct {
	Status   forge.CIStatus
	Checks   []forge.Check
	CachedAt time.Time
}

// maxAge is the TTL for both cached results and notification records.
const maxAge = 7 * 24 * time.Hour

// Cache is a thread-safe persistent store of terminal CI results keyed by
// "owner/repo/sha". Pending states are never cached. The cache also tracks
// which (taskID, sha) pairs have already been notified to avoid duplicate
// messages to agents. Both maps are pruned on load: entries older than maxAge
// are discarded.
type Cache struct {
	mu   sync.Mutex
	path string // empty → in-memory only
	data map[string]Result
	// notified tracks taskID+sha pairs that have already had their CI result
	// sent to the agent. Value is the time the notification was recorded.
	// Persisted so dedup survives restarts.
	notified map[string]time.Time
}

// Open loads or creates a Cache backed by path. If path is empty, the cache
// operates in-memory only (no persistence). Returns a functional empty cache
// if the file does not exist or cannot be parsed. Malformed files are logged
// and discarded in full, including notification records; filesystem errors
// other than missing files are returned.
func Open(path string) (*Cache, error) {
	c := &Cache{path: path, data: make(map[string]Result), notified: make(map[string]time.Time)}
	if path == "" {
		return c, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from os.UserCacheDir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c, nil
		}
		return nil, fmt.Errorf("forgecache open %s: %w", path, err)
	}
	var f data.File
	if err := json.Unmarshal(raw, &f); err != nil {
		// Discard the complete file, including any partially decoded entries.
		slog.Warn("discarding malformed CI cache", "path", path, "err", err)
		return c, nil
	}
	cutoff := time.Now().Add(-maxAge)
	for k, r := range f.Results {
		if !r.CachedAt.IsZero() && r.CachedAt.Before(cutoff) {
			continue
		}
		c.data[k] = resultFromData(r)
	}
	for k, t := range f.Notified {
		if !t.IsZero() && t.Before(cutoff) {
			continue
		}
		c.notified[k] = t
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
	r.CachedAt = time.Now()
	c.data[cacheKey(owner, repo, sha)] = r
	if c.path == "" {
		return nil
	}
	return c.save()
}

// IsNotified reports whether a CI result has already been sent to the agent
// for the given task and SHA.
func (c *Cache) IsNotified(taskID, sha string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.notified[taskID+"/"+sha]
	return ok
}

// MarkNotified records that a CI result has been sent to the agent for the
// given task and SHA, and persists to disk.
func (c *Cache) MarkNotified(taskID, sha string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notified[taskID+"/"+sha] = time.Now()
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
	results := make(map[string]data.Result, len(c.data))
	for k, r := range c.data {
		results[k] = resultToData(r)
	}
	raw, err := json.MarshalIndent(data.File{Results: results, Notified: c.notified}, "", "  ")
	if err != nil {
		return fmt.Errorf("forgecache marshal: %w", err)
	}
	raw = append(raw, '\n')
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("forgecache write: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("forgecache rename: %w", err)
	}
	return nil
}

func resultFromData(r data.Result) Result {
	var checks []forge.Check
	if r.Checks != nil {
		checks = make([]forge.Check, len(r.Checks))
	}
	for i := range r.Checks {
		c := &r.Checks[i]
		checks[i] = forge.Check{
			Name:        c.Name,
			Owner:       c.Owner,
			Repo:        c.Repo,
			RunID:       c.RunID,
			JobID:       c.JobID,
			Status:      forge.CheckRunStatus(c.Status),
			Conclusion:  forge.CheckRunConclusion(c.Conclusion),
			Labels:      c.Labels,
			QueuedAt:    c.QueuedAt,
			StartedAt:   c.StartedAt,
			CompletedAt: c.CompletedAt,
		}
	}
	return Result{Status: forge.CIStatus(r.Status), Checks: checks, CachedAt: r.CachedAt}
}

func resultToData(r Result) data.Result {
	var checks []data.Check
	if r.Checks != nil {
		checks = make([]data.Check, len(r.Checks))
	}
	for i := range r.Checks {
		c := &r.Checks[i]
		checks[i] = data.Check{
			Name:        c.Name,
			Owner:       c.Owner,
			Repo:        c.Repo,
			RunID:       c.RunID,
			JobID:       c.JobID,
			Status:      data.CheckRunStatus(c.Status),
			Conclusion:  data.CheckRunConclusion(c.Conclusion),
			Labels:      c.Labels,
			QueuedAt:    c.QueuedAt,
			StartedAt:   c.StartedAt,
			CompletedAt: c.CompletedAt,
		}
	}
	return data.Result{Status: data.CIStatus(r.Status), Checks: checks, CachedAt: r.CachedAt}
}
