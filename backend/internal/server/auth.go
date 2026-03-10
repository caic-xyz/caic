// HTTP handlers for OAuth 2.0 login endpoints and session management.
package server

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

const (
	sessionCookieName = "caic_session"
	sessionTTL        = 30 * 24 * time.Hour
	sessionMaxAge     = 30 * 24 * 60 * 60 // 30 days in seconds
)

// handleAuthStart redirects the browser to the OAuth provider's authorization URL.
// Accepts ?return=app to redirect to caic://auth after callback.
func (s *Server) handleAuthStart(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.oauthConfigFor(provider)
		if cfg == nil {
			writeError(w, dto.NotFound("provider"))
			return
		}
		returnMode := r.URL.Query().Get("return")
		if returnMode != "" && returnMode != "app" {
			writeError(w, dto.BadRequest("return must be empty or \"app\""))
			return
		}

		state, err := auth.GenerateState()
		if err != nil {
			slog.WarnContext(r.Context(), "generate oauth state", "err", err)
			writeError(w, dto.InternalError("generate state"))
			return
		}
		// Prefix state with redirect target so the callback knows where to go.
		prefix := "web:"
		if returnMode == "app" {
			prefix = "app:"
		}
		fullState := prefix + state
		cookieValue := auth.SignState(fullState, s.sessionSecret)

		http.SetCookie(w, &http.Cookie{
			Name:     auth.StateCookieName,
			Value:    cookieValue,
			MaxAge:   600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   s.useSecureCookies(),
			Path:     "/",
		})
		http.Redirect(w, r, cfg.AuthURL(fullState), http.StatusFound)
	}
}

// handleAuthCallback handles the OAuth callback: validates state, exchanges code,
// fetches user info, upserts the user, issues a JWT, and redirects.
func (s *Server) handleAuthCallback(provider string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.oauthConfigFor(provider)
		if cfg == nil {
			writeError(w, dto.NotFound("provider"))
			return
		}

		// Always clear the state cookie.
		http.SetCookie(w, &http.Cookie{
			Name:     auth.StateCookieName,
			Value:    "",
			MaxAge:   -1,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   s.useSecureCookies(),
			Path:     "/",
		})

		// Validate state cookie.
		stateCookie, err := r.Cookie(auth.StateCookieName)
		if err != nil {
			writeError(w, dto.BadRequest("missing state cookie"))
			return
		}
		fullState, ok := auth.ValidateState(stateCookie.Value, s.sessionSecret)
		if !ok {
			writeError(w, dto.BadRequest("invalid state"))
			return
		}

		// Extract redirect prefix.
		redirectMode := "web"
		if strings.HasPrefix(fullState, "app:") {
			redirectMode = "app"
		}

		// Verify the state query parameter matches the value extracted from the
		// HMAC-validated cookie. The provider echoes back the raw (unsigned) state
		// we originally sent in AuthURL, so compare against fullState directly.
		qState := r.URL.Query().Get("state")
		if qState != fullState {
			writeError(w, dto.BadRequest("state mismatch"))
			return
		}

		// Check for error from provider.
		if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
			writeError(w, dto.BadRequest("oauth error: "+oauthErr))
			return
		}

		code := r.URL.Query().Get("code")
		if code == "" {
			writeError(w, dto.BadRequest("missing code"))
			return
		}

		// Exchange code for tokens.
		accessToken, refreshToken, tokenExpiry, err := auth.ExchangeCode(r.Context(), cfg, code)
		if err != nil {
			slog.WarnContext(r.Context(), "oauth exchange", "provider", provider, "err", err)
			writeError(w, dto.InternalError("token exchange failed"))
			return
		}

		// Fetch user identity.
		providerID, username, avatarURL, err := auth.FetchUserInfo(r.Context(), cfg, accessToken)
		if err != nil {
			slog.WarnContext(r.Context(), "oauth userinfo", "provider", provider, "err", err)
			writeError(w, dto.InternalError("userinfo failed"))
			return
		}

		// Check allowlist.
		if allowed := s.allowedUsersFor(provider); allowed != nil {
			if _, ok := allowed[strings.ToLower(username)]; !ok {
				slog.WarnContext(r.Context(), "user not in allowlist", "provider", provider, "username", username)
				writeError(w, dto.Forbidden("user "+username+" is not in the "+provider+" allowlist"))
				return
			}
		}

		// Upsert user in store.
		u, err := s.authStore.UpsertUser(&auth.User{
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
			writeError(w, dto.InternalError("save user"))
			return
		}

		// Issue JWT.
		jwt, err := auth.IssueToken(&u, s.sessionSecret, sessionTTL)
		if err != nil {
			slog.WarnContext(r.Context(), "issue token", "err", err)
			writeError(w, dto.InternalError("issue token"))
			return
		}

		// Set session cookie.
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    jwt,
			MaxAge:   sessionMaxAge,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   s.useSecureCookies(),
			Path:     "/",
		})

		if redirectMode == "app" {
			http.Redirect(w, r, "caic://auth?token="+url.QueryEscape(jwt), http.StatusFound)
		} else {
			http.Redirect(w, r, "/", http.StatusFound)
		}
	}
}

// handleGetMe handles GET /api/v1/auth/me.
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		writeError(w, dto.NotFound("user"))
		return
	}
	writeJSONResponse(w, &v1.UserResp{
		ID:        u.ID,
		Provider:  string(u.Provider),
		Username:  u.Username,
		AvatarURL: u.AvatarURL,
	}, nil)
}

// handleLogout handles POST /api/v1/auth/logout.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.useSecureCookies(),
		Path:     "/",
	})
	writeJSONResponse(w, &v1.StatusResp{Status: "ok"}, nil)
}

// allowedUsersFor returns the allowlist for the named provider, or nil.
func (s *Server) allowedUsersFor(provider string) map[string]struct{} {
	switch provider {
	case "github":
		return s.githubAllowedUsers
	case "gitlab":
		return s.gitlabAllowedUsers
	}
	return nil
}

// oauthConfigFor returns the ProviderConfig for the named provider, or nil.
func (s *Server) oauthConfigFor(provider string) *auth.ProviderConfig {
	switch provider {
	case "github":
		return s.githubOAuth
	case "gitlab":
		return s.gitlabOAuth
	}
	return nil
}

// useSecureCookies reports whether to set the Secure flag on cookies.
// True when the external URL starts with "https://".
func (s *Server) useSecureCookies() bool {
	if s.githubOAuth != nil {
		ru := s.githubOAuth.RedirectURI
		if idx := strings.Index(ru, "/api/v1/"); idx >= 0 {
			return strings.HasPrefix(ru[:idx], "https://")
		}
	}
	if s.gitlabOAuth != nil {
		ru := s.gitlabOAuth.RedirectURI
		if idx := strings.Index(ru, "/api/v1/"); idx >= 0 {
			return strings.HasPrefix(ru[:idx], "https://")
		}
	}
	return false
}
