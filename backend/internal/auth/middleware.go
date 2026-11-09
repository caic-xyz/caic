// HTTP middleware resolves local or authenticated authority and binds sessions to revocable account lifetimes.

package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/gomodedev/gomode/oauth"
)

type contextKey struct{}
type sessionContextKey struct{}
type localOperatorContextKey struct{}

// TokenRefresher is called after loading a user whose access token has expired
// but still holds a refresh token. On success it returns an updated *User; on
// failure it returns nil and the middleware uses the original (possibly stale)
// user for this request.
type TokenRefresher func(ctx context.Context, u *User) *User

// Middleware validates the caic_session cookie (or Authorization: Bearer header
// as a fallback for Android/API clients). Injects *User into context.
// When store is nil, resolves the implicit local operator without an identity file.
// A configured store with a missing secret never becomes local authority.
// When refresh is non-nil, expired tokens with a refresh token are renewed
// before the user is injected into the context.
func Middleware(store *Store, secret []byte, refresh TokenRefresher) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil {
				next.ServeHTTP(w, r.WithContext(NewLocalContext(r.Context())))
				return
			}
			token := tokenFromRequest(r)
			if token != "" && len(secret) != 0 {
				if claims, err := validateToken(token, secret); err == nil {
					if session, valid := store.validateSession(claims, token); valid {
						if user, ok := store.ActiveUser(claims.UserID); ok && user.AccountEpoch == session.AccountEpoch {
							bound, release, err := store.BindUser(r.Context(), &user)
							if err != nil {
								next.ServeHTTP(w, r)
								return
							}
							defer release()
							bound, cancel := context.WithDeadline(bound, session.ExpiresAt)
							defer cancel()
							// Refresh expired tokens before they are used.
							tok := oauth.Token{RefreshToken: user.RefreshToken, Expiry: user.TokenExpiry}
							if refresh != nil && tok.Expired() && tok.RefreshToken != "" {
								if updated := refresh(bound, &user); updated != nil {
									user = *updated
								}
							}
							// A provider refresh may cross a disable/re-enable transition.
							// It cannot advance the already-authenticated session generation.
							if user.AccountEpoch != session.AccountEpoch || bound.Err() != nil {
								next.ServeHTTP(w, r)
								return
							}
							ctx := context.WithValue(bound, contextKey{}, &user)
							ctx = context.WithValue(ctx, sessionContextKey{}, session)
							r = r.WithContext(ctx)
						}
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireUser returns a 401 JSON error if no authenticated user is in context.
// Used to protect all non-auth API routes when auth is enabled.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromContext(r.Context()); !ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"authentication required"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewContext returns a context with the given user attached.
func NewContext(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// UserFromContext returns the authenticated user, or (nil, false) in no-auth mode.
func UserFromContext(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(contextKey{}).(*User)
	return u, ok && u != nil
}

// NewLocalContext marks an auth-free request as the implicit local operator.
// Only the request boundary that owns disabled authentication may call this.
func NewLocalContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, localOperatorContextKey{}, true)
}

// LocalOperatorFromContext reports explicit auth-free operator authority.
func LocalOperatorFromContext(ctx context.Context) bool {
	local, _ := ctx.Value(localOperatorContextKey{}).(bool)
	return local
}

// tokenFromRequest extracts the JWT from the request: first from the
// caic_session cookie, then from the Authorization: Bearer header.
func tokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie("caic_session"); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// SessionIDFromContext identifies the tracked host session authenticated by Middleware.
func SessionIDFromContext(ctx context.Context) string {
	session, _ := ctx.Value(sessionContextKey{}).(Session)
	return session.ID
}
