package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	secret := []byte("mysecret")
	body := []byte(`{"action":"opened"}`)

	validSig := func(s, b []byte) string {
		mac := hmac.New(sha256.New, s)
		mac.Write(b)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	t.Run("valid signature passes", func(t *testing.T) {
		sig := validSig(secret, body)
		if err := VerifySignature(secret, body, sig); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	t.Run("wrong secret fails", func(t *testing.T) {
		sig := validSig([]byte("wrongsecret"), body)
		if err := VerifySignature(secret, body, sig); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("tampered body fails", func(t *testing.T) {
		sig := validSig(secret, body)
		tampered := []byte(`{"action":"deleted"}`)
		if err := VerifySignature(secret, tampered, sig); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("malformed sig format fails", func(t *testing.T) {
		for _, bad := range []string{
			"",
			"nosigprefix",
			"sha1=abc123",
			"sha256=notvalidhex!!",
		} {
			if err := VerifySignature(secret, body, bad); err == nil {
				t.Fatalf("expected error for sig %q, got nil", bad)
			}
		}
	})
}
