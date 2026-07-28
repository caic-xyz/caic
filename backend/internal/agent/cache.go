// Shared disk cache for per-harness model inventories.

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

const cacheMaxAge = 24 * time.Hour

// HarnessCacheEntry holds cached data for a single harness.
type HarnessCacheEntry struct {
	Inventory ModelInventory `json:"inventory"`
	Updated   time.Time      `json:"updated"`
	EnvHash   string         `json:"env_hash,omitempty"` // SHA-256 of *_API_KEY env vars from config.toml
}

// HarnessCache is a thread-safe disk-backed cache for per-harness model
// inventories. The file is shared across harnesses; each harness owns its own
// key.
type HarnessCache struct {
	mu   sync.Mutex
	path string
	data map[harness.Name]*HarnessCacheEntry
}

// OpenHarnessCache loads the cache from path. A missing or corrupt file
// starts with an empty cache — no error is returned.
func OpenHarnessCache(path string) *HarnessCache {
	c := &HarnessCache{path: path, data: make(map[harness.Name]*HarnessCacheEntry)}
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the server's cache directory, not user input
	if err != nil {
		return c
	}
	_ = json.Unmarshal(raw, &c.data)
	return c
}

// CachedModelInventory loads a harness inventory from cacheDir. An empty
// cacheDir returns an empty inventory.
func CachedModelInventory(cacheDir string, h harness.Name, envVars []string) ModelInventory {
	if cacheDir == "" {
		return ModelInventory{}
	}
	inventory, _ := OpenHarnessCache(filepath.Join(cacheDir, "harnesses.json")).ModelInventory(h, APIKeyHash(envVars))
	return inventory
}

// ModelInventory returns the cached inventory for h and whether it is fresh
// (updated within the last 24 h) and its API-key hash matches envHash. Invalid
// inventory entries are treated as unavailable.
func (c *HarnessCache) ModelInventory(h harness.Name, envHash string) (inventory ModelInventory, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.data[h]
	if e == nil || !e.Inventory.valid() || e.EnvHash != envHash {
		return ModelInventory{}, false
	}
	return e.Inventory, time.Since(e.Updated) < cacheMaxAge
}

// SetModelInventory updates the cache for h and writes to disk atomically.
func (c *HarnessCache) SetModelInventory(h harness.Name, inventory ModelInventory, envHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data[h] = &HarnessCacheEntry{
		Inventory: inventory,
		Updated:   time.Now(),
		EnvHash:   envHash,
	}
	c.flush()
}

// APIKeyHash computes a deterministic SHA-256 hex digest of the *_API_KEY
// environment variable entries from the harness env list (KEY=VALUE pairs).
// Variables whose name does not end with _API_KEY are ignored. An empty
// input produces an empty hash.
func APIKeyHash(envVars []string) string {
	if len(envVars) == 0 {
		return ""
	}
	sorted := make([]string, 0, len(envVars))
	for _, kv := range envVars {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.HasSuffix(k, "_API_KEY") {
			sorted = append(sorted, kv)
		}
	}
	if len(sorted) == 0 {
		return ""
	}
	slices.Sort(sorted)
	h := sha256.New()
	for i, kv := range sorted {
		if i > 0 {
			h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(kv))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *HarnessCache) flush() {
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
