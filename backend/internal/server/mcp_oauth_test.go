// Tests for MCP OAuth token lifecycle behavior.

package server

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestMCPOAuthTokenLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("authorization code returns refresh token and refresh rotates", func(t *testing.T) {
		t.Parallel()
		s, h, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, h, &user, &registered)
		if tokenResp.RefreshToken == "" {
			t.Fatal("refresh token is empty")
		}

		rotated := refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.AccessToken == "" || rotated.RefreshToken == "" || rotated.RefreshToken == tokenResp.RefreshToken {
			t.Fatalf("rotated token response = %+v", rotated)
		}
		if rotated.TokenType != mcp.OAuthTokenTypeBearer || rotated.Scope != tokenResp.Scope {
			t.Fatalf("rotated token metadata = %+v", rotated)
		}
		if _, _, err := s.mcp.verifyMCPBearer(newMCPBearerRequest(t), rotated.AccessToken); err != nil {
			t.Fatalf("verify rotated access token: %v", err)
		}
		refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("registered client survives server restart", func(t *testing.T) {
		t.Parallel()
		s, _, user, registered := newMCPOAuthLifecycleRouter(t)

		restarted := newTestRouter(t)
		restarted.hostState = auth.NewHostState("https://caic.example.com")
		restarted.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
		restarted.authStore = s.authStore
		restarted.mcp.refreshTokenStorePath = s.mcp.refreshTokenStorePath
		h := mustBuildMCPOAuthLifecycleHandler(t, restarted)

		tokenResp := authorizeMCPClient(t, h, &user, &registered)
		if tokenResp.RefreshToken == "" {
			t.Fatal("refresh token is empty after client reload")
		}
	})

	t.Run("refresh token and grant list survive server restart", func(t *testing.T) {
		t.Parallel()
		s, _, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, mustBuildMCPOAuthLifecycleHandler(t, s), &user, &registered)

		restarted := newTestRouter(t)
		restarted.hostState = auth.NewHostState("https://caic.example.com")
		restarted.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
		restarted.authStore = s.authStore
		restarted.mcp.refreshTokenStorePath = s.mcp.refreshTokenStorePath
		h := mustBuildMCPOAuthLifecycleHandler(t, restarted)

		grants := listMCPGrants(t, h, &user)
		if len(grants.Grants) != 1 || grants.Grants[0].ClientName != "Claude" || grants.Grants[0].ClientID != registered.ClientID {
			t.Fatalf("grants after restart = %+v", grants.Grants)
		}
		rotated := refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		if rotated.RefreshToken == "" || rotated.RefreshToken == tokenResp.RefreshToken {
			t.Fatalf("rotated token response = %+v", rotated)
		}
	})

	t.Run("revoked refresh token is rejected", func(t *testing.T) {
		t.Parallel()
		_, h, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, h, &user, &registered)

		form := url.Values{
			"client_id":       {registered.ClientID},
			"token":           {tokenResp.RefreshToken},
			"token_type_hint": {"refresh_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("approval refresh and revocation are audited", func(t *testing.T) {
		t.Parallel()
		s, h, user, registered := newMCPOAuthLifecycleRouter(t)
		path := filepath.Join(t.TempDir(), "mcp_audit.jsonl")
		s.mcp.audit.path = path

		tokenResp := authorizeMCPClient(t, h, &user, &registered)
		rotated := refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
		form := url.Values{
			"client_id":       {registered.ClientID},
			"token":           {rotated.RefreshToken},
			"token_type_hint": {"refresh_token"},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/revoke", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		events := readMCPAuditEvents(t, path)
		seen := map[string]struct{}{}
		for _, event := range events {
			seen[event.Operation+":"+event.Status] = struct{}{}
			if strings.HasPrefix(event.Operation, "oauth/") && event.UserID != user.ID {
				t.Fatalf("audit event userID = %q, want %q: %+v", event.UserID, user.ID, event)
			}
		}
		for _, want := range []string{"oauth/authorize:approved", "oauth/token:issued", "oauth/token:refreshed", "oauth/revoke:revoked"} {
			if _, ok := seen[want]; !ok {
				t.Fatalf("audit events = %+v, missing %s", events, want)
			}
		}
		data, err := os.ReadFile(path) //nolint:gosec // test path from t.TempDir.
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		for _, secret := range []string{tokenResp.AccessToken, tokenResp.RefreshToken, rotated.AccessToken, rotated.RefreshToken} {
			if secret != "" && strings.Contains(string(data), secret) {
				t.Fatalf("audit log contains token %q: %s", secret, string(data))
			}
		}
	})

	t.Run("grant list shows authenticated user client details", func(t *testing.T) {
		t.Parallel()
		s, h, user, registered := newMCPOAuthLifecycleRouter(t)
		authorizeMCPClient(t, h, &user, &registered)

		grants := listMCPGrants(t, h, &user)
		if len(grants.Grants) != 1 {
			t.Fatalf("grants = %+v, want one", grants.Grants)
		}
		grant := grants.Grants[0]
		if grant.ClientID != registered.ClientID || grant.ClientName != "Claude" {
			t.Fatalf("grant client = %+v, want registered client", grant)
		}
		if grant.Resource != "https://caic.example.com/api/caic/v1/mcp" {
			t.Fatalf("grant resource = %q", grant.Resource)
		}
		if strings.Join(grant.Scopes, " ") != mcpScopeRead+" "+mcpScopeTasksRead {
			t.Fatalf("grant scopes = %#v", grant.Scopes)
		}
		if grant.CreatedAt.IsZero() || grant.ExpiresAt.IsZero() || grant.Status != v1.MCPGrantStatusActive {
			t.Fatalf("grant timestamps/status = %+v", grant)
		}

		bob, err := s.authStore.UpsertUser(&auth.User{Provider: forge.KindGitHub, ProviderID: "2", Username: "bob", AccessToken: "forge-token"})
		if err != nil {
			t.Fatalf("upsert bob: %v", err)
		}
		bobGrants := listMCPGrants(t, h, &bob)
		if len(bobGrants.Grants) != 0 {
			t.Fatalf("bob grants = %+v, want none", bobGrants.Grants)
		}
	})

	t.Run("user grant revocation blocks only that grant refresh", func(t *testing.T) {
		t.Parallel()
		s, h, user, registered := newMCPOAuthLifecycleRouter(t)
		first := authorizeMCPClient(t, h, &user, &registered)
		secondClient := registerTestClient(t, h, "Codex", []string{"https://codex.example.com/auth/callback"})
		second := authorizeMCPClientWithRedirect(t, h, &user, &secondClient, "https://codex.example.com/auth/callback")

		grants := listMCPGrants(t, h, &user)
		if len(grants.Grants) != 2 {
			t.Fatalf("grants = %+v, want two", grants.Grants)
		}
		var firstGrantID string
		for _, grant := range grants.Grants {
			if grant.ClientID == registered.ClientID {
				firstGrantID = grant.ID
			}
		}
		if firstGrantID == "" {
			t.Fatalf("first grant missing: %+v", grants.Grants)
		}
		bob, err := s.authStore.UpsertUser(&auth.User{Provider: forge.KindGitHub, ProviderID: "2", Username: "bob", AccessToken: "forge-token"})
		if err != nil {
			t.Fatalf("upsert bob: %v", err)
		}
		revokeMCPGrant(t, h, &bob, firstGrantID, http.StatusNotFound)
		first = refreshMCPToken(t, h, registered.ClientID, first.RefreshToken, http.StatusOK)

		revokeMCPGrant(t, h, &user, firstGrantID, http.StatusOK)
		refreshMCPToken(t, h, registered.ClientID, first.RefreshToken, http.StatusBadRequest)
		refreshMCPToken(t, h, secondClient.ClientID, second.RefreshToken, http.StatusOK)
	})

	t.Run("expired access token is rejected", func(t *testing.T) {
		t.Parallel()
		s, h, user, _ := newMCPOAuthLifecycleRouter(t)
		token := signTestMCPAccessToken(t, s, "https://caic.example.com", &user, "https://caic.example.com/api/caic/v1/mcp", mcpScopeTasksRead, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))

		req := newMCPBearerRequest(t)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("expired refresh token is rejected", func(t *testing.T) {
		t.Parallel()
		s, h, user, registered := newMCPOAuthLifecycleRouter(t)
		opaque, err := randomToken()
		if err != nil {
			t.Fatalf("generate refresh token: %v", err)
		}
		s.mcp.oauth.mu.Lock()
		s.mcp.oauth.refreshTokens[mcpOAuthRefreshTokenKey(opaque)] = mcpOAuthRefreshToken{UserID: user.ID, ClientID: registered.ClientID, Resource: "https://caic.example.com/api/caic/v1/mcp", Scope: mcpScopeRead, ExpiresAt: time.Now().Add(-time.Minute)}
		s.mcp.oauth.mu.Unlock()

		refreshMCPToken(t, h, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("unknown user refresh token is rejected", func(t *testing.T) {
		t.Parallel()
		s, h, _, registered := newMCPOAuthLifecycleRouter(t)
		opaque, err := randomToken()
		if err != nil {
			t.Fatalf("generate refresh token: %v", err)
		}
		s.mcp.oauth.mu.Lock()
		s.mcp.oauth.refreshTokens[mcpOAuthRefreshTokenKey(opaque)] = mcpOAuthRefreshToken{UserID: "usr_missing", ClientID: registered.ClientID, Resource: "https://caic.example.com/api/caic/v1/mcp", Scope: mcpScopeRead, ExpiresAt: time.Now().Add(time.Hour)}
		s.mcp.oauth.mu.Unlock()

		refreshMCPToken(t, h, registered.ClientID, opaque, http.StatusBadRequest)
	})
}

func newMCPOAuthLifecycleRouter(t *testing.T) (*testRouter, http.Handler, auth.User, mcp.OAuthRegisterResponse) {
	s := newTestRouter(t)
	s.hostState = auth.NewHostState("https://caic.example.com")
	s.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
	store, err := auth.Open(t.TempDir() + "/users.json")
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	user, err := store.UpsertUser(&auth.User{Provider: forge.KindGitHub, ProviderID: "1", Username: "alice", AccessToken: "forge-token"})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	s.authStore = store
	s.mcp.refreshTokenStorePath = t.TempDir() + "/mcp_oauth_refresh_tokens.json"
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}

	registerBody := `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"token_endpoint_auth_method":"none"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/register", strings.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var registered mcp.OAuthRegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	return s, h, user, registered
}

func mustBuildMCPOAuthLifecycleHandler(t *testing.T, s *testRouter) http.Handler {
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}
	return h
}

func authorizeMCPClient(t *testing.T, h http.Handler, user *auth.User, registered *mcp.OAuthRegisterResponse) mcp.OAuthTokenResponse {
	return authorizeMCPClientWithRedirect(t, h, user, registered, "https://claude.ai/api/mcp/auth_callback")
}

func authorizeMCPClientWithRedirect(t *testing.T, h http.Handler, user *auth.User, registered *mcp.OAuthRegisterResponse, redirectURI string) mcp.OAuthTokenResponse {
	verifier := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	resource := "https://caic.example.com/api/caic/v1/mcp"
	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {registered.ClientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {resource},
		"scope":                 {mcpAuthDefaultScope},
	}
	jwt, err := auth.IssueToken(user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
	if err != nil {
		t.Fatalf("issue session token: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/oauth/authorize"+"?"+form.Encode(), http.NoBody)
	req.Host = "caic.example.com"
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("consent status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	_, tokenSuffix, ok := strings.Cut(w.Body.String(), `name="consent_token" value="`)
	if !ok {
		t.Fatalf("consent token missing from page: %s", w.Body.String())
	}
	consentToken, _, ok := strings.Cut(tokenSuffix, `"`)
	if !ok || consentToken == "" {
		t.Fatalf("invalid consent token in page: %s", w.Body.String())
	}

	consentForm := url.Values{
		"consent_token": {consentToken},
		"scope_form":    {"1"},
		"scope":         {mcpScopeRead, mcpScopeTasksRead},
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/authorize", strings.NewReader(consentForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "caic.example.com"
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("authorize status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
	}
	redirectURL, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	code := redirectURL.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect URL = %s", redirectURL.String())
	}

	tokenForm := url.Values{
		"grant_type":    {mcp.OAuthGrantAuthorizationCode},
		"code":          {code},
		"client_id":     {registered.ClientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/token", strings.NewReader(tokenForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "caic.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var tokenResp mcp.OAuthTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
		t.Fatal(err)
	}
	return tokenResp
}

func listMCPGrants(t *testing.T, h http.Handler, user *auth.User) v1.MCPGrantsResp {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/mcp-grants", http.NoBody)
	req.Host = "caic.example.com"
	addTestSessionCookie(t, req, user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list MCP grants status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var grants v1.MCPGrantsResp
	if err := json.NewDecoder(w.Body).Decode(&grants); err != nil {
		t.Fatal(err)
	}
	return grants
}

func revokeMCPGrant(t *testing.T, h http.Handler, user *auth.User, grantID string, wantStatus int) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp-grants/"+grantID+"/revoke", http.NoBody)
	req.Host = "caic.example.com"
	addTestSessionCookie(t, req, user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("revoke MCP grant status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
}

func addTestSessionCookie(t *testing.T, req *http.Request, user *auth.User) {
	jwt, err := auth.IssueToken(user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
	if err != nil {
		t.Fatalf("issue session token: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func refreshMCPToken(t *testing.T, h http.Handler, clientID, refreshToken string, wantStatus int) mcp.OAuthTokenResponse {
	form := url.Values{
		"grant_type":    {mcp.OAuthGrantRefreshToken},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("refresh status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var tokenResp mcp.OAuthTokenResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
			t.Fatal(err)
		}
	}
	return tokenResp
}

func newMCPBearerRequest(t *testing.T) *http.Request {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/mcp", strings.NewReader(mcpRequestJSON("tools/call", `"name":"tasks_list","arguments":{}`)))
	req.Host = "caic.example.com"
	req.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "tasks_list")
	return req
}

func signTestMCPAccessToken(t *testing.T, s *testRouter, issuer string, user *auth.User, audience, scope string, issuedAt, expiresAt time.Time) string {
	headerJSON, err := json.Marshal(map[string]string{"alg": mcp.JWTAlgRS256, "typ": "JWT", "kid": s.mcp.oauth.kid})
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"iss":      issuer,
		"sub":      user.ID,
		"aud":      audience,
		"username": user.Username,
		"scope":    scope,
		"iat":      issuedAt.Unix(),
		"nbf":      issuedAt.Unix(),
		"exp":      expiresAt.Unix(),
		"typ":      "access_token",
	})
	if err != nil {
		t.Fatal(err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := s.mcp.oauth.key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
