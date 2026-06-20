// Tests for voice gateway scoped service tokens.

package gomode

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestServiceScopedToken(t *testing.T) {
	t.Parallel()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	claims := ScopedTokenClaims{
		ServiceKind:       "caic",
		ServiceInstanceID: "home",
		BackendOrigin:     "https://caic.example.com",
		Subject:           "user-1",
		Capabilities:      []string{"voice.session"},
		Audience:          ScopedTokenAudience,
		Expiry:            time.Now().Add(time.Hour),
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		token, err := IssueServiceScopedToken(&claims, privateKey)
		if err != nil {
			t.Fatal(err)
		}
		got, err := VerifyServiceScopedToken(token, publicKey, ScopedTokenAudience)
		if err != nil {
			t.Fatal(err)
		}
		if got.ServiceKind != "caic" {
			t.Fatalf("ServiceKind = %q, want caic", got.ServiceKind)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		expired := claims
		expired.Expiry = time.Now().Add(-time.Minute)
		if _, err := IssueServiceScopedToken(&expired, privateKey); err == nil {
			t.Fatal("expected expired token error")
		}
		if _, err := IssueServiceScopedToken(nil, privateKey); err == nil {
			t.Fatal("expected nil claims error")
		}
	})
}

func TestServiceSigningPublicKey(t *testing.T) {
	t.Parallel()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		encoded, err := EncodeServiceSigningPublicKey(publicKey)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseServiceSigningPublicKey(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(publicKey, got) {
			t.Fatal("parsed public key does not match")
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseServiceSigningPublicKey("bogus"); err == nil {
			t.Fatal("expected public key parse error")
		}
	})
}
