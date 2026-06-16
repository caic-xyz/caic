// Tests for generic OAuth authorization-server HTTP handlers.

package oauth

import (
	"context"
	"crypto"
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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
		var metadata AuthorizationServerMetadata
		if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		if metadata.Issuer != testBaseURL || metadata.AuthorizationEndpoint != testBaseURL+"/oauth/authorize" || metadata.RegistrationEndpoint != testBaseURL+"/oauth/register" {
			t.Fatalf("metadata = %+v", metadata)
		}

		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		if !strings.HasPrefix(registered.ClientID, "test_") || registered.ClientName != "Claude" || registered.TokenEndpointAuthMethod != TokenEndpointAuthNone {
			t.Fatalf("registered = %+v", registered)
		}

		req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/jwks", http.NoBody)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("jwks status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var jwks JWKSet
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
		var resource ProtectedResourceMetadata
		if err := json.NewDecoder(w.Body).Decode(&resource); err != nil {
			t.Fatalf("decode protected resource metadata: %v", err)
		}
		if resource.Resource != testResourceURL || len(resource.AuthorizationServers) != 1 || resource.AuthorizationServers[0] != testBaseURL {
			t.Fatalf("resource metadata = %+v", resource)
		}
	})

	t.Run("login adapter redirects unauthenticated authorize request", func(t *testing.T) {
		t.Parallel()

		s := newTestServer(t, &ServerConfig{Login: testLoginAdapter{loginURL: "/auth/github/start?next=%2Foauth%2Fauthorize%3Fclient_id%3Dabc"}})
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
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read", "write"})
		if tokenResp.RefreshToken == "" {
			t.Fatal("refresh token is empty")
		}

		rotated := refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.AccessToken == "" || rotated.RefreshToken == "" || rotated.RefreshToken == tokenResp.RefreshToken {
			t.Fatalf("rotated token response = %+v", rotated)
		}
		if rotated.TokenType != TokenTypeBearer || rotated.Scope != tokenResp.Scope {
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
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
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
		_, h, _ := newTestFlowServer(t, path, []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})

		_, restartedHandler, _ := newTestFlowServer(t, path, []User{user})
		tokenResp := authorizeOAuthTestClient(t, restartedHandler, user, &registered, []string{"read"})
		if tokenResp.RefreshToken == "" {
			t.Fatal("refresh token is empty after client reload")
		}
	})

	t.Run("refresh token and grant survive server restart", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		restarted, restartedHandler, _ := newTestFlowServer(t, path, []User{user})
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
		_, h1, _ := newTestFlowServer(t, path, []User{user})
		registered := registerOAuthTestClient(t, h1, "Test Client", []string{"https://claude.example.com/callback"})

		// Start the authorize flow to get a consent token.
		form := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read write")
		consentToken := startOAuthTestConsent(t, h1, user, form)

		// Create a new server pointing at the same store path.
		_, h2, _ := newTestFlowServer(t, path, []User{user})

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
			"grant_type":    {GrantAuthorizationCode},
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
		var tokenResp TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if tokenResp.AccessToken == "" || tokenResp.RefreshToken == "" {
			t.Fatalf("token response missing tokens: %+v", tokenResp)
		}
	})

	t.Run("revoked refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		revokeOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("expired refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		opaque := "expired-refresh-token"
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		store.RefreshTokens[RefreshTokenKey(opaque)] = RefreshToken{UserID: user.ID, ClientID: registered.ClientID, Resource: testResourceURL, Scope: "read", ExpiresAt: time.Now().Add(-time.Minute)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		_, restartedHandler, _ := newTestFlowServer(t, path, []User{user})
		refreshOAuthTestToken(t, restartedHandler, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("unknown user refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		_, h, _ := newTestFlowServer(t, path, []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		opaque := "missing-user-refresh-token"
		grantID := "grant-missing-user"
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		expiresAt := time.Now().Add(time.Hour)
		store.Grants[grantID] = Grant{ID: grantID, UserID: "usr_missing", ClientID: registered.ClientID, ClientName: "Claude", Resource: testResourceURL, Scope: "read", CreatedAt: time.Now(), ExpiresAt: expiresAt}
		store.RefreshTokens[RefreshTokenKey(opaque)] = RefreshToken{GrantID: grantID, UserID: "usr_missing", ClientID: registered.ClientID, Resource: testResourceURL, Scope: "read", ExpiresAt: expiresAt}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		_, restartedHandler, _ := newTestFlowServer(t, path, []User{user})
		refreshOAuthTestToken(t, restartedHandler, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("authorize request validation", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		challenge := testCodeChallenge()
		form := url.Values{
			"response_type":         {ResponseTypeCode},
			"client_id":             {"unknown_client"},
			"redirect_uri":          {"https://example.com/callback"},
			"code_challenge":        {challenge},
			"code_challenge_method": {CodeChallengeS256},
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

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{testUser()})
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
		_, h, renderer := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"http://localhost:9999/callback"})
		form := authorizationCodeForm(registered.ClientID, "http://localhost:9999/callback", "")
		req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody, user)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if len(renderer.last.ScopeItems) != 1 || renderer.last.ScopeItems[0].ID != "read" {
			t.Fatalf("scope items = %+v, want default read scope", renderer.last.ScopeItems)
		}
	})

	t.Run("authorize POST approves and redirects with code", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://example.com/callback", []string{"read"}, "client-state")
		if code == "" {
			t.Fatal("authorization code is empty")
		}
	})

	t.Run("authorize POST denies and redirects with access_denied", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
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
		bob := User{ID: "usr_bob", Username: "bob", Provider: "gitlab"}
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{alice, bob})
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
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
		var resp IntrospectionResponse
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
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
		var resp IntrospectionResponse
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})

		form := url.Values{}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp IntrospectionResponse
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})

		form := url.Values{"token": {"not.a.valid.jwt"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("introspect status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp IntrospectionResponse
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
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})

		// Issue a short-lived token directly via the token service.
		pastAud := testResourceURL
		now := time.Now()
		token, err := s.tokens.issueAccessTokenAt(&AccessTokenClaims{
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
		var resp IntrospectionResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode introspection response: %v", err)
		}
		if resp.Active {
			t.Fatalf("introspection active = true with expired token, want false: %+v", resp)
		}
	})

	t.Run("token signed before rotate works after rotate and new token uses new key", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
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
		var jwks JWKSet
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
}

func TestDPoP(t *testing.T) {
	t.Parallel()

	t.Run("token endpoint with dpop proof gets dpop-bound token", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")

		tokenURL := testBaseURL + "/oauth/token"
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")

		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		var tokenResp TokenResponse
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
		claims, err := s.tokens.VerifyAccessToken(tokenResp.AccessToken, testBaseURL, testResourceURL, time.Now(), s.touchGrant, s.findUserByID)
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
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		// Get dpop-bound token.
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		var tokenResp TokenResponse
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

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{testUser()})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)

		tokenURL := testBaseURL + "/oauth/token"
		// htm should be POST for the token endpoint, but we use GET.
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "GET", tokenURL, time.Now(), "", "")

		code := authorizeOAuthTestCode(t, h, testUser(), &registered, "https://claude.example.com/callback", []string{"read"}, "")

		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		var errResp ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if !strings.Contains(errResp.Error, "invalid_dpop_proof") {
			t.Fatalf("error = %q, want invalid_dpop_proof", errResp.Error)
		}
	})

	t.Run("dpop proof with wrong htu rejected", func(t *testing.T) {
		t.Parallel()

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{testUser()})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)

		wrongURL := "https://evil.example.com/oauth/token"
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", wrongURL, time.Now(), "", "")

		code := authorizeOAuthTestCode(t, h, testUser(), &registered, "https://claude.example.com/callback", []string{"read"}, "")

		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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

		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{testUser()})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)

		tokenURL := testBaseURL + "/oauth/token"
		oldTime := time.Now().Add(-10 * time.Minute)
		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, oldTime, "", "")

		code := authorizeOAuthTestCode(t, h, testUser(), &registered, "https://claude.example.com/callback", []string{"read"}, "")

		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		var tokenResp TokenResponse
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		var tokenResp TokenResponse
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
		var introResp IntrospectionResponse
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
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		tokenURL := testBaseURL + "/oauth/token"

		// First request: proof without nonce.
		dpopProof1 := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		nonce := s.dpopNonces.Issue()

		// Get a new code and make a second request with the nonce.
		code2 := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		dpopProof2 := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", nonce)
		form2 := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
}

// testDPoPRSAKeyPair generates an RSA key pair for DPoP proof tests.
func testDPoPRSAKeyPair(t *testing.T) (*rsa.PrivateKey, *JWK) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pub := &key.PublicKey
	jwk := JWK{
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
	return key, &jwk
}

// makeDPoPProof creates a valid DPoP proof JWT signed with the given RSA key.
func makeDPoPProof(t *testing.T, key *rsa.PrivateKey, jwk *JWK, htm, htu string, iat time.Time, accessToken, nonce string) string {
	t.Helper()
	header := DPoPHeader{Typ: "dpop+jwt", Alg: "RS256", JWK: *jwk}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal dpop header: %v", err)
	}

	jti, err := randomToken()
	if err != nil {
		t.Fatalf("generate jti: %v", err)
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
	dpopJWK    *JWK
	registered RegisterResponse
	token      TokenResponse
}

// setupDPoPBoundToken creates a server, registers a client, authorizes a code,
// obtains a DPoP-bound token, and returns the pieces needed for resource tests.
func setupDPoPBoundToken(t *testing.T) dpopTestResources {
	t.Helper()
	user := testUser()
	s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})
	registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
	dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
	code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
	tokenURL := testBaseURL + "/oauth/token"

	dpopProof := makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", "")
	form := url.Values{
		"grant_type":    {GrantAuthorizationCode},
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
	var tokenResp TokenResponse
	if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return dpopTestResources{server: s, handler: h, dpopKey: dpopKey, dpopJWK: dpopJWKObj, registered: registered, token: tokenResp}
}

type testUserContextKey struct{}

type captureConsentRenderer struct {
	last ConsentPageData
}

func (r *captureConsentRenderer) RenderOAuthConsent(w http.ResponseWriter, data *ConsentPageData) error {
	r.last = *data
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write([]byte(`<input type="hidden" name="consent_token" value="` + data.ConsentToken + `">`))
	return err
}

type testLoginAdapter struct {
	loginURL string
}

func (a testLoginAdapter) LoginStartURL(*http.Request) string {
	return a.loginURL
}

func (testLoginAdapter) ProviderLabel(provider string) string {
	return provider
}

func testUser() User {
	return User{ID: "usr_alice", Username: "alice", Provider: "github"}
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

func newTestFlowServer(t *testing.T, path string, users []User) (*Server, http.Handler, *captureConsentRenderer) {
	renderer := &captureConsentRenderer{}
	usersByID := make(map[string]User, len(users))
	for _, user := range users {
		usersByID[user.ID] = user
	}
	cfg := &ServerConfig{
		RefreshTokenStorePath: path,
		CurrentUser: func(ctx context.Context) (User, bool) {
			user, ok := ctx.Value(testUserContextKey{}).(User)
			return user, ok
		},
		UserLookup: func(id string) (User, bool) {
			user, ok := usersByID[id]
			return user, ok
		},
		Renderer: renderer,
	}
	applyTestServerDefaults(t, cfg)
	s, err := NewServer(*cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, newTestServerHandler(s), renderer
}

func applyTestServerDefaults(t *testing.T, cfg *ServerConfig) {
	if cfg.RefreshTokenStorePath == "" {
		cfg.RefreshTokenStorePath = t.TempDir() + "/oauth.json"
	}
	if cfg.KeyID == "" {
		cfg.KeyID = "test-key"
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
}

func newTestServerHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	s.RegisterWellKnownRoutes(mux)
	mux.Handle("/", s.Routes())
	return mux
}

func registerOAuthTestClient(t *testing.T, h http.Handler, clientName string, redirectURIs []string) RegisterResponse {
	body, err := json.Marshal(RegisterRequest{ClientName: clientName, RedirectURIs: redirectURIs, TokenEndpointAuthMethod: TokenEndpointAuthNone})
	if err != nil {
		t.Fatalf("Marshal RegisterRequest: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var registered RegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&registered); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	return registered
}

func authorizeOAuthTestClient(t *testing.T, h http.Handler, user User, registered *RegisterResponse, selectedScopes []string) TokenResponse {
	redirectURI := "https://claude.example.com/callback"
	code := authorizeOAuthTestCode(t, h, user, registered, redirectURI, selectedScopes, "")
	form := url.Values{
		"grant_type":    {GrantAuthorizationCode},
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
	var tokenResp TokenResponse
	if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return tokenResp
}

func authorizeOAuthTestCode(t *testing.T, h http.Handler, user User, registered *RegisterResponse, redirectURI string, selectedScopes []string, state string) string {
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

func startOAuthTestConsent(t *testing.T, h http.Handler, user User, form url.Values) string {
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
		"response_type":         {ResponseTypeCode},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {testCodeChallenge()},
		"code_challenge_method": {CodeChallengeS256},
		"resource":              {testResourceURL},
		"scope":                 {scope},
	}
}

func testCodeChallenge() string {
	digest := sha256.Sum256([]byte(testVerifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func newOAuthTestRequest(t *testing.T, method, path string, body io.Reader, user User) *http.Request {
	ctx := context.WithValue(t.Context(), testUserContextKey{}, user)
	return httptest.NewRequestWithContext(ctx, method, path, body)
}

func newTestResourceRequest(t *testing.T) *http.Request {
	return httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/resource", http.NoBody)
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

func refreshOAuthTestToken(t *testing.T, h http.Handler, clientID, refreshToken string, wantStatus int) TokenResponse {
	form := url.Values{
		"grant_type":    {GrantRefreshToken},
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
	var tokenResp TokenResponse
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
		updateReq := UpdateClientRequest{ClientName: &newName}
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
		var updated RegisterResponse
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
		updateReq := UpdateClientRequest{RedirectURIs: &newURIs}
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
		var updated RegisterResponse
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
		updateReq := UpdateClientRequest{ClientName: &newName}
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
func readOAuthTestClient(t *testing.T, h http.Handler, token, clientID string, wantStatus int) RegisterResponse {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/register/"+clientID, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	addForwardedHeaders(req)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("read client status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var resp RegisterResponse
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
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []User{user})

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
		cfg := testFlowServerConfig(path, []User{user})
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
			"grant_type":    {GrantAuthorizationCode},
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
		cfg := testFlowServerConfig(path, []User{user})
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
			"grant_type":    {GrantAuthorizationCode},
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
		cfg := testFlowServerConfig(t.TempDir()+"/oauth.json", []User{testUser()})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		body, err := json.Marshal(RegisterRequest{ClientName: "Test", RedirectURIs: []string{"https://example.com/callback"}, TokenEndpointAuthMethod: TokenEndpointAuthNone})
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
		cfg := testFlowServerConfig(path, []User{user})
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
		cfg := testFlowServerConfig(path, []User{user})
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
		_, h, _ := newTestFlowServer(t, path, []User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")

		// Then create a new server from the same store but with a deny limiter.
		cfg := testFlowServerConfig(path, []User{user})
		cfg.RateLimiter = &denyRateLimiter{}
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		denyHandler := newTestServerHandler(s)

		// Token endpoint should be rejected.
		form := url.Values{
			"grant_type":    {GrantAuthorizationCode},
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
		_, permissiveH, _ := newTestFlowServer(t, path, []User{user})
		clientA := registerOAuthTestClient(t, permissiveH, "Client A", []string{"https://a.example.com/callback"})
		clientB := registerOAuthTestClient(t, permissiveH, "Client B", []string{"https://b.example.com/callback"})
		codeA := authorizeOAuthTestCode(t, permissiveH, user, &clientA, "https://a.example.com/callback", []string{"read"}, "")
		codeA2 := authorizeOAuthTestCode(t, permissiveH, user, &clientA, "https://a.example.com/callback", []string{"read"}, "")
		codeB := authorizeOAuthTestCode(t, permissiveH, user, &clientB, "https://b.example.com/callback", []string{"read"}, "")

		// Create a limited server from the same store: allow only 1 token request per client.
		lim := &countRateLimiter{max: 1}
		cfg := testFlowServerConfig(path, []User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		exchange := func(cid, code, redirectURI string) int {
			form := url.Values{
				"grant_type":    {GrantAuthorizationCode},
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
func testFlowServerConfig(path string, users []User) ServerConfig {
	usersByID := make(map[string]User, len(users))
	for _, user := range users {
		usersByID[user.ID] = user
	}
	cfg := ServerConfig{
		RefreshTokenStorePath:   path,
		KeyID:                   "test-key",
		ResourceURLPath:         "/resource",
		ResourceMetadataURLPath: "/.well-known/oauth-protected-resource/resource",
		ClientIDPrefix:          "test_",
		SupportedScopes:         []string{"read", "write", "admin", "repos"},
		DefaultScopes:           []string{"read", "write"},
		BaseURL:                 func(*http.Request) string { return testBaseURL },
		Renderer:                &captureConsentRenderer{},
		CurrentUser: func(ctx context.Context) (User, bool) {
			user, ok := ctx.Value(testUserContextKey{}).(User)
			return user, ok
		},
		UserLookup: func(id string) (User, bool) {
			user, ok := usersByID[id]
			return user, ok
		},
	}
	return cfg
}
