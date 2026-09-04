// Tests for generic OAuth authorization-server HTTP handlers.

package oauthserver

import (
	"bytes"
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
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
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

	t.Run("issuer and audience ignore request authority headers", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		statePath := t.TempDir() + "/oauth.json"
		s, h, _ := newTestFlowServer(t, statePath, []oauth.User{user})

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/oauth-authorization-server", http.NoBody)
		req.Host = "poison.example"
		req.Header.Set("Forwarded", "host=forwarded-poison.example;proto=http")
		req.Header.Set("X-Forwarded-Host", "x-poison.example")
		req.Header.Set("X-Forwarded-Proto", "http")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var metadata oauth.AuthorizationServerMetadata
		if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
			t.Fatalf("decode metadata: %v", err)
		}
		if metadata.Issuer != testBaseURL || metadata.TokenEndpoint != testBaseURL+"/oauth/token" {
			t.Fatalf("metadata authority was request-derived: %+v", metadata)
		}

		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://claude.example.com/callback"})
		token := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"}).AccessToken
		resourceRequest := newTestResourceRequest(t)
		resourceRequest.Host = "poison.example"
		resourceRequest.Header.Set("Forwarded", "host=forwarded-poison.example;proto=http")
		claims, err := s.verifyBearer(resourceRequest, token)
		if err != nil {
			t.Fatalf("verify immutable-audience token: %v", err)
		}
		if claims.Issuer != testBaseURL || claims.Audience != testResourceURL {
			t.Fatalf("claims identity = issuer %q audience %q", claims.Issuer, claims.Audience)
		}
	})

	t.Run("issuer configuration validates secure and local origins", func(t *testing.T) {
		t.Parallel()
		for _, issuer := range []string{"", "http://public.example", "https://user@example.com", "https://example.com/path", "https://example.com?", "https://example.com?query=1", "https://example.com#"} {
			cfg := &ServerConfig{Issuer: issuer}
			applyTestServerDefaults(t, cfg)
			cfg.Issuer = issuer
			if _, err := NewServer(*cfg); err == nil {
				t.Errorf("NewServer Issuer %q error = nil", issuer)
			}
		}
		cfg := &ServerConfig{Issuer: "http://127.0.0.1:2242"}
		applyTestServerDefaults(t, cfg)
		s, err := NewServer(*cfg)
		if err != nil {
			t.Fatalf("NewServer loopback issuer: %v", err)
		}
		s.Close()
		canonical, err := validateIssuer("HTTPS://Example.COM:443/")
		if err != nil {
			t.Fatalf("validate canonical issuer: %v", err)
		}
		if canonical != "HTTPS://Example.COM:443" {
			t.Fatalf("canonical issuer = %q", canonical)
		}
	})

	t.Run("legacy foreign-resource authorization state is contained on restart", func(t *testing.T) {
		t.Parallel()
		path := t.TempDir() + "/oauth.json"
		user := testUser()
		const (
			clientID     = "legacy-client"
			code         = "legacy-code"
			foreignGrant = "legacy-foreign-grant"
			foreignToken = "legacy-foreign-refresh"
			mixedGrant   = "legacy-mixed-grant"
			mixedRefresh = "legacy-mixed-refresh"
			foreignURL   = "https://poison.example/resource"
		)
		now := time.Now()
		legacy, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		err = legacy.transact(func(next *storeFile) bool {
			next.Clients[clientID] = Client{ID: clientID, RedirectURIs: []string{"https://client.example/callback"}}
			next.Codes[oauth.RefreshTokenKey(code)] = Code{UserID: user.ID, ClientID: clientID, RedirectURI: "https://client.example/callback", CodeChallenge: testCodeChallenge(), Resource: foreignURL, Scope: "read", ExpiresAt: now.Add(time.Hour)}
			next.Consents[oauth.RefreshTokenKey("legacy-consent")] = ConsentParams{UserID: user.ID, Params: map[string]string{"client_id": clientID, "resource": foreignURL}, ExpiresAt: now.Add(time.Hour)}
			next.Grants[foreignGrant] = Grant{ID: foreignGrant, UserID: user.ID, ClientID: clientID, Resource: foreignURL, Scope: "read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			next.RefreshTokens[oauth.RefreshTokenKey(foreignToken)] = RefreshToken{GrantID: foreignGrant, UserID: user.ID, ClientID: clientID, Resource: foreignURL, Scope: "read", ExpiresAt: now.Add(time.Hour)}
			next.Grants[mixedGrant] = Grant{ID: mixedGrant, UserID: user.ID, ClientID: clientID, Resource: testResourceURL, Scope: "read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			next.RefreshTokens[oauth.RefreshTokenKey(mixedRefresh)] = RefreshToken{GrantID: mixedGrant, UserID: user.ID, ClientID: clientID, Resource: foreignURL, Scope: "read", ExpiresAt: now.Add(time.Hour)}
			return true
		})
		if err != nil {
			t.Fatalf("seed legacy state: %v", err)
		}
		tokens, err := NewAccessTokenService(testSigningKeyPEM, "test-key", time.Hour)
		if err != nil {
			t.Fatalf("NewAccessTokenService: %v", err)
		}
		legacyAccessToken, err := tokens.IssueAccessToken(testBaseURL, user, testResourceURL, "read", foreignGrant)
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}

		cfg := testFlowServerConfig(path, []oauth.User{user})
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		codeForm := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "code": {code}, "client_id": {clientID}, "redirect_uri": {"https://client.example/callback"}, "code_verifier": {testVerifier}, "resource": {testResourceURL}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(codeForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("legacy code status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		refreshOAuthTestToken(t, h, clientID, foreignToken, http.StatusBadRequest)
		refreshOAuthTestToken(t, h, clientID, mixedRefresh, http.StatusBadRequest)
		if _, err := s.verifyBearer(newTestResourceRequest(t), legacyAccessToken); err == nil {
			t.Fatal("legacy access token backed by foreign-resource grant remained valid")
		}
		s.Close()

		restarted, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("restart NewServer: %v", err)
		}
		t.Cleanup(restarted.Close)
		restarted.mu.Lock()
		defer restarted.mu.Unlock()
		if _, found := restarted.state.Codes[oauth.RefreshTokenKey(code)]; found || len(restarted.state.Consents) != 0 {
			t.Fatalf("foreign transient state survived restart: codes=%d consents=%d", len(restarted.state.Codes), len(restarted.state.Consents))
		}
		for _, grantID := range []string{foreignGrant, mixedGrant} {
			if restarted.state.Grants[grantID].RevokedAt.IsZero() {
				t.Fatalf("grant %q was not durably revoked", grantID)
			}
		}
		for _, token := range []string{foreignToken, mixedRefresh} {
			if restarted.state.RefreshTokens[oauth.RefreshTokenKey(token)].RevokedAt.IsZero() {
				t.Fatalf("refresh token %q was not durably revoked", token)
			}
		}
	})

	t.Run("configured issuer spelling remains compatible across upgrade", func(t *testing.T) {
		t.Parallel()
		path := t.TempDir() + "/oauth.json"
		user := testUser()
		const (
			issuer   = "https://Caic.Example.COM"
			resource = issuer + "/resource"
			clientID = "case-compatible-client"
			grantID  = "case-compatible-grant"
			refresh  = "case-compatible-refresh"
		)
		now := time.Now()
		legacy, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		err = legacy.transact(func(next *storeFile) bool {
			next.Clients[clientID] = Client{ID: clientID}
			next.Grants[grantID] = Grant{ID: grantID, UserID: user.ID, ClientID: clientID, Resource: resource, Scope: "read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			next.RefreshTokens[oauth.RefreshTokenKey(refresh)] = RefreshToken{GrantID: grantID, UserID: user.ID, ClientID: clientID, Resource: resource, Scope: "read", ExpiresAt: now.Add(time.Hour)}
			return true
		})
		if err != nil {
			t.Fatalf("seed compatible state: %v", err)
		}
		tokens, err := NewAccessTokenService(testSigningKeyPEM, "test-key", time.Hour)
		if err != nil {
			t.Fatalf("NewAccessTokenService: %v", err)
		}
		accessToken, err := tokens.IssueAccessToken(issuer, user, resource, "read", grantID)
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}

		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.Issuer = issuer
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if s.issuer != issuer || s.resourceURL != resource {
			t.Fatalf("configured identity changed: issuer=%q resource=%q", s.issuer, s.resourceURL)
		}
		if _, err := s.verifyBearer(newTestResourceRequest(t), accessToken); err != nil {
			t.Fatalf("compatible access token rejected: %v", err)
		}
		rotated := refreshOAuthTestToken(t, newTestServerHandler(s), clientID, refresh, http.StatusOK)
		if rotated.RefreshToken == "" {
			t.Fatal("compatible refresh family did not rotate")
		}
		s.Close()

		restarted, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("restart NewServer: %v", err)
		}
		t.Cleanup(restarted.Close)
		if _, err := restarted.verifyBearer(newTestResourceRequest(t), accessToken); err != nil {
			t.Fatalf("compatible access token rejected after restart: %v", err)
		}
		restarted.mu.Lock()
		grant := restarted.state.Grants[grantID]
		refreshResources := make([]string, 0)
		for _, entry := range restarted.state.RefreshTokens {
			if entry.GrantID == grantID {
				refreshResources = append(refreshResources, entry.Resource)
			}
		}
		restarted.mu.Unlock()
		if !grant.RevokedAt.IsZero() || grant.Resource != resource {
			t.Fatalf("compatible grant was changed or revoked: %+v", grant)
		}
		for _, refreshResource := range refreshResources {
			if refreshResource != resource {
				t.Fatalf("refresh resource was rewritten to %q", refreshResource)
			}
		}
	})

	t.Run("foreign-resource state introduced after startup fails closed", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		s := newTestServer(t, &ServerConfig{Session: &testSessionManager{users: map[string]oauth.User{user.ID: user}}})
		const (
			accessGrantID = "foreign-access-grant"
			clientID      = "foreign-client"
			grantID       = "foreign-grant"
			refresh       = "foreign-refresh"
			foreignURL    = "https://poison.example/resource"
		)
		now := time.Now()
		s.mu.Lock()
		err := s.state.transact(func(next *storeFile) bool {
			next.Clients[clientID] = Client{ID: clientID}
			next.Grants[accessGrantID] = Grant{ID: accessGrantID, UserID: user.ID, ClientID: clientID, Resource: foreignURL, Scope: "read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			next.Grants[grantID] = Grant{ID: grantID, UserID: user.ID, ClientID: clientID, Resource: foreignURL, Scope: "read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			next.RefreshTokens[oauth.RefreshTokenKey(refresh)] = RefreshToken{GrantID: grantID, UserID: user.ID, ClientID: clientID, Resource: foreignURL, Scope: "read", ExpiresAt: now.Add(time.Hour)}
			return true
		})
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("seed foreign state: %v", err)
		}
		if _, err := s.issueTokenResponse(user, foreignURL, "read", grantID, "new-refresh", ""); err == nil {
			t.Fatal("issueTokenResponse accepted a foreign resource")
		}
		accessToken, err := s.tokens.IssueAccessToken(s.issuer, user, s.resourceURL, "read", accessGrantID)
		if err != nil {
			t.Fatalf("IssueAccessToken: %v", err)
		}
		if _, err := s.verifyBearer(newTestResourceRequest(t), accessToken); err == nil {
			t.Fatal("foreign-resource grant remained valid for access")
		}
		result, _, err := s.exchangeRefreshToken(refresh, clientID, user.ID, "next-refresh", dpopBinding{})
		if err != nil || result != refreshExchangeUnknown {
			t.Fatalf("exchangeRefreshToken result=%v err=%v", result, err)
		}
		s.mu.Lock()
		accessGrant := s.state.Grants[accessGrantID]
		grant := s.state.Grants[grantID]
		entry := s.state.RefreshTokens[oauth.RefreshTokenKey(refresh)]
		s.mu.Unlock()
		if accessGrant.RevokedAt.IsZero() || grant.RevokedAt.IsZero() || entry.RevokedAt.IsZero() {
			t.Fatalf("foreign state was not revoked: accessGrant=%+v grant=%+v refresh=%+v", accessGrant, grant, entry)
		}
	})

	t.Run("introspection authenticates and scopes callers", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		cfg := testFlowServerConfig(t.TempDir()+"/oauth.json", []oauth.User{user})
		cfg.IntrospectionAuth = func(r *http.Request) (IntrospectionPrincipal, bool) {
			clientID := r.Header.Get("X-Test-Introspection-Client")
			return IntrospectionPrincipal{ClientID: clientID}, clientID != ""
		}
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		client := registerOAuthTestClient(t, h, "Authorized", []string{"https://claude.example.com/callback"})
		wrongClient := registerOAuthTestClient(t, h, "Wrong", []string{"https://claude.example.com/callback"})
		token := authorizeOAuthTestClient(t, h, user, &client, []string{"read"}).AccessToken

		introspect := func(clientID, rawToken string) (int, string, oauth.IntrospectionResponse) {
			form := url.Values{"token": {rawToken}}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if clientID != "" {
				req.Header.Set("X-Test-Introspection-Client", clientID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			var response oauth.IntrospectionResponse
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			return w.Code, w.Body.String(), response
		}

		if status, _, response := introspect(client.ClientID, token); status != http.StatusOK || !response.Active {
			t.Fatalf("authorized introspection = status %d response %+v", status, response)
		}
		_, wrongBody, wrongResponse := introspect(wrongClient.ClientID, token)
		_, invalidBody, invalidResponse := introspect(wrongClient.ClientID, "invalid.token")
		if wrongResponse.Active || invalidResponse.Active || wrongBody != invalidBody {
			t.Fatalf("wrong-client response leaked token state: wrong=%q invalid=%q", wrongBody, invalidBody)
		}
		anonymousStatus, anonymousBody, _ := introspect("", token)
		invalidAnonymousStatus, invalidAnonymousBody, _ := introspect("", "invalid.token")
		if anonymousStatus != http.StatusUnauthorized || invalidAnonymousStatus != http.StatusUnauthorized || anonymousBody != invalidAnonymousBody {
			t.Fatalf("anonymous response leaked token state: valid=(%d,%q) invalid=(%d,%q)", anonymousStatus, anonymousBody, invalidAnonymousStatus, invalidAnonymousBody)
		}
	})

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
		if metadata.IntrospectionEndpoint != "" || len(metadata.IntrospectionEndpointAuthMethodsSupported) != 0 {
			t.Fatalf("metadata advertises disabled OAuth capabilities: %+v", metadata)
		}
		if !slices.Equal(metadata.DPoPSigningAlgValuesSupported, []string{"RS256", "ES256", "EdDSA"}) {
			t.Fatalf("dpop algorithms = %v", metadata.DPoPSigningAlgValuesSupported)
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

	t.Run("concurrent authorization code redemption has one durable winner", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		path := t.TempDir() + "/oauth.json"
		s, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		clientID := "concurrent-client"
		code := "concurrent-code-secret"
		now := time.Now()
		s.mu.Lock()
		err := s.state.transact(func(next *storeFile) bool {
			next.Clients[clientID] = Client{ID: clientID, Name: "Concurrent client", RedirectURIs: []string{"https://client.example/callback"}}
			next.Codes[oauth.RefreshTokenKey(code)] = Code{UserID: user.ID, ClientID: clientID, RedirectURI: "https://client.example/callback", CodeChallenge: testCodeChallenge(), Resource: testResourceURL, Scope: "read", ExpiresAt: now.Add(time.Minute)}
			return true
		})
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("seed authorization code: %v", err)
		}

		const attempts = 24
		statuses := make(chan int, attempts)
		var wg sync.WaitGroup
		for range attempts {
			wg.Go(func() {
				form := url.Values{
					"grant_type":    {oauth.GrantAuthorizationCode},
					"client_id":     {clientID},
					"code":          {code},
					"code_verifier": {testVerifier},
					"redirect_uri":  {"https://client.example/callback"},
					"resource":      {testResourceURL},
				}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				statuses <- w.Code
			})
		}
		wg.Wait()
		close(statuses)
		successes := 0
		for status := range statuses {
			if status == http.StatusOK {
				successes++
				continue
			}
			if status != http.StatusBadRequest {
				t.Fatalf("redemption status = %d, want 200 or 400", status)
			}
		}
		if successes != 1 {
			t.Fatalf("successful redemptions = %d, want 1", successes)
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if _, ok := reloaded.Codes[oauth.RefreshTokenKey(code)]; ok || len(reloaded.Grants) != 1 || len(reloaded.RefreshTokens) != 1 {
			t.Fatalf("restart state after redemption = codes=%d grants=%d refreshTokens=%d", len(reloaded.Codes), len(reloaded.Grants), len(reloaded.RefreshTokens))
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
		original, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		original.Close()

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
		original, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})

		original.Close()
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
		original, h1, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h1, "Test Client", []string{"https://claude.example.com/callback"})

		// Start the authorize flow to get a consent token.
		form := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read write")
		consentToken := startOAuthTestConsent(t, h1, user, form)
		original.Close()

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
		original, h, _ := newTestFlowServer(t, path, []oauth.User{user})
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

		original.Close()
		_, restartedHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		refreshOAuthTestToken(t, restartedHandler, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("unknown user refresh token is rejected", func(t *testing.T) {
		t.Parallel()

		path := t.TempDir() + "/oauth.json"
		user := testUser()
		original, h, _ := newTestFlowServer(t, path, []oauth.User{user})
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

		original.Close()
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

	t.Run("concurrent refresh redemption revokes the raced family", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		const attempts = 16
		statuses := make(chan int, attempts)
		var wg sync.WaitGroup
		for range attempts {
			wg.Go(func() {
				form := url.Values{"grant_type": {oauth.GrantRefreshToken}, "client_id": {registered.ClientID}, "refresh_token": {tokenResp.RefreshToken}}
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				statuses <- w.Code
			})
		}
		wg.Wait()
		close(statuses)
		successes := 0
		for status := range statuses {
			if status == http.StatusOK {
				successes++
			} else if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 200 or 400", status)
			}
		}
		if successes != 1 {
			t.Fatalf("successful redemptions = %d, want 1", successes)
		}
		grants := s.ListUserGrants(user.ID)
		if len(grants) != 1 || grants[0].RevokedAt.IsZero() {
			t.Fatalf("raced refresh family remained active: %+v", grants)
		}
		s.mu.Lock()
		for digest, token := range s.state.RefreshTokens {
			if token.GrantID == grants[0].ID && token.RevokedAt.IsZero() {
				s.mu.Unlock()
				t.Fatalf("refresh token %q remained active after raced reuse", digest)
			}
		}
		s.mu.Unlock()
	})

	t.Run("refresh signing failure does not consume token", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		tokenResp := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		currentKID := s.tokens.currentKID
		s.tokens.currentKID = "missing-signing-key"
		refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusInternalServerError)
		s.tokens.currentKID = currentKID
		rotated := refreshOAuthTestToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.RefreshToken == "" {
			t.Fatal("refresh token was consumed by signing failure")
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

func TestTransactionalOAuthMutations(t *testing.T) {
	t.Parallel()

	t.Run("configured state path has one server owner", func(t *testing.T) {
		t.Parallel()
		path := t.TempDir() + "/oauth.json"
		cfg := testFlowServerConfig(path, []oauth.User{testUser()})
		first, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("first NewServer: %v", err)
		}
		if _, err := NewServer(cfg); err == nil || !strings.Contains(err.Error(), "already has an owner") {
			t.Fatalf("second NewServer error = %v, want exclusive ownership error", err)
		}
		first.Close()
		second, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer after Close: %v", err)
		}
		second.Close()
	})

	t.Run("authorization code consume failure preserves code across restart", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		path := t.TempDir() + "/oauth.json"
		s, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		clientID := "fault-client"
		code := "fault-code"
		s.mu.Lock()
		err := s.state.transact(func(next *storeFile) bool {
			next.Clients[clientID] = Client{ID: clientID, RedirectURIs: []string{"https://client.example/callback"}}
			next.Codes[oauth.RefreshTokenKey(code)] = Code{UserID: user.ID, ClientID: clientID, RedirectURI: "https://client.example/callback", CodeChallenge: testCodeChallenge(), Resource: testResourceURL, Scope: "read", ExpiresAt: time.Now().Add(time.Hour)}
			return true
		})
		s.state.io = failingRenameStoreIO{storeIO: osStoreIO{}}
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("seed state: %v", err)
		}
		form := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "client_id": {clientID}, "code": {code}, "code_verifier": {testVerifier}, "redirect_uri": {"https://client.example/callback"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if _, ok := reloaded.Codes[oauth.RefreshTokenKey(code)]; !ok || len(reloaded.Grants) != 0 || len(reloaded.RefreshTokens) != 0 {
			t.Fatalf("failed code exchange changed restart state: codes=%d grants=%d refreshTokens=%d", len(reloaded.Codes), len(reloaded.Grants), len(reloaded.RefreshTokens))
		}
		s.state.io = osStoreIO{}
		currentKID := s.tokens.currentKID
		s.tokens.currentKID = "missing-signing-key"
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		s.tokens.currentKID = currentKID
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("signing failure status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
		}
		reloaded, err = LoadStore(path)
		if err != nil {
			t.Fatalf("reload signing failure: %v", err)
		}
		if _, ok := reloaded.Codes[oauth.RefreshTokenKey(code)]; !ok || len(reloaded.Grants) != 0 || len(reloaded.RefreshTokens) != 0 {
			t.Fatal("signing failure consumed authorization code")
		}
	})

	t.Run("refresh rotation failure preserves replay state across restart", func(t *testing.T) {
		t.Parallel()
		path := t.TempDir() + "/oauth.json"
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		now := time.Now()
		store.Clients["client"] = Client{ID: "client", RedirectURIs: []string{"https://client.example/callback"}}
		store.Grants["grant"] = Grant{ID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		store.RefreshTokens[oauth.RefreshTokenKey("refresh")] = RefreshToken{GrantID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		store.io = failingRenameStoreIO{storeIO: osStoreIO{}}
		server := &Server{state: store, refreshTokenTTL: time.Hour}
		if result, _, err := server.exchangeRefreshToken("refresh", "client", "user", "next-refresh", dpopBinding{}); err == nil || result != refreshExchangeRotated {
			t.Fatalf("exchangeRefreshToken = %d, %v; want rotation persistence failure", result, err)
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !reloaded.RefreshTokens[oauth.RefreshTokenKey("refresh")].UsedAt.IsZero() || len(reloaded.RefreshTokens) != 1 {
			t.Fatalf("failed rotation changed replay state: %+v", reloaded.RefreshTokens)
		}
	})

	t.Run("revocation failure leaves grant active across restart", func(t *testing.T) {
		t.Parallel()
		path := t.TempDir() + "/oauth.json"
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		now := time.Now()
		store.Grants["grant"] = Grant{ID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		store.RefreshTokens["refresh-digest"] = RefreshToken{GrantID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		store.io = failingRenameStoreIO{storeIO: osStoreIO{}}
		server := &Server{state: store}
		if revoked, err := server.RevokeUserGrant("user", "grant"); err == nil || !revoked {
			t.Fatalf("RevokeUserGrant = %t, %v; want matched grant and persistence failure", revoked, err)
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !reloaded.Grants["grant"].RevokedAt.IsZero() || !reloaded.RefreshTokens["refresh-digest"].RevokedAt.IsZero() {
			t.Fatal("failed revocation persisted partial state")
		}
		store.io = osStoreIO{}
		if revoked, err := server.RevokeUserGrant("user", "grant"); err != nil || !revoked {
			t.Fatalf("successful RevokeUserGrant = %t, %v", revoked, err)
		}
		reloaded, err = LoadStore(path)
		if err != nil {
			t.Fatalf("reload successful revocation: %v", err)
		}
		if reloaded.Grants["grant"].RevokedAt.IsZero() || reloaded.RefreshTokens["refresh-digest"].RevokedAt.IsZero() {
			t.Fatal("successful revocation did not survive restart")
		}
	})

	t.Run("client deletion failure preserves client and credentials", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t)
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Fault client", []string{"https://client.example/callback"})
		s.mu.Lock()
		err := s.state.transact(func(next *storeFile) bool {
			next.Grants["grant"] = Grant{ID: "grant", ClientID: registered.ClientID, ExpiresAt: time.Now().Add(time.Hour)}
			return true
		})
		s.state.io = failingRenameStoreIO{storeIO: osStoreIO{}}
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("seed grant: %v", err)
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/oauth/register/"+registered.ClientID, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
		}
		reloaded, err := LoadStore(s.state.Path())
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if _, ok := reloaded.Clients[registered.ClientID]; !ok || reloaded.Grants["grant"].ClientID != registered.ClientID {
			t.Fatalf("failed deletion changed restart state: %+v", reloaded)
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
		onDisk := readOAuthStateFile(t, s.state.Path())
		if strings.Contains(onDisk, devResp.UserCode) || !strings.Contains(onDisk, oauth.RefreshTokenKey(devResp.UserCode)) {
			t.Fatal("device user code was not persisted exclusively as a digest")
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
		currentKID := s.tokens.currentKID
		s.tokens.currentKID = "missing-signing-key"
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		s.tokens.currentKID = currentKID
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("device signing failure status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
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

	t.Run("pending device poll reserves dpop proof", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(url.Values{"client_id": {registered.ClientID}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		if w.Code != http.StatusOK {
			t.Fatalf("device authorization status = %d: %s", w.Code, w.Body.String())
		}
		var device oauth.DeviceAuthorizationResponse
		if err := json.NewDecoder(w.Body).Decode(&device); err != nil {
			t.Fatalf("decode device authorization: %v", err)
		}
		key, jwk := testDPoPRSAKeyPair(t)
		proof := makeDPoPProof(t, key, jwk, http.MethodPost, dpopTokenURL, time.Now(), "", "")
		tokenForm := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {device.DeviceCode}, "client_id": {registered.ClientID}}
		poll := func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("DPoP", proof)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w
		}
		if w := poll(); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "authorization_pending") {
			t.Fatalf("pending poll status = %d: %s", w.Code, w.Body.String())
		}
		approve := newOAuthTestRequest(t, http.MethodPost, "/oauth/device", strings.NewReader(url.Values{"user_code": {device.UserCode}}.Encode()), user)
		approve.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, approve)
		if w.Code != http.StatusOK {
			t.Fatalf("approve status = %d: %s", w.Code, w.Body.String())
		}
		if w := poll(); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_dpop_proof") {
			t.Fatalf("replayed poll status = %d: %s", w.Code, w.Body.String())
		}
		proof = makeDPoPProof(t, key, jwk, http.MethodPost, dpopTokenURL, time.Now(), "", "")
		w = poll()
		if w.Code != http.StatusOK {
			t.Fatalf("approved dpop poll status = %d: %s", w.Code, w.Body.String())
		}
		var token oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
			t.Fatalf("decode dpop device token: %v", err)
		}
		if token.TokenType != DPoPTokenType {
			t.Fatalf("device token type = %q, want %q", token.TokenType, DPoPTokenType)
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
		original, h1, _ := newTestFlowServer(t, path, []oauth.User{user})
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
		original.Close()

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
		approveForm := url.Values{"user_code": {devResp.UserCode}}
		req = newOAuthTestRequest(t, http.MethodPost, "/oauth/device", strings.NewReader(approveForm.Encode()), user)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device approval after restart status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		h2.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("device exchange after restart status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
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

func TestOAuthSurfaceContainment(t *testing.T) {
	t.Parallel()

	t.Run("public introspection route is disabled", func(t *testing.T) {
		t.Parallel()

		s := newTestServer(t)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/introspect", strings.NewReader("token=secret"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNotFound, w.Body.String())
		}
	})

	t.Run("token endpoint rejects malformed dpop and preserves bearer exchange", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
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
		req.Header.Set("DPoP", "unsupported-proof")
		addForwardedHeaders(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("DPoP token status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if w.Header().Get("DPoP-Nonce") == "" {
			t.Fatal("DPoP-Nonce is empty, want retry nonce")
		}

		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		addForwardedHeaders(req)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("Bearer token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var token oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
			t.Fatalf("decode token response: %v", err)
		}
		if token.TokenType != oauth.TokenTypeBearer || token.AccessToken == "" {
			t.Fatalf("token response = %+v, want Bearer access token", token)
		}
		if w.Header().Get("DPoP-Nonce") != "" {
			t.Fatalf("DPoP-Nonce = %q, want empty", w.Header().Get("DPoP-Nonce"))
		}
	})

	t.Run("resource middleware rejects dpop authorization scheme", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		token := authorizeOAuthTestClient(t, h, user, &registered, []string{"read"})
		protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		req := newTestResourceRequest(t)
		req.Header.Set("Authorization", DPoPTokenType+" "+token.AccessToken)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}

		grants := s.ListUserGrants(user.ID)
		if len(grants) != 1 {
			t.Fatalf("grants = %+v, want one grant", grants)
		}
		legacyToken, err := s.tokens.IssueDPoPAccessToken(testBaseURL, user, testResourceURL, "read", grants[0].ID, "legacy-key-thumbprint")
		if err != nil {
			t.Fatalf("issue legacy DPoP access token: %v", err)
		}
		for _, scheme := range []string{oauth.TokenTypeBearer, DPoPTokenType} {
			req = newTestResourceRequest(t)
			req.Header.Set("Authorization", scheme+" "+legacyToken)
			w = httptest.NewRecorder()
			protected.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s status = %d, want %d: %s", scheme, w.Code, http.StatusUnauthorized, w.Body.String())
			}
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

	t.Run("ec and ed25519 token flows get dpop-bound tokens", func(t *testing.T) {
		t.Parallel()
		ecKey, ecJWK := testDPoPECKeyPair(t, elliptic.P256(), "P-256")
		edPublic, edPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate ed25519 key: %v", err)
		}
		edJWK := &oauth.JWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(edPublic)}
		for _, tc := range []struct {
			name   string
			alg    string
			signer crypto.Signer
			jwk    *oauth.JWK
		}{
			{name: "ec", alg: "ES256", signer: ecKey, jwk: ecJWK},
			{name: "ed25519", alg: "EdDSA", signer: edPrivate, jwk: edJWK},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				user := testUser()
				s, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
				registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
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
				req.Header.Set("DPoP", makeDPoPProofSigned(t, tc.alg, tc.signer, tc.jwk))
				addForwardedHeaders(req)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
				}
				var token oauth.TokenResponse
				if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
					t.Fatalf("decode token: %v", err)
				}
				claims, err := s.tokens.VerifyAccessToken(token.AccessToken, testBaseURL, testResourceURL, time.Now(), s.touchGrant, s.session)
				if err != nil {
					t.Fatalf("verify token: %v", err)
				}
				wantJKT, err := JWKThumbprint(tc.jwk)
				if err != nil {
					t.Fatalf("thumbprint: %v", err)
				}
				if token.TokenType != DPoPTokenType || claims.Confirmation == nil || claims.Confirmation.JKT != wantJKT {
					t.Fatalf("token = %+v, claims = %+v", token, claims)
				}
				protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
				resourceRequest := newTestResourceRequest(t)
				resourceRequest.Header.Set("Authorization", DPoPTokenType+" "+token.AccessToken)
				resourceRequest.Header.Set("DPoP", makeDPoPRequestProofSigned(t, tc.alg, tc.signer, tc.jwk, http.MethodPost, testResourceURL, token.AccessToken, ""))
				w = httptest.NewRecorder()
				protected.ServeHTTP(w, resourceRequest)
				if w.Code != http.StatusNoContent {
					t.Fatalf("resource status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
				}
				for refreshNumber := range 2 {
					refreshForm := url.Values{"grant_type": {oauth.GrantRefreshToken}, "refresh_token": {token.RefreshToken}, "client_id": {registered.ClientID}}
					refreshRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(refreshForm.Encode()))
					refreshRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					refreshRequest.Header.Set("DPoP", makeDPoPRequestProofSigned(t, tc.alg, tc.signer, tc.jwk, http.MethodPost, dpopTokenURL, "", ""))
					addForwardedHeaders(refreshRequest)
					w = httptest.NewRecorder()
					h.ServeHTTP(w, refreshRequest)
					if w.Code != http.StatusOK {
						t.Fatalf("refresh %d status = %d, want %d: %s", refreshNumber, w.Code, http.StatusOK, w.Body.String())
					}
					if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
						t.Fatalf("decode refresh %d: %v", refreshNumber, err)
					}
				}
			})
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
		protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		statePath := t.TempDir() + "/oauth.json"
		s, h, _ := newTestFlowServer(t, statePath, []oauth.User{user})
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
		nonce, err := s.issueDPoPNonce()
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

		s.Close()
		_, restartedHandler, _ := newTestFlowServer(t, statePath, []oauth.User{user})
		code3 := authorizeOAuthTestCode(t, restartedHandler, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		form2.Set("code", code3)
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form2.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("DPoP", makeDPoPProof(t, dpopKey, dpopJWKObj, "POST", tokenURL, time.Now(), "", nonce))
		addForwardedHeaders(req)
		w = httptest.NewRecorder()
		restartedHandler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("reused nonce after restart status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})

	t.Run("replayed resource proof with same jti rejected", func(t *testing.T) {
		t.Parallel()

		res := setupDPoPBoundToken(t)
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	t.Run("resource proof replay remains rejected after restart", func(t *testing.T) {
		t.Parallel()
		statePath := t.TempDir() + "/oauth.json"
		user := testUser()
		s, h, _ := newTestFlowServer(t, statePath, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWK := testDPoPRSAKeyPair(t)
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		proof := makeDPoPProof(t, dpopKey, dpopJWK, http.MethodPost, dpopTokenURL, time.Now(), "", "")
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
			t.Fatalf("token status = %d: %s", w.Code, w.Body.String())
		}
		var token oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
			t.Fatalf("decode token: %v", err)
		}

		resourceProof := makeDPoPProof(t, dpopKey, dpopJWK, http.MethodPost, testResourceURL, time.Now(), token.AccessToken, "")
		protected := s.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		resourceRequest := newTestResourceRequest(t)
		resourceRequest.Header.Set("Authorization", DPoPTokenType+" "+token.AccessToken)
		resourceRequest.Header.Set("DPoP", resourceProof)
		addForwardedHeaders(resourceRequest)
		w = httptest.NewRecorder()
		protected.ServeHTTP(w, resourceRequest)
		if w.Code != http.StatusNoContent {
			t.Fatalf("initial resource status = %d: %s", w.Code, w.Body.String())
		}

		s.Close()
		restarted, _, _ := newTestFlowServer(t, statePath, []oauth.User{user})
		protected = restarted.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		resourceRequest = newTestResourceRequest(t)
		resourceRequest.Header.Set("Authorization", DPoPTokenType+" "+token.AccessToken)
		resourceRequest.Header.Set("DPoP", resourceProof)
		addForwardedHeaders(resourceRequest)
		w = httptest.NewRecorder()
		protected.ServeHTTP(w, resourceRequest)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("replayed resource status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("refresh token remains bound to its original key", func(t *testing.T) {
		t.Parallel()
		res := setupDPoPBoundToken(t)
		wrongKey, wrongJWK := testDPoPRSAKeyPair(t)
		form := url.Values{
			"grant_type":    {oauth.GrantRefreshToken},
			"refresh_token": {res.token.RefreshToken},
			"client_id":     {res.registered.ClientID},
		}
		exchange := func(proof string) *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("DPoP", proof)
			addForwardedHeaders(req)
			w := httptest.NewRecorder()
			res.handler.ServeHTTP(w, req)
			return w
		}
		if w := exchange(makeDPoPProof(t, wrongKey, wrongJWK, http.MethodPost, dpopTokenURL, time.Now(), "", "")); w.Code != http.StatusBadRequest {
			t.Fatalf("wrong-key refresh status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		acceptedProof := makeDPoPProof(t, res.dpopKey, res.dpopJWK, http.MethodPost, dpopTokenURL, time.Now(), "", "")
		w := exchange(acceptedProof)
		if w.Code != http.StatusOK {
			t.Fatalf("original-key refresh status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var token oauth.TokenResponse
		if err := json.NewDecoder(w.Body).Decode(&token); err != nil {
			t.Fatalf("decode refreshed token: %v", err)
		}
		if token.TokenType != DPoPTokenType {
			t.Fatalf("token type = %q, want %q", token.TokenType, DPoPTokenType)
		}
		if w := exchange(makeDPoPProof(t, wrongKey, wrongJWK, http.MethodPost, dpopTokenURL, time.Now(), "", "")); w.Code != http.StatusBadRequest {
			t.Fatalf("used-token wrong-key status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		if w := exchange(acceptedProof); w.Code != http.StatusBadRequest {
			t.Fatalf("exact refresh replay status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		form.Set("refresh_token", token.RefreshToken)
		if w := exchange(makeDPoPProof(t, res.dpopKey, res.dpopJWK, http.MethodPost, dpopTokenURL, time.Now(), "", "")); w.Code != http.StatusOK {
			t.Fatalf("family refresh after attack status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("resource proof is bound to the requested path", func(t *testing.T) {
		t.Parallel()
		res := setupDPoPBoundToken(t)
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		proof := makeDPoPProof(t, res.dpopKey, res.dpopJWK, http.MethodPost, testResourceURL, time.Now(), res.token.AccessToken, "")
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, testBaseURL+"/different", http.NoBody)
		req.Header.Set("Authorization", DPoPTokenType+" "+res.token.AccessToken)
		req.Header.Set("DPoP", proof)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusUnauthorized, w.Body.String())
		}
	})

	t.Run("fresh resource proof per request accepted", func(t *testing.T) {
		t.Parallel()

		res := setupDPoPBoundToken(t)
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		protected := res.server.BearerAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	t.Run("token endpoint rejects a repeated jti", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		_, h, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		dpopKey, dpopJWKObj := testDPoPRSAKeyPair(t)
		tokenURL := testBaseURL + "/oauth/token"

		// Two distinct token exchanges that reuse the same jti demonstrate proof
		// replay even though the authorization codes themselves differ.
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
			want := http.StatusOK
			if i == 1 {
				want = http.StatusBadRequest
			}
			if w.Code != want {
				t.Fatalf("token exchange %d status = %d, want %d: %s", i, w.Code, want, w.Body.String())
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

	t.Run("embedded private jwk member is rejected", func(t *testing.T) {
		t.Parallel()
		key, jwk := testDPoPRSAKeyPair(t)
		header := map[string]any{"typ": "dpop+jwt", "alg": "RS256", "jwk": map[string]any{"kty": jwk.Kty, "n": jwk.N, "e": jwk.E, "d": "private"}}
		headerJSON, err := json.Marshal(header)
		if err != nil {
			t.Fatalf("marshal header: %v", err)
		}
		claimsJSON, err := json.Marshal(DPoPClaims{JTI: "private-jwk", HTM: http.MethodPost, HTU: dpopTokenURL, IAT: time.Now().Unix()})
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		input := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
		signature, err := signJWS("RS256", key, []byte(input))
		if err != nil {
			t.Fatalf("sign proof: %v", err)
		}
		if _, _, err := DPoPProof(httpDPoPRequest(t, input+"."+base64.RawURLEncoding.EncodeToString(signature))); err == nil {
			t.Fatal("DPoPProof accepted a private JWK member")
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

func TestDPoPReplayState(t *testing.T) {
	t.Parallel()

	t.Run("lower integer-second boundary remains reserved", func(t *testing.T) {
		t.Parallel()
		now := time.Unix(1_000, int64(500*time.Millisecond))
		binding := dpopBinding{jkt: "key", jti: "boundary", iat: now.Unix() - int64(defaultDPoPMaxAge/time.Second)}
		state := newEmptyStore("").snapshot()
		if !reserveDPoPBinding(&state, binding, now) {
			t.Fatal("lower-bound proof was not reserved through the remainder of its acceptance second")
		}
		if reserveDPoPBinding(&state, binding, now) {
			t.Fatal("lower-bound proof replay was accepted")
		}
	})

	t.Run("active proof capacity fails closed", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		state := newEmptyStore("").snapshot()
		for i := range maxDPoPJTIEntries {
			state.DPoPProofs[strconv.Itoa(i)] = now.Add(time.Minute)
		}
		binding := dpopBinding{jkt: "key", jti: "overflow", iat: now.Unix()}
		if reserveDPoPBinding(&state, binding, now) {
			t.Fatal("proof was reserved beyond active capacity")
		}
		if len(state.DPoPProofs) != maxDPoPJTIEntries {
			t.Fatalf("proof entries = %d, want %d without eviction", len(state.DPoPProofs), maxDPoPJTIEntries)
		}
	})

	t.Run("active nonce capacity fails closed", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		state := newEmptyStore("").snapshot()
		for i := range maxDPoPJTIEntries {
			state.DPoPNonces[strconv.Itoa(i)] = now.Add(time.Minute)
		}
		binding := dpopBinding{jkt: "key", jti: "nonce-overflow", iat: now.Unix(), nonce: "nonce", nonceExpiresAt: now.Add(time.Minute)}
		if reserveDPoPBinding(&state, binding, now) {
			t.Fatal("nonce was reserved beyond active capacity")
		}
		if len(state.DPoPNonces) != maxDPoPJTIEntries {
			t.Fatalf("nonce entries = %d, want %d without eviction", len(state.DPoPNonces), maxDPoPJTIEntries)
		}
	})
}

// testDPoPRSAKeyPair generates an RSA key pair for DPoP proof tests.
func testDPoPRSAKeyPair(t testing.TB) (*rsa.PrivateKey, *oauth.JWK) {
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
func makeDPoPProof(t testing.TB, key *rsa.PrivateKey, jwk *oauth.JWK, htm, htu string, iat time.Time, accessToken, nonce string) string {
	jti, err := randomToken()
	if err != nil {
		t.Fatalf("generate jti: %v", err)
	}
	return makeDPoPProofWithJTI(t, key, jwk, htm, htu, iat, accessToken, nonce, jti)
}

// makeDPoPProofWithJTI creates a DPoP proof JWT with an explicit jti (which may
// be empty to exercise the missing-jti rejection path).
func makeDPoPProofWithJTI(t testing.TB, key *rsa.PrivateKey, jwk *oauth.JWK, htm, htu string, iat time.Time, accessToken, nonce, jti string) string {
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
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, dpopTokenURL, http.NoBody)
	r.Header.Set("DPoP", proof)
	addForwardedHeaders(r)
	return r
}

// makeDPoPProofSigned builds a valid DPoP proof with a chosen header alg, signed
// by signer. RSA and ECDSA sign the SHA-256 digest; Ed25519 signs the raw input.
func makeDPoPProofSigned(t *testing.T, alg string, signer crypto.Signer, jwk *oauth.JWK) string {
	return makeDPoPRequestProofSigned(t, alg, signer, jwk, http.MethodPost, dpopTokenURL, "", "")
}

func makeDPoPRequestProofSigned(t *testing.T, alg string, signer crypto.Signer, jwk *oauth.JWK, method, requestURL, accessToken, nonce string) string {
	jti, err := randomToken()
	if err != nil {
		t.Fatalf("generate jti: %v", err)
	}
	header := DPoPHeader{Typ: "dpop+jwt", Alg: alg, JWK: *jwk}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal dpop header: %v", err)
	}
	claims := DPoPClaims{JTI: jti, HTM: method, HTU: requestURL, IAT: time.Now().Unix(), Nonce: nonce}
	if accessToken != "" {
		claims.ATH = DPoPAccessTokenHash(accessToken)
	}
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
	if cfg.Issuer == "" {
		cfg.Issuer = testBaseURL
	}
	if cfg.Session == nil {
		cfg.Session = &testSessionManager{}
	}
	if cfg.UI == nil {
		cfg.UI = &testAuthorizationUI{}
	}
	if cfg.IntrospectionAuth == nil {
		cfg.IntrospectionAuth = func(*http.Request) (IntrospectionPrincipal, bool) {
			return IntrospectionPrincipal{}, true
		}
	}
}

func newTestServerHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	s.RegisterWellKnownRoutes(mux)
	// Keep the dormant implementation covered until authenticated
	// introspection is restored in a later hardening phase.
	mux.HandleFunc("POST /oauth/introspect", s.handleOAuthIntrospect)
	mux.Handle("/", s.Routes())
	return mux
}

func registerOAuthTestClient(t *testing.T, h http.Handler, clientName string, redirectURIs []string) oauth.RegisterResponse {
	return registerOAuthTestClientRequest(t, h, &oauth.RegisterRequest{
		ClientName:              clientName,
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone,
		GrantTypes:              []string{oauth.GrantAuthorizationCode, oauth.GrantRefreshToken, oauth.GrantDeviceCode},
	}, http.StatusCreated)
}

func registerOAuthTestClientRequest(t *testing.T, h http.Handler, registration *oauth.RegisterRequest, wantStatus int) oauth.RegisterResponse {
	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatalf("Marshal oauth.RegisterRequest: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("register status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var registered oauth.RegisterResponse
	if w.Code == http.StatusCreated {
		if err := json.NewDecoder(w.Body).Decode(&registered); err != nil {
			t.Fatalf("decode register response: %v", err)
		}
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

		s := newTestServer(t)
		h := newTestServerHandler(s)
		registered := registerOAuthTestClient(t, h, "Test Client", []string{"https://example.com/callback"})
		now := time.Now()
		s.mu.Lock()
		err := s.state.transact(func(next *storeFile) bool {
			next.Codes[oauth.RefreshTokenKey("client-code")] = Code{ClientID: registered.ClientID, ExpiresAt: now.Add(time.Hour)}
			next.Consents[oauth.RefreshTokenKey("client-consent")] = ConsentParams{Params: map[string]string{"client_id": registered.ClientID}, ExpiresAt: now.Add(time.Hour)}
			next.DeviceCodes[oauth.RefreshTokenKey("client-device-code")] = &DeviceCode{ClientID: registered.ClientID, ExpiresAt: now.Add(time.Hour)}
			next.Grants["client-grant"] = Grant{ID: "client-grant", ClientID: registered.ClientID, ExpiresAt: now.Add(time.Hour)}
			next.RefreshTokens[oauth.RefreshTokenKey("client-refresh-token")] = RefreshToken{GrantID: "client-grant", ClientID: registered.ClientID, ExpiresAt: now.Add(time.Hour)}
			return true
		})
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("seed client credentials: %v", err)
		}

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
		reloaded, err := LoadStore(s.state.Path())
		if err != nil {
			t.Fatalf("reload deleted client state: %v", err)
		}
		if _, ok := reloaded.Clients[registered.ClientID]; ok || len(reloaded.Codes) != 0 || len(reloaded.Consents) != 0 || len(reloaded.DeviceCodes) != 0 || len(reloaded.Grants) != 0 || len(reloaded.RefreshTokens) != 0 {
			t.Fatalf("client credentials survived atomic deletion: %+v", reloaded)
		}
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

func TestClientLifecyclePolicy(t *testing.T) {
	t.Parallel()

	validRegistration := func() oauth.RegisterRequest {
		return oauth.RegisterRequest{
			ClientName:              "Client",
			RedirectURIs:            []string{"https://example.com/callback"},
			TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone,
			GrantTypes:              []string{oauth.GrantAuthorizationCode},
		}
	}

	t.Run("registration metadata boundaries", func(t *testing.T) {
		t.Parallel()
		tooManyRedirects := make([]string, maxOAuthRedirectURIs+1)
		for i := range tooManyRedirects {
			tooManyRedirects[i] = fmt.Sprintf("https://example.com/callback/%d", i)
		}
		tests := []struct {
			name   string
			mutate func(*oauth.RegisterRequest)
		}{
			{name: "long client name", mutate: func(r *oauth.RegisterRequest) { r.ClientName = strings.Repeat("x", maxOAuthClientName+1) }},
			{name: "control in client name", mutate: func(r *oauth.RegisterRequest) { r.ClientName = "bad\nname" }},
			{name: "too many redirects", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = tooManyRedirects }},
			{name: "duplicate redirect", mutate: func(r *oauth.RegisterRequest) {
				r.RedirectURIs = []string{"https://example.com/callback", "https://example.com/callback"}
			}},
			{name: "redirect fragment", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = []string{"https://example.com/callback#fragment"} }},
			{name: "empty redirect fragment", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = []string{"https://example.com/callback#"} }},
			{name: "empty redirect hostname", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = []string{"https://:443/callback"} }},
			{name: "unescaped redirect space", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = []string{"https://example.com/call back"} }},
			{name: "redirect credentials", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = []string{"https://user:pass@example.com/callback"} }},
			{name: "non-loopback HTTP", mutate: func(r *oauth.RegisterRequest) { r.RedirectURIs = []string{"http://example.com/callback"} }},
			{name: "unsupported grant", mutate: func(r *oauth.RegisterRequest) { r.GrantTypes = []string{"client_credentials"} }},
			{name: "duplicate grant", mutate: func(r *oauth.RegisterRequest) {
				r.GrantTypes = []string{oauth.GrantAuthorizationCode, oauth.GrantAuthorizationCode}
			}},
			{name: "refresh without issuing grant", mutate: func(r *oauth.RegisterRequest) { r.GrantTypes = []string{oauth.GrantRefreshToken} }},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				registration := validRegistration()
				test.mutate(&registration)
				registerOAuthTestClientRequest(t, newTestServerHandlerOnly(t), &registration, http.StatusBadRequest)
			})
		}
	})

	t.Run("registration JSON rejects duplicate unknown and trailing fields", func(t *testing.T) {
		t.Parallel()
		handler := newTestServerHandlerOnly(t)
		bodies := []string{
			`{"client_name":"a","client_name":"b","redirect_uris":["https://example.com/callback"]}`,
			`{"client_name":"a","redirect_uris":["https://example.com/callback"],"unknown":true}`,
			`{"client_name":"a","redirect_uris":["https://example.com/callback"]} {}`,
			`{"client_name":"a","redirect_uris":["https://example.com/callback"],"grant_types":null}`,
			`{"client_name":"a","redirect_uris":["https://example.com/callback"],"grant_types":[]}`,
			`{"client_name":null,"redirect_uris":["https://example.com/callback"]}`,
			`{"client_name":"","redirect_uris":["https://example.com/callback"]}`,
			`{"client_name":"a","redirect_uris":["https://example.com/callback"],"token_endpoint_auth_method":null}`,
			`{"client_name":"a","redirect_uris":["https://example.com/callback"],"token_endpoint_auth_method":""}`,
		}
		for _, body := range bodies {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %q status = %d, want %d", body, w.Code, http.StatusBadRequest)
			}
		}
	})

	t.Run("omitted optional strings retain their defaults", func(t *testing.T) {
		t.Parallel()
		handler := newTestServerHandlerOnly(t)
		body := `{"redirect_uris":["https://example.com/callback"]}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("omitted defaults status = %d: %s", w.Code, w.Body.String())
		}
		var registered oauth.RegisterResponse
		if err := json.NewDecoder(w.Body).Decode(&registered); err != nil {
			t.Fatalf("decode registration: %v", err)
		}
		if registered.ClientName != "" || registered.TokenEndpointAuthMethod != oauth.TokenEndpointAuthNone {
			t.Fatalf("omitted defaults = name %q, auth %q", registered.ClientName, registered.TokenEndpointAuthMethod)
		}
	})

	t.Run("registration JSON rejects malformed Unicode", func(t *testing.T) {
		t.Parallel()
		handler := newTestServerHandlerOnly(t)
		bodies := [][]byte{
			[]byte(`{"client_name":"\uD800","redirect_uris":["https://example.com/callback"]}`),
			append([]byte(`{"client_name":"`), append([]byte{0xff}, []byte(`","redirect_uris":["https://example.com/callback"]}`)...)...),
		}
		for _, body := range bodies {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %q status = %d, want %d", body, w.Code, http.StatusBadRequest)
			}
		}
	})

	t.Run("refresh token requires requested grant", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		_, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClientRequest(t, handler, &oauth.RegisterRequest{
			ClientName:              "Access only",
			RedirectURIs:            []string{"https://claude.example.com/callback"},
			TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone,
			GrantTypes:              []string{oauth.GrantAuthorizationCode},
		}, http.StatusCreated)
		if !slices.Equal(registered.GrantTypes, []string{oauth.GrantAuthorizationCode}) {
			t.Fatalf("grant_types = %v", registered.GrantTypes)
		}
		token := authorizeOAuthTestClient(t, handler, user, &registered, []string{"read"})
		if token.AccessToken == "" || token.RefreshToken != "" {
			t.Fatalf("access/refresh tokens = %q/%q, want access only", token.AccessToken, token.RefreshToken)
		}
	})

	t.Run("refresh eligibility is rechecked inside rotation", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t)
		now := time.Now()
		server.state.Clients["client"] = Client{ID: "client", RedirectURIs: []string{"https://example.com/callback"}, GrantTypes: []string{oauth.GrantAuthorizationCode}}
		server.state.Grants["grant"] = Grant{ID: "grant", UserID: "user", ClientID: "client", Resource: testResourceURL, ExpiresAt: now.Add(time.Hour)}
		server.state.RefreshTokens[oauth.RefreshTokenKey("refresh")] = RefreshToken{GrantID: "grant", UserID: "user", ClientID: "client", Resource: testResourceURL, ExpiresAt: now.Add(time.Hour)}
		result, _, err := server.exchangeRefreshToken("refresh", "client", "user", "next", dpopBinding{})
		if err != nil || result != refreshExchangeUnknown {
			t.Fatalf("refresh rotation = %v, %v; want ineligible", result, err)
		}
		if !server.state.RefreshTokens[oauth.RefreshTokenKey("refresh")].UsedAt.IsZero() {
			t.Fatal("ineligible refresh token was consumed")
		}
	})

	t.Run("omitted grants default to authorization code", func(t *testing.T) {
		t.Parallel()
		registration := validRegistration()
		registration.GrantTypes = nil
		registered := registerOAuthTestClientRequest(t, newTestServerHandlerOnly(t), &registration, http.StatusCreated)
		if !slices.Equal(registered.GrantTypes, []string{oauth.GrantAuthorizationCode}) {
			t.Fatalf("grant_types = %v, want authorization_code", registered.GrantTypes)
		}
	})

	t.Run("loopback IP ports are dynamic but localhost is exact", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		_, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		for _, test := range []struct {
			registered string
			requested  string
		}{
			{registered: "http://127.0.0.1:1000/callback?flow=1", requested: "http://127.0.0.1:2000/callback?flow=1"},
			{registered: "http://[::1]:1000/callback", requested: "http://[::1]:2000/callback"},
		} {
			registered := registerOAuthTestClient(t, handler, "Native", []string{test.registered})
			if code := authorizeOAuthTestCode(t, handler, user, &registered, test.requested, []string{"read"}, ""); code == "" {
				t.Fatal("dynamic loopback authorization code is empty")
			}
		}
		registered := registerOAuthTestClient(t, handler, "Localhost", []string{"http://localhost:1000/callback"})
		form := authorizationCodeForm(registered.ClientID, "http://localhost:2000/callback", "read")
		req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+form.Encode(), http.NoBody, user)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("localhost dynamic port status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		registered = registerOAuthTestClient(t, handler, "IPv6 text", []string{"http://[::1]:1000/callback"})
		form = authorizationCodeForm(registered.ClientID, "http://[0:0:0:0:0:0:0:1]:2000/callback", "read")
		req = newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+form.Encode(), http.NoBody, user)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("alternate IPv6 text status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		registered = registerOAuthTestClient(t, handler, "Empty fragment", []string{"http://127.0.0.1:1000/callback"})
		form = authorizationCodeForm(registered.ClientID, "http://127.0.0.1:2000/callback#", "read")
		if redirectURIRegistered([]string{"http://127.0.0.1:1000/callback"}, form.Get("redirect_uri")) {
			t.Fatal("dynamic loopback matcher accepted an empty fragment")
		}
		if redirectURIRegistered([]string{"http://127.0.0.1:1000/callback#"}, "http://127.0.0.1:1000/callback#") {
			t.Fatal("exact loopback matcher accepted an empty fragment")
		}
		req = newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+form.Encode(), http.NoBody, user)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("empty-fragment authorization status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		if redirectURIRegistered([]string{"http://127.0.0.1:1000/call%20back"}, "http://127.0.0.1:2000/call back") {
			t.Fatal("dynamic loopback matcher normalized a non-identical path")
		}
	})

	t.Run("PKCE syntax fails before code consumption", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		_, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, handler, "Client", []string{"https://claude.example.com/callback"})
		badChallenge := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read")
		badChallenge.Set("code_challenge", "too-short")
		req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+badChallenge.Encode(), http.NoBody, user)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("bad challenge status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		badChallenge.Set("code_challenge", strings.Repeat("a", 42)+".")
		req = newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+badChallenge.Encode(), http.NoBody, user)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("non-base64url challenge status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		badChallenge.Set("code_challenge", strings.Repeat("A", 42)+"B")
		req = newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+badChallenge.Encode(), http.NoBody, user)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("noncanonical challenge status = %d, want %d", w.Code, http.StatusBadRequest)
		}

		code := authorizeOAuthTestCode(t, handler, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {strings.Repeat("a", 42)},
			"resource":      {testResourceURL},
		}
		exchange := func() int {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w.Code
		}
		if got := exchange(); got != http.StatusBadRequest {
			t.Fatalf("bad verifier status = %d, want %d", got, http.StatusBadRequest)
		}
		form.Set("code_verifier", testVerifier)
		if got := exchange(); got != http.StatusOK {
			t.Fatalf("valid verifier retry status = %d, want %d", got, http.StatusOK)
		}
	})

	t.Run("client registrations are capped", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t)
		handler := newTestServerHandler(server)
		for i := range maxOAuthClients {
			id := strconv.Itoa(i)
			server.state.Clients[id] = Client{ID: id, RedirectURIs: []string{"https://example.com/callback"}}
		}
		registration := validRegistration()
		registerOAuthTestClientRequest(t, handler, &registration, http.StatusServiceUnavailable)
		if len(server.state.Clients) != maxOAuthClients {
			t.Fatalf("clients = %d, want %d", len(server.state.Clients), maxOAuthClients)
		}
	})

	t.Run("legacy grants survive unrelated update", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t)
		handler := newTestServerHandler(server)
		registered := registerOAuthTestClient(t, handler, "Legacy", []string{"https://example.com/callback"})
		server.mu.Lock()
		client := server.state.Clients[registered.ClientID]
		client.GrantTypes = nil
		err := server.state.transact(func(next *storeFile) bool {
			next.Clients[registered.ClientID] = client
			return true
		})
		server.mu.Unlock()
		if err != nil {
			t.Fatalf("seed legacy client: %v", err)
		}
		body := `{"client_name":"Renamed"}`
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/oauth/register/"+registered.ClientID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("legacy update status = %d: %s", w.Code, w.Body.String())
		}
		if stored := server.state.Clients[registered.ClientID]; stored.GrantTypes != nil {
			t.Fatalf("legacy grant sentinel = %v, want nil", stored.GrantTypes)
		}
	})

	t.Run("update rejects null and empty grants", func(t *testing.T) {
		t.Parallel()
		handler := newTestServerHandlerOnly(t)
		registered := registerOAuthTestClient(t, handler, "Client", []string{"https://example.com/callback"})
		for _, body := range []string{`{"grant_types":null}`, `{"grant_types":[]}`} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/oauth/register/"+registered.ClientID, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+registered.RegistrationAccessToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("update %s status = %d, want %d", body, w.Code, http.StatusBadRequest)
			}
		}
	})

	t.Run("device authorization requires requested grant", func(t *testing.T) {
		t.Parallel()
		handler := newTestServerHandlerOnly(t)
		registration := validRegistration()
		registered := registerOAuthTestClientRequest(t, handler, &registration, http.StatusCreated)
		form := url.Values{"client_id": {registered.ClientID}, "scope": {"read"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unauthorized_client") {
			t.Fatalf("device status = %d, want unauthorized_client: %s", w.Code, w.Body.String())
		}
	})

	t.Run("registration management token survives access-token windows", func(t *testing.T) {
		t.Parallel()
		server := newTestServer(t)
		handler := newTestServerHandler(server)
		registered := registerOAuthTestClient(t, handler, "Managed", []string{"https://example.com/callback"})
		clientID, err := server.tokens.VerifyRegistrationAccessToken(registered.RegistrationAccessToken, server.issuer, server.issuer+"/oauth/register", time.Now().Add(10*365*24*time.Hour))
		if err != nil || clientID != registered.ClientID {
			t.Fatalf("long-lived registration token = %q, %v", clientID, err)
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

func TestOAuthInputBounds(t *testing.T) {
	t.Parallel()

	t.Run("repeated consent scopes remain valid", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		server, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, handler, "Claude", []string{"https://claude.example.com/callback"})
		code := authorizeOAuthTestCode(t, handler, user, &registered, "https://claude.example.com/callback", []string{"read", "write"}, "")
		entry, found := server.state.Codes[oauth.RefreshTokenKey(code)]
		if !found {
			t.Fatal("authorization code was not stored")
		}
		if entry.Scope != "read write" {
			t.Fatalf("scope = %q, want %q", entry.Scope, "read write")
		}
	})

	t.Run("duplicate token parameter is rejected before code consumption", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		_, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, handler, "Claude", []string{"https://claude.example.com/callback"})
		code := authorizeOAuthTestCode(t, handler, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		form := url.Values{
			"grant_type":    {oauth.GrantAuthorizationCode},
			"code":          {code},
			"client_id":     {registered.ClientID, registered.ClientID},
			"redirect_uri":  {"https://claude.example.com/callback"},
			"code_verifier": {testVerifier},
			"resource":      {testResourceURL},
		}
		exchange := func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w
		}
		if w := exchange(); w.Code != http.StatusBadRequest {
			t.Fatalf("duplicate status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
		}
		form.Set("client_id", registered.ClientID)
		if w := exchange(); w.Code != http.StatusOK {
			t.Fatalf("valid retry status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
	})

	t.Run("oversized parameter is rejected", func(t *testing.T) {
		t.Parallel()
		_, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		form := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "code": {strings.Repeat("x", maxOAuthParameterBytes+1)}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "too long") {
			t.Fatalf("status = %d, want oversized rejection: %s", w.Code, w.Body.String())
		}
	})

	t.Run("pushed authorization requests are pruned and capped", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		server, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, handler, "Claude", []string{"https://claude.example.com/callback"})
		now := time.Now()
		for i := range maxPendingPARRequests {
			server.parRequests[strconv.Itoa(i)] = ConsentParams{ExpiresAt: now.Add(time.Minute)}
		}
		form := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read")
		push := func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/par", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w
		}
		if w := push(); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("full PAR status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
		}
		server.parRequests["0"] = ConsentParams{ExpiresAt: now.Add(-time.Second)}
		if w := push(); w.Code != http.StatusCreated {
			t.Fatalf("pruned PAR status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
		}
		if len(server.parRequests) != maxPendingPARRequests {
			t.Fatalf("PAR requests = %d, want %d", len(server.parRequests), maxPendingPARRequests)
		}
	})

	t.Run("pending consents are pruned and capped", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		server, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, handler, "Claude", []string{"https://claude.example.com/callback"})
		now := time.Now()
		for i := range maxPendingConsents {
			server.state.Consents[strconv.Itoa(i)] = ConsentParams{ExpiresAt: now.Add(time.Minute)}
		}
		form := authorizationCodeForm(registered.ClientID, "https://claude.example.com/callback", "read")
		start := func() *httptest.ResponseRecorder {
			req := newOAuthTestRequest(t, http.MethodGet, "/oauth/authorize?"+form.Encode(), http.NoBody, user)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w
		}
		if w := start(); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("full consent status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
		}
		server.state.Consents["0"] = ConsentParams{ExpiresAt: now.Add(-time.Second)}
		if w := start(); w.Code != http.StatusOK {
			t.Fatalf("pruned consent status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if len(server.state.Consents) != maxPendingConsents {
			t.Fatalf("consents = %d, want %d", len(server.state.Consents), maxPendingConsents)
		}
	})

	t.Run("pending device authorizations are capped", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		server, handler, _ := newTestFlowServer(t, t.TempDir()+"/oauth.json", []oauth.User{user})
		registered := registerOAuthTestClient(t, handler, "Claude", []string{"https://claude.example.com/callback"})
		now := time.Now()
		for i := range maxPendingDeviceCodes {
			server.state.DeviceCodes[strconv.Itoa(i)] = &DeviceCode{ClientID: registered.ClientID, Status: "pending", ExpiresAt: now.Add(time.Minute)}
		}
		form := url.Values{"client_id": {registered.ClientID}, "scope": {"read"}}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/device_authorization", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("device status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
		}
	})
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

	t.Run("token endpoint uses per-IP and per-client rate limit keys", func(t *testing.T) {
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

		// Token requests charge both the source IP and the registered client
		// within that IP.
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
		if !lim.sawKey("client_ip:192.0.2.1:" + registered.ClientID) {
			t.Fatalf("rate limiter did not see per-IP-client key: keys=%v", lim.keys)
		}
		if !lim.sawKey("ip:192.0.2.1") {
			t.Fatalf("rate limiter did not see per-IP key: keys=%v", lim.keys)
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
		if !lim.sawKey("ip:10.0.0.1") {
			t.Fatalf("rate limiter did not see per-IP fallback key: keys=%v", lim.keys)
		}
	})

	t.Run("unknown client IDs share a port-independent IP key", func(t *testing.T) {
		t.Parallel()
		lim := &inspectRateLimiter{}
		cfg := testFlowServerConfig(t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)
		for i, clientID := range []string{"attacker-a", "attacker-b"} {
			form := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "client_id": {clientID}}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = fmt.Sprintf("10.0.0.9:%d", 2000+i)
			h.ServeHTTP(httptest.NewRecorder(), req)
		}
		for _, key := range lim.keys {
			if strings.HasPrefix(key, "client_ip:") {
				t.Fatalf("unknown client IDs created client keys: %v", lim.keys)
			}
		}
		for _, key := range lim.keys {
			if key != "ip:10.0.0.9" {
				t.Fatalf("rate-limit key = %q, want stable IP key", key)
			}
		}
	})

	t.Run("registered client keys ignore source ports", func(t *testing.T) {
		t.Parallel()
		limiter := &inspectRateLimiter{}
		cfg := testFlowServerConfig(t.TempDir()+"/oauth.json", []oauth.User{testUser()})
		cfg.RateLimiter = limiter
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		handler := newTestServerHandler(server)
		registered := registerOAuthTestClient(t, handler, "Client", []string{"https://example.com/callback"})
		limiter.keys = nil
		for _, port := range []string{"1001", "2002"} {
			form := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "client_id": {registered.ClientID}, "code": {"missing"}}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = "10.0.0.8:" + port
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}
		wantKeys := []string{
			"ip:10.0.0.8",
			"client_ip:10.0.0.8:" + registered.ClientID,
			"ip:10.0.0.8",
			"client_ip:10.0.0.8:" + registered.ClientID,
		}
		if !slices.Equal(limiter.keys, wantKeys) {
			t.Fatalf("rate-limit keys = %v, want %v", limiter.keys, wantKeys)
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
		if !lim.sawKey("ip:10.0.0.5") {
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
		if !lim.sawKey("ip:10.0.0.7") {
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
		if !lim.sawKey("client_ip:192.0.2.1:" + registered.ClientID) {
			t.Fatalf("rate limiter did not see per-IP-client key: keys=%v", lim.keys)
		}
	})

	t.Run("registered clients share an aggregate IP limit but have isolated secondary limits", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		path := t.TempDir() + "/oauth.json"
		permissive, permissiveHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		clientA := registerOAuthTestClient(t, permissiveHandler, "Client A", []string{"https://a.example.com/callback"})
		clientB := registerOAuthTestClient(t, permissiveHandler, "Client B", []string{"https://b.example.com/callback"})
		permissive.Close()

		limiter := &tieredCountRateLimiter{ipMax: 3, clientMax: 1}
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = limiter
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		handler := newTestServerHandler(server)
		request := func(clientID string) int {
			form := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "client_id": {clientID}, "code": {"missing"}}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = "10.0.0.1:1234"
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w.Code
		}
		if got := request(clientA.ClientID); got == http.StatusTooManyRequests {
			t.Fatalf("client A first request = %d, want non-rate-limit response", got)
		}
		if got := request(clientA.ClientID); got != http.StatusTooManyRequests {
			t.Fatalf("client A secondary-limit response = %d, want %d", got, http.StatusTooManyRequests)
		}
		if got := request(clientB.ClientID); got == http.StatusTooManyRequests {
			t.Fatalf("client B isolated allowance response = %d, want non-rate-limit response", got)
		}
		if got := request(clientB.ClientID); got != http.StatusTooManyRequests {
			t.Fatalf("aggregate IP-limit response = %d, want %d", got, http.StatusTooManyRequests)
		}
	})

	t.Run("one source cannot exhaust another source's client allowance", func(t *testing.T) {
		t.Parallel()
		user := testUser()
		path := t.TempDir() + "/oauth.json"
		permissive, permissiveHandler, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, permissiveHandler, "Client", []string{"https://example.com/callback"})
		permissive.Close()

		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = &countRateLimiter{max: 1}
		server, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		handler := newTestServerHandler(server)
		request := func(remoteAddr string) int {
			form := url.Values{"grant_type": {oauth.GrantAuthorizationCode}, "client_id": {registered.ClientID}, "code": {"missing"}}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.RemoteAddr = remoteAddr
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			return w.Code
		}
		if got := request("10.0.0.1:1001"); got == http.StatusTooManyRequests {
			t.Fatalf("source A response = %d, want non-rate-limit response", got)
		}
		if got := request("10.0.0.2:1001"); got == http.StatusTooManyRequests {
			t.Fatalf("source B response = %d, want independent allowance", got)
		}
	})

	t.Run("rate limiter rejection returns 429", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		path := t.TempDir() + "/oauth.json"

		// First, register a client and authorize a code with a permissive server.
		permissive, h, _ := newTestFlowServer(t, path, []oauth.User{user})
		registered := registerOAuthTestClient(t, h, "Claude", []string{"https://claude.example.com/callback"})
		code := authorizeOAuthTestCode(t, h, user, &registered, "https://claude.example.com/callback", []string{"read"}, "")
		permissive.Close()

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

	t.Run("client rotation cannot bypass IP limit", func(t *testing.T) {
		t.Parallel()

		user := testUser()
		path := t.TempDir() + "/oauth.json"

		// Register clients and authorize codes with a permissive server.
		permissive, permissiveH, _ := newTestFlowServer(t, path, []oauth.User{user})
		clientA := registerOAuthTestClient(t, permissiveH, "Client A", []string{"https://a.example.com/callback"})
		clientB := registerOAuthTestClient(t, permissiveH, "Client B", []string{"https://b.example.com/callback"})
		codeA := authorizeOAuthTestCode(t, permissiveH, user, &clientA, "https://a.example.com/callback", []string{"read"}, "")
		codeA2 := authorizeOAuthTestCode(t, permissiveH, user, &clientA, "https://a.example.com/callback", []string{"read"}, "")
		codeB := authorizeOAuthTestCode(t, permissiveH, user, &clientB, "https://b.example.com/callback", []string{"read"}, "")
		permissive.Close()

		// Create a limited server from the same store: allow only one token
		// request per source IP and registered client.
		lim := &countRateLimiter{max: 1}
		cfg := testFlowServerConfig(path, []oauth.User{user})
		cfg.RateLimiter = lim
		s, err := NewServer(cfg)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		h := newTestServerHandler(s)

		exchange := func(cid, code, redirectURI, remoteAddr string) int {
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
			req.RemoteAddr = remoteAddr
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w.Code
		}

		// First request from client A: succeeds.
		if got := exchange(clientA.ClientID, codeA, "https://a.example.com/callback", "10.0.0.1:1001"); got != http.StatusOK {
			t.Fatalf("client A first request = %d, want %d", got, http.StatusOK)
		}

		// Second request from client A: denied by the source-IP quota.
		if got := exchange(clientA.ClientID, codeA2, "https://a.example.com/callback", "10.0.0.1:1002"); got != http.StatusTooManyRequests {
			t.Fatalf("client A second request = %d, want %d", got, http.StatusTooManyRequests)
		}

		// Rotating to client B from the same source is still denied by the IP
		// quota, while a different source gets an independent allowance.
		if got := exchange(clientB.ClientID, codeB, "https://b.example.com/callback", "10.0.0.1:1003"); got != http.StatusTooManyRequests {
			t.Fatalf("same-IP client B request = %d, want %d", got, http.StatusTooManyRequests)
		}
		if got := exchange(clientB.ClientID, codeB, "https://b.example.com/callback", "10.0.0.2:1001"); got != http.StatusOK {
			t.Fatalf("different-IP client B request = %d, want %d", got, http.StatusOK)
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

// tieredCountRateLimiter gives aggregate IP and per-IP-client keys independent
// limits so tests can observe both server-side dimensions.
type tieredCountRateLimiter struct {
	mu        sync.Mutex
	ipMax     int
	clientMax int
	counts    map[string]int
}

func (l *tieredCountRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil {
		l.counts = map[string]int{}
	}
	l.counts[key]++
	limit := l.clientMax
	if strings.HasPrefix(key, "ip:") {
		limit = l.ipMax
	}
	return l.counts[key] <= limit
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
		Issuer:                  testBaseURL,
		Session:                 &testSessionManager{users: usersByID},
		UI:                      &captureAuthorizationUI{},
		IntrospectionAuth: func(*http.Request) (IntrospectionPrincipal, bool) {
			return IntrospectionPrincipal{}, true
		},
	}
	return cfg
}
