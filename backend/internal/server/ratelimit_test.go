// Tests fixed-window request rate limiting and bucket lifecycle bounds.

package server

import (
	"strconv"
	"testing"
	"time"
)

func TestRateLimiterBounds(t *testing.T) {
	t.Parallel()

	t.Run("expired buckets are pruned", func(t *testing.T) {
		t.Parallel()
		limiter := newRateLimiter(1, time.Minute)
		limiter.buckets["expired"] = rateBucket{Start: time.Now().Add(-2 * time.Minute), Count: 1}
		if _, ok := limiter.Allow("current"); !ok {
			t.Fatal("current request was rejected")
		}
		if _, found := limiter.buckets["expired"]; found {
			t.Fatal("expired bucket was not pruned")
		}
	})

	t.Run("a rejection waits until its window resets", func(t *testing.T) {
		t.Parallel()
		limiter := newRateLimiter(1, time.Minute)
		limiter.buckets["busy"] = rateBucket{Start: time.Now().Add(-50 * time.Second), Count: 1}
		limiter.nextPrune = time.Now().Add(time.Minute)
		wait, ok := limiter.Allow("busy")
		if ok || wait <= 9*time.Second || wait > 10*time.Second {
			t.Fatalf("Allow(busy) = %v, %v; want a rejection for about 10s", wait, ok)
		}
	})

	t.Run("new identities fail closed at capacity", func(t *testing.T) {
		t.Parallel()
		limiter := newRateLimiter(1, time.Hour)
		now := time.Now()
		limiter.nextPrune = now.Add(time.Hour)
		for i := range maxRateLimitBuckets {
			limiter.buckets[strconv.Itoa(i)] = rateBucket{Start: now, Count: 1}
		}
		if wait, ok := limiter.Allow("overflow"); ok || wait <= 0 || wait > time.Hour {
			t.Fatalf("Allow(overflow) = %v, %v; want a rejection until the next prune", wait, ok)
		}
		if len(limiter.buckets) != maxRateLimitBuckets {
			t.Fatalf("buckets = %d, want %d", len(limiter.buckets), maxRateLimitBuckets)
		}
		limiter.buckets["0"] = rateBucket{Start: now.Add(-2 * time.Hour), Count: 1}
		if _, ok := limiter.Allow("0"); !ok {
			t.Fatal("existing expired identity was rejected at capacity")
		}
	})
}
