// Tests for DPoP nonce management.

package oauth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDPoPNonceManager(t *testing.T) {
	t.Parallel()
	t.Run("valid issue and validate", func(t *testing.T) {
		t.Parallel()
		m := NewDPoPNonceManager(time.Minute)
		nonce, err := m.Issue()
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if nonce == "" {
			t.Fatal("Issue returned empty nonce")
		}
		if !m.Validate(nonce) {
			t.Fatal("Validate(issued nonce) = false, want true")
		}
		// One-time use: the second validation must fail.
		if m.Validate(nonce) {
			t.Fatal("Validate(consumed nonce) = true, want false")
		}
	})

	t.Run("error RNG failure fails closed", func(t *testing.T) { //nolint:paralleltest // mutates package-global randRead; must run serially
		// Override the entropy source to force a failure.
		orig := randRead
		t.Cleanup(func() { randRead = orig })
		randRead = func([]byte) (int, error) {
			return 0, errors.New("rng failure")
		}

		m := NewDPoPNonceManager(time.Minute)
		nonce, err := m.Issue()
		if err == nil {
			t.Fatal("Issue with failing RNG = nil error, want error")
		}
		if nonce != "" {
			t.Fatalf("Issue with failing RNG returned nonce %q, want empty", nonce)
		}
		// The deleted fallback produced a 0xAA-padded value; guard against it.
		if strings.Contains(nonce, "qqqq") {
			t.Fatalf("Issue returned a 0xAA-padded fallback nonce: %q", nonce)
		}
	})
}
