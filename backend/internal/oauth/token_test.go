// Tests for OAuth access-token signing and verification.

package oauth

import (
	"strings"
	"testing"
	"time"
)

func TestAccessTokenService(t *testing.T) {
	t.Parallel()

	const (
		issuer   = "https://caic.example.com"
		audience = "https://caic.example.com/api/caic/v1/mcp"
		scope    = "caic:mcp.read caic:tasks.read"
	)
	user := User{ID: "usr_1", Username: "alice", Provider: "github"}

	t.Run("valid issue and verify", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-valid")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-1")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		touched := ""
		claims, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), func(grantID string, _ time.Time) (bool, string, error) {
			touched = grantID
			return true, "test-client-1", nil
		}, func(subject string) (User, bool) {
			if subject != user.ID {
				return User{}, false
			}
			return user, true
		})
		if err != nil {
			t.Fatalf("VerifyAccessToken: %v", err)
		}
		if touched != "grant-1" || claims.Subject != user.ID || claims.User.Username != user.Username || strings.Join(claims.Scopes, " ") != scope || claims.ClientID != "test-client-1" {
			t.Fatalf("claims = %+v, touched = %q", claims, touched)
		}
	})

	t.Run("error invalid signature", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-signature")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		parts := strings.Split(token, ".")
		if parts[2][0] == 'A' {
			parts[2] = "B" + parts[2][1:]
		} else {
			parts[2] = "A" + parts[2][1:]
		}
		token = strings.Join(parts, ".")
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), nil, func(string) (User, bool) { return user, true }); err == nil || !strings.Contains(err.Error(), "invalid token signature") {
			t.Fatalf("VerifyAccessToken error = %v, want invalid token signature", err)
		}
	})

	t.Run("error wrong key id", func(t *testing.T) {
		t.Parallel()
		issuerSvc := newTestAccessTokenService(t, "kid-issuer")
		verifierSvc := newTestAccessTokenService(t, "kid-verifier")
		token, err := issuerSvc.IssueAccessToken(issuer, user, audience, scope, "")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := verifierSvc.VerifyAccessToken(token, issuer, audience, time.Now(), nil, func(string) (User, bool) { return user, true }); err == nil || !strings.Contains(err.Error(), "unknown token key id") {
			t.Fatalf("VerifyAccessToken error = %v, want unknown token key id", err)
		}
	})

	t.Run("error wrong issuer", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-issuer-check")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, "https://wrong.example.com", audience, time.Now(), nil, func(string) (User, bool) { return user, true }); err == nil || !strings.Contains(err.Error(), "invalid token issuer") {
			t.Fatalf("VerifyAccessToken error = %v, want invalid token issuer", err)
		}
	})

	t.Run("error wrong audience", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-audience-check")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, "https://caic.example.com/api/wrong", time.Now(), nil, func(string) (User, bool) { return user, true }); err == nil || !strings.Contains(err.Error(), "invalid token audience") {
			t.Fatalf("VerifyAccessToken error = %v, want invalid token audience", err)
		}
	})

	t.Run("error expired", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-expired")
		now := time.Now()
		token, err := svc.issueAccessTokenAt(issuer, user, audience, scope, "", now.Add(-2*time.Hour), now.Add(-time.Hour), nil)
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, now, nil, func(string) (User, bool) { return user, true }); err == nil || !strings.Contains(err.Error(), "token is not valid now") {
			t.Fatalf("VerifyAccessToken error = %v, want token is not valid now", err)
		}
	})

	t.Run("error inactive grant", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-inactive-grant")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-1")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), func(string, time.Time) (bool, string, error) { return false, "", nil }, func(string) (User, bool) { return user, true }); err == nil || !strings.Contains(err.Error(), "token grant is not active") {
			t.Fatalf("VerifyAccessToken error = %v, want token grant is not active", err)
		}
	})

	t.Run("key rotation", func(t *testing.T) {
		t.Parallel()

		svc := newTestAccessTokenService(t, "")
		oldKID := svc.currentKID

		// Issue token with the initial key.
		tokenOld, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-1")
		if err != nil {
			t.Fatalf("IssueAccessToken (old): %v", err)
		}
		if _, err := svc.VerifyAccessToken(tokenOld, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			func(subject string) (User, bool) {
				if subject != user.ID {
					return User{}, false
				}
				return user, true
			}); err != nil {
			t.Fatalf("VerifyAccessToken (old key): %v", err)
		}

		// Rotate the key.
		newKID, err := svc.RotateKey()
		if err != nil {
			t.Fatalf("RotateKey: %v", err)
		}
		if newKID == oldKID {
			t.Fatal("RotateKey returned same KID")
		}
		if svc.currentKID != newKID {
			t.Fatalf("currentKID = %q, want %q", svc.currentKID, newKID)
		}
		if _, ok := svc.keys[oldKID]; !ok {
			t.Fatal("old key was removed from active set")
		}
		if _, ok := svc.keys[newKID]; !ok {
			t.Fatal("new key was not added to active set")
		}
		if len(svc.keys) != 2 {
			t.Fatalf("keys count = %d, want 2", len(svc.keys))
		}

		// Old token should still verify (old key still in map).
		if _, err := svc.VerifyAccessToken(tokenOld, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			func(subject string) (User, bool) {
				if subject != user.ID {
					return User{}, false
				}
				return user, true
			}); err != nil {
			t.Fatalf("VerifyAccessToken (old token after rotate): %v", err)
		}

		// New token should use the rotated key.
		tokenNew, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-2")
		if err != nil {
			t.Fatalf("IssueAccessToken (new): %v", err)
		}
		if _, err := svc.VerifyAccessToken(tokenNew, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			func(subject string) (User, bool) {
				if subject != user.ID {
					return User{}, false
				}
				return user, true
			}); err != nil {
			t.Fatalf("VerifyAccessToken (new key): %v", err)
		}

		// Rotate again. Oldest key should still be valid.
		_, err = svc.RotateKey()
		if err != nil {
			t.Fatalf("RotateKey (second): %v", err)
		}
		if len(svc.keys) != 3 {
			t.Fatalf("keys count after second rotate = %d, want 3", len(svc.keys))
		}
		if _, err := svc.VerifyAccessToken(tokenOld, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			func(subject string) (User, bool) {
				if subject != user.ID {
					return User{}, false
				}
				return user, true
			}); err != nil {
			t.Fatalf("VerifyAccessToken (old token after second rotate): %v", err)
		}
	})
}

func newTestAccessTokenService(t *testing.T, kid string) *AccessTokenService {
	svc, err := NewAccessTokenService(nil, kid, time.Hour)
	if err != nil {
		t.Fatalf("NewAccessTokenService: %v", err)
	}
	return svc
}
