// Tests for generic OAuth authorization-server HTTP handlers.

package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
