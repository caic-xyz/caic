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
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/mcp"
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
		if _, _, err := s.verifyMCPBearer(newMCPBearerRequest(t), rotated.AccessToken); err != nil {
			t.Fatalf("verify rotated access token: %v", err)
		}
		refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
	})

	t.Run("refresh token survives server restart", func(t *testing.T) {
		t.Parallel()
		s, _, user, registered := newMCPOAuthLifecycleRouter(t)
		tokenResp := authorizeMCPClient(t, mustBuildMCPOAuthLifecycleHandler(t, s), &user, &registered)

		restarted := newTestRouter(t)
		restarted.hostState = auth.NewHostState("https://caic.example.com")
		restarted.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
		restarted.authStore = s.authStore
		restarted.mcpOAuthRefreshTokenStorePath = s.mcpOAuthRefreshTokenStorePath
		h := mustBuildMCPOAuthLifecycleHandler(t, restarted)

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
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthRevokePath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Host = "caic.example.com"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}

		refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
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
		s.mcpOAuth.mu.Lock()
		s.mcpOAuth.refreshTokens[mcpOAuthRefreshTokenKey(opaque)] = mcpOAuthRefreshToken{UserID: user.ID, ClientID: registered.ClientID, Resource: "https://caic.example.com/api/caic/v1/mcp", Scope: mcpScopeRead, ExpiresAt: time.Now().Add(-time.Minute)}
		s.mcpOAuth.mu.Unlock()

		refreshMCPToken(t, h, registered.ClientID, opaque, http.StatusBadRequest)
	})

	t.Run("unknown user refresh token is rejected", func(t *testing.T) {
		t.Parallel()
		s, h, _, registered := newMCPOAuthLifecycleRouter(t)
		opaque, err := randomToken()
		if err != nil {
			t.Fatalf("generate refresh token: %v", err)
		}
		s.mcpOAuth.mu.Lock()
		s.mcpOAuth.refreshTokens[mcpOAuthRefreshTokenKey(opaque)] = mcpOAuthRefreshToken{UserID: "usr_missing", ClientID: registered.ClientID, Resource: "https://caic.example.com/api/caic/v1/mcp", Scope: mcpScopeRead, ExpiresAt: time.Now().Add(time.Hour)}
		s.mcpOAuth.mu.Unlock()

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
	s.mcpOAuthRefreshTokenStorePath = t.TempDir() + "/mcp_oauth_refresh_tokens.json"
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}

	registerBody := `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"token_endpoint_auth_method":"none"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthRegisterPath, strings.NewReader(registerBody))
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
	verifier := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	resource := "https://caic.example.com/api/caic/v1/mcp"
	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {registered.ClientID},
		"redirect_uri":          {"https://claude.ai/api/mcp/auth_callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {resource},
		"scope":                 {mcpAuthDefaultScope},
	}
	jwt, err := auth.IssueToken(user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
	if err != nil {
		t.Fatalf("issue session token: %v", err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, mcpOAuthAuthorizePath+"?"+form.Encode(), http.NoBody)
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
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthAuthorizePath, strings.NewReader(consentForm.Encode()))
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
		"redirect_uri":  {"https://claude.ai/api/mcp/auth_callback"},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthTokenPath, strings.NewReader(tokenForm.Encode()))
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

func refreshMCPToken(t *testing.T, h http.Handler, clientID, refreshToken string, wantStatus int) mcp.OAuthTokenResponse {
	form := url.Values{
		"grant_type":    {mcp.OAuthGrantRefreshToken},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, mcpOAuthTokenPath, strings.NewReader(form.Encode()))
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
	headerJSON, err := json.Marshal(map[string]string{"alg": mcp.JWTAlgRS256, "typ": "JWT", "kid": s.mcpOAuth.kid})
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
	signature, err := s.mcpOAuth.key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
