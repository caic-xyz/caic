// Package forgecache provides a persistent cache for CI check-run results from
// code hosting forges (GitHub, GitLab, etc.). Only terminal results (all checks
// completed) are stored. The cache is backed by a single JSON file and is safe
// for concurrent use.
package forgecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// Result is the cached outcome for a commit SHA.
// Only written once all check-runs for that SHA have completed.
type Result struct {
	Status   forge.CIStatus `json:"status"`
	Checks   []forge.Check  `json:"checks,omitempty"`
	CachedAt time.Time      `json:"cachedAt"`
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

// fileData is the on-disk format.
type fileData struct {
	Results  map[string]Result    `json:"results"`
	Notified map[string]time.Time `json:"notified,omitempty"`
}

// Open loads or creates a Cache backed by path. If path is empty, the cache
// operates in-memory only (no persistence). Returns a functional empty cache
// if the file does not exist or cannot be parsed.
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
	var f fileData
	if err := json.Unmarshal(raw, &f); err != nil {
		// Corrupted — start fresh rather than failing startup.
		return c, nil //nolint:nilerr // intentional: treat corrupt cache as empty
	}
	cutoff := time.Now().Add(-maxAge)
	for k, r := range f.Results {
		if !r.CachedAt.IsZero() && r.CachedAt.Before(cutoff) {
			continue
		}
		c.data[k] = r
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
	raw, err := json.MarshalIndent(fileData{Results: c.data, Notified: c.notified}, "", "  ")
	if err != nil {
		return fmt.Errorf("forgecache marshal: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("forgecache mkdir: %w", err)
	}
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
