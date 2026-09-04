// Benchmarks DPoP request-proof parsing and validation.

package oauthserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func BenchmarkDPoPBearerAuth(b *testing.B) {
	user := testUser()
	session := &testSessionManager{users: map[string]oauth.User{user.ID: user}}
	server, err := NewServer(ServerConfig{
		KeyPEM:                  testSigningKeyPEM,
		KeyID:                   "benchmark-key",
		Issuer:                  testBaseURL,
		RefreshTokenStorePath:   b.TempDir() + "/oauth.json",
		ResourceURLPath:         "/resource",
		ResourceMetadataURLPath: "/.well-known/oauth-protected-resource/resource",
		Session:                 session,
		UI:                      &testAuthorizationUI{},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(server.Close)
	const grantID = "benchmark-grant"
	now := time.Now()
	server.state.Grants[grantID] = Grant{ID: grantID, UserID: user.ID, ClientID: "benchmark-client", Resource: testResourceURL, Scope: "read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := server.state.Save(); err != nil {
		b.Fatal(err)
	}
	key, jwk := testDPoPRSAKeyPair(b)
	jkt, err := JWKThumbprint(jwk)
	if err != nil {
		b.Fatal(err)
	}
	accessToken, err := server.tokens.IssueDPoPAccessToken(testBaseURL, user, testResourceURL, "read", grantID, jkt)
	if err != nil {
		b.Fatal(err)
	}
	const jti = "benchmark-proof"
	proof := makeDPoPProofWithJTI(b, key, jwk, http.MethodPost, testResourceURL, time.Now(), accessToken, "", jti)
	proofKey := oauth.RefreshTokenKey(jkt + "\x00" + jti)
	protected := server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		server.mu.Lock()
		delete(server.state.DPoPProofs, proofKey)
		server.mu.Unlock()
		request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, testResourceURL, http.NoBody)
		request.Header.Set("Authorization", DPoPTokenType+" "+accessToken)
		request.Header.Set(DPoPTokenType, proof)
		response := httptest.NewRecorder()
		b.StartTimer()
		protected.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			b.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	}
}
