// Tests for OAuth token lifecycle behavior.

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
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

const mcpAuthDefaultScope = mcpScopeRead + " " + mcpScopeTasksRead + " " + mcpScopeTasksWrite + " " + mcpScopeTasksAdmin + " " + mcpScopeReposWrite

func TestOAuthServer(t *testing.T) {
	t.Parallel()
	t.Run("token lifecycle", func(t *testing.T) {
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
			if rotated.TokenType != OAuthTokenTypeBearer || rotated.Scope != tokenResp.Scope {
				t.Fatalf("rotated token metadata = %+v", rotated)
			}
			if _, err := s.oauth.verifyBearer(newMCPBearerRequest(t), rotated.AccessToken); err != nil {
				t.Fatalf("verify rotated access token: %v", err)
			}
			refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusBadRequest)
		})

		t.Run("registered client survives server restart", func(t *testing.T) {
			t.Parallel()
			s, _, user, registered := newMCPOAuthLifecycleRouter(t)

			restarted := newTestRouterWithAuth(t, s.authStore, s.oauth.refreshTokenStorePath)
			restarted.hostState = auth.NewHostState("https://caic.example.com")
			restarted.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
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

			restarted := newTestRouterWithAuth(t, s.authStore, s.oauth.refreshTokenStorePath)
			restarted.hostState = auth.NewHostState("https://caic.example.com")
			restarted.sessionSecret = []byte("0123456789abcdef0123456789abcdef")
			h := mustBuildMCPOAuthLifecycleHandler(t, restarted)

			grants := listOAuthGrants(t, h, &user)
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
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
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
			s.oauth.audit.path = path

			tokenResp := authorizeMCPClient(t, h, &user, &registered)
			rotated := refreshMCPToken(t, h, registered.ClientID, tokenResp.RefreshToken, http.StatusOK)
			form := url.Values{
				"client_id":       {registered.ClientID},
				"token":           {rotated.RefreshToken},
				"token_type_hint": {"refresh_token"},
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/revoke", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "caic.example.com"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("revoke status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
			}

			events := readAuditEvents(t, path)
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

			grants := listOAuthGrants(t, h, &user)
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
			if grant.CreatedAt.IsZero() || grant.ExpiresAt.IsZero() || grant.Status != v1.OAuthGrantStatusActive {
				t.Fatalf("grant timestamps/status = %+v", grant)
			}

			bob, err := s.authStore.UpsertUser(&auth.User{Provider: forge.KindGitHub, ProviderID: "2", Username: "bob", AccessToken: "forge-token"})
			if err != nil {
				t.Fatalf("upsert bob: %v", err)
			}
			bobGrants := listOAuthGrants(t, h, &bob)
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

			grants := listOAuthGrants(t, h, &user)
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
			revokeOAuthGrant(t, h, &bob, firstGrantID, http.StatusNotFound)
			first = refreshMCPToken(t, h, registered.ClientID, first.RefreshToken, http.StatusOK)

			revokeOAuthGrant(t, h, &user, firstGrantID, http.StatusOK)
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
			s.oauth.mu.Lock()
			s.oauth.refreshTokens[oauthRefreshTokenKey(opaque)] = oauthRefreshToken{UserID: user.ID, ClientID: registered.ClientID, Resource: "https://caic.example.com/api/caic/v1/mcp", Scope: mcpScopeRead, ExpiresAt: time.Now().Add(-time.Minute)}
			s.oauth.mu.Unlock()

			refreshMCPToken(t, h, registered.ClientID, opaque, http.StatusBadRequest)
		})

		t.Run("unknown user refresh token is rejected", func(t *testing.T) {
			t.Parallel()
			s, h, _, registered := newMCPOAuthLifecycleRouter(t)
			opaque, err := randomToken()
			if err != nil {
				t.Fatalf("generate refresh token: %v", err)
			}
			s.oauth.mu.Lock()
			s.oauth.refreshTokens[oauthRefreshTokenKey(opaque)] = oauthRefreshToken{UserID: "usr_missing", ClientID: registered.ClientID, Resource: "https://caic.example.com/api/caic/v1/mcp", Scope: mcpScopeRead, ExpiresAt: time.Now().Add(time.Hour)}
			s.oauth.mu.Unlock()

			refreshMCPToken(t, h, registered.ClientID, opaque, http.StatusBadRequest)
		})
	})
	t.Run("approveScope", func(t *testing.T) {
		t.Parallel()

		s := &oauthServer{supportedScopes: []string{mcpScopeRead, mcpScopeTasksRead, mcpScopeTasksWrite, mcpScopeTasksAdmin, mcpScopeReposWrite}, defaultScopes: []string{mcpScopeRead, mcpScopeTasksRead}}
		requested := mcpScopeRead + " " + mcpScopeTasksRead + " " + mcpScopeTasksWrite
		scope, err := s.approveScope(requested, url.Values{"scope_form": {"1"}, "scope": {mcpScopeTasksWrite, mcpScopeRead}})
		if err != nil {
			t.Fatalf("approveScope: %v", err)
		}
		if scope != mcpScopeRead+" "+mcpScopeTasksWrite {
			t.Fatalf("scope = %q, want selected scopes in canonical order", scope)
		}
		if _, err := s.approveScope(requested, url.Values{"scope_form": {"1"}, "scope": {mcpScopeReposWrite}}); err == nil {
			t.Fatal("approveScope unrequested scope error = nil")
		}
		if _, err := s.approveScope(requested, url.Values{"scope_form": {"1"}}); err == nil {
			t.Fatal("approveScope empty selection error = nil")
		}
	})
	t.Run("consent page", func(t *testing.T) {
		t.Parallel()

		s, _, user := newAuthEnabledRouter(t)
		h, err := s.buildHandler()
		if err != nil {
			t.Fatalf("buildHandler: %v", err)
		}

		verifier := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digest := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(digest[:])

		t.Run("renders consent page with user info and scopes", func(t *testing.T) {
			t.Parallel()
			s2, _, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Claude Code", []string{"https://claude.ai/api/mcp/auth_callback"})

			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"https://claude.ai/api/mcp/auth_callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {mcpAuthDefaultScope},
			}
			jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
			}
			body := w.Body.String()

			// Verify client identity is shown as unverified, with redirect URI and client ID.
			if !strings.Contains(body, "Claude Code") {
				t.Errorf("body missing client name: %s", body)
			}
			if !strings.Contains(body, "self-declared") {
				t.Errorf("body missing unverified client warning: %s", body)
			}
			if !strings.Contains(body, "https://claude.ai/api/mcp/auth_callback") {
				t.Errorf("body missing redirect URI: %s", body)
			}
			if !strings.Contains(body, registered.ClientID) {
				t.Errorf("body missing client ID: %s", body)
			}

			// Verify username.
			if !strings.Contains(body, user2.Username) {
				t.Errorf("body missing username %q: %s", user2.Username, body)
			}

			// Verify provider.
			if !strings.Contains(body, "GitHub") {
				t.Errorf("body missing provider: %s", body)
			}

			// Verify the avatar does not load a third-party URL and leak OAuth request details.
			if user2.AvatarURL != "" && strings.Contains(body, user2.AvatarURL) {
				t.Errorf("body contains external avatar URL: %s", body)
			}

			// Verify resource.
			if !strings.Contains(body, "caic.example.com/api/caic/v1/mcp") {
				t.Errorf("body missing resource URL: %s", body)
			}

			// Verify scope descriptions.
			if !strings.Contains(body, "caic:mcp.read") {
				t.Error("body missing scope caic:mcp.read")
			}
			if !strings.Contains(body, "Use basic MCP tools including usage and non-task resources") {
				t.Error("body missing scope description for caic:mcp.read")
			}
			if !strings.Contains(body, "caic:tasks.read") {
				t.Error("body missing scope caic:tasks.read")
			}
			if !strings.Contains(body, "Read task information") {
				t.Error("body missing scope description for caic:tasks.read")
			}
			if !strings.Contains(body, `type="checkbox" name="scope" value="caic:tasks.write" form="consent-form"`) {
				t.Error("body missing selectable write scope attached to consent form")
			}
			if !strings.Contains(body, "Manage repositories") {
				t.Error("body missing repos write scope description")
			}

			// Verify security warning.
			if !strings.Contains(body, "caic MCP only") {
				t.Error("body missing security warning")
			}
			if !strings.Contains(body, "GitHub") && !strings.Contains(body, "GitLab") {
				t.Error("body missing forge credential disclaimer")
			}

			// Verify consent token is present.
			if !strings.Contains(body, `name="consent_token"`) {
				t.Error("body missing consent_token field")
			}

			// Verify form action.
			if !strings.Contains(body, `id="consent-form" method="post" action="https://caic.example.com/oauth/authorize"`) {
				t.Error("body missing consent form action")
			}

			// Verify Deny and Authorize buttons.
			if !strings.Contains(body, "Deny") {
				t.Error("body missing Deny button")
			}
			if !strings.Contains(body, "Authorize") {
				t.Error("body missing Authorize button")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
			}
			if w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", w.Header().Get("Referrer-Policy"))
			}
			csp := w.Header().Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'none'") {
				t.Errorf("Content-Security-Policy = %q, want locked-down default-src", csp)
			}
			if strings.Contains(csp, "form-action") {
				t.Errorf("Content-Security-Policy = %q, want form-action omitted for hosted OAuth popups", csp)
			}
		})

		t.Run("shows spoofable client name as unverified", func(t *testing.T) {
			t.Parallel()
			s2, _, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Claude Code", []string{"https://evil.example/callback"})
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"https://evil.example/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
			}
			jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{"Claude Code", "self-declared", "https://evil.example/callback", registered.ClientID} {
				if !strings.Contains(body, want) {
					t.Fatalf("body missing %q: %s", want, body)
				}
			}
		})

		t.Run("renders with default scope when empty", func(t *testing.T) {
			t.Parallel()
			s2, _, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Test Client", []string{"http://localhost:9999/callback"})

			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"http://localhost:9999/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {""},
			}
			jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
			}
			// Default scope should be shown.
			if !strings.Contains(w.Body.String(), "caic:mcp.read") {
				t.Error("body missing default scope caic:mcp.read")
			}
		})

		t.Run("rejects unknown client", func(t *testing.T) {
			t.Parallel()
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {"unknown_client"},
				"redirect_uri":          {"https://example.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
			}
			jwt, err := auth.IssueToken(&user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})

		t.Run("redirects unauthenticated web authorization to caic login", func(t *testing.T) {
			t.Parallel()
			s2, _, _ := newAuthEnabledRouter(t)
			s2.authHandlers.hostState = s2.hostState
			s2.authHandlers.githubOAuth = &auth.ProviderConfig{ //nolint:gosec // Test-only OAuth credentials.
				ClientID:     "github-client",
				ClientSecret: "github-secret",
				AuthEndpoint: "https://github.com/login/oauth/authorize",
				TokenURL:     "https://github.com/login/oauth/access_token",
				UserInfoURL:  "https://api.github.com/user",
				Scopes:       []string{"repo"},
				Provider:     "github",
				Host:         s2.hostState,
			}
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {"any"},
				"redirect_uri":          {"https://example.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
			}
			authorizePath := "/oauth/authorize" + "?" + form.Encode()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, authorizePath, http.NoBody)
			req.Host = "caic.example.com"
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusFound {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusFound, w.Body.String())
			}
			location, err := url.Parse(w.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if location.Path != "/auth/github/start" {
				t.Fatalf("Location path = %q, want GitHub login start", location.Path)
			}
			if location.Query().Get("next") != authorizePath {
				t.Fatalf("next = %q, want %q", location.Query().Get("next"), authorizePath)
			}
		})

		t.Run("rejects unauthenticated request", func(t *testing.T) {
			t.Parallel()
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {"any"},
				"redirect_uri":          {"https://example.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d (login_required)", w.Code, http.StatusUnauthorized)
			}
		})

		t.Run("rejects invalid redirect URI", func(t *testing.T) {
			t.Parallel()
			s2, _, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})

			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"https://different.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
			}
			jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})

		t.Run("POST approves and redirects with authorization code", func(t *testing.T) {
			t.Parallel()
			s2, _, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"https://example.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
				"state":                 {"client-state"},
			}
			jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("consent status = %d, want %d", w.Code, http.StatusOK)
			}
			consentForm := url.Values{"consent_token": {consentTokenFromHTML(t, w.Body.String())}, "decision": {"approve"}}
			req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/authorize", strings.NewReader(consentForm.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w = httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
			}
			location, err := url.Parse(w.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if location.Scheme != "https" || location.Host != "example.com" || location.Path != "/callback" {
				t.Fatalf("Location = %q, want callback redirect", location.String())
			}
			if location.Query().Get("code") == "" {
				t.Fatalf("Location = %q, missing code", location.String())
			}
			if location.Query().Get("state") != "client-state" {
				t.Fatalf("state = %q, want client-state", location.Query().Get("state"))
			}
			if location.Query().Get("iss") != "https://caic.example.com" {
				t.Fatalf("iss = %q, want https://caic.example.com", location.Query().Get("iss"))
			}
		})

		t.Run("POST denies and redirects with access_denied", func(t *testing.T) {
			t.Parallel()
			s2, _, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"https://example.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
				"state":                 {"deny-state"},
			}
			jwt, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("consent status = %d, want %d", w.Code, http.StatusOK)
			}
			consentForm := url.Values{"consent_token": {consentTokenFromHTML(t, w.Body.String())}, "decision": {"deny"}}
			req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/authorize", strings.NewReader(consentForm.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w = httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusSeeOther, w.Body.String())
			}
			location, err := url.Parse(w.Header().Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if location.Query().Get("error") != "access_denied" {
				t.Fatalf("error = %q, want access_denied", location.Query().Get("error"))
			}
			if location.Query().Get("state") != "deny-state" {
				t.Fatalf("state = %q, want deny-state", location.Query().Get("state"))
			}
			if location.Query().Get("iss") != "https://caic.example.com" {
				t.Fatalf("iss = %q, want https://caic.example.com", location.Query().Get("iss"))
			}
		})

		t.Run("POST rejects invalid consent token", func(t *testing.T) {
			t.Parallel()
			form := url.Values{"consent_token": {"invalid-token"}}
			jwt, err := auth.IssueToken(&user, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})

		t.Run("POST rejects wrong user", func(t *testing.T) {
			t.Parallel()
			s2, store2, user2 := newAuthEnabledRouter(t)
			h2, err := s2.buildHandler()
			if err != nil {
				t.Fatalf("buildHandler: %v", err)
			}
			registered := registerTestClient(t, h2, "Test Client", []string{"https://example.com/callback"})

			// Create consent as user2.
			form := url.Values{
				"response_type":         {"code"},
				"client_id":             {registered.ClientID},
				"redirect_uri":          {"https://example.com/callback"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"resource":              {"https://caic.example.com/api/caic/v1/mcp"},
				"scope":                 {"caic:mcp.read"},
			}
			jwt2, err := auth.IssueToken(&user2, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwt2, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w := httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("consent status = %d, want %d", w.Code, http.StatusOK)
			}
			consentToken := consentTokenFromHTML(t, w.Body.String())

			// Try to submit consent as a different user.
			otherUser, err := store2.UpsertUser(&auth.User{
				Provider:    forge.KindGitLab,
				ProviderID:  "999",
				Username:    "bob",
				AccessToken: "other-token",
			})
			if err != nil {
				t.Fatalf("upsert other user: %v", err)
			}
			jwtOther, err := auth.IssueToken(&otherUser, []byte("0123456789abcdef0123456789abcdef"), sessionTTL)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			consentForm := url.Values{"consent_token": {consentToken}}
			req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/authorize", strings.NewReader(consentForm.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Host = "caic.example.com"
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: jwtOther, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
			w = httptest.NewRecorder()
			h2.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (wrong user)", w.Code, http.StatusBadRequest)
			}
		})
	})
}

func newMCPOAuthLifecycleRouter(t *testing.T) (*testRouter, http.Handler, auth.User, OAuthRegisterResponse) {
	store, err := auth.Open(t.TempDir() + "/users.json")
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	user, err := store.UpsertUser(&auth.User{Provider: forge.KindGitHub, ProviderID: "1", Username: "alice", AccessToken: "forge-token"})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	checker, err := ipgeo.NewChecker(t.Context(), "0.0.0.0/0,::/0", "", "")
	if err != nil {
		t.Fatalf("ipgeo.NewChecker: %v", err)
	}
	backend := &mdruntime.Backend{}
	taskMgr := tasks.New(tasks.Config{ServerCtx: t.Context()})
	repoSvc := repos.NewService("", "", "", nil, repos.NewRegistry(nil), taskMgr, backend, nil)
	prefs := newTestPrefs(t)
	forgeManager := forgemanager.New("", "", nil)

	s, err := New(t.Context(), Dependencies{
		Repos:                      repoSvc,
		ProcessBackend:             backend,
		TaskManager:                taskMgr,
		Preferences:                prefs,
		IPGeoChecker:               checker,
		Forge:                      forgeManager,
		AuthStore:                  store,
		SessionSecret:              []byte("0123456789abcdef0123456789abcdef"),
		HostState:                  auth.NewHostState("https://caic.example.com"),
		OAuthRefreshTokenStorePath: t.TempDir() + "/mcp_oauth_refresh_tokens.json",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tr := &testRouter{Router: s, taskMgr: taskMgr, repos: repoSvc, prefs: prefs, forge: forgeManager}

	// Re-export authStore for restart tests that need to share refresh tokens.
	tr.authStore = store

	h, err := tr.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}

	registerBody := `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],"token_endpoint_auth_method":"none"}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/register", strings.NewReader(registerBody))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var registered OAuthRegisterResponse
	if err := json.NewDecoder(w.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	return tr, h, user, registered
}

func mustBuildMCPOAuthLifecycleHandler(t *testing.T, s *testRouter) http.Handler {
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler() error = %v", err)
	}
	return h
}

func authorizeMCPClient(t *testing.T, h http.Handler, user *auth.User, registered *OAuthRegisterResponse) OAuthTokenResponse {
	return authorizeMCPClientWithRedirect(t, h, user, registered, "https://claude.ai/api/mcp/auth_callback")
}

func authorizeMCPClientWithRedirect(t *testing.T, h http.Handler, user *auth.User, registered *OAuthRegisterResponse, redirectURI string) OAuthTokenResponse {
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
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/oauth/authorize"+"?"+form.Encode(), http.NoBody)
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
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/authorize", strings.NewReader(consentForm.Encode()))
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
		"grant_type":    {OAuthGrantAuthorizationCode},
		"code":          {code},
		"client_id":     {registered.ClientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {resource},
	}
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "caic.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("token status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var tokenResp OAuthTokenResponse
	if err := json.NewDecoder(w.Body).Decode(&tokenResp); err != nil {
		t.Fatal(err)
	}
	return tokenResp
}

func listOAuthGrants(t *testing.T, h http.Handler, user *auth.User) v1.OAuthGrantsResp {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/oauth/grants", http.NoBody)
	req.Host = "caic.example.com"
	addTestSessionCookie(t, req, user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list MCP grants status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var grants v1.OAuthGrantsResp
	if err := json.NewDecoder(w.Body).Decode(&grants); err != nil {
		t.Fatal(err)
	}
	return grants
}

func revokeOAuthGrant(t *testing.T, h http.Handler, user *auth.User, grantID string, wantStatus int) {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/caic/v1/oauth/grants/"+grantID+"/revoke", http.NoBody)
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

func refreshMCPToken(t *testing.T, h http.Handler, clientID, refreshToken string, wantStatus int) OAuthTokenResponse {
	form := url.Values{
		"grant_type":    {OAuthGrantRefreshToken},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "caic.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != wantStatus {
		t.Fatalf("refresh status = %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var tokenResp OAuthTokenResponse
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
	headerJSON, err := json.Marshal(map[string]string{"alg": JWTAlgRS256, "typ": "JWT", "kid": s.oauth.kid})
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
	signature, err := s.oauth.key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
