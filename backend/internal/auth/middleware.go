// HTTP middleware for JWT session validation and user context injection.

package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey struct{}

// Middleware validates the caic_session cookie (or Authorization: Bearer header
// as a fallback for Android/API clients). Injects *User into context.
// When secret is nil (auth disabled), passes through unconditionally.
func Middleware(store *Store, secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if secret == nil {
				next.ServeHTTP(w, r)
				return
			}
			token := tokenFromRequest(r)
			if token != "" {
				if claims, err := ValidateToken(token, secret); err == nil {
					if user, ok := store.FindByID(claims.UserID); ok {
						r = r.WithContext(context.WithValue(r.Context(), contextKey{}, &user))
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
