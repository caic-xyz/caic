// HTTP handlers for OAuth 2.0 login endpoints and session management.

package server

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/oauth"
	"github.com/caic-xyz/caic/oauth/oauthclient"
	"github.com/caic-xyz/caic/oauth/oauthserver"
)

const (
	sessionCookieName = "caic_session"
	sessionTTL        = 30 * 24 * time.Hour
	sessionMaxAge     = 30 * 24 * 60 * 60 // 30 days in seconds
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

// loginProviders is the fixed dispatch order for login providers: the default
// provider is the first one configured.
var loginProviders = []string{"github", "gitlab", "google"}

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
			writeError(r.Context(), w, api.NotFound("provider"))
			return
		}
		returnMode := r.URL.Query().Get("return")
		next := r.URL.Query().Get("next")
		if returnMode != "" && returnMode != "app" {
			writeError(r.Context(), w, api.BadRequest("return must be empty or \"app\""))
			return
		}
		if returnMode == "app" && next != "" {
			writeError(r.Context(), w, api.BadRequest("next is only valid for web login"))
			return
		}
		if next != "" && !validWebRedirectPath(next) {
			writeError(r.Context(), w, api.BadRequest("next must be a same-origin absolute path"))
			return
		}

		state, err := oauthserver.GenerateState()
		if err != nil {
			h.log.WarnContext(r.Context(), "generate oauth state", "err", err)
			writeError(r.Context(), w, api.InternalError("generate state"))
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
			writeError(r.Context(), w, api.NotFound("provider"))
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
			writeError(r.Context(), w, api.BadRequest("missing state cookie"))
			return
		}
		fullState, ok := oauthserver.ValidateState(stateCookie.Value, h.sessionSecret)
		if !ok {
			writeError(r.Context(), w, api.BadRequest("invalid state"))
			return
		}

		// Extract redirect target.
		redirectMode, next := parseLoginState(fullState)

		// Verify the state query parameter matches the value extracted from the
		// HMAC-validated cookie. The provider echoes back the raw (unsigned) state
		// we originally sent in AuthURL, so compare against fullState directly.
		qState := r.URL.Query().Get("state")
		if qState != fullState {
			writeError(r.Context(), w, api.BadRequest("state mismatch"))
			return
		}

		// Check for error from provider.
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			writeError(r.Context(), w, api.BadRequest("oauth error: "+oauthErr))
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			writeError(r.Context(), w, api.BadRequest("missing code"))
			return
		}

		// Exchange code for tokens.
		// PKCE is off; all forge providers are confidential clients with
		// HMAC-signed state.
		token, err := oauthclient.ExchangeCode(r.Context(), cfg.OAuthClientConfig(r), code, "")
		if err != nil {
			h.log.WarnContext(r.Context(), "oauth exchange", "provider", provider, "err", err)
			writeError(r.Context(), w, api.InternalError("token exchange failed"))
			return
		}

		// Fetch user identity from the provider's userinfo endpoint.
		providerID, username, avatarURL, err := cfg.FetchUser(r.Context(), token.AccessToken)
		if err != nil {
			h.log.WarnContext(r.Context(), "oauth userinfo", "provider", provider, "err", err)
			writeError(r.Context(), w, api.InternalError("userinfo failed"))
			return
		}

		// Check allowlist.
		if allowed := h.allowedUsersFor(provider); allowed != nil {
			if !slices.Contains(allowed, strings.ToLower(username)) {
				h.log.WarnContext(r.Context(), "user not in allowlist", "provider", provider, "username", username)
				writeError(r.Context(), w, api.Forbidden("user "+username+" is not in the "+provider+" allowlist"))
				return
			}
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
			writeError(r.Context(), w, api.InternalError("save user"))
			return
		}

		// Issue JWT.
		jwt, err := auth.IssueToken(&u, h.sessionSecret, sessionTTL)
		if err != nil {
			h.log.WarnContext(r.Context(), "issue token", "err", err)
			writeError(r.Context(), w, api.InternalError("issue token"))
			return
		}

		// Set session cookie.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
			Name:     sessionCookieName,
			Value:    jwt,
			MaxAge:   sessionMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.useSecureCookies(r),
			Path:     "/",
		})

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
		writeError(r.Context(), w, api.NotFound("user"))
		return
	}
	writeJSONResponse(r.Context(), w, &v1.UserResp{
		ID:        u.ID,
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
	return m
}
