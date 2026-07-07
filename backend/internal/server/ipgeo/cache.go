// Disk cache for named IP origin CIDR ranges.

package ipgeo

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"time"
)

const originCacheMaxAge = 24 * time.Hour

type originCacheEntry struct {
	Updated  time.Time `json:"updated"`
	Prefixes []string  `json:"prefixes"`
}

type originCache struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	data map[string]originCacheEntry
}

func openOriginCache(path string) (*originCache, error) {
	c := &originCache{path: path, now: time.Now, data: make(map[string]originCacheEntry)}
	raw, err := os.ReadFile(path) //nolint:gosec // path is derived from the server's cache directory, not user input.
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(raw, &c.data); err != nil {
		return c, fmt.Errorf("decode %s: %w", path, err)
	}
	return c, nil
}

func (c *originCache) fresh(name string) ([]netip.Prefix, bool) {
	prefixes, updated, ok := c.get(name)
	if !ok || c.now().Sub(updated) >= originCacheMaxAge {
		return nil, false
	}
	return prefixes, true
}

func (c *originCache) stale(name string) ([]netip.Prefix, bool) {
	prefixes, _, ok := c.get(name)
	return prefixes, ok
}

func (c *originCache) get(name string) ([]netip.Prefix, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[name]
	if !ok || len(e.Prefixes) == 0 {
		return nil, time.Time{}, false
	}
	prefixes := make([]netip.Prefix, 0, len(e.Prefixes))
	for _, cidr := range e.Prefixes {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, time.Time{}, false
		}
		prefixes = append(prefixes, p.Masked())
	}
	return prefixes, e.Updated, true
}

func (c *originCache) set(name string, prefixes []netip.Prefix) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefixStrings := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		prefixStrings = append(prefixStrings, p.Masked().String())
	}
	c.data[name] = originCacheEntry{Updated: c.now(), Prefixes: prefixStrings}
	return c.flushLocked()
}

func (c *originCache) flushLocked() error {
	data, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
