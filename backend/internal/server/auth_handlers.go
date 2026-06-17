// HTTP handlers for OAuth 2.0 login endpoints and session management.

package server

import (
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/oauth"
)

const (
	sessionCookieName = "caic_session"
	sessionTTL        = 30 * 24 * time.Hour
	sessionMaxAge     = 30 * 24 * 60 * 60 // 30 days in seconds
)

type authHandlers struct {
	store         *auth.Store
	sessionSecret []byte
	hostState     *auth.HostState

	githubOAuth        *auth.ProviderConfig
	gitlabOAuth        *auth.ProviderConfig
	githubAllowedUsers []string
	gitlabAllowedUsers []string
}

// LoginStartURL returns the login-start path for the first configured forge
// provider, with next set to resume r after login. Empty when no provider is
// configured.
func (h *authHandlers) LoginStartURL(r *http.Request) string {
	provider := h.defaultProvider()
	if provider == "" {
		return ""
	}
	values := url.Values{"next": {r.URL.RequestURI()}}
	return "/auth/" + provider + "/start?" + values.Encode()
}

// ProviderLabel returns the human-readable name for a forge provider, falling
// back to the raw provider name when it is not configured.
func (h *authHandlers) ProviderLabel(provider string) string {
	if cfg := h.providerConfig(provider); cfg != nil && cfg.Label != "" {
		return cfg.Label
	}
	if provider == "" {
		return "unknown provider"
	}
	return provider
}

// handleStart redirects the browser to the OAuth provider's authorization URL.
// Accepts ?return=app to redirect to caic://auth after callback, or ?next=/path
// to resume a same-origin web flow after callback.
func (h *authHandlers) handleStart(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.providerConfig(provider)
		if cfg == nil || cfg.RedirectURI() == "" {
			writeError(w, api.NotFound("provider"))
			return
		}
		returnMode := r.URL.Query().Get("return")
		next := r.URL.Query().Get("next")
		if returnMode != "" && returnMode != "app" {
			writeError(w, api.BadRequest("return must be empty or \"app\""))
			return
		}
		if returnMode == "app" && next != "" {
			writeError(w, api.BadRequest("next is only valid for web login"))
			return
		}
		if next != "" && !validWebRedirectPath(next) {
			writeError(w, api.BadRequest("next must be a same-origin absolute path"))
			return
		}

		state, err := oauth.GenerateState()
		if err != nil {
			slog.WarnContext(r.Context(), "generate oauth state", "err", err)
			writeError(w, api.InternalError("generate state"))
			return
		}
		// Prefix state with redirect target so the callback knows where to go.
		mode := "web"
		if returnMode == "app" {
			mode = "app"
		}
		fullState := buildLoginState(mode, state, next)
		cookieValue := oauth.SignState(fullState, h.sessionSecret)

		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
			Name:     auth.StateCookieName,
			Value:    cookieValue,
			MaxAge:   600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.useSecureCookies(),
			Path:     "/",
		})
		authURL := cfg.AuthURL(fullState)
		http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // G710: configured OAuth provider; next is same-origin state only.
	}
}

// handleCallback handles the OAuth callback: validates state, exchanges code,
// fetches user info, upserts the user, issues a JWT, and redirects.
func (h *authHandlers) handleCallback(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.providerConfig(provider)
		if cfg == nil || cfg.RedirectURI() == "" {
			writeError(w, api.NotFound("provider"))
			return
		}

		// Always clear the state cookie.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
			Name:     auth.StateCookieName,
			Value:    "",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.useSecureCookies(),
			Path:     "/",
		})

		// Validate state cookie.
		stateCookie, err := r.Cookie(auth.StateCookieName)
		if err != nil {
			writeError(w, api.BadRequest("missing state cookie"))
			return
		}
		fullState, ok := oauth.ValidateState(stateCookie.Value, h.sessionSecret)
		if !ok {
			writeError(w, api.BadRequest("invalid state"))
			return
		}

		// Extract redirect target.
		redirectMode, next := parseLoginState(fullState)

		// Verify the state query parameter matches the value extracted from the
		// HMAC-validated cookie. The provider echoes back the raw (unsigned) state
		// we originally sent in AuthURL, so compare against fullState directly.
		qState := r.URL.Query().Get("state")
		if qState != fullState {
			writeError(w, api.BadRequest("state mismatch"))
			return
		}

		// Check for error from provider.
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			writeError(w, api.BadRequest("oauth error: "+oauthErr))
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			writeError(w, api.BadRequest("missing code"))
			return
		}

		// Exchange code for tokens.
		accessToken, refreshToken, tokenExpiry, err := oauth.ExchangeCode(r.Context(), cfg.OAuthClientConfig(), code)
		if err != nil {
			slog.WarnContext(r.Context(), "oauth exchange", "provider", provider, "err", err)
			writeError(w, api.InternalError("token exchange failed"))
			return
		}

		// Fetch user identity.
		providerID, username, avatarURL, err := auth.FetchUserInfo(r.Context(), cfg, accessToken)
		if err != nil {
			slog.WarnContext(r.Context(), "oauth userinfo", "provider", provider, "err", err)
			writeError(w, api.InternalError("userinfo failed"))
			return
		}

		// Check allowlist.
		if allowed := h.allowedUsersFor(provider); allowed != nil {
			if !slices.Contains(allowed, strings.ToLower(username)) {
				slog.WarnContext(r.Context(), "user not in allowlist", "provider", provider, "username", username)
				writeError(w, api.Forbidden("user "+username+" is not in the "+provider+" allowlist"))
				return
			}
		}

		// Upsert user in store.
		u, err := h.store.UpsertUser(&auth.User{
			Provider:     forge.Kind(provider),
			ProviderID:   providerID,
			Username:     username,
			AvatarURL:    avatarURL,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			TokenExpiry:  tokenExpiry,
		})
		if err != nil {
			slog.WarnContext(r.Context(), "upsert user", "err", err)
			writeError(w, api.InternalError("save user"))
			return
		}

		// Issue JWT.
		jwt, err := auth.IssueToken(&u, h.sessionSecret, sessionTTL)
		if err != nil {
			slog.WarnContext(r.Context(), "issue token", "err", err)
			writeError(w, api.InternalError("issue token"))
			return
		}

		// Set session cookie.
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
			Name:     sessionCookieName,
			Value:    jwt,
			MaxAge:   sessionMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   h.useSecureCookies(),
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
		writeError(w, api.NotFound("user"))
		return
	}
	writeJSONResponse(w, &v1.UserResp{
		ID:        u.ID,
		Provider:  string(u.Provider),
		Username:  u.Username,
		AvatarURL: u.AvatarURL,
	}, nil)
}

// handleLogout handles POST /auth/logout.
func (h *authHandlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is set dynamically; all required attributes are present
		Name:     sessionCookieName,
		Value:    "",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.useSecureCookies(),
		Path:     "/",
	})
	writeJSONResponse(w, &v1.StatusResp{Status: "ok"}, nil)
}

// allowedUsersFor returns the allowlist for the named provider, or nil.
func (h *authHandlers) allowedUsersFor(provider string) []string {
	switch provider {
	case "github":
		return h.githubAllowedUsers
	case "gitlab":
		return h.gitlabAllowedUsers
	}
	return nil
}

// providerConfig returns the ProviderConfig for the named provider, or nil.
func (h *authHandlers) providerConfig(provider string) *auth.ProviderConfig {
	switch provider {
	case "github":
		return h.githubOAuth
	case "gitlab":
		return h.gitlabOAuth
	}
	return nil
}

// defaultProvider returns the first configured forge login provider, or empty.
func (h *authHandlers) defaultProvider() string {
	for _, provider := range []string{"github", "gitlab"} {
		if cfg := h.providerConfig(provider); cfg != nil && cfg.RedirectURI() != "" {
			return provider
		}
	}
	return ""
}

// useSecureCookies reports whether to set the Secure flag on cookies.
// True when the external URL starts with "https://".
func (h *authHandlers) useSecureCookies() bool {
	return h.hostState != nil && strings.HasPrefix(h.hostState.ExternalURL(), "https://")
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
	m.HandleFunc("GET /auth/me", h.handleGetMe)
	m.HandleFunc("POST /auth/logout", h.handleLogout)
	return m
}
