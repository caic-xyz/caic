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
		if !limiter.Allow("current") {
			t.Fatal("current request was rejected")
		}
		if _, found := limiter.buckets["expired"]; found {
			t.Fatal("expired bucket was not pruned")
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
		if limiter.Allow("overflow") {
			t.Fatal("new identity was accepted beyond bucket capacity")
		}
		if len(limiter.buckets) != maxRateLimitBuckets {
			t.Fatalf("buckets = %d, want %d", len(limiter.buckets), maxRateLimitBuckets)
		}
		limiter.buckets["0"] = rateBucket{Start: now.Add(-2 * time.Hour), Count: 1}
		if !limiter.Allow("0") {
			t.Fatal("existing expired identity was rejected at capacity")
		}
	})
}
