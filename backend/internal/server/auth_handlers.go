// HTTP handlers for OAuth login, session-bound device pairing, and normal session management.

package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/maruel/gomode/devicelogin"
	"github.com/maruel/gomode/oauth"
	"github.com/maruel/gomode/oauth/oauthclient"
	"github.com/maruel/gomode/oauth/oauthserver"
)

const (
	sessionCookieName = "caic_session"
	sessionTTL        = 30 * 24 * time.Hour
)

type authHandlers struct {
	log           *slog.Logger
	store         *auth.Store
	sessionSecret []byte
	hostState     *auth.HostState

	githubOAuth        *oauthclient.ProviderConfig
	gitlabOAuth        *oauthclient.ProviderConfig
	googleOAuth        *oauthclient.ProviderConfig
	githubAllowedUsers []string
	gitlabAllowedUsers []string
	googleAllowedUsers []string
}

// LoginStartURL returns the login-start path for the first configured forge
// provider, with next set to resume r after login. Empty when no provider is
// configured.
func (h *authHandlers) LoginStartURL(r *http.Request) string {
	provider := h.defaultProvider(r)
	if provider == "" {
		return ""
	}
	values := url.Values{"next": {r.URL.RequestURI()}}
	return "/auth/" + provider + "/start?" + values.Encode()
}

// ProviderLabel returns the human-readable name for a login provider, falling
// back to the raw provider name when it is not configured.
func (h *authHandlers) ProviderLabel(provider string) string {
	if provider == "" {
		return "unknown provider"
	}
	if h.oauthFor(provider) != nil {
		return auth.Provider(provider).Label()
	}
	return provider
}

// handleStart redirects the browser to the OAuth provider's authorization URL.
// Accepts ?return=app to redirect to caic://auth after callback, or ?next=/path
// to resume a same-origin web flow after callback.
func (h *authHandlers) handleStart(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.oauthFor(provider)
		if cfg == nil || cfg.RedirectURI(r) == "" {
			writeError(r.Context(), w, &api.Error{Status: http.StatusNotFound, Code: api.CodeOAuthProviderUnavailable, Message: "provider not found"})
			return
		}
		returnMode := r.URL.Query().Get("return")
		next := r.URL.Query().Get("next")
		if returnMode != "" && returnMode != "app" {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "return must be empty or \"app\""})
			return
		}
		if returnMode == "app" && next != "" {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "next is only valid for web login"})
			return
		}
		if next != "" && !validWebRedirectPath(next) {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "next must be a same-origin absolute path"})
			return
		}

		state, err := oauthserver.GenerateState()
		if err != nil {
			h.log.WarnContext(r.Context(), "generate oauth state", "err", err)
			writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "generate state"})
			return
		}
		// Prefix state with redirect target so the callback knows where to go.
		mode := "web"
		if returnMode == "app" {
			mode = "app"
		}
		fullState := buildLoginState(mode, state, next)
		cookieValue := oauthserver.SignState(fullState, h.sessionSecret)

		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
			Name:     auth.StateCookieName,
			Value:    cookieValue,
			MaxAge:   600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.useSecureCookies(r),
			Path:     "/",
		})
		authURL := cfg.AuthURL(r, fullState)
		http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // G710: configured OAuth provider; next is same-origin state only.
	}
}

// handleCallback handles the OAuth callback: validates state, exchanges code,
// fetches user info, upserts the user, issues a JWT, and redirects.
func (h *authHandlers) handleCallback(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.oauthFor(provider)
		if cfg == nil || cfg.RedirectURI(r) == "" {
			writeError(r.Context(), w, &api.Error{Status: http.StatusNotFound, Code: api.CodeOAuthProviderUnavailable, Message: "provider not found"})
			return
		}

		// Always clear the state cookie.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
			Name:     auth.StateCookieName,
			Value:    "",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.useSecureCookies(r),
			Path:     "/",
		})

		// Validate state cookie.
		stateCookie, err := r.Cookie(auth.StateCookieName)
		if err != nil {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeInvalidOAuthState, Message: "missing state cookie"})
			return
		}
		fullState, ok := oauthserver.ValidateState(stateCookie.Value, h.sessionSecret)
		if !ok {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeInvalidOAuthState, Message: "invalid state"})
			return
		}

		// Extract redirect target.
		redirectMode, next := parseLoginState(fullState)

		// Verify the state query parameter matches the value extracted from the
		// HMAC-validated cookie. The provider echoes back the raw (unsigned) state
		// we originally sent in AuthURL, so compare against fullState directly.
		qState := r.URL.Query().Get("state")
		if qState != fullState {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeInvalidOAuthState, Message: "state mismatch"})
			return
		}

		// Check for error from provider.
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "oauth error: " + oauthErr})
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "missing code"})
			return
		}

		// Exchange code for tokens.
		// PKCE is off; all forge providers are confidential clients with
		// HMAC-signed state.
		token, err := oauthclient.ExchangeCode(r.Context(), cfg.OAuthClientConfig(r), code, "")
		if err != nil {
			h.log.WarnContext(r.Context(), "oauth exchange", "provider", provider, "err", err)
			writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "token exchange failed"})
			return
		}

		// Fetch user identity from the provider's userinfo endpoint.
		providerID, username, avatarURL, err := cfg.FetchUser(r.Context(), token.AccessToken)
		if err != nil {
			h.log.WarnContext(r.Context(), "oauth userinfo", "provider", provider, "err", err)
			writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "userinfo failed"})
			return
		}

		// Check allowlist.
		if err := h.checkLogin(auth.Provider(provider), username); err != nil {
			h.log.WarnContext(r.Context(), "login denied", "provider", provider, "username", username)
			writeError(r.Context(), w, &api.Error{Status: http.StatusForbidden, Code: api.CodeForbidden, Message: err.Error()})
			return
		}

		// Upsert user in store.
		u, err := h.store.UpsertUser(&auth.User{
			Provider:     auth.Provider(provider),
			ProviderID:   providerID,
			Username:     username,
			AvatarURL:    avatarURL,
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenExpiry:  token.Expiry,
		})
		if err != nil {
			h.log.WarnContext(r.Context(), "upsert user", "err", err)
			writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "save user"})
			return
		}

		// Issue JWT.
		jwt, session, err := h.issueSession(r, &u, "provider")
		if err != nil {
			h.log.WarnContext(r.Context(), "issue token", "err", err)
			writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "issue token"})
			return
		}

		h.writeSessionCookie(w, r, jwt, session.ExpiresAt)

		if redirectMode == "app" {
			http.Redirect(w, r, "caic://auth?token="+url.QueryEscape(jwt), http.StatusFound)
		} else {
			redirectTarget := "/"
			if next != "" {
				redirectTarget = next
			}
			http.Redirect(w, r, redirectTarget, http.StatusFound)
		}
	}
}

// handleGetMe handles GET /auth/me.
func (h *authHandlers) handleGetMe(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(r.Context(), w, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "user" + " not found"})
		return
	}
	writeJSONResponse(r.Context(), w, &v1.UserResp{
		ID:        u.ID,
		SessionID: auth.SessionIDFromContext(r.Context()),
		Provider:  string(u.Provider),
		Username:  u.Username,
		AvatarURL: u.AvatarURL,
	}, nil)
}

// refreshTokenRefresher returns a TokenRefresher that renews forge access
// tokens using the configured OAuth providers. The returned function is safe
// for concurrent use.
func (h *authHandlers) refreshTokenRefresher() auth.TokenRefresher {
	return func(ctx context.Context, u *auth.User) *auth.User {
		cfg := h.oauthFor(string(u.Provider))
		if cfg == nil {
			return nil
		}
		// RedirectURI is not needed for refresh; use a zero-value ClientConfig.
		ocfg := oauth.ClientConfig{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			TokenURL:     cfg.TokenURL,
		}
		token, err := oauthclient.RefreshAccessToken(ctx, ocfg, u.RefreshToken)
		if err != nil {
			h.log.WarnContext(ctx, "token refresh failed", "provider", u.Provider, "user", u.ID, "err", err)
			return nil
		}
		u.AccessToken = token.AccessToken
		u.TokenExpiry = token.Expiry
		if token.RefreshToken != "" {
			u.RefreshToken = token.RefreshToken
		}
		if updated, err := h.store.UpsertUser(u); err != nil {
			h.log.WarnContext(ctx, "token refresh upsert failed", "user", u.ID, "err", err)
			return nil
		} else {
			return &updated
		}
	}
}

// handleLogout handles POST /auth/logout.
func (h *authHandlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !h.sessionMutationAllowed(w, r) {
		return
	}
	u, ok := auth.UserFromContext(r.Context())
	if ok {
		if err := h.store.RevokeSession(u.ID, auth.SessionIDFromContext(r.Context())); err != nil {
			writeError(r.Context(), w, &api.Error{Status: 500, Code: api.CodeInternalError, Message: "revoke session"})
			return
		}
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
		Name:     sessionCookieName,
		Value:    "",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.useSecureCookies(r),
		Path:     "/",
	})
	writeJSONResponse(r.Context(), w, &v1.StatusResp{Status: "ok"}, nil)
}

// allowedUsersFor returns the allowlist for the named provider, or nil.
func (h *authHandlers) allowedUsersFor(provider string) []string {
	switch auth.Provider(provider) {
	case auth.ProviderGitHub:
		return h.githubAllowedUsers
	case auth.ProviderGitLab:
		return h.gitlabAllowedUsers
	case auth.ProviderGoogle:
		return h.googleAllowedUsers
	}
	return nil
}

// oauthFor returns the OAuth client config for the named login provider, or nil
// when the provider is unknown or not configured.
func (h *authHandlers) oauthFor(provider string) *oauthclient.ProviderConfig {
	switch auth.Provider(provider) {
	case auth.ProviderGitHub:
		return h.githubOAuth
	case auth.ProviderGitLab:
		return h.gitlabOAuth
	case auth.ProviderGoogle:
		return h.googleOAuth
	}
	return nil
}

// defaultProvider returns the first configured login provider, or empty.
func (h *authHandlers) defaultProvider(r *http.Request) string {
	for _, provider := range loginProviders {
		if cfg := h.oauthFor(provider); cfg != nil && cfg.RedirectURI(r) != "" {
			return provider
		}
	}
	return ""
}

// useSecureCookies reports whether to set the Secure flag on cookies.
// True when the external URL starts with "https://".
func (h *authHandlers) useSecureCookies(r *http.Request) bool {
	return h.hostState != nil && strings.HasPrefix(h.hostState.ExternalURL(r), "https://")
}

func buildLoginState(mode, state, next string) string {
	if mode == "app" {
		return "app:" + state
	}
	if next == "" {
		return "web:" + state
	}
	return "web:" + state + ":" + base64.RawURLEncoding.EncodeToString([]byte(next))
}

func parseLoginState(fullState string) (mode, next string) {
	if strings.HasPrefix(fullState, "app:") {
		return "app", ""
	}
	mode = "web"
	stateBody := strings.TrimPrefix(fullState, "web:")
	_, encodedNext, ok := strings.Cut(stateBody, ":")
	if !ok {
		return mode, ""
	}
	decodedNext, err := base64.RawURLEncoding.DecodeString(encodedNext)
	if err != nil || !validWebRedirectPath(string(decodedNext)) {
		return mode, ""
	}
	return mode, string(decodedNext)
}

func validWebRedirectPath(raw string) bool {
	if strings.Contains(raw, `\`) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Path == "" || u.Fragment != "" {
		return false
	}
	return strings.HasPrefix(u.Path, "/") && !strings.HasPrefix(raw, "//")
}

// routes returns the handler for the OAuth login and session endpoints.
// Patterns use absolute paths (/auth/...) and are mounted at /auth/ on the
// root mux without prefix stripping. These routes are outside /api/ and
// thus exempt from RequireUser; handleGetMe and handleLogout check the
// session internally.
func (h *authHandlers) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /auth/github/start", h.handleStart("github"))
	m.HandleFunc("GET /auth/github/callback", h.handleCallback("github"))
	m.HandleFunc("GET /auth/gitlab/start", h.handleStart("gitlab"))
	m.HandleFunc("GET /auth/gitlab/callback", h.handleCallback("gitlab"))
	m.HandleFunc("GET /auth/google/start", h.handleStart("google"))
	m.HandleFunc("GET /auth/google/callback", h.handleCallback("google"))
	m.HandleFunc("GET /auth/me", h.handleGetMe)
	m.HandleFunc("POST /auth/logout", h.handleLogout)
	m.HandleFunc("GET /auth/sessions", h.handleSessions)
	m.HandleFunc("POST /auth/sessions/{sessionID}/revoke", h.handleRevokeSession)
	return m
}

// loginProviders is the fixed dispatch order for login providers: the default
// provider is the first one configured.
var loginProviders = []string{"github", "gitlab", "google"}

func (h *authHandlers) checkLogin(provider auth.Provider, username string) error {
	if h.oauthFor(string(provider)) == nil {
		return errors.New("login provider unavailable")
	}
	if allowed := h.allowedUsersFor(string(provider)); allowed != nil && !slices.Contains(allowed, strings.ToLower(username)) {
		return fmt.Errorf("user %s is not in the %s allowlist: access denied", username, provider)
	}
	return nil
}

func (h *authHandlers) issueSession(r *http.Request, u *auth.User, source string) (string, auth.Session, error) {
	if err := h.checkLogin(u.Provider, u.Username); err != nil {
		return "", auth.Session{}, err
	}
	return h.store.IssueToken(u, h.sessionSecret, sessionTTL, h.sessionInfo(r, source))
}

func (h *authHandlers) writeSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows the same trusted host policy as normal OAuth login.
		Name: sessionCookieName, Value: token, MaxAge: max(1, int(time.Until(expires).Seconds())), Expires: expires, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: h.useSecureCookies(r), Path: "/",
	})
}

func (h *authHandlers) sessionInfo(r *http.Request, source string) auth.SessionInfo {
	ip, _ := netip.ParseAddr(httpLogContextFromRequest(r).clientIP)
	coarse := "unknown"
	if ip.IsValid() {
		bits := 48
		if ip.Is4() {
			bits = 24
		}
		coarse = netip.PrefixFrom(ip, bits).Masked().String()
	}
	return auth.SessionInfo{Source: source, IP: coarse, UserAgent: devicelogin.UserAgent(r.UserAgent())}
}

// Cookie-authenticated mutations require a non-simple request header and the
// same origin. The host never grants CORS access to these routes, so a remote
// page cannot provide this header through a successful browser preflight.
func (h *authHandlers) sessionMutationAllowed(w http.ResponseWriter, r *http.Request) bool {
	if expected := r.Header.Get("X-Caic-Session"); expected != "" && expected != auth.SessionIDFromContext(r.Context()) {
		writeError(r.Context(), w, &api.Error{Status: http.StatusUnauthorized, Code: api.CodeUnauthorized, Message: "session changed"})
		return false
	}
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		origin := r.Header.Get("Origin")
		if r.Header.Get("X-Caic-Csrf") != "1" || (origin != "" && (h.hostState == nil || origin != h.hostState.ExternalURL(r))) {
			writeError(r.Context(), w, &api.Error{Status: 403, Code: api.CodeForbidden, Message: "same-origin session action required"})
			return false
		}
	}
	return true
}

func (h *authHandlers) handleSessions(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(r.Context(), w, &api.Error{Status: 401, Code: api.CodeUnauthorized, Message: "sign in required"})
		return
	}
	sessions := []v1.SessionResp{}
	ownedSessions := h.store.Sessions(u.ID)
	for i := range ownedSessions {
		p := &ownedSessions[i]
		sessions = append(sessions, v1.SessionResp{ID: p.ID, Current: p.ID == auth.SessionIDFromContext(r.Context()), CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, Source: p.Source, IP: p.IP, UserAgent: p.UserAgent})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONResponse(r.Context(), w, &v1.SessionsResp{Sessions: sessions}, nil)
}

func (h *authHandlers) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(r.Context(), w, &api.Error{Status: 401, Code: api.CodeUnauthorized, Message: "sign in required"})
		return
	}
	if !h.sessionMutationAllowed(w, r) {
		return
	}
	if err := h.store.RevokeSession(u.ID, r.PathValue("sessionID")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(r.Context(), w, &api.Error{Status: 404, Code: api.CodeNotFound, Message: "session not found"})
		} else {
			writeError(r.Context(), w, &api.Error{Status: 500, Code: api.CodeInternalError, Message: "revoke session"})
		}
		return
	}
	writeJSONResponse(r.Context(), w, &v1.StatusResp{Status: "ok"}, nil)
}
