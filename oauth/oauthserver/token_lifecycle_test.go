// Tests durable OAuth access-token signing-key lifecycle and external JWT interoperability.

package oauthserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func TestAccessTokenSigningKeyLifecycle(t *testing.T) {
	t.Parallel()

	const (
		issuer   = "https://caic.example.com"
		audience = issuer + "/api/caic/v1/mcp"
		clientID = "client-1"
		grantID  = "grant-1"
	)
	now := time.Now().Truncate(time.Second)
	ttl := time.Hour
	path := t.TempDir() + "/oauth.json"
	key1 := generateTestSigningKeyPEM(t)
	key2 := generateTestSigningKeyPEM(t)
	user := oauth.User{ID: "user-1", Username: "alice"}

	state1, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore first start: %v", err)
	}
	service1, err := configureAccessTokenService(state1, key1, "key-1", ttl, now)
	if err != nil {
		t.Fatalf("configure first signing key: %v", err)
	}
	token1, err := service1.IssueAccessToken(issuer, user, audience, "read", grantID, clientID)
	if err != nil {
		t.Fatalf("issue token with first key: %v", err)
	}

	state2, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore rotation restart: %v", err)
	}
	service2, err := configureAccessTokenService(state2, key2, "key-2", ttl, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("configure rotated signing key: %v", err)
	}
	if got := jwkIDs(service2.JWK()); !slices.Equal(got, []string{"key-1", "key-2"}) {
		t.Fatalf("rotated JWKS key IDs = %v", got)
	}
	claims, err := service2.VerifyAccessToken(token1, issuer, audience, now.Add(time.Minute), activeGrant(clientID), &tokenTestSession{user: user})
	if err != nil {
		t.Fatalf("verify pre-rotation token after restart: %v", err)
	}
	if claims.ClientID != clientID {
		t.Fatalf("verified client ID = %q, want %q", claims.ClientID, clientID)
	}
	if _, err := configureAccessTokenService(state2, key1, "key-1", ttl, now.Add(2*time.Minute)); err == nil {
		t.Fatal("reactivating a retired signing key succeeded")
	}

	retiredAt := now.Add(ttl + tokenClockSkew + 2*time.Minute)
	state3, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore retirement restart: %v", err)
	}
	service3, err := configureAccessTokenService(state3, key2, "key-2", ttl, retiredAt)
	if err != nil {
		t.Fatalf("configure after retirement: %v", err)
	}
	if got := jwkIDs(service3.JWK()); !slices.Equal(got, []string{"key-2"}) {
		t.Fatalf("retired JWKS key IDs = %v", got)
	}
	if _, err := service3.VerifyAccessToken(token1, issuer, audience, retiredAt, activeGrant(clientID), &tokenTestSession{user: user}); err == nil || !strings.Contains(err.Error(), "unsupported token header") {
		t.Fatalf("verify retired-key token error = %v", err)
	}
}

func TestAccessTokenSigningKeyLifecycleRejectsUnsafeState(t *testing.T) {
	t.Parallel()

	now := time.Now()
	key1 := generateTestSigningKeyPEM(t)
	key2 := generateTestSigningKeyPEM(t)
	state := newEmptyStore("")
	if _, err := configureAccessTokenService(state, key1, "same-kid", time.Hour, now); err != nil {
		t.Fatalf("configure initial key: %v", err)
	}
	if _, err := configureAccessTokenService(state, key2, "same-kid", time.Hour, now); err == nil {
		t.Fatal("same KID with different key material succeeded")
	}

	state = newEmptyStore("")
	state.currentSigningKID = "key-0"
	state.accessTokenSigningKeys = make([]storedSigningKey, maxAccessTokenSigningKeys)
	for i := range maxAccessTokenSigningKeys {
		state.accessTokenSigningKeys[i] = storedSigningKey{
			KID:           "key-" + big.NewInt(int64(i)).String(),
			PrivateKeyPEM: string(key1),
			VerifyUntil:   now.Add(time.Hour),
		}
	}
	state.accessTokenSigningKeys[0].VerifyUntil = time.Time{}
	if _, err := configureAccessTokenService(state, key2, "overflow", time.Hour, now); err == nil || !strings.Contains(err.Error(), "20-key limit") {
		t.Fatalf("capacity error = %v", err)
	}
}

func TestRFC9068AccessTokenExternalVerification(t *testing.T) {
	t.Parallel()

	const (
		issuer   = "https://caic.example.com"
		audience = issuer + "/api/caic/v1/mcp"
		clientID = "client-1"
	)
	service := newTestAccessTokenService(t, "external-key")
	token, err := service.IssueDPoPAccessToken(issuer, oauth.User{ID: "user-1", Username: "alice"}, audience, "read create", "grant-1", "dpop-thumbprint", clientID)
	if err != nil {
		t.Fatalf("IssueDPoPAccessToken: %v", err)
	}
	claims := externallyVerifyES256AccessToken(t, token, service.JWK())
	if claims.Issuer != issuer || claims.Audience != audience || claims.Subject != "user-1" || claims.ClientID != clientID || claims.JWTID == "" || claims.IssuedAt == 0 || claims.Expiry <= claims.IssuedAt {
		t.Fatalf("RFC 9068 claims = %+v", claims)
	}
	if claims.Scope != "read create" || claims.Confirmation == nil || claims.Confirmation.JKT != "dpop-thumbprint" {
		t.Fatalf("authorization claims = %+v", claims)
	}
}

func externallyVerifyES256AccessToken(t *testing.T, raw string, jwks []oauth.JWK) oauth.AccessTokenClaims {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT parts = %d", len(parts))
	}
	decode := func(part string) []byte {
		value, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			t.Fatalf("decode JWT part: %v", err)
		}
		return value
	}
	var header oauth.JWTHeader
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatalf("decode JWT header: %v", err)
	}
	if header.Alg != "ES256" || header.Typ != accessTokenHeaderTyp || header.KID == "" {
		t.Fatalf("JWT header = %+v", header)
	}
	var jwk oauth.JWK
	for i := range jwks {
		if jwks[i].Kid == header.KID {
			jwk = jwks[i]
			break
		}
	}
	x := decode(jwk.X)
	y := decode(jwk.Y)
	encodedKey := make([]byte, 1+len(x)+len(y))
	encodedKey[0] = 4
	copy(encodedKey[1:], x)
	copy(encodedKey[1+len(x):], y)
	publicKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), encodedKey)
	if err != nil {
		t.Fatalf("parse external JWK: %v", err)
	}
	signature := decode(parts[2])
	if len(signature) != 64 {
		t.Fatalf("ES256 signature length = %d", len(signature))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("external ES256 verification failed")
	}
	var claims oauth.AccessTokenClaims
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatalf("decode JWT claims: %v", err)
	}
	return claims
}

func generateTestSigningKeyPEM(t *testing.T) []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal signing key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func jwkIDs(jwks []oauth.JWK) []string {
	ids := make([]string, len(jwks))
	for i := range jwks {
		ids[i] = jwks[i].Kid
	}
	return ids
}

func activeGrant(clientID string) GrantTouchFunc {
	return func(string, time.Time) (bool, string, error) { return true, clientID, nil }
}
