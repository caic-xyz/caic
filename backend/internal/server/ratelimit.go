// Bounded fixed-window request rate limiting with expired-bucket cleanup.

package server

import (
	"sync"
	"time"
)

const maxRateLimitBuckets = 100_000

type rateLimiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	buckets   map[string]rateBucket
	nextPrune time.Time
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
	if r.nextPrune.IsZero() || !now.Before(r.nextPrune) {
		for bucketKey, bucket := range r.buckets {
			if now.Sub(bucket.Start) >= r.window {
				delete(r.buckets, bucketKey)
			}
		}
		r.nextPrune = now.Add(r.window)
	}
	bucket, found := r.buckets[key]
	if bucket.Start.IsZero() || now.Sub(bucket.Start) >= r.window {
		if !found && len(r.buckets) >= maxRateLimitBuckets {
			return false
		}
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
