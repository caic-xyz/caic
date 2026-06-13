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

func (l *rateLimiter) allow(key string) bool {
	if l == nil || l.limit <= 0 || l.window <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.buckets[key]
	if bucket.Start.IsZero() || now.Sub(bucket.Start) >= l.window {
		l.buckets[key] = rateBucket{Start: now, Count: 1}
		return true
	}
	if bucket.Count >= l.limit {
		return false
	}
	bucket.Count++
	l.buckets[key] = bucket
	return true
}
