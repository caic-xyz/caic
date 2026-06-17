// DPoP proof replay prevention via a bounded jti seen-set (RFC 9449 §11.1).

package oauthserver

import (
	"sync"
	"time"
)

// maxDPoPJTIEntries bounds the jti seen-set so a flood of proofs cannot grow it
// without limit. When the cap is reached a prune runs first; if it still does
// not fit, the oldest-expiring entries are dropped.
const maxDPoPJTIEntries = 100_000

// DPoPJTICache tracks recently seen DPoP proof jti values to reject replays.
//
// The resource server checks and inserts each proof's jti; a duplicate within
// the TTL window is a replay. Entries expire after ttl (the DPoP proof max age)
// because a proof older than that is already rejected on its iat.
type DPoPJTICache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

// NewDPoPJTICache returns a jti replay cache with the given entry TTL.
func NewDPoPJTICache(ttl time.Duration) *DPoPJTICache {
	if ttl <= 0 {
		ttl = defaultDPoPMaxAge
	}
	return &DPoPJTICache{
		seen: map[string]time.Time{},
		ttl:  ttl,
	}
}

// CheckAndStore records jti and reports whether it is fresh.
//
// It returns true the first time a jti is seen and false on any subsequent
// sighting within the TTL window (a replay).
func (c *DPoPJTICache) CheckAndStore(jti string) bool {
	if jti == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked()
	if expiry, ok := c.seen[jti]; ok && !time.Now().After(expiry) {
		return false
	}
	if len(c.seen) >= maxDPoPJTIEntries {
		c.evictOldestLocked()
	}
	c.seen[jti] = time.Now().Add(c.ttl)
	return true
}

func (c *DPoPJTICache) pruneLocked() {
	now := time.Now()
	for jti, exp := range c.seen {
		if now.After(exp) {
			delete(c.seen, jti)
		}
	}
}

// evictOldestLocked drops the entry with the earliest expiry to make room.
func (c *DPoPJTICache) evictOldestLocked() {
	var oldestJTI string
	var oldestExp time.Time
	for jti, exp := range c.seen {
		if oldestJTI == "" || exp.Before(oldestExp) {
			oldestJTI, oldestExp = jti, exp
		}
	}
	if oldestJTI != "" {
		delete(c.seen, oldestJTI)
	}
}
