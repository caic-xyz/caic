// Package tokens preserves cache-write counts by reported duration with immutable aggregation.
package tokens

import (
	"slices"
	"time"
)

// CacheWrite counts tokens written with one applied cache lifetime. TTL zero
// means unknown; it does not imply any provider's default duration.
type CacheWrite struct {
	TTL    time.Duration
	Tokens int64
}

// CacheWrites contains disjoint cache-write buckets. Treat the slice as immutable;
// operations return owned slices, so aggregation cannot change earlier snapshots.
type CacheWrites []CacheWrite

// NewCacheWrites partitions total writes, capping reported counts to the
// remaining total in order. Missing counts use remainderTTL (zero = unknown).
func NewCacheWrites(total int64, remainderTTL time.Duration, reported ...CacheWrite) CacheWrites {
	remaining := max(int64(0), total)
	if remaining == 0 {
		return nil
	}
	out := make(CacheWrites, 0, len(reported)+1)
	for _, b := range reported {
		n := min(remaining, max(int64(0), b.Tokens))
		if n > 0 {
			out = append(out, CacheWrite{TTL: b.TTL, Tokens: n})
			remaining -= n
		}
	}
	if remaining > 0 {
		out = append(out, CacheWrite{TTL: remainderTTL, Tokens: remaining})
	}
	return out.Add(nil)
}

// Total returns all cache-write tokens, including unknown lifetimes.
func (b CacheWrites) Total() int64 {
	var n int64
	for _, v := range b {
		n += v.Tokens
	}
	return n
}

// Count returns the tokens for exactly ttl, without inferring duration ranges.
func (b CacheWrites) Count(ttl time.Duration) int64 {
	var n int64
	for _, v := range b {
		if v.TTL == ttl {
			n += v.Tokens
		}
	}
	return n
}

// Add merges equal lifetimes into a sorted, independently owned slice.
func (b CacheWrites) Add(other CacheWrites) CacheWrites {
	if len(b)+len(other) == 0 {
		return nil
	}
	out := make(CacheWrites, 0, max(len(b), len(other)))
	for _, group := range []CacheWrites{b, other} {
		for _, v := range group {
			if v.Tokens == 0 {
				continue
			}
			i := slices.IndexFunc(out, func(e CacheWrite) bool { return e.TTL == v.TTL })
			if i < 0 {
				out = append(out, v)
			} else {
				out[i].Tokens += v.Tokens
			}
		}
	}
	slices.SortFunc(out, func(a, b CacheWrite) int {
		if a.TTL < b.TTL {
			return -1
		}
		if a.TTL > b.TTL {
			return 1
		}
		return 0
	})
	return out
}

// Subtract removes covered tokens without counting them again when an aggregate
// lacks the per-call duration detail. Unattributable residuals retain unknown TTL.
func (b CacheWrites) Subtract(covered CacheWrites) CacheWrites {
	remaining := max(int64(0), b.Total()-covered.Total())
	if remaining == 0 {
		return nil
	}
	var out CacheWrites
	for _, v := range b.Add(nil) {
		if v.TTL == 0 {
			continue
		}
		n := min(remaining, max(int64(0), v.Tokens-covered.Count(v.TTL)))
		if n > 0 {
			out = append(out, CacheWrite{TTL: v.TTL, Tokens: n})
			remaining -= n
		}
	}
	if remaining > 0 {
		out = append(out, CacheWrite{Tokens: remaining})
	}
	return out.Add(nil)
}
