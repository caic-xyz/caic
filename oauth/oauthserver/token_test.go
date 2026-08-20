// Tests for OAuth access-token signing and verification.

package oauthserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func TestAccessTokenService(t *testing.T) {
	t.Parallel()

	const (
		issuer   = "https://caic.example.com"
		audience = "https://caic.example.com/api/caic/v1/mcp"
		scope    = "caic:mcp.read caic:tasks.read"
	)
	user := oauth.User{ID: "usr_1", Username: "alice", Provider: "github"}

	t.Run("valid issue and verify", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-valid")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-1")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}

		// Verify the header alg is ES256 (default key type).
		parts := strings.Split(token, ".")
		headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatalf("decode header: %v", err)
		}
		if !strings.Contains(string(headerJSON), "\"ES256\"") {
			t.Fatalf("header alg is not ES256: %s", string(headerJSON))
		}

		touched := ""
		claims, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), func(grantID string, _ time.Time) (bool, string, error) {
			touched = grantID
			return true, "test-client-1", nil
		}, &tokenTestSession{user: user})
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
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "invalid token signature") {
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
		if _, err := verifierSvc.VerifyAccessToken(token, issuer, audience, time.Now(), nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "unsupported token header") {
			t.Fatalf("VerifyAccessToken error = %v, want unsupported token header", err)
		}
	})

	t.Run("error wrong alg in header", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-wrong-alg")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		// Replace the alg in the header from ES256 to RS256.
		parts := strings.Split(token, ".")
		headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatalf("decode header: %v", err)
		}
		tamperedHeader := strings.Replace(string(headerJSON), "\"ES256\"", "\"RS256\"", 1)
		parts[0] = base64.RawURLEncoding.EncodeToString([]byte(tamperedHeader))
		token = strings.Join(parts, ".")
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "unsupported token header") {
			t.Fatalf("VerifyAccessToken error = %v, want unsupported token header", err)
		}
	})

	t.Run("error wrong issuer", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-issuer-check")
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "")
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, "https://wrong.example.com", audience, time.Now(), nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "invalid token issuer") {
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
		if _, err := svc.VerifyAccessToken(token, issuer, "https://caic.example.com/api/wrong", time.Now(), nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "invalid token audience") {
			t.Fatalf("VerifyAccessToken error = %v, want invalid token audience", err)
		}
	})

	t.Run("error expired", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-expired")
		now := time.Now()
		token, err := svc.issueAccessTokenAt(&oauth.AccessTokenClaims{
			Issuer:   issuer,
			Subject:  user.ID,
			Audience: audience,
			Username: user.Username,
			Scope:    scope,
			Type:     accessTokenType,
		}, now.Add(-2*time.Hour), now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, now, nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "token is not valid now") {
			t.Fatalf("VerifyAccessToken error = %v, want token is not valid now", err)
		}
	})

	t.Run("token 30s in the future is accepted", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-future-skew")
		now := time.Now()
		token, err := svc.issueAccessTokenAt(&oauth.AccessTokenClaims{
			Issuer:   issuer,
			Subject:  user.ID,
			Audience: audience,
			Username: user.Username,
			Scope:    scope,
			Type:     accessTokenType,
		}, now.Add(30*time.Second), now.Add(30*time.Second).Add(time.Hour))
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, now, nil, &tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken: %v", err)
		}
	})

	t.Run("token 2 min in the future is rejected", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-future-reject")
		now := time.Now()
		token, err := svc.issueAccessTokenAt(&oauth.AccessTokenClaims{
			Issuer:   issuer,
			Subject:  user.ID,
			Audience: audience,
			Username: user.Username,
			Scope:    scope,
			Type:     accessTokenType,
		}, now.Add(2*time.Minute), now.Add(2*time.Minute).Add(time.Hour))
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, now, nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "token is not valid now") {
			t.Fatalf("VerifyAccessToken error = %v, want token is not valid now", err)
		}
	})

	t.Run("token expired 30s ago with skew is accepted", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-expired-skew")
		now := time.Now()
		token, err := svc.issueAccessTokenAt(&oauth.AccessTokenClaims{
			Issuer:   issuer,
			Subject:  user.ID,
			Audience: audience,
			Username: user.Username,
			Scope:    scope,
			Type:     accessTokenType,
		}, now.Add(-time.Hour), now.Add(-30*time.Second))
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, now, nil, &tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken: %v", err)
		}
	})

	t.Run("token expired 2 min ago is rejected", func(t *testing.T) {
		t.Parallel()
		svc := newTestAccessTokenService(t, "kid-expired-reject")
		now := time.Now()
		token, err := svc.issueAccessTokenAt(&oauth.AccessTokenClaims{
			Issuer:   issuer,
			Subject:  user.ID,
			Audience: audience,
			Username: user.Username,
			Scope:    scope,
			Type:     accessTokenType,
		}, now.Add(-time.Hour), now.Add(-2*time.Minute))
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, now, nil, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "token is not valid now") {
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
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(), func(string, time.Time) (bool, string, error) { return false, "", nil }, &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "token grant is not active") {
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
			&tokenTestSession{user: user}); err != nil {
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
		// Verify the new key is ES256.
		if svc.keys[newKID].alg != "ES256" {
			t.Fatalf("new key alg = %q, want ES256", svc.keys[newKID].alg)
		}
		if _, ok := svc.keys[newKID].key.(*ecdsa.PrivateKey); !ok {
			t.Fatal("new key is not ECDSA")
		}

		// Verify oauth.JWK for the new key is EC.
		jwks := svc.JWK()
		for _, jwk := range jwks {
			if jwk.Kid == newKID {
				if jwk.Kty != "EC" {
					t.Fatalf("new key oauth.JWK kty = %q, want EC", jwk.Kty)
				}
				if jwk.Crv != "P-256" {
					t.Fatalf("new key oauth.JWK crv = %q, want P-256", jwk.Crv)
				}
			}
		}

		// Old token should still verify (old key still in map).
		if _, err := svc.VerifyAccessToken(tokenOld, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			&tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken (old token after rotate): %v", err)
		}

		// New token should use the rotated key (ES256).
		tokenNew, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-2")
		if err != nil {
			t.Fatalf("IssueAccessToken (new): %v", err)
		}
		if _, err := svc.VerifyAccessToken(tokenNew, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			&tokenTestSession{user: user}); err != nil {
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
			&tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken (old token after second rotate): %v", err)
		}
	})

	t.Run("RotateKeyWithAlg RS256", func(t *testing.T) {
		t.Parallel()

		svc := newTestAccessTokenService(t, "")
		// Default is ES256.
		if svc.keys[svc.currentKID].alg != "ES256" {
			t.Fatalf("default key alg = %q, want ES256", svc.keys[svc.currentKID].alg)
		}

		// Add an RSA key via RotateKeyWithAlg.
		rsaKID, err := svc.RotateKeyWithAlg("RS256")
		if err != nil {
			t.Fatalf("RotateKeyWithAlg(RS256): %v", err)
		}
		if svc.keys[rsaKID].alg != "RS256" {
			t.Fatalf("RS256 key alg = %q, want RS256", svc.keys[rsaKID].alg)
		}
		if _, ok := svc.keys[rsaKID].key.(*rsa.PrivateKey); !ok {
			t.Fatal("RotateKeyWithAlg(RS256) did not produce RSA key")
		}

		// Issue and verify with the RSA key.
		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-rsa")
		if err != nil {
			t.Fatalf("IssueAccessToken (RSA): %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			&tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken (RSA key): %v", err)
		}

		// Verify RSA header alg.
		parts := strings.Split(token, ".")
		headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatalf("decode header: %v", err)
		}
		if !strings.Contains(string(headerJSON), "\"RS256\"") {
			t.Fatalf("RSA header alg is not RS256: %s", string(headerJSON))
		}

		// RotateKeyWithAlg with unknown alg.
		if _, err := svc.RotateKeyWithAlg("BOGUS256"); err == nil {
			t.Fatal("RotateKeyWithAlg(BOGUS256) error = nil")
		}
	})

	t.Run("mixed RSA and EC JWKS", func(t *testing.T) {
		t.Parallel()

		svc := newTestAccessTokenService(t, "")
		// Add an RSA key.
		_, err := svc.RotateKeyWithAlg("RS256")
		if err != nil {
			t.Fatalf("RotateKeyWithAlg(RS256): %v", err)
		}
		// Add another EC key.
		_, err = svc.RotateKeyWithAlg("ES256")
		if err != nil {
			t.Fatalf("RotateKeyWithAlg(ES256): %v", err)
		}

		jwks := svc.JWK()
		if len(jwks) != 3 {
			t.Fatalf("jwks count = %d, want 3", len(jwks))
		}
		var rsaCount, ecCount int
		for _, jwk := range jwks {
			switch jwk.Kty {
			case "RSA":
				rsaCount++
				if jwk.N == "" || jwk.E == "" {
					t.Fatalf("RSA oauth.JWK missing n or e: %+v", jwk)
				}
				if jwk.Crv != "" || jwk.X != "" || jwk.Y != "" {
					t.Fatalf("RSA oauth.JWK has EC fields: %+v", jwk)
				}
			case "EC":
				ecCount++
				if jwk.Crv != "P-256" || jwk.X == "" || jwk.Y == "" {
					t.Fatalf("EC oauth.JWK missing crv/x/y: %+v", jwk)
				}
				if jwk.N != "" || jwk.E != "" {
					t.Fatalf("EC oauth.JWK has RSA fields: %+v", jwk)
				}
			default:
				t.Fatalf("unexpected kty: %s", jwk.Kty)
			}
		}
		if rsaCount != 1 {
			t.Fatalf("RSA jwk count = %d, want 1", rsaCount)
		}
		if ecCount != 2 {
			t.Fatalf("EC jwk count = %d, want 2", ecCount)
		}
	})

	t.Run("EC key from PEM", func(t *testing.T) {
		t.Parallel()

		// Generate an EC P-256 key and marshal to PKCS8 PEM.
		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate ec key: %v", err)
		}
		pkcs8DER, err := x509.MarshalPKCS8PrivateKey(ecKey)
		if err != nil {
			t.Fatalf("marshal pkcs8: %v", err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})

		svc, err := NewAccessTokenService(keyPEM, "pem-ec-kid", time.Hour)
		if err != nil {
			t.Fatalf("NewAccessTokenService (EC PEM): %v", err)
		}
		if svc.keys["pem-ec-kid"].alg != "ES256" {
			t.Fatalf("EC PEM key alg = %q, want ES256", svc.keys["pem-ec-kid"].alg)
		}

		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-ec-pem")
		if err != nil {
			t.Fatalf("IssueAccessToken (EC PEM): %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			&tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken (EC PEM key): %v", err)
		}
	})

	t.Run("RSA key from PEM", func(t *testing.T) {
		t.Parallel()

		// Generate an RSA key and marshal to PKCS1 PEM.
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate rsa key: %v", err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)})

		svc, err := NewAccessTokenService(keyPEM, "pem-rsa-kid", time.Hour)
		if err != nil {
			t.Fatalf("NewAccessTokenService (RSA PEM): %v", err)
		}
		if svc.keys["pem-rsa-kid"].alg != "RS256" {
			t.Fatalf("RSA PEM key alg = %q, want RS256", svc.keys["pem-rsa-kid"].alg)
		}

		token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-rsa-pem")
		if err != nil {
			t.Fatalf("IssueAccessToken (RSA PEM): %v", err)
		}
		if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(),
			func(grantID string, _ time.Time) (bool, string, error) {
				return true, "test-client-1", nil
			},
			&tokenTestSession{user: user}); err != nil {
			t.Fatalf("VerifyAccessToken (RSA PEM key): %v", err)
		}
	})

	t.Run("error empty PEM", func(t *testing.T) {
		t.Parallel()

		if _, err := NewAccessTokenService(nil, "kid-empty", time.Hour); err == nil {
			t.Fatal("NewAccessTokenService(nil) = nil error, want error")
		}
	})

	t.Run("ES token signature is raw R||S", func(t *testing.T) {
		t.Parallel()

		// RFC 7518 §3.4: the ES* signature is a fixed-size R||S concatenation, not
		// ASN.1/DER. These tokens are verified by third parties through the
		// published JWKS, so the wire format is part of the contract.
		for _, tc := range []struct {
			alg     string
			sigSize int
			crv     string
		}{
			{"ES256", 64, "P-256"},
			{"ES384", 96, "P-384"},
			{"ES512", 132, "P-521"},
		} {
			svc := newTestAccessTokenService(t, "")
			kid, err := svc.RotateKeyWithAlg(tc.alg)
			if err != nil {
				t.Fatalf("RotateKeyWithAlg(%s): %v", tc.alg, err)
			}
			token, err := svc.IssueAccessToken(issuer, user, audience, scope, "grant-"+tc.alg)
			if err != nil {
				t.Fatalf("IssueAccessToken(%s): %v", tc.alg, err)
			}
			parts := strings.Split(token, ".")
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatalf("decode %s signature: %v", tc.alg, err)
			}
			if len(sig) != tc.sigSize {
				t.Errorf("%s signature = %d bytes, want %d (raw R||S, not DER)", tc.alg, len(sig), tc.sigSize)
			}
			// A DER signature starts with the SEQUENCE tag 0x30; raw R||S must not
			// be parseable as one.
			if _, err := asn1.Unmarshal(sig, &struct{ R, S *big.Int }{}); err == nil {
				t.Errorf("%s signature parsed as ASN.1 DER, want raw R||S", tc.alg)
			}
			// The published JWK must name the curve the key actually uses.
			var found bool
			for _, jwk := range svc.JWK() {
				if jwk.Kid != kid {
					continue
				}
				found = true
				if jwk.Crv != tc.crv || jwk.Alg != tc.alg {
					t.Errorf("%s jwk = crv %q alg %q, want %q/%q", tc.alg, jwk.Crv, jwk.Alg, tc.crv, tc.alg)
				}
			}
			if !found {
				t.Errorf("%s: kid %q missing from JWKS", tc.alg, kid)
			}
			if _, err := svc.VerifyAccessToken(token, issuer, audience, time.Now(),
				func(string, time.Time) (bool, string, error) { return true, "test-client-1", nil },
				&tokenTestSession{user: user}); err != nil {
				t.Errorf("VerifyAccessToken(%s): %v", tc.alg, err)
			}
		}
	})

	t.Run("EC PEM key takes its curve's alg", func(t *testing.T) {
		t.Parallel()

		// A curve-blind mapping reported ES256 for every EC key, mislabelling the
		// token header and JWKS for P-384/P-521 (RFC 7518 §3.4).
		for _, tc := range []struct {
			curve elliptic.Curve
			alg   string
		}{
			{elliptic.P256(), "ES256"},
			{elliptic.P384(), "ES384"},
			{elliptic.P521(), "ES512"},
		} {
			ecKey, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			if err != nil {
				t.Fatalf("generate %s key: %v", tc.alg, err)
			}
			pkcs8DER, err := x509.MarshalPKCS8PrivateKey(ecKey)
			if err != nil {
				t.Fatalf("marshal pkcs8: %v", err)
			}
			keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})
			svc, err := NewAccessTokenService(keyPEM, "pem-kid", time.Hour)
			if err != nil {
				t.Fatalf("NewAccessTokenService(%s): %v", tc.alg, err)
			}
			if got := svc.keys["pem-kid"].alg; got != tc.alg {
				t.Errorf("%s PEM key alg = %q, want %q", tc.curve.Params().Name, got, tc.alg)
			}
		}
	})
}

// tokenTestSession is a minimal SessionManager for token tests.
// FindUser returns the hardcoded user; other methods panic if called.
type tokenTestSession struct {
	user oauth.User
}

func (s *tokenTestSession) CurrentUser(ctx context.Context) (oauth.User, bool) {
	return s.user, true
}

func (s *tokenTestSession) AttachUser(ctx context.Context, u oauth.User) context.Context {
	return ctx
}

func (s *tokenTestSession) FindUser(id string) (oauth.User, bool) {
	if id == s.user.ID {
		return s.user, true
	}
	return oauth.User{}, false
}

func (s *tokenTestSession) EndSession(ctx context.Context, r *http.Request, u oauth.User) (redirectURL string) {
	return ""
}

// testSigningKeyPEM is one ephemeral EC P-256 signing key, PEM-encoded, shared
// across tests. Tests only read it, so a single key is safe under parallelism.
var testSigningKeyPEM = func() []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic("generate test signing key: " + err.Error())
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic("marshal test signing key: " + err.Error())
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}()

func newTestAccessTokenService(t *testing.T, kid string) *AccessTokenService {
	svc, err := NewAccessTokenService(testSigningKeyPEM, kid, time.Hour)
	if err != nil {
		t.Fatalf("NewAccessTokenService: %v", err)
	}
	return svc
}
