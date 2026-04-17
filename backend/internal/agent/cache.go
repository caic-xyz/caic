// Shared disk cache for per-harness data (model lists).

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const cacheMaxAge = 24 * time.Hour

// HarnessCacheEntry holds cached data for a single harness.
type HarnessCacheEntry struct {
	Models  []string  `json:"models"`
	Updated time.Time `json:"updated"`
}

// HarnessCache is a thread-safe disk-backed cache for per-harness model lists.
// The file is shared across harnesses; each harness owns its own key.
type HarnessCache struct {
	mu   sync.Mutex
	path string
	data map[Harness]*HarnessCacheEntry
}

// OpenHarnessCache loads the cache from path. A missing or corrupt file
// starts with an empty cache — no error is returned.
func OpenHarnessCache(path string) *HarnessCache {
	c := &HarnessCache{path: path, data: make(map[Harness]*HarnessCacheEntry)}
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the server's cache directory, not user input
	if err != nil {
		return c
	}
	_ = json.Unmarshal(raw, &c.data)
	return c
}

// Models returns the cached model list for h and whether the entry is fresh
// (updated within the last 24 h).
func (c *HarnessCache) Models(h Harness) (models []string, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.data[h]
	if e == nil || len(e.Models) == 0 {
		return nil, false
	}
	return e.Models, time.Since(e.Updated) < cacheMaxAge
}

// SetModels updates the cache for h and writes to disk atomically.
func (c *HarnessCache) SetModels(h Harness, models []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[h] = &HarnessCacheEntry{Models: models, Updated: time.Now()}
	c.flush()
}

func (c *HarnessCache) flush() {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return
	}
	data, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}
