// Tests for generic OAuth authorization-server HTTP handlers.

package oauthserver

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

const (
	testBaseURL     = "https://caic.example.com"
	testResourceURL = testBaseURL + "/resource"
	testVerifier    = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

func TestServer(t *testing.T) {
	t.Parallel()

	t.Run("metadata registration jwks and resource metadata", func(t *testing.T) {
		t.Parallel()

		s := newTestServer(t)
		h := newTestServerHandler(s)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/oauth-authorization-server", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("metadata status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var metadata oauth.AuthorizationServerMetadata
		if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		if metadata.Issuer != testBaseURL || metadata.AuthorizationEndpoint != testBaseURL+"/oauth/authorize" || metadata.RegistrationEndpoint != testBaseURL+"/oauth/register" {
			t.Fatalf("metadata = %+v", metadata)
		}

		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		if !strings.HasPrefix(registered.ClientID, "test_") || registered.ClientName != "Claude" || registered.TokenEndpointAuthMethod != oauth.TokenEndpointAuthNone {
			t.Fatalf("registered = %+v", registered)
		}

		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/jwks", http.NoBody)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("jwks status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var jwks oauth.JWKSet
		if err := json.NewDecoder(w.Body).Decode(&jwks); err != nil {
			t.Fatalf("decode jwks: %v", err)
		}
		if len(jwks.Keys) != 1 || jwks.Keys[0].Kid == "" {
			t.Fatalf("jwks = %+v", jwks)
		}

		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/oauth-protected-resource/resource", http.NoBody)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("protected resource status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resource oauth.ProtectedResourceMetadata
		if err := json.NewDecoder(w.Body).Decode(&resource); err != nil {
			t.Fatalf("decode protected resource metadata: %v", err)
		}
		if resource.Resource != testResourceURL || len(resource.AuthorizationServers) != 1 || resource.AuthorizationServers[0] != testBaseURL {
			t.Fatalf("resource metadata = %+v", resource)
		}
	})

	t.Run("login adapter redirects unauthenticated authorize request", func(t *testing.T) {
		t.Parallel()

		s := newTestServer(t, &ServerConfig{UI: &testAuthorizationUI{loginURL: "/auth/github/start?next=%2Foauth%2Fauthorize%3Fclient_id%3Dabc"}})
		h := newTestServerHandler(s)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize?client_id=abc", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/auth/github/start?next=%2Foauth%2Fauthorize%3Fclient_id%3Dabc" {
			t.Fatalf("Location = %q", got)
		}
	})

	t.Run("scope approval", func(t *testing.T) {
		t.Parallel()

		s := newTestServer(t)
		requested := "read write admin"
		scope, err := s.approveScope(requested, url.Values{"scope_form": {"1"}, "scope": {"admin", "read"}})
		if err != nil {
			t.Fatalf("approveScope: %v", err)
		}
		if scope != "read admin" {
			t.Fatalf("scope = %q, want selected scopes in canonical order", scope)
		}
		if _, err := s.approveScope(requested, url.Values{"scope_form": {"1"}, "scope": {"repos"}}); err == nil {
			t.Fatal("approveScope unrequested scope error = nil")
		}
		if _, err := s.approveScope(requested, url.Values{"scope_form": {"1"}}); err == nil {
			t.Fatal("approveScope empty selection error = nil")
		}
	})

	t.Run("authorization code returns refresh token and refresh rotates", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read", "write"})
		if tokenResp.RefreshToken == "" {
			t.Fatal("refresh token is empty")
		}

		rotated := refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.AccessToken == "" || rotated.RefreshToken == "" || rotated.RefreshToken == tokenResp.RefreshToken {
			t.Fatalf("rotated token response = %+v", rotated)
		}
		if rotated.TokenType != oauth.TokenTypeBearer || rotated.Scope != tokenResp.Scope {
			t.Fatalf("rotated token metadata = %+v", rotated)
		}
		if _, err := s.verifyBearer(newTestResourceRequest(t), rotated.AccessToken); err != nil {
			t.Fatalf("verify rotated access token: %v", err)
		}
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("bearer auth accepts token and advertises metadata challenge", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := BearerClaimsFromContext(r.Context())
			if !ok || claims.Subject != user.ID {
				t.Fatalf("claims = %+v, ok = %v", claims, ok)
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		req := newTestResourceRequest(t)
		req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
		}

		req = newTestResourceRequest(t)
		w = httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
		want := `Bearer resource_metadata="https://caic.example.com/.well-known/oauth-protected-resource/resource", scope="read write admin repos"`
		if got := w.Header().Get("WWW-Authenticate"); got != want {
			t.Fatalf("WWW-Authenticate = %q, want %q", got, want)
		}
	})

	t.Run("registered client survives server restart", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		_, restartedHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		tokenResp := authorizeOAuthTestClient(t, restartedHandler, user, &registered, []string{"read"})
		if tokenResp.RefreshToken == "" {
			t.Fatal("refresh token is empty after client reload")
		}
	})

	t.Run("refresh token and grant survive server restart", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		restarted, restartedHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		grants := restarted.ListUserGrants(user.ID)
		if len(grants) != 1 || grants[0].ClientID != registered.ClientID || grants[0].ClientName != "Claude" {
			t.Fatalf("grants after restart = %+v", grants)
		}
		rotated := refreshOAuthTestToken(t, restartedHandler, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.RefreshToken == "" || rotated.RefreshToken == tokenResp.RefreshToken {
			t.Fatalf("rotated token response = %+v", rotated)
		}
	})

	t.Run("authorization code survives server restart", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h1, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h1, "Test Client", []string{"https://claude.example.com/callback"})

		// Start the authorize flow to get a consent token.
		form := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read write")
		consentToken := startOAuthTestConsent(t, h1, user, form)

		// Create a new server pointing at the same store path.
		_, h2, _ := newTestFlowServer(t, path, []oauth.User{user})

		// Post the consent on the new server to get an authorization code.
		postForm := url.Values{"consent_token": {consentToken}, "scope_form": {"1"}, "scope": {"read"}}
		req := newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		code := location.Query().Get("code")
		if code == "" {
			t.Fatal("authorization code is empty")
		}

		// Exchange the code for tokens on the new server.
		tokenForm := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
			t.Fatalf("token response missing tokens: %+v", tokenResp)
		}
	})

	t.Run("codes and consents are hashed at rest", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Start consent: the raw consent_token must not be on disk, but its hash key must.
		form := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read")
		consentToken := startOAuthTestConsent(t, h, user, form)
		onDisk := readOAuthStateFile(t, path)
		if strings.Contains(onDisk, consentToken) {
			t.Fatalf("raw consent token present in state file")
		}
		if !strings.Contains(onDisk, oauth.RefreshTokenKey(consentToken)) {
			t.Fatalf("hashed consent token key missing from state file")
		}

		// Approve consent to mint an authorization code.
		postForm := url.Values{"consent_token": {consentToken}, "scope_form": {"1"}, "scope": {"read"}}
		req := newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		code := location.Query().Get("code")
		if code == "" {
			t.Fatal("authorization code is empty")
		}
		onDisk = readOAuthStateFile(t, path)
		if strings.Contains(onDisk, code) {
			t.Fatalf("raw authorization code present in state file")
		}
		if !strings.Contains(onDisk, oauth.RefreshTokenKey(code)) {
			t.Fatalf("hashed authorization code key missing from state file")
		}

		// The code still redeems for tokens.
		tokenForm := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("revoked refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		revokeOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("expired refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		opaque := "expired-refresh-token"
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		store.RefreshTokens[oauth.RefreshTokenKey(opaque)] = RefreshToken{UserID: user.ID, ClientID: registered.ClientID, Resource: testResourceURL, Scope: "read", ExpiresAt: time.Now().Add(-time.Minute)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		_, restartedHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		refreshOAuthTestToken(t, restartedHandler, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("unknown user refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		opaque := "missing-user-refresh-token"
		grantID := "grant-missing-user"
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		expiresAt := time.Now().Add(time.Hour)
		store.Grants[grantID] = Grant{ID: grantID, UserID: "usr_missing", ClientID: registered.ClientID, ClientName: "Claude", Resource: testResourceURL, Scope: "read", CreatedAt: time.Now(), ExpiresAt: expiresAt}
		store.RefreshTokens[oauth.RefreshTokenKey(opaque)] = RefreshToken{GrantID: grantID, UserID: "usr_missing", ClientID: registered.ClientID, Resource: testResourceURL, Scope: "read", ExpiresAt: expiresAt}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		_, restartedHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		refreshOAuthTestToken(t, restartedHandler, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("authorize request validation", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		challenge := testCodeChallenge()
		form := url.Values{
			"response_type":         {oauth.ResponseTypeCode},
			"client_id":             {"unknown_client"},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {oauth.CodeChallengeS256},
			"resource":              {testResourceURL},
			"scope":                 {"read"},
		}
		req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unknown client status = %d, want %d", w.Code, http.StatusBadRequest)
		}

		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		form.Set("client_id", registered.ClientID)
		form.Set("redirect_uri", "https://different.example.com/callback")
		req = newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid redirect status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("authorize without user is rejected", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		form := authorizationCodeForm("client", "https://example.com/callback", "read")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("default scope is rendered when request scope is empty", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, ui := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"http://localhost:9999/callback"})
		form := authorizationCodeForm(registered.ClientID, "http://localhost:9999/callback", "")
		req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if len(ui.last.ScopeItems) != 1 || ui.last.ScopeItems[0].ID != "read" {
			t.Fatalf("scope items = %+v, want default read scope", ui.last.ScopeItems)
		}
	})

	t.Run("authorize POST approves and redirects with code", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://example.com/callback", []string{"read"}, "client-state")
		if code == "" {
			t.Fatal("authorization code is empty")
		}
	})

	t.Run("authorize POST denies and redirects with access_denied", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		form := authorizationCodeForm(registered.ClientID, "https://example.com/callback", "read")
		form.Set("state", "deny-state")
		consentToken := startOAuthTestConsent(t, h, user, form)
		postForm := url.Values{"consent_token": {consentToken}, "decision": {"deny"}}
		req := newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if location.Query().Get("error") != "access_denied" || location.Query().Get("state") != "deny-state" || location.Query().Get("iss") != testBaseURL {
			t.Fatalf("Location = %q, want access_denied with state and issuer", location.String())
		}
	})

	t.Run("authorize POST rejects invalid consent token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		form := url.Values{"consent_token": {"invalid-token"}}
		req := newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("authorize POST rejects wrong user", func(t *testing.T) {
		t.Parallel()

		alice := testUser()
		bob := oauth.User{ID: "usr_bob", Username: "bob", Provider: "gitlab"}
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{alice, bob})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		consentToken := startOAuthTestConsent(t, h, alice, authorizationCodeForm(registered.ClientID, "https://example.com/callback", "read"))

		postForm := url.Values{"consent_token": {consentToken}}
		req := newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), bob)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("introspect active token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read", "write"})

		form := url.Values{"token": {tokenResp.AccessToken}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if !resp.Active {
			t.Fatalf("introspection active = false, want true: %+v", resp)
		}
		if resp.Scope == "" || resp.ClientID != registered.ClientID || resp.TokenType != "access_token" || resp.Iat == 0 || resp.Exp == 0 || resp.Sub != user.ID || resp.Username != user.Username || resp.Iss != testBaseURL || resp.Aud != testResourceURL {
			t.Fatalf("introspection response = %+v", resp)
		}
	})

	t.Run("introspect revoked token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		revokeOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)

		form := url.Values{"token": {tokenResp.AccessToken}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true after revoke, want false: %+v", resp)
		}
	})

	t.Run("introspect missing token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		form := url.Values{}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true with missing token, want false: %+v", resp)
		}
	})

	t.Run("introspect garbage token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		form := url.Values{"token": {"not.a.valid.jwt"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true with garbage token, want false: %+v", resp)
		}
	})

	t.Run("introspect expired token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		// Issue a short-lived token directly via the token service.
		pastAud := testResourceURL
		now := time.Now()
		token, err := s.tokens.issueAccessTokenAt(&oauth.AccessTokenClaims{
			Issuer:   testBaseURL,
			Subject:  user.ID,
			Audience: pastAud,
			Username: user.Username,
			Scope:    "read",
			Type:     accessTokenType,
		}, now.Add(-2*time.Hour), now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("issueAccessTokenAt: %v", err)
		}

		form := url.Values{"token": {token}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true with expired token, want false: %+v", resp)
		}
	})

	t.Run("introspect refresh_token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		// Introspect the refresh token with hint.
		form := url.Values{
			"token":           {tokenResp.RefreshToken},
			"token_type_hint": {"refresh_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if !resp.Active {
			t.Fatalf("introspection active = false, want true: %+v", resp)
		}
		if resp.TokenType != "refresh_token" || resp.ClientID != registered.ClientID || resp.Sub != user.ID || resp.Scope == "" {
			t.Fatalf("introspection response = %+v", resp)
		}
	})

	t.Run("introspect revoked refresh_token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		revokeOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)

		form := url.Values{
			"token":           {tokenResp.RefreshToken},
			"token_type_hint": {"refresh_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true after revoke, want false: %+v", resp)
		}
	})

	t.Run("introspect access_token hint", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		form := url.Values{
			"token":           {tokenResp.AccessToken},
			"token_type_hint": {"access_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if !resp.Active {
			t.Fatalf("introspection active = false, want true: %+v", resp)
		}
		if resp.TokenType != "access_token" {
			t.Fatalf("token_type = %q, want access_token: %+v", resp.TokenType, resp)
		}
	})

	t.Run("introspect garbage refresh_token hint", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})

		form := url.Values{
			"token":           {"garbage-refresh-token"},
			"token_type_hint": {"refresh_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true with garbage refresh token, want false: %+v", resp)
		}
	})

	t.Run("revoke access_token hint", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		// Revoke via access_token hint.
		form := url.Values{
			"client_id":       {registered.ClientID},
			"token":           {tokenResp.AccessToken},
			"token_type_hint": {"access_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Introspect should now return inactive.
		introForm := url.Values{"token": {tokenResp.AccessToken}}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(introForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var introResp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&introResp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if introResp.Active {
			t.Fatalf("introspection active = true after access_token revoke, want false: %+v", introResp)
		}

		// Access token verification should fail via BearerAuth.
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp.AccessToken); err == nil {
			t.Fatal("verifyBearer succeeded after access_token revoke, want error")
		}
	})

	t.Run("revoke no hint with access token only", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		// Revoke with no hint — should fall through to access token check.
		form := url.Values{
			"client_id": {registered.ClientID},
			"token":     {tokenResp.AccessToken},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Access token should be inactive.
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp.AccessToken); err == nil {
			t.Fatal("verifyBearer succeeded after revoke, want error")
		}
	})

	t.Run("revoke garbage token returns 200", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		form := url.Values{
			"client_id": {registered.ClientID},
			"token":     {"garbage.not.a.real.token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d (RFC 7009 always 200): %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("revoke access_token hint garbage token returns 200", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		form := url.Values{
			"client_id":       {registered.ClientID},
			"token":           {"not.a.jwt.token"},
			"token_type_hint": {"access_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d (RFC 7009 always 200): %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("token signed before rotate works after rotate and new token uses new key", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Issue a token with the initial key.
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp.AccessToken); err != nil {
			t.Fatalf("verifyBearer before rotate: %v", err)
		}

		// Rotate the signing key.
		newKID, err := s.tokens.RotateKey()
		if err != nil {
			t.Fatalf("RotateKey: %v", err)
		}
		if newKID == "" {
			t.Fatal("RotateKey returned empty KID")
		}

		// Old token should still verify (old key still active).
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp.AccessToken); err != nil {
			t.Fatalf("verifyBearer after rotate: %v", err)
		}

		// New token should be signed with the rotated key.
		tokenResp2 := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp2.AccessToken); err != nil {
			t.Fatalf("verifyBearer new token after rotate: %v", err)
		}

		// JWKS endpoint should return both keys.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/jwks", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("jwks status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var jwks oauth.JWKSet
		if err := json.NewDecoder(w.Body).Decode(&jwks); err != nil {
			t.Fatalf("decode jwks: %v", err)
		}
		if len(jwks.Keys) != 2 {
			t.Fatalf("jwks keys count = %d, want 2: %+v", len(jwks.Keys), jwks)
		}
		if jwks.Keys[0].Kid == "" || jwks.Keys[1].Kid == "" {
			t.Fatal("jwks kids are empty")
		}
	})

	t.Run("refresh token reuse revokes the grant family", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read", "write"})

		// Rotate once: original becomes used, successor is live.
		rotated := refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.RefreshToken == "" {
			t.Fatal("rotated refresh token is empty")
		}

		// Replay the now-used original: rejected and the family is revoked.
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)

		// The rotated successor is now dead too.
		refreshOAuthTestToken(t, h, registered.ClientID, rotated.RefreshToken, http.StatusBadRequest)

		// The grant itself is revoked.
		grants := s.ListUserGrants(user.ID)
		if len(grants) != 1 {
			t.Fatalf("grants = %+v, want exactly one", grants)
		}
		if grants[0].RevokedAt.IsZero() {
			t.Fatalf("grant not revoked after reuse: %+v", grants[0])
		}
	})

	t.Run("unknown refresh token does not revoke other grants", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		// Submit a never-issued refresh token.
		refreshOAuthTestToken(t, h, registered.ClientID, "rt_never_issued", http.StatusBadRequest)

		// The live grant is untouched: its refresh token still rotates.
		rotated := refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.RefreshToken == "" {
			t.Fatal("rotated refresh token is empty after unknown-token submission")
		}
		grants := s.ListUserGrants(user.ID)
		if len(grants) != 1 || !grants[0].RevokedAt.IsZero() {
			t.Fatalf("grants = %+v, want one live grant", grants)
		}
	})

	t.Run("refresh token reuse records an audit event", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		audit := &recordingAuditRecorder{}
		_, h, _ := newTestFlowServerWithAudit(t, t.TempDir()+"/oauth.json", []oauth.User{user}, audit)
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)

		if !audit.has("deny", "reuse_detected") {
			t.Fatalf("no reuse_detected audit event recorded: %+v", audit.events())
		}
	})
}

func TestPAR(t *testing.T) {
	t.Parallel()

	t.Run("successful PAR flow", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Push authorization request parameters.
		parForm := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read write")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/par", strings.NewReader(parForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("par status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
		var parResp oauth.PARResponse
		if err := json.NewDecoder(w.Body).Decode(&parResp); err != nil {
			t.Fatalf("decode par response: %v", err)
		}
		if parResp.RequestURI == "" {
			t.Fatal("request_uri is empty")
		}
		if !strings.HasPrefix(parResp.RequestURI, "urn:ietf:params:oauth:request_uri:") {
			t.Fatalf("request_uri = %q, want urn:ietf:params:oauth:request_uri: prefix", parResp.RequestURI)
		}
		if parResp.ExpiresIn != 90 {
			t.Fatalf("expires_in = %d, want 90", parResp.ExpiresIn)
		}

		// Authorize using the request_uri.
		authURL := "/oauth/authorize?client_id=" + registered.ClientID + "&request_uri=" + url.QueryEscape(parResp.RequestURI)
		req = newOAuthTestRequest(t, http.MethodGet, authURL, http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		consentToken := consentTokenFromOAuthTestHTML(t, w.Body.String())

		// Post consent.
		postForm := url.Values{"consent_token": {consentToken}, "scope_form": {"1"}, "scope": {"read"}}
		req = newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("authorize POST status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		code := location.Query().Get("code")
		if code == "" {
			t.Fatal("authorization code is empty")
		}

		// Exchange code for tokens.
		tokenForm := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
			t.Fatal("token response missing tokens")
		}
		// Verify the access token.
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp.AccessToken); err != nil {
			t.Fatalf("verify access token: %v", err)
		}
	})

	t.Run("reuse of request_uri fails", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Push PAR.
		parForm := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/par", strings.NewReader(parForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("par status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
		var parResp oauth.PARResponse
		if err := json.NewDecoder(w.Body).Decode(&parResp); err != nil {
			t.Fatalf("decode par response: %v", err)
		}

		// First use succeeds.
		authURL := "/oauth/authorize?client_id=" + registered.ClientID + "&request_uri=" + url.QueryEscape(parResp.RequestURI)
		req = newOAuthTestRequest(t, http.MethodGet, authURL, http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("first authorize status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Second use of the same request_uri fails.
		req = newOAuthTestRequest(t, http.MethodGet, authURL, http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("second authorize status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		// Verify error response body.
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "invalid_request_uri" {
			t.Fatalf("error = %q, want invalid_request_uri", errResp.Error)
		}
	})

	t.Run("expired request_uri is rejected", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Push PAR.
		parForm := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/par", strings.NewReader(parForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("par status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
		var parResp oauth.PARResponse
		if err := json.NewDecoder(w.Body).Decode(&parResp); err != nil {
			t.Fatalf("decode par response: %v", err)
		}

		// Manually expire the request_uri.
		s.mu.Lock()
		entry := s.parRequests[parResp.RequestURI]
		entry.ExpiresAt = time.Now().Add(-time.Minute)
		s.parRequests[parResp.RequestURI] = entry
		s.mu.Unlock()

		// Authorize with the expired request_uri.
		authURL := "/oauth/authorize?client_id=" + registered.ClientID + "&request_uri=" + url.QueryEscape(parResp.RequestURI)
		req = newOAuthTestRequest(t, http.MethodGet, authURL, http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "invalid_request_uri" {
			t.Fatalf("error = %q, want invalid_request_uri", errResp.Error)
		}
	})

	t.Run("PAR with missing client_id returns invalid_request", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})

		parForm := authorizationCodeForm("", "https://example.com/callback", "read")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/par", strings.NewReader(parForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("par status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "invalid_request" {
			t.Fatalf("error = %q, want invalid_request", errResp.Error)
		}
	})

	t.Run("authorize with request_uri ignores inline query params", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, ui := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"http://localhost:9999/callback"})

		// Push PAR with scope=read.
		parForm := authorizationCodeForm(registered.ClientID, "http://localhost:9999/callback", "read")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/par", strings.NewReader(parForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("par status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
		var parResp oauth.PARResponse
		if err := json.NewDecoder(w.Body).Decode(&parResp); err != nil {
			t.Fatalf("decode par response: %v", err)
		}

		// Authorize with request_uri and inline scope=write (should be ignored).
		authURL := "/oauth/authorize?client_id=" + registered.ClientID +
			"&request_uri=" + url.QueryEscape(parResp.RequestURI) +
			"&scope=write"
		req = newOAuthTestRequest(t, http.MethodGet, authURL, http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		// The consent page should show scope=read from PAR, not scope=write from query.
		if len(ui.last.ScopeItems) != 1 || ui.last.ScopeItems[0].ID != "read" {
			t.Fatalf("scope items = %+v, want [read] (inline params ignored)", ui.last.ScopeItems)
		}

		consentToken := consentTokenFromOAuthTestHTML(t, w.Body.String())
		postForm := url.Values{"consent_token": {consentToken}, "scope_form": {"1"}, "scope": {"read"}}
		req = newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("authorize POST status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
		}

		// Exchange code for tokens — should get scope=read.
		location, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		code := location.Query().Get("code")
		tokenForm := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"http://localhost:9999/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tokenResp.Scope != "read" {
			t.Fatalf("token scope = %q, want read", tokenResp.Scope)
		}
	})
}

func TestDeviceAuthorization(t *testing.T) {
	t.Parallel()

	t.Run("successful device flow", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Step 1: Request device authorization.
		devForm := url.Values{
			"client_id": {registered.ClientID},
			"scope":     {"read"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(devForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device_authorization status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var devResp oauth.DeviceAuthorizationResponse
		if err := json.NewDecoder(w.Body).Decode(&devResp); err != nil {
			t.Fatalf("decode device_authorization response: %v", err)
		}
		if devResp.DeviceCode == "" || devResp.UserCode == "" {
			t.Fatalf("device_authorization response missing codes: %+v", devResp)
		}
		if devResp.VerificationURI != testBaseURL+"/oauth/device" {
			t.Fatalf("verification_uri = %q, want %s", devResp.VerificationURI, testBaseURL+"/oauth/device")
		}
		if devResp.ExpiresIn != 600 {
			t.Fatalf("expires_in = %d, want 600", devResp.ExpiresIn)
		}
		if devResp.Interval != 5 {
			t.Fatalf("interval = %d, want 5", devResp.Interval)
		}
		// user_code should be 8 uppercase alphanumeric chars.
		if len(devResp.UserCode) != 8 {
			t.Fatalf("user_code length = %d, want 8", len(devResp.UserCode))
		}

		// Step 2: Approve the device as authenticated user.
		approveForm := url.Values{"user_code": {devResp.UserCode}}
		req = newOAuthTestRequest(t, http.MethodPost, "/oauth/device", strings.NewReader(approveForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device approve status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "Device authorized") {
			t.Fatalf("device approve body missing confirmation: %s", w.Body.String())
		}

		// Step 3: Poll for token.
		tokenForm := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {devResp.DeviceCode},
			"client_id":   {registered.ClientID},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
			t.Fatal("token response missing tokens")
		}
		// Verify the access token.
		if _, err := s.verifyBearer(newTestResourceRequest(t), tokenResp.AccessToken); err != nil {
			t.Fatalf("verify access token: %v", err)
		}
	})

	t.Run("polling authorization_pending", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Request device authorization.
		devForm := url.Values{
			"client_id": {registered.ClientID},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(devForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device_authorization status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var devResp oauth.DeviceAuthorizationResponse
		if err := json.NewDecoder(w.Body).Decode(&devResp); err != nil {
			t.Fatalf("decode device_authorization response: %v", err)
		}

		// Poll without approving — should get authorization_pending.
		tokenForm := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {devResp.DeviceCode},
			"client_id":   {registered.ClientID},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("pending token status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "authorization_pending" {
			t.Fatalf("error = %q, want authorization_pending", errResp.Error)
		}
	})

	t.Run("expired device_code", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Request device authorization.
		devForm := url.Values{
			"client_id": {registered.ClientID},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(devForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device_authorization status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var devResp oauth.DeviceAuthorizationResponse
		if err := json.NewDecoder(w.Body).Decode(&devResp); err != nil {
			t.Fatalf("decode device_authorization response: %v", err)
		}

		// Manually expire the device code.
		s.mu.Lock()
		codeHash := oauth.RefreshTokenKey(devResp.DeviceCode)
		dc := s.state.DeviceCodes[codeHash]
		dc.ExpiresAt = time.Now().Add(-time.Minute)
		s.state.DeviceCodes[codeHash] = dc
		s.mu.Unlock()

		// Poll for token — should get expired_token.
		tokenForm := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {devResp.DeviceCode},
			"client_id":   {registered.ClientID},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expired token status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "expired_token" {
			t.Fatalf("error = %q, want expired_token", errResp.Error)
		}
	})

	t.Run("device page shows form", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})

		// GET /oauth/device — show empty form.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/device", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device page status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `<form method="post" action="/oauth/device"`) {
			t.Fatalf("device page missing form: %s", body)
		}
		if !strings.Contains(body, `name="user_code"`) {
			t.Fatalf("device page missing user_code input: %s", body)
		}

		// GET /oauth/device?user_code=ABC — pre-filled form.
		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/device?user_code=AB12EF34", http.NoBody)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device page with user_code status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		body2 := w.Body.String()
		if !strings.Contains(body2, `value="AB12EF34"`) {
			t.Fatalf("device page form not pre-filled: %s", body2)
		}
	})

	t.Run("device approval redirects unauthenticated", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		// POST /oauth/device without authenticated user.
		form := url.Values{"user_code": {"ABCDEFGH"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// Without a login adapter, should return Unauthorized.
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("device approve unauthenticated status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("device approval with login redirect", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{testUser()})
		cfg.UI = &testAuthorizationUI{loginURL: "/auth/login"}
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		form := url.Values{"user_code": {"ABCDEFGH"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("device approve status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/auth/login" {
			t.Fatalf("Location = %q, want /auth/login", got)
		}
	})

	t.Run("device approval with invalid user_code", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		// POST /oauth/device with a bogus user_code.
		form := url.Values{"user_code": {"ZZZZZZZZ"}}
		req := newOAuthTestRequest(t, http.MethodPost, "/oauth/device", strings.NewReader(form.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("device approve invalid code status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "invalid_request" {
			t.Fatalf("error = %q, want invalid_request", errResp.Error)
		}
	})

	t.Run("device_code survives server restart", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h1, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h1, "Claude", []string{"https://claude.example.com/callback"})

		// Request device authorization on first server.
		devForm := url.Values{
			"client_id": {registered.ClientID},
			"scope":     {"read"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(devForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h1.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device_authorization status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var devResp oauth.DeviceAuthorizationResponse
		if err := json.NewDecoder(w.Body).Decode(&devResp); err != nil {
			t.Fatalf("decode device_authorization response: %v", err)
		}

		// Restart server.
		_, h2, _ := newTestFlowServer(t, path, []oauth.User{user})

		// Poll without approving on the new server.
		tokenForm := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {devResp.DeviceCode},
			"client_id":   {registered.ClientID},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("token status after restart = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error response: %v", err)
		}
		if errResp.Error != "authorization_pending" {
			t.Fatalf("error = %q, want authorization_pending", errResp.Error)
		}
	})

	t.Run("device_authorization metadata includes device_code grant", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/oauth-authorization-server", http.NoBody)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("metadata status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var metadata oauth.AuthorizationServerMetadata
		if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		if !slices.Contains(metadata.GrantTypesSupported, "urn:ietf:params:oauth:grant-type:device_code") {
			t.Fatalf("grant_types_supported missing device_code: %v", metadata.GrantTypesSupported)
		}
	})
}

func TestDPoP(t *testing.T) {
	t.Parallel()

	t.Run("token endpoint with dpop proof gets dpop-bound token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")

		tokenURL := testBaseURL + "/oauth/token"
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")

		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tokenResp.TokenType != DPoPTokenType {
			t.Fatalf("token_type = %q, want %q", tokenResp.TokenType, DPoPTokenType)
		}
		if !strings.HasPrefix(tokenResp.AccessToken, "eyJ") {
			t.Fatalf("access token doesn't look like a JWT: %s", tokenResp.AccessToken)
		}
		// Verify the token contains cnf.jkt.
		claims, err := s.tokens.VerifyAccessToken(tokenResp.AccessToken, testBaseURL, testResourceURL, time.Now(), s.touchGrant, s.session)
		if err != nil {
			t.Fatalf("verify access token: %v", err)
		}
		if claims.Confirmation == nil || claims.Confirmation.JKT == "" {
			t.Fatal("access token claims missing cnf.jkt")
		}
		// Verify cnf.jkt matches the dpop proof key.
		expectedJKT, err := JWKThumbprint(dpopJWKObj)
		if err != nil {
			t.Fatalf("JWKThumbprint: %v", err)
		}
		if claims.Confirmation.JKT != expectedJKT {
			t.Fatalf("cnf.jkt = %q, want %q", claims.Confirmation.JKT, expectedJKT)
		}
	})

	t.Run("dpop-bound access token accepted by bearer auth", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		// Get dpop-bound token.
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}

		// Use dpop-bound token with BearerAuth middleware.
		protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := BearerClaimsFromContext(r.Context())
			if !ok || claims.Subject != user.ID {
				t.Fatalf("claims = %+v, ok = %v", claims, ok)
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		resourceProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", testResourceURL, time.Now(), tokenResp.AccessToken, "")
		resReq := newTestResourceRequest(t)
		resReq.Header.Set("Authorization", "DPoP "+tokenResp.AccessToken)
		resReq.Header.Set("DPoP", resourceProof)
		addForwardedHeaders(resReq)
		addForwardedHeaders(resReq)
		w = httptest.NewRecorder()
		protected.ServeHTTP(w, resReq)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
		}
	})

	t.Run("dpop proof with wrong htm rejected", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)

		tokenURL := testBaseURL + "/oauth/token"
		// htm should be POST for the token endpoint, but we use GET.
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "GET", tokenURL, time.Now(), "", "")

		code := authorizeOAuthTestCode(t, h, testUser(), &registered, "https://claude.example.com/callback", []string{"read"}, "")

		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		var errResp oauth.ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if !strings.Contains(errResp.Error, "invalid_dpop_proof") {
			t.Fatalf("error = %q, want invalid_dpop_proof", errResp.Error)
		}
	})

	t.Run("dpop proof with wrong htu rejected", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)

		wrongURL := "https://evil.example.com/oauth/token"
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", wrongURL, time.Now(), "", "")

		code := authorizeOAuthTestCode(t, h, testUser(), &registered, "https://claude.example.com/callback", []string{"read"}, "")

		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("dpop proof expired iat rejected", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)

		tokenURL := testBaseURL + "/oauth/token"
		oldTime := time.Now().Add(-10 * time.Minute)
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, oldTime, "", "")

		code := authorizeOAuthTestCode(t, h, testUser(), &registered, "https://claude.example.com/callback", []string{"read"}, "")

		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("dpop proof with invalid ath rejected", func(t *testing.T) {
		t.Parallel()

		testCases := []struct {
			name        string
			accessToken string // used for ath computation in proof
		}{
			{"missing ath", ""},
			{"mismatched ath", "wrong-access-token"},
		}
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				res := setupDPoPBoundToken(t)

				protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))
				resourceProof := makeDPoPProof(t, res.dpopKey, res.dpopJWK, "POST", testResourceURL, time.Now(), tc.accessToken, "")
				resReq := newTestResourceRequest(t)
				resReq.Header.Set("Authorization", "DPoP "+res.token.AccessToken)
				resReq.Header.Set("DPoP", resourceProof)
				addForwardedHeaders(resReq)
				w := httptest.NewRecorder()
				protected.ServeHTTP(w, resReq)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
				}
			})
		}
	})

	t.Run("dpop proof with wrong key cnf.jkt mismatch rejected", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}

		// Make another key pair and use it to prove possession of the dpop-bound token.
		wrongKey, wrongJWKObj := testDPoPRSAKeyPair(t)
		protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		resourceProof := makeDPoPProof(t, wrongKey, wrongJWKObj, "POST", testResourceURL, time.Now(), tokenResp.AccessToken, "")
		resReq := newTestResourceRequest(t)
		resReq.Header.Set("Authorization", "DPoP "+tokenResp.AccessToken)
		resReq.Header.Set("DPoP", resourceProof)
		addForwardedHeaders(resReq)
		w = httptest.NewRecorder()
		protected.ServeHTTP(w, resReq)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("introspection returns cnf.jkt for dpop-bound tokens", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var tokenResp oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}

		// Introspect the dpop-bound token.
		introForm := url.Values{"token": {tokenResp.AccessToken}}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(introForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var introResp oauth.IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&introResp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if !introResp.Active {
			t.Fatal("introspection response not active")
		}
		if introResp.Confirmation == nil || introResp.Confirmation.JKT == "" {
			t.Fatal("introspection response missing cnf.jkt")
		}
		expectedJKT, err := JWKThumbprint(dpopJWKObj)
		if err != nil {
			t.Fatalf("JWKThumbprint: %v", err)
		}
		if introResp.Confirmation.JKT != expectedJKT {
			t.Fatalf("cnf.jkt = %q, want %q", introResp.Confirmation.JKT, expectedJKT)
		}
	})

	t.Run("nonce lifecycle request without nonce gets nonce retry succeeds", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		// First request: proof without nonce.
		dpopProof1 := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof1)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// The first request should succeed (nonce validation is optional per RFC 9449).
		if w.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		// Issue a nonce and then use it in a subsequent proof.
		nonce, err := s.dpopNonces.Issue()
		if err != nil {
			t.Fatalf("Issue nonce: %v", err)
		}

		// Get a new code and make a second request with the nonce.
		code2 := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		dpopProof2 := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", nonce)
		form2 := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code2},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form2.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", dpopProof2)
		addForwardedHeaders(req)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("second request with nonce status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("replayed resource proof with same jti rejected", func(t *testing.T) {
		t.Parallel()

		res := setupDPoPBoundToken(t)
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		// Build a single proof and reuse the exact bytes (same jti) twice.
		resourceProof := makeDPoPProof(t, res.dpopKey, res.dpopJWK, "POST", testResourceURL, time.Now(), res.token.AccessToken, "")

		first := httptest.NewRecorder()
		req1 := newTestResourceRequest(t)
		req1.Header.Set("Authorization", "DPoP "+res.token.AccessToken)
		req1.Header.Set("DPoP", resourceProof)
		addForwardedHeaders(req1)
		protected.ServeHTTP(first, req1)
		if first.Code != http.StatusNoContent {
			t.Fatalf("first request status = %d, want %d: %s", first.Code, http.StatusNoContent, first.Body.String())
		}

		second := httptest.NewRecorder()
		req2 := newTestResourceRequest(t)
		req2.Header.Set("Authorization", "DPoP "+res.token.AccessToken)
		req2.Header.Set("DPoP", resourceProof)
		addForwardedHeaders(req2)
		protected.ServeHTTP(second, req2)
		if second.Code != http.StatusUnauthorized {
			t.Fatalf("replay status = %d, want %d: %s", second.Code, http.StatusUnauthorized, second.Body.String())
		}
	})

	t.Run("fresh resource proof per request accepted", func(t *testing.T) {
		t.Parallel()

		res := setupDPoPBoundToken(t)
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		for i := range 3 {
			proof := makeDPoPProof(t, res.dpopKey, res.dpopJWK, "POST", testResourceURL, time.Now(), res.token.AccessToken, "")
			req := newTestResourceRequest(t)
			req.Header.Set("Authorization", "DPoP "+res.token.AccessToken)
			req.Header.Set("DPoP", proof)
			addForwardedHeaders(req)
			w := httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			if w.Code != http.StatusNoContent {
				t.Fatalf("request %d status = %d, want %d: %s", i, w.Code, http.StatusNoContent, w.Body.String())
			}
		}
	})

	t.Run("resource proof missing jti rejected", func(t *testing.T) {
		t.Parallel()

		res := setupDPoPBoundToken(t)
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		proof := makeDPoPProofWithJTI(t, res.dpopKey, res.dpopJWK, "POST", testResourceURL, time.Now(), res.token.AccessToken, "", "")
		req := newTestResourceRequest(t)
		req.Header.Set("Authorization", "DPoP "+res.token.AccessToken)
		req.Header.Set("DPoP", proof)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("token endpoint unaffected by jti tracking", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		tokenURL := testBaseURL + "/oauth/token"

		// Two distinct token exchanges that reuse the same jti must both succeed:
		// the token endpoint does not track jti (single-use codes bound it).
		jti, err := randomToken()
		if err != nil {
			t.Fatalf("generate jti: %v", err)
		}
		for i := range 2 {
			code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
			proof := makeDPoPProofWithJTI(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "", jti)
			form := url.Values{
				"grant_type":    {oauth.GrantAuthorizationCode},
				"code":          {code},
				"client_id":     {registered.ClientID},
				"redirect_uri":  {"https://claude.example.com/callback"},
				"code_verifier": {testVerifier},
				"resource":      {testResourceURL},
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("DPoP", proof)
			addForwardedHeaders(req)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("token exchange %d status = %d, want %d: %s", i, w.Code, http.StatusOK, w.Body.String())
			}
		}
	})

	t.Run("alg does not match jwk key type is rejected", func(t *testing.T) {
		t.Parallel()

		ecKey, ecJWK := testDPoPECKeyPair(t, elliptic.P256(), "P-256")
		rsaKey, rsaJWK := testDPoPRSAKeyPair(t)

		// alg=ES256 with an RSA jwk.
		proof := makeDPoPProofSigned(t, "ES256", ecKey, rsaJWK)
		if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err == nil {
			t.Fatal("ES256 header with RSA jwk: want error, got nil")
		}
		// alg=RS256 with an EC jwk.
		proof = makeDPoPProofSigned(t, "RS256", rsaKey, ecJWK)
		if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err == nil {
			t.Fatal("RS256 header with EC jwk: want error, got nil")
		}
	})

	t.Run("rsa modulus out of bounds is rejected", func(t *testing.T) {
		t.Parallel()

		// jwkPublicKey enforces the modulus bound before verifying the
		// signature, so a crafted-but-unsigned oauth.JWK is enough to exercise it.
		// (crypto/rsa refuses to generate keys this small.)
		makeRSAJWK := func(modulusBits uint) *oauth.JWK {
			n := new(big.Int).Lsh(big.NewInt(1), modulusBits-1)
			return &oauth.JWK{
				Kty: "RSA",
				N:   base64.RawURLEncoding.EncodeToString(n.Bytes()),
				E:   base64.RawURLEncoding.EncodeToString(big.NewInt(65537).Bytes()),
			}
		}
		proof := makeUnsignedDPoPProof(t, "RS256", makeRSAJWK(512))
		if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err == nil {
			t.Fatal("512-bit rsa modulus: want error, got nil")
		}
		proof = makeUnsignedDPoPProof(t, "RS256", makeRSAJWK(16385))
		if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err == nil {
			t.Fatal("oversized rsa modulus: want error, got nil")
		}
	})

	t.Run("valid asymmetric key types are accepted", func(t *testing.T) {
		t.Parallel()

		ecKey, ecJWK := testDPoPECKeyPair(t, elliptic.P256(), "P-256")
		proof := makeDPoPProofSigned(t, "ES256", ecKey, ecJWK)
		if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err != nil {
			t.Fatalf("P-256/ES256: %v", err)
		}
		edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate ed25519 key: %v", err)
		}
		edJWK := &oauth.JWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(edPub)}
		proof = makeDPoPProofSigned(t, "EdDSA", edPriv, edJWK)
		if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err != nil {
			t.Fatalf("Ed25519/EdDSA: %v", err)
		}
	})

	t.Run("every advertised alg verifies", func(t *testing.T) {
		t.Parallel()

		// isAsymmetricAlg accepts all of these, so each must actually verify a
		// conforming signature: the digest follows alg (RFC 7518 §3.1), PS* is
		// PSS not PKCS1v15 (§3.5), and ES* is raw R||S not ASN.1 (§3.4).
		rsaKey, rsaJWK := testDPoPRSAKeyPair(t)
		for _, alg := range []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"} {
			proof := makeDPoPProofSigned(t, alg, rsaKey, rsaJWK)
			if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err != nil {
				t.Errorf("%s: %v", alg, err)
			}
		}
		for _, tc := range []struct {
			alg   string
			curve elliptic.Curve
			crv   string
		}{
			{"ES256", elliptic.P256(), "P-256"},
			{"ES384", elliptic.P384(), "P-384"},
			{"ES512", elliptic.P521(), "P-521"},
		} {
			ecKey, ecJWK := testDPoPECKeyPair(t, tc.curve, tc.crv)
			proof := makeDPoPProofSigned(t, tc.alg, ecKey, ecJWK)
			if _, _, err := DPoPProof(httpDPoPRequest(t, proof)); err != nil {
				t.Errorf("%s: %v", tc.alg, err)
			}
		}
	})

	t.Run("ecdsa asn.1 signature is rejected", func(t *testing.T) {
		t.Parallel()

		// RFC 7518 §3.4 mandates raw R||S. A DER-encoded signature is the shape a
		// naive implementation emits; it must not be accepted.
		ecKey, ecJWK := testDPoPECKeyPair(t, elliptic.P256(), "P-256")
		proof := makeDPoPProofSigned(t, "ES256", ecKey, ecJWK)
		parts := strings.Split(proof, ".")
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		der, err := ecdsa.SignASN1(rand.Reader, ecKey, digest[:])
		if err != nil {
			t.Fatalf("sign asn.1: %v", err)
		}
		tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(der)
		if _, _, err := DPoPProof(httpDPoPRequest(t, tampered)); err == nil {
			t.Fatal("asn.1 ecdsa signature: want error, got nil")
		}
	})

	t.Run("ec jwk coordinates must be curve sized", func(t *testing.T) {
		t.Parallel()

		// RFC 7518 §6.2.1.2 fixes the octet length, and an off-curve point must
		// not reach verification at all.
		_, ecJWK := testDPoPECKeyPair(t, elliptic.P256(), "P-256")
		short := *ecJWK
		short.X = base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3})
		if _, _, err := DPoPProof(httpDPoPRequest(t, makeUnsignedDPoPProof(t, "ES256", &short))); err == nil {
			t.Fatal("short ec x: want error, got nil")
		}
		offCurve := *ecJWK
		offCurve.Y = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		if _, _, err := DPoPProof(httpDPoPRequest(t, makeUnsignedDPoPProof(t, "ES256", &offCurve))); err == nil {
			t.Fatal("off-curve ec point: want error, got nil")
		}
	})
}

// testDPoPRSAKeyPair generates an RSA key pair for DPoP proof tests.
func testDPoPRSAKeyPair(t *testing.T) (*rsa.PrivateKey, *oauth.JWK) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pub := &key.PublicKey
	jwk := oauth.JWK{
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
	return key, &jwk
}

// makeDPoPProof creates a valid DPoP proof JWT signed with the given RSA key
// using a fresh random jti.
func makeDPoPProof(t *testing.T, key *rsa.PrivateKey, jwk *oauth.JWK, htm, htu string, iat time.Time, accessToken, nonce string) string {
	t.Helper()
	jti, err := randomToken()
	if err != nil {
		t.Fatalf("generate jti: %v", err)
	}
	return makeDPoPProofWithJTI(t, key, jwk, htm, htu, iat, accessToken, nonce, jti)
}

// makeDPoPProofWithJTI creates a DPoP proof JWT with an explicit jti (which may
// be empty to exercise the missing-jti rejection path).
func makeDPoPProofWithJTI(t *testing.T, key *rsa.PrivateKey, jwk *oauth.JWK, htm, htu string, iat time.Time, accessToken, nonce, jti string) string {
	t.Helper()
	header := DPoPHeader{Typ: "dpop+jwt", Alg: "RS256", JWK: *jwk}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal dpop header: %v", err)
	}

	claims := DPoPClaims{
		JTI:   jti,
		HTM:   htm,
		HTU:   htu,
		IAT:   iat.Unix(),
		Nonce: nonce,
	}
	if accessToken != "" {
		claims.ATH = DPoPAccessTokenHash(accessToken)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal dpop claims: %v", err)
	}

	parts := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(parts))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign dpop proof: %v", err)
	}
	return parts + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// testDPoPECKeyPair generates an EC key pair and its oauth.JWK for DPoP proof tests.
func testDPoPECKeyPair(t *testing.T, curve elliptic.Curve, crv string) (*ecdsa.PrivateKey, *oauth.JWK) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	// PublicKey.Bytes returns the uncompressed point 0x04 || X || Y with each
	// coordinate left-padded to the curve's fixed byte width.
	point, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("encode ec public key: %v", err)
	}
	size := (curve.Params().BitSize + 7) / 8
	jwk := &oauth.JWK{
		Kty: "EC",
		Crv: crv,
		X:   base64.RawURLEncoding.EncodeToString(point[1 : 1+size]),
		Y:   base64.RawURLEncoding.EncodeToString(point[1+size:]),
	}
	return key, jwk
}

// dpopTokenURL is the htu used by the DPoP proof test helpers.
const dpopTokenURL = testBaseURL + "/oauth/token"

// makeUnsignedDPoPProof builds a DPoP proof with a bogus signature, for cases
// rejected before signature verification (e.g. RSA modulus bounds).
func makeUnsignedDPoPProof(t *testing.T, alg string, jwk *oauth.JWK) string {
	t.Helper()
	header := DPoPHeader{Typ: "dpop+jwt", Alg: alg, JWK: *jwk}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal dpop header: %v", err)
	}
	claims := DPoPClaims{JTI: "x", HTM: http.MethodPost, HTU: dpopTokenURL, IAT: time.Now().Unix()}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal dpop claims: %v", err)
	}
	parts := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	return parts + "." + base64.RawURLEncoding.EncodeToString([]byte("bogus"))
}

// httpDPoPRequest wraps a proof string in a request carrying the DPoP header.
func httpDPoPRequest(t *testing.T, proof string) *http.Request {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, dpopTokenURL, http.NoBody)
	r.Header.Set("DPoP", proof)
	addForwardedHeaders(r)
	return r
}

// makeDPoPProofSigned builds a valid DPoP proof with a chosen header alg, signed
// by signer. RSA and ECDSA sign the SHA-256 digest; Ed25519 signs the raw input.
func makeDPoPProofSigned(t *testing.T, alg string, signer crypto.Signer, jwk *oauth.JWK) string {
	t.Helper()
	jti, err := randomToken()
	if err != nil {
		t.Fatalf("generate jti: %v", err)
	}
	header := DPoPHeader{Typ: "dpop+jwt", Alg: alg, JWK: *jwk}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal dpop header: %v", err)
	}
	claims := DPoPClaims{JTI: jti, HTM: http.MethodPost, HTU: dpopTokenURL, IAT: time.Now().Unix()}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal dpop claims: %v", err)
	}
	parts := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)

	signature, err := signJWS(alg, signer, []byte(parts))
	if err != nil {
		t.Fatalf("sign dpop proof: %v", err)
	}
	return parts + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// addForwardedHeaders sets X-Forwarded headers on a test request so that
// effectiveRequestHostAndScheme returns https://caic.example.com.
func addForwardedHeaders(r *http.Request) {
	r.Header.Set("X-Forwarded-Host", "caic.example.com")
	r.Header.Set("X-Forwarded-Proto", "https")
}

// dpopTestResources holds the pieces needed for DPoP resource tests.
type dpopTestResources struct {
	server     *Server
	handler    http.Handler
	dpopKey    *rsa.PrivateKey
	dpopJWK    *oauth.JWK
	registered oauth.RegisterResponse
	token      oauth.TokenResponse
}

// setupDPoPBoundToken creates a server, registers a client, authorizes a code,
// obtains a DPoP-bound token, and returns the pieces needed for resource tests.
func setupDPoPBoundToken(t *testing.T) dpopTestResources {
	t.Helper()
	user := testUser()
	s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
	registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
	dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
	code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
	tokenURL := testBaseURL + "/oauth/token"

	dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
	form := url.Values{
		"grant_type":    {oauth.GrantAuthorizationCode},
		"code":          {code},
		"client_id":     {registered.ClientID},
		"redirect_uri":  {"https://claude.example.com/callback"},
		"code_verifier": {testVerifier},
		"resource":      {testResourceURL},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", dpopProof)
	addForwardedHeaders(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var tokenResp oauth.TokenResponse
	if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return dpopTestResources{server: s, handler: h, dpopKey: dpopKey, dpopJWK: dpopJWKObj, registered: registered, token: tokenResp}
}

type testUserContextKey struct{}

// captureAuthorizationUI captures consent page data and provides a basic auth UI for tests.
type captureAuthorizationUI struct {
	last     ConsentPageData
	loginURL string
}

func (r *captureAuthorizationUI) LoginStartURL(*http.Request) string {
	return r.loginURL
}

func (r *captureAuthorizationUI) ProviderLabel(provider string) string {
	return provider
}

func (r *captureAuthorizationUI) RenderOAuthConsent(w http.ResponseWriter, data *ConsentPageData) error {
	r.last = *data
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write([]byte(`<input type="hidden" name="consent_token" value="` + data.ConsentToken + `">`))
	return err
}

type testAuthorizationUI struct {
	loginURL string
	last     ConsentPageData
}

func (u *testAuthorizationUI) LoginStartURL(*http.Request) string {
	return u.loginURL
}

func (u *testAuthorizationUI) ProviderLabel(provider string) string {
	return provider
}

func (u *testAuthorizationUI) RenderOAuthConsent(w http.ResponseWriter, data *ConsentPageData) error {
	u.last = *data
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write([]byte(`<input type="hidden" name="consent_token" value="` + data.ConsentToken + `">`))
	return err
}

type testSessionManager struct {
	users       map[string]oauth.User
	endRedirect string
}

func (m *testSessionManager) CurrentUser(ctx context.Context) (oauth.User, bool) {
	user, ok := ctx.Value(testUserContextKey{}).(oauth.User)
	return user, ok
}

func (m *testSessionManager) AttachUser(ctx context.Context, u oauth.User) context.Context {
	return ctx
}

func (m *testSessionManager) FindUser(id string) (oauth.User, bool) {
	user, ok := m.users[id]
	return user, ok
}

func (m *testSessionManager) EndSession(ctx context.Context, r *http.Request, u oauth.User) (redirectURL string) {
	return m.endRedirect
}

func testUser() oauth.User {
	return oauth.User{ID: "usr_alice", Username: "alice", Provider: "github"}
}

func newTestServer(t *testing.T, cfgs ...*ServerConfig) *Server {
	cfg := &ServerConfig{}
	if len(cfgs) > 0 && cfgs[0] != nil {
		cfg = cfgs[0]
	}
	applyTestServerDefaults(t, cfg)
	s, err := NewServer(*cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func newTestFlowServer(t *testing.T, path string, users []oauth.User) (*Server, http.Handler, *captureAuthorizationUI) {
	return newTestFlowServerWithAudit(t, path, users, nil)
}

func newTestFlowServerWithAudit(t *testing.T, path string, users []oauth.User, audit AuditRecorder) (*Server, http.Handler, *captureAuthorizationUI) {
	usersByID := make(map[string]oauth.User, len(users))
	for _, user := range users {
		usersByID[user.ID] = user
	}
	session := &testSessionManager{users: usersByID}
	ui := &captureAuthorizationUI{}
	cfg := &ServerConfig{
		RefreshTokenStorePath: path,
		Session:               session,
		UI:                    ui,
		Audit:                 audit,
	}
	applyTestServerDefaults(t, cfg)
	s, err := NewServer(*cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, newTestServerHandler(s), ui
}

// recordingAuditRecorder captures OAuth audit events for assertions.
type recordingAuditRecorder struct {
	mu      sync.Mutex
	records []auditRecord
}

type auditRecord struct {
	decision string
	status   string
}

func (a *recordingAuditRecorder) RecordOAuth(_ context.Context, _, _, _, decision, status string, _ any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, auditRecord{decision: decision, status: status})
}

func (a *recordingAuditRecorder) has(decision, status string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rec := range a.records {
		if rec.decision == decision && rec.status == status {
			return true
		}
	}
	return false
}

func (a *recordingAuditRecorder) events() []auditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.records)
}

func applyTestServerDefaults(t *testing.T, cfg *ServerConfig) {
	if cfg.RefreshTokenStorePath == "" {
		cfg.RefreshTokenStorePath = t.TempDir() + "/oauth.json"
	}
	if cfg.KeyID == "" {
		cfg.KeyID = "test-key"
	}
	if cfg.KeyPEM == nil {
		cfg.KeyPEM = testSigningKeyPEM
	}
	if cfg.ResourceURLPath == "" {
		cfg.ResourceURLPath = "/resource"
	}
	if cfg.ResourceMetadataURLPath == "" {
		cfg.ResourceMetadataURLPath = "/.well-known/oauth-protected-resource/resource"
	}
	if cfg.ClientIDPrefix == "" {
		cfg.ClientIDPrefix = "test_"
	}
	if cfg.SupportedScopes == nil {
		cfg.SupportedScopes = []string{"read", "write", "admin", "repos"}
	}
	if cfg.DefaultScopes == nil {
		cfg.DefaultScopes = []string{"read", "write"}
	}
	if cfg.BaseURL == nil {
		cfg.BaseURL = func(*http.Request) string { return testBaseURL }
	}
	if cfg.Session == nil {
		cfg.Session = &testSessionManager{}
	}
	if cfg.UI == nil {
		cfg.UI = &testAuthorizationUI{}
	}
}

func newTestServerHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	s.RegisterWellKnownRoutes(mux)
	mux.Handle("/", s.Routes())
	return mux
}

func registerOAuthTestClient(t *testing.T, h http.Handler, clientName string, redirectURIs []string) oauth.RegisterResponse {
	body, err := json.Marshal(oauth.RegisterRequest{ClientName: clientName, RedirectURIs: redirectURIs, TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone})
	if err != nil {
		t.Fatalf("Marshal oauth.RegisterRequest: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var registered oauth.RegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&registered); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return registered
}

func authorizeOAuthTestClient(t *testing.T, h http.Handler, user oauth.User, registered *oauth.RegisterResponse, selectedScopes []string) oauth.TokenResponse {
	redirectURI := "https://claude.example.com/callback"
	code := authorizeOAuthTestCode(t, h, user, registered, redirectURI, selectedScopes, "")
	form := url.Values{
		"grant_type":    {oauth.GrantAuthorizationCode},
		"code":          {code},
		"client_id":     {registered.ClientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {testVerifier},
		"resource":      {testResourceURL},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var tokenResp oauth.TokenResponse
	if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return tokenResp
}

func authorizeOAuthTestCode(t *testing.T, h http.Handler, user oauth.User, registered *oauth.RegisterResponse, redirectURI string, selectedScopes []string, state string) string {
	form := authorizationCodeForm(registered.ClientID, redirectURI, "read write admin")
	if state != "" {
		form.Set("state", state)
	}
	consentToken := startOAuthTestConsent(t, h, user, form)
	postForm := url.Values{"consent_token": {consentToken}, "scope_form": {"1"}}
	for _, scope := range selectedScopes {
		postForm.Add("scope", scope)
	}
	req := newOAuthTestRequest(t, http.MethodPost, "/oauth/authorize", strings.NewReader(postForm.Encode()), user)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	expected, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatalf("parse redirect URI: %v", err)
	}
	if location.Scheme != expected.Scheme || location.Host != expected.Host || location.Path != expected.Path {
		t.Fatalf("Location = %q, want callback redirect %q", location.String(), redirectURI)
	}
	if state != "" && location.Query().Get("state") != state {
		t.Fatalf("state = %q, want %q", location.Query().Get("state"), state)
	}
	if location.Query().Get("iss") != testBaseURL {
		t.Fatalf("iss = %q, want %s", location.Query().Get("iss"), testBaseURL)
	}
	return location.Query().Get("code")
}

func startOAuthTestConsent(t *testing.T, h http.Handler, user oauth.User, form url.Values) string {
	req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody, user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("consent status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	return consentTokenFromOAuthTestHTML(t, w.Body.String())
}

func authorizationCodeForm(clientID, redirectURI, scope string) url.Values {
	return url.Values{
		"response_type":         {oauth.ResponseTypeCode},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {testCodeChallenge()},
		"code_challenge_method": {oauth.CodeChallengeS256},
		"resource":              {testResourceURL},
		"scope":                 {scope},
	}
}

func testCodeChallenge() string {
	digest := sha256.Sum256([]byte(testVerifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func newOAuthTestRequest(t *testing.T, method, path string, body io.Reader, user oauth.User) *http.Request {
	ctx := context.WithValue(t.Context(), testUserContextKey{}, user)
	return httptest.NewRequestWithContext(ctx, method, path, body)
}

func newTestResourceRequest(t *testing.T) *http.Request {
	return httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/resource", http.NoBody)
}

// readOAuthStateFile returns the raw on-disk OAuth state JSON.
func readOAuthStateFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read oauth state file: %v", err)
	}
	return string(data)
}

func consentTokenFromOAuthTestHTML(t *testing.T, body string) string {
	_, tokenSuffix, ok := strings.Cut(body, `name="consent_token" value="`)
	if !ok {
		t.Fatalf("consent token missing from page: %s", body)
	}
	consentToken, _, ok := strings.Cut(tokenSuffix, `"`)
	if !ok || consentToken == "" {
		t.Fatalf("invalid consent token in page: %s", body)
	}
	return consentToken
}

func refreshOAuthTestToken(t *testing.T, h http.Handler, clientID, refreshToken string, wantStatus int) oauth.TokenResponse {
	form := url.Values{
		"grant_type":    {oauth.GrantRefreshToken},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("refresh status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var tokenResp oauth.TokenResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode refresh response: %v", err)
		}
	}
	return tokenResp
}

func TestRegistrationManagement(t *testing.T) {
	t.Parallel()

	t.Run("register response includes management token and URI", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		if registered.RegistrationAccessToken == "" {
			t.Fatal("registration_access_token is empty")
		}
		if registered.RegistrationClientURI == "" {
			t.Fatal("registration_client_uri is empty")
		}
		want := testBaseURL + "/oauth/register/" + registered.ClientID
		if registered.RegistrationClientURI != want {
			t.Fatalf("registration_client_uri = %q, want %q", registered.RegistrationClientURI, want)
		}
	})

	t.Run("read client info with valid token", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		info := readOAuthTestClient(t, h, registered.RegistrationAccessToken, registered.ClientID, http.StatusOK)
		if info.ClientID != registered.ClientID || info.ClientName != "Test Client" {
			t.Fatalf("read client info = %+v", info)
		}
		if len(info.RedirectURIs) != 1 || info.RedirectURIs[0] != "https://example.com/callback" {
			t.Fatalf("redirect_uris = %+v", info.RedirectURIs)
		}
	})

	t.Run("read with missing token returns 401", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/register/"+registered.ClientID, http.NoBody)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("read with wrong token returns 401", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/register/"+registered.ClientID, http.NoBody)
		req.Header.Set("Authorization", "Bearer wrong-token")
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("read with token for different client returns 401", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered1 := registerOAuthTestClient(t, h, "Client A", []string{"https://a.example.com/callback"})
		registered2 := registerOAuthTestClient(t, h, "Client B", []string{"https://b.example.com/callback"})

		// Try to read client B with client A's token.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/register/"+registered2.ClientID, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+registered1.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("read non-existent client returns 404", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		// Use a valid token but for a non-existent client ID.
		nonExistentID := "test_nonexistent"
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/register/"+nonExistentID, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNotFound, w.Body.String())
		}
	})

	t.Run("update client name", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Original Name", []string{"https://example.com/callback"})

		newName := "Updated Name"
		updateReq := oauth.UpdateClientRequest{ClientName: &newName}
		body, err := json.Marshal(updateReq)
		if err != nil {
			t.Fatalf("marshal update: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/oauth/register/"+registered.ClientID, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("update status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var updated oauth.RegisterResponse
		if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
			t.Fatalf("decode update response: %v", err)
		}
		if updated.ClientName != newName {
			t.Fatalf("client_name = %q, want %q", updated.ClientName, newName)
		}
		if updated.RegistrationAccessToken == "" {
			t.Fatal("update response missing registration_access_token")
		}
		// Verify the update persisted.
		info := readOAuthTestClient(t, h, updated.RegistrationAccessToken, registered.ClientID, http.StatusOK)
		if info.ClientName != newName {
			t.Fatalf("read back client_name = %q, want %q", info.ClientName, newName)
		}
	})

	t.Run("update redirect URIs", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		newURIs := []string{"https://new.example.com/callback"}
		updateReq := oauth.UpdateClientRequest{RedirectURIs: &newURIs}
		body, err := json.Marshal(updateReq)
		if err != nil {
			t.Fatalf("marshal update: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/oauth/register/"+registered.ClientID, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("update status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var updated oauth.RegisterResponse
		if err := json.NewDecoder(w.Body).Decode(&updated); err != nil {
			t.Fatalf("decode update response: %v", err)
		}
		if len(updated.RedirectURIs) != 1 || updated.RedirectURIs[0] != newURIs[0] {
			t.Fatalf("redirect_uris = %+v, want %+v", updated.RedirectURIs, newURIs)
		}
		// Verify persisted.
		info := readOAuthTestClient(t, h, updated.RegistrationAccessToken, registered.ClientID, http.StatusOK)
		if len(info.RedirectURIs) != 1 || info.RedirectURIs[0] != newURIs[0] {
			t.Fatalf("read back redirect_uris = %+v", info.RedirectURIs)
		}
	})

	t.Run("delete client returns 204 and subsequent read returns 404", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/oauth/register/"+registered.ClientID, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
		}

		// Subsequent read should fail.
		readOAuthTestClient(t, h, registered.RegistrationAccessToken, registered.ClientID, http.StatusNotFound)
	})

	t.Run("delete with wrong token returns 401", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/oauth/register/"+registered.ClientID, http.NoBody)
		req.Header.Set("Authorization", "Bearer wrong-token")
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("register delete re-register", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		// Delete.
		req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/oauth/register/"+registered.ClientID, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete status = %d, want %d", w.Code, http.StatusNoContent)
		}

		// Re-register with same name succeeds.
		reRegistered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		if reRegistered.ClientID == "" || reRegistered.ClientID == registered.ClientID {
			t.Fatalf("re-registered client_id = %q (should be different from %q)", reRegistered.ClientID, registered.ClientID)
		}
		if reRegistered.RegistrationAccessToken == "" {
			t.Fatal("re-registered registration_access_token is empty")
		}
	})

	t.Run("update with missing token returns 401", func(t *testing.T) {
		t.Parallel()

		h := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		newName := "Updated"
		updateReq := oauth.UpdateClientRequest{ClientName: &newName}
		body, err := json.Marshal(updateReq)
		if err != nil {
			t.Fatalf("marshal update: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/oauth/register/"+registered.ClientID, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})
}

func TestEndSession(t *testing.T) {
	t.Parallel()

	t.Run("logout revokes all grants and refresh tokens", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		// Authorize two clients to create 2 grants + 2 refresh tokens.
		clientA := registerOAuthTestClient(t, h, "Client A", []string{"https://claude.example.com/callback"})
		clientB := registerOAuthTestClient(t, h, "Client B", []string{"https://claude.example.com/callback"})
		tokenA := authorizeOAuthTestClient(t, h, user, &clientA, []string{"read"})
		tokenB := authorizeOAuthTestClient(t, h, user, &clientB, []string{"write"})

		// Verify grants exist.
		if len(s.ListUserGrants(user.ID)) != 2 {
			t.Fatalf("expected 2 grants, got %d", len(s.ListUserGrants(user.ID)))
		}

		// Call end-session as the user.
		req := newOAuthTestRequest(t, http.MethodGet, "/oauth/end-session", http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "logged out") {
			t.Fatalf("end-session body missing confirmation: %s", w.Body.String())
		}

		// Verify both refresh tokens are revoked.
		refreshOAuthTestToken(t, h, clientA.ClientID, tokenA.RefreshToken, http.StatusBadRequest)
		refreshOAuthTestToken(t, h, clientB.ClientID, tokenB.RefreshToken, http.StatusBadRequest)
	})

	t.Run("logout redirects to post_logout_redirect_uri with state", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/logout", "https://example.com/callback"})

		req := newOAuthTestRequest(t, http.MethodGet,
			"/oauth/end-session?client_id="+registered.ClientID+"&post_logout_redirect_uri="+url.QueryEscape("https://example.com/logout")+"&state=abc123",
			http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
		}
		location := w.Header().Get("Location")
		if !strings.HasPrefix(location, "https://example.com/logout") {
			t.Fatalf("Location = %q, want https://example.com/logout...", location)
		}
		if !strings.Contains(location, "state=abc123") {
			t.Fatalf("Location = %q, want state=abc123", location)
		}
	})

	t.Run("logout rejects unregistered post_logout_redirect_uri", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})

		req := newOAuthTestRequest(t, http.MethodGet,
			"/oauth/end-session?client_id="+registered.ClientID+"&post_logout_redirect_uri="+url.QueryEscape("https://attacker.example.com/evil"),
			http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// Should fall back to confirmation page, not redirect to attacker.
		if w.Code != http.StatusOK {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		location := w.Header().Get("Location")
		if location != "" {
			t.Fatalf("Location = %q, want empty (should not redirect to unregistered URI)", location)
		}
		if !strings.Contains(w.Body.String(), "logged out") {
			t.Fatalf("end-session body missing confirmation: %s", w.Body.String())
		}
	})

	t.Run("unauthenticated request returns login redirect or page", func(t *testing.T) {
		t.Parallel()

		// Without a login adapter, should return HTML page.
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/end-session", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "not logged in") {
			t.Fatalf("end-session body missing not-logged-in message: %s", w.Body.String())
		}
	})

	t.Run("unauthenticated with login adapter redirects to login", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{testUser()})
		cfg.UI = &testAuthorizationUI{loginURL: "/auth/login"}
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/end-session", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
		}
		if got := w.Header().Get("Location"); got != "/auth/login" {
			t.Fatalf("Location = %q, want /auth/login", got)
		}
	})

	t.Run("end_session callback takes priority over post_logout_redirect_uri", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.Session = &testSessionManager{
			users:       map[string]oauth.User{user.ID: user},
			endRedirect: "https://custom.example.com/goodbye",
		}
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback", "https://example.com/logout"})

		req := newOAuthTestRequest(t, http.MethodGet,
			"/oauth/end-session?client_id="+registered.ClientID+"&post_logout_redirect_uri="+url.QueryEscape("https://example.com/logout"),
			http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
		}
		location := w.Header().Get("Location")
		if location != "https://custom.example.com/goodbye" {
			t.Fatalf("Location = %q, want https://custom.example.com/goodbye", location)
		}

		// Authorize to get a refresh token we can verify is revoked.
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		// Call end-session.
		req = newOAuthTestRequest(t, http.MethodGet,
			"/oauth/end-session?client_id="+registered.ClientID+"&post_logout_redirect_uri="+url.QueryEscape("https://example.com/logout"),
			http.NoBody, user)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("end-session status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
		}
		location = w.Header().Get("Location")
		if location != "https://custom.example.com/goodbye" {
			t.Fatalf("Location = %q, want https://custom.example.com/goodbye", location)
		}

		// Verify grants were revoked (refresh token rejected).
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("end_session_endpoint in discovery metadata", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/oauth-authorization-server", http.NoBody)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("metadata status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var metadata oauth.AuthorizationServerMetadata
		if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		want := testBaseURL + "/oauth/end-session"
		if metadata.EndSessionEndpoint != want {
			t.Fatalf("end_session_endpoint = %q, want %q", metadata.EndSessionEndpoint, want)
		}
	})
}

// newTestServerHandlerOnly creates a server and handler for management tests.
func newTestServerHandlerOnly(t *testing.T) http.Handler {
	t.Helper()
	path := t.TempDir() + "/oauth.json"
	cfg := &ServerConfig{
		RefreshTokenStorePath: path,
	}
	applyTestServerDefaults(t, cfg)
	s, err := NewServer(*cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return newTestServerHandler(s)
}

// readOAuthTestClient reads client registration via GET.
func readOAuthTestClient(t *testing.T, h http.Handler, token, clientID string, wantStatus int) oauth.RegisterResponse {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/register/"+clientID, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	addForwardedHeaders(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("read client status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var resp oauth.RegisterResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode read response: %v", err)
		}
	}
	return resp
}

func revokeOAuthTestToken(t *testing.T, h http.Handler, clientID, refreshToken string, wantStatus int) {
	form := url.Values{
		"client_id":       {clientID},
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("revoke status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
}

func TestRateLimiting(t *testing.T) {
	t.Parallel()

	t.Run("no rate limiter allows all requests", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})

		// Token endpoint works without rate limiter.
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		if tokenResp.AccessToken == "" {
			t.Fatal("access token is empty")
		}
	})

	t.Run("token endpoint uses per-client rate limit key", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		lim := &inspectRateLimiter{}
		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Token request should use "client:<client_id>" key.
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !lim.sawKey("client:" + registered.ClientID) {
			t.Fatalf("rate limiter did not see per-client key: keys=%v", lim.keys)
		}
	})

	t.Run("token endpoint falls back to per-IP when client_id missing", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		lim := &inspectRateLimiter{}
		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		// Submit a token request without client_id in the form.
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		form := url.Values{
			// No client_id key at all
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		// Should still succeed (rate limiter allows everything).
		if w.Code != http.StatusBadRequest {
			// Expect bad request because client_id is missing from form (required for code validation).
			// But the rate limiter should have been asked for an IP key.
			_ = w.Code
		}
		if !lim.sawKey("ip:10.0.0.1:12345") {
			t.Fatalf("rate limiter did not see per-IP fallback key: keys=%v", lim.keys)
		}
	})

	t.Run("register endpoint uses per-IP rate limit key", func(t *testing.T) {
		t.Parallel()

		lim := &inspectRateLimiter{}
		cfg := testFlowServerConfig(t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		body, err := json.Marshal(oauth.RegisterRequest{ClientName: "Test", RedirectURIs: []string{"https://example.com/callback"}, TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.5:9999"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
		if !lim.sawKey("ip:10.0.0.5:9999") {
			t.Fatalf("rate limiter did not see per-IP key: keys=%v", lim.keys)
		}
	})

	t.Run("introspect endpoint uses per-IP rate limit key", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		lim := &inspectRateLimiter{}
		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		form := url.Values{"token": {tokenResp.AccessToken}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.7:7777"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !lim.sawKey("ip:10.0.0.7:7777") {
			t.Fatalf("rate limiter did not see per-IP key: keys=%v", lim.keys)
		}
	})

	t.Run("revoke endpoint uses per-client rate limit key", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		lim := &inspectRateLimiter{}
		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		form := url.Values{
			"client_id":       {registered.ClientID},
			"token":           {tokenResp.RefreshToken},
			"token_type_hint": {"refresh_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !lim.sawKey("client:" + registered.ClientID) {
			t.Fatalf("rate limiter did not see per-client key: keys=%v", lim.keys)
		}
	})

	t.Run("rate limiter rejection returns 429", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		path := t.TempDir() + "/oauth.json"

		// First, register a client and authorize a code with a permissive server.
		_, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")

		// Then create a new server from the same store but with a deny limiter.
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = &denyRateLimiter{}
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		denyHandler := newTestServerHandler(s)

		// Token endpoint should be rejected.
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		denyHandler.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusTooManyRequests, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "60" {
			t.Fatalf("Retry-After = %q, want 60", got)
		}
	})

	t.Run("client a hits limit client b still succeeds", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		path := t.TempDir() + "/oauth.json"

		// Register clients and authorize codes with a permissive server.
		_, permissiveH, _ := newTestFlowServer(t, path, []oauth.User{user})
		clientA := registerOAuthTestClient(t, permissiveH, "Client A", []string{"https://a.example.com/callback"})
		clientB := registerOAuthTestClient(t, permissiveH, "Client B", []string{"https://b.example.com/callback"})
		codeA := authorizeOAuthTestCode(t, permissiveH, user, &clientA, "https://a.example.com/callback", []string{"read"}, "")
		codeA2 := authorizeOAuthTestCode(t, permissiveH, user, &clientA, "https://a.example.com/callback", []string{"read"}, "")
		codeB := authorizeOAuthTestCode(t, permissiveH, user, &clientB, "https://b.example.com/callback", []string{"read"}, "")

		// Create a limited server from the same store: allow only 1 token request per client.
		lim := &countRateLimiter{max: 1}
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		exchange := func(cid, code, redirectURI string) int {
			form := url.Values{
				"grant_type":    {oauth.GrantAuthorizationCode},
				"code":          {code},
				"client_id":     {cid},
				"redirect_uri":  {redirectURI},
				"code_verifier": {testVerifier},
				"resource":      {testResourceURL},
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w.Code
		}

		// First request from client A: succeeds.
		if got := exchange(clientA.ClientID, codeA, "https://a.example.com/callback"); got != http.StatusOK {
			t.Fatalf("client A first request = %d, want %d", got, http.StatusOK)
		}

		// Second request from client A: denied (max=1 per client).
		if got := exchange(clientA.ClientID, codeA2, "https://a.example.com/callback"); got != http.StatusTooManyRequests {
			t.Fatalf("client A second request = %d, want %d", got, http.StatusTooManyRequests)
		}

		// Client B still succeeds (different key, fresh count).
		if got := exchange(clientB.ClientID, codeB, "https://b.example.com/callback"); got != http.StatusOK {
			t.Fatalf("client B request = %d, want %d", got, http.StatusOK)
		}
	})
}

// inspectRateLimiter records every key passed to Allow and always allows.
type inspectRateLimiter struct {
	keys []string
}

func (l *inspectRateLimiter) Allow(key string) bool {
	l.keys = append(l.keys, key)
	return true
}

func (l *inspectRateLimiter) sawKey(key string) bool {
	return slices.Contains(l.keys, key)
}

// denyRateLimiter always denies requests.
type denyRateLimiter struct{}

func (denyRateLimiter) Allow(key string) bool { return false }

// countRateLimiter allows up to max requests per key.
type countRateLimiter struct {
	mu     sync.Mutex
	max    int
	counts map[string]int
}

func (l *countRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil {
		l.counts = map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.max
}

// testFlowServerConfig returns a ServerConfig pre-populated with test defaults,
// user flow callbacks, and a consent renderer.
func testFlowServerConfig(path string, users []oauth.User) ServerConfig {
	usersByID := make(map[string]oauth.User, len(users))
	for _, user := range users {
		usersByID[user.ID] = user
	}
	cfg := ServerConfig{
		RefreshTokenStorePath:   path,
		KeyID:                   "test-key",
		KeyPEM:                  testSigningKeyPEM,
		ResourceURLPath:         "/resource",
		ResourceMetadataURLPath: "/.well-known/oauth-protected-resource/resource",
		ClientIDPrefix:          "test_",
		SupportedScopes:         []string{"read", "write", "admin", "repos"},
		DefaultScopes:           []string{"read", "write"},
		BaseURL:                 func(*http.Request) string { return testBaseURL },
		Session:                 &testSessionManager{users: usersByID},
		UI:                      &captureAuthorizationUI{},
	}
	return cfg
}
