// Fixed-window request rate limiting.

package server

import (
	"sync"
	"time"
)

type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]rateBucket
}

type rateBucket struct {
	Start time.Time
	Count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, buckets: map[string]rateBucket{}}
}

// Allow reports whether key is within the fixed-window request limit.
func (r *rateLimiter) Allow(key string) bool {
	if r.limit <= 0 || r.window <= 0 {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := r.buckets[key]
	if bucket.Start.IsZero() || now.Sub(bucket.Start) >= r.window {
		r.buckets[key] = rateBucket{Start: now, Count: 1}
		return true
	}
	if bucket.Count >= r.limit {
		return false
	}
	bucket.Count++
	r.buckets[key] = bucket
	return true
}
