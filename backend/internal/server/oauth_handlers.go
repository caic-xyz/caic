// OAuth grant API routes and caic-specific OAuth adapters.

package server

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"time"

	_ "embed"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/oauth"
)

//go:embed oauth_consent.html
var oauthConsentHTML string

var oauthConsentTemplate = template.Must(template.New("oauth-consent").Parse(oauthConsentHTML))

// OAuth adapters are the boundary between generic protocol code and caic state.
// Keep auth.Store lookups, consent HTML, API DTOs, and audit formatting here so
// internal/oauth remains reusable and unaware of caic packages.

// caicSessionManager implements oauth.SessionManager backed by auth.Store.
type caicSessionManager struct {
	store *auth.Store
}

func (m *caicSessionManager) CurrentUser(ctx context.Context) (oauth.User, bool) {
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		return oauth.User{}, false
	}
	return oauth.User{ID: u.ID, Username: u.Username, Provider: string(u.Provider)}, true
}

func (m *caicSessionManager) AttachUser(ctx context.Context, u oauth.User) context.Context {
	user, ok := m.store.FindByID(u.ID)
	if !ok {
		return ctx
	}
	return auth.NewContext(ctx, &user)
}

func (m *caicSessionManager) FindUser(id string) (oauth.User, bool) {
	user, ok := m.store.FindByID(id)
	if !ok {
		return oauth.User{}, false
	}
	return oauth.User{ID: user.ID, Username: user.Username, Provider: string(user.Provider)}, true
}

// EndSession clears cookies for the user. The browser redirects to "/" after logout.
func (m *caicSessionManager) EndSession(ctx context.Context, r *http.Request, u oauth.User) (redirectURL string) {
	return ""
}

// caicAuthorizationUI implements oauth.AuthorizationUI.
type caicAuthorizationUI struct {
	login *authHandlers
}

func (u caicAuthorizationUI) LoginStartURL(r *http.Request) string {
	return u.login.LoginStartURL(r)
}

func (u caicAuthorizationUI) ProviderLabel(p string) string {
	return u.login.ProviderLabel(p)
}

// RenderOAuthConsent renders the caic OAuth consent page.
func (caicAuthorizationUI) RenderOAuthConsent(w http.ResponseWriter, data *oauth.ConsentPageData) error {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	return oauthConsentTemplate.Execute(w, data)
}

// RecordOAuth records an OAuth audit event.
func (a *auditStore) RecordOAuth(ctx context.Context, userID, operation, name, decision, status string, args any) {
	a.record(ctx, &auditEvent{
		UserID:    userID,
		Operation: operation,
		Name:      name,
		Args:      auditValueSummary(args),
		Decision:  decision,
		Status:    status,
	})
}

type oauthGrantHandlers struct {
	oauthServer *oauth.Server
}

// listOAuthGrants returns the authenticated user's OAuth client grants.
func (h *oauthGrantHandlers) listOAuthGrants(ctx context.Context, _ *api.EmptyReq) (*v1.OAuthGrantsResp, error) {
	now := time.Now()
	grants := h.oauthServer.ListUserGrants(userIDFromCtx(ctx))
	resp := make([]v1.OAuthGrantResp, len(grants))
	for i := range grants {
		resp[i] = oauthGrantResponse(&grants[i], now)
	}
	return &v1.OAuthGrantsResp{Grants: resp}, nil
}

// revokeOAuthGrant revokes one OAuth client grant for the authenticated user.
func (h *oauthGrantHandlers) revokeOAuthGrant(ctx context.Context, req *v1.RevokeOAuthGrantReq) (*v1.StatusResp, error) {
	revoked, err := h.oauthServer.RevokeUserGrant(userIDFromCtx(ctx), req.GrantID)
	if err != nil {
		return nil, api.InternalError("save OAuth grant revocation: " + err.Error())
	}
	if !revoked {
		return nil, api.NotFound("OAuth grant")
	}
	return &v1.StatusResp{Status: "ok"}, nil
}

// oauthGrantResponse converts an internal oauth.Grant to an API response.
func oauthGrantResponse(grant *oauth.Grant, now time.Time) v1.OAuthGrantResp {
	status := v1.OAuthGrantStatusActive
	if !grant.RevokedAt.IsZero() {
		status = v1.OAuthGrantStatusRevoked
	} else if now.After(grant.ExpiresAt) {
		status = v1.OAuthGrantStatusExpired
	}
	return v1.OAuthGrantResp{
		ID:         grant.ID,
		ClientID:   grant.ClientID,
		ClientName: grant.ClientName,
		Scopes:     strings.Fields(grant.Scope),
		Resource:   grant.Resource,
		CreatedAt:  grant.CreatedAt,
		LastUsedAt: grant.LastUsedAt,
		ExpiresAt:  grant.ExpiresAt,
		RevokedAt:  grant.RevokedAt,
		Status:     status,
	}
}

// oauthGrantRoutes returns the handler for OAuth client grant management.
// Patterns are relative to the API version prefix, stripped at mount time.
func oauthGrantRoutes(s *oauth.Server) http.Handler {
	h := &oauthGrantHandlers{oauthServer: s}
	m := http.NewServeMux()
	m.HandleFunc("GET /oauth/grants", handle(h.listOAuthGrants))
	m.HandleFunc("POST /oauth/grants/{grantID}/revoke", handle(h.revokeOAuthGrant))
	return m
}
