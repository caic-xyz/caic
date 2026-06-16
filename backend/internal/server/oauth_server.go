// OAuth authorization server: authorization code grant with PKCE (RFC 7636),
// authorization server metadata (RFC 8414), protected resource metadata
// (RFC 9728), JWKS (RFC 7517), dynamic client registration (RFC 7591), token
// revocation (RFC 7009), OpenID Connect Discovery, and a consent page.

package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/oauth"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

const (
	oauthAccessTokenTTL  = time.Hour
	oauthAuthCodeTTL     = 10 * time.Minute
	oauthRefreshTokenTTL = 30 * 24 * time.Hour
)

//go:embed oauth_consent.html
var oauthConsentHTML string

var oauthConsentTemplate = template.Must(template.New("oauth-consent").Parse(oauthConsentHTML))

// oauthConsentTemplateData is the server-rendered OAuth consent page model.
type oauthConsentTemplateData struct {
	Action        string
	ConsentToken  string
	ClientName    string
	ClientID      string
	RedirectURI   string
	Username      string
	UserInitial   string
	ProviderLabel string
	Resource      string
	ScopeItems    []oauthScopeItem
}

// oauthScopeItem holds a scope identifier and its human-readable label.
type oauthScopeItem struct {
	ID      string
	Label   string
	Checked bool
}

// oauthServer stores OAuth state for remote clients.
//
// Clients use Authorization Code with PKCE S256. Authorization always requires
// a web session, so an unauthenticated browser request is sent through login
// before consent is shown. Access tokens are resource-scoped JWTs. Refresh
// tokens are resource-scoped opaque tokens, persisted as hashes, and rotated on
// every refresh grant. Dynamic client registrations and user grants are durable
// so clients survive restarts and users can revoke individual client grants
// from settings.
type oauthServer struct {
	mu       sync.Mutex
	state    *oauth.Store
	codes    map[string]oauth.Code
	consents map[string]oauthConsent
	tokens   *oauth.AccessTokenService

	supportedScopes []string // canonical ordered scope list.
	defaultScopes   []string // pre-checked on the consent form.

	scopeLabels map[string]string // human-readable labels for supported scopes.

	resourceURLPath         string
	resourceMetadataURLPath string
	clientIDPrefix          string

	// Shared dependency seams (not owned).
	userLookup  oauthUserLookup
	currentUser oauthCurrentUserFunc
	attachUser  oauthAttachUserFunc
	baseURL     oauthBaseURLFunc
	login       oauthLoginAdapter
	audit       oauthAuditRecorder
	rateLimiter oauthRateLimiter
}

// oauthConsent holds an in-progress user consent session.
type oauthConsent struct {
	UserID    string
	Values    url.Values
	ExpiresAt time.Time
}

type oauthBaseURLFunc func(*http.Request) string

type oauthCurrentUserFunc func(context.Context) (*auth.User, bool)

type oauthAttachUserFunc func(context.Context, *auth.User) context.Context

type oauthUserLookup interface {
	FindByID(id string) (auth.User, bool)
}

type oauthLoginAdapter interface {
	LoginStartURL(r *http.Request) string
	ProviderLabel(provider string) string
}

type oauthAuditRecorder interface {
	RecordOAuth(ctx context.Context, userID, operation, name, decision, status string, args any)
}

type oauthRateLimiter interface {
	Allow(key string) bool
}

func userInitial(username string) string {
	for _, r := range strings.TrimSpace(username) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func newOAuthServer(keyPEM []byte, kid, refreshTokenStorePath, resourceURLPath, resourceMetadataURLPath, clientIDPrefix string, supportedScopes, defaultScopes []string, scopeLabels map[string]string, userLookup oauthUserLookup, baseURL oauthBaseURLFunc, currentUser oauthCurrentUserFunc, attachUser oauthAttachUserFunc, login oauthLoginAdapter, audit oauthAuditRecorder, rateLimiter oauthRateLimiter) (*oauthServer, error) {
	tokens, err := oauth.NewAccessTokenService(keyPEM, kid, oauthAccessTokenTTL)
	if err != nil {
		return nil, err
	}
	state, err := oauth.LoadStore(refreshTokenStorePath)
	if err != nil {
		return nil, err
	}
	return &oauthServer{
		state:                   state,
		codes:                   map[string]oauth.Code{},
		consents:                map[string]oauthConsent{},
		tokens:                  tokens,
		supportedScopes:         supportedScopes,
		defaultScopes:           defaultScopes,
		userLookup:              userLookup,
		currentUser:             currentUser,
		attachUser:              attachUser,
		baseURL:                 baseURL,
		login:                   login,
		scopeLabels:             scopeLabels,
		audit:                   audit,
		rateLimiter:             rateLimiter,
		resourceURLPath:         resourceURLPath,
		resourceMetadataURLPath: resourceMetadataURLPath,
		clientIDPrefix:          clientIDPrefix,
	}, nil
}

// SetRefreshTokenStorePath updates the path and reloads persisted OAuth state
// (clients, refresh tokens, grants) from the new location.
func (s *oauthServer) SetRefreshTokenStorePath(path string) error {
	state, err := oauth.LoadStore(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	return nil
}

// bearerClaims holds the verified bearer token claims and the caic auth user
// adapted from them for request context propagation.
type bearerClaims struct {
	oauth.BearerClaims

	authUser auth.User
}

type bearerClaimsContextKey struct{}

func newBearerClaimsContext(ctx context.Context, claims *bearerClaims) context.Context {
	return context.WithValue(ctx, bearerClaimsContextKey{}, claims)
}

func bearerClaimsFromContext(ctx context.Context) (*bearerClaims, bool) {
	claims, ok := ctx.Value(bearerClaimsContextKey{}).(*bearerClaims)
	return claims, ok && claims != nil
}

// BearerAuth is an HTTP middleware that verifies OAuth bearer tokens. On success
// the authenticated user and bearer claims are set in the request context.
// Requests without a bearer token are rejected with 401 unless the request
// already carries a session user.
func (s *oauthServer) BearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := oauth.BearerToken(r)
		if token == "" {
			if _, ok := s.currentRequestUser(r.Context()); ok {
				next.ServeHTTP(w, r)
				return
			}
			s.writeUnauthorized(w, r)
			return
		}
		claims, err := s.verifyBearer(r, token)
		if err != nil {
			s.writeUnauthorized(w, r)
			return
		}
		authUser := claims.authUser
		ctx := s.newUserContext(r.Context(), &authUser)
		ctx = newBearerClaimsContext(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *oauthServer) currentRequestUser(ctx context.Context) (*auth.User, bool) {
	if s.currentUser == nil {
		return nil, false
	}
	return s.currentUser(ctx)
}

func (s *oauthServer) newUserContext(ctx context.Context, u *auth.User) context.Context {
	if s.attachUser == nil {
		return ctx
	}
	return s.attachUser(ctx, u)
}

func (s *oauthServer) findUserByID(id string) (auth.User, bool) {
	if s.userLookup == nil {
		return auth.User{}, false
	}
	return s.userLookup.FindByID(id)
}

func (s *oauthServer) providerLabel(provider string) string {
	if s.login != nil {
		return s.login.ProviderLabel(provider)
	}
	if provider == "" {
		return "unknown provider"
	}
	return provider
}

func (s *oauthServer) scopeItems(scope string) []oauthScopeItem {
	parts := strings.Fields(scope)
	if len(parts) == 0 {
		parts = []string{s.supportedScopes[0]}
	}
	items := make([]oauthScopeItem, 0, len(parts))
	for _, p := range parts {
		label := s.scopeLabels[p]
		if label == "" {
			label = p
		}
		items = append(items, oauthScopeItem{ID: p, Label: label, Checked: slices.Contains(s.defaultScopes, p)})
	}
	return items
}

func (s *oauthServer) externalBaseURL(r *http.Request) string {
	if s.baseURL != nil {
		return s.baseURL(r)
	}
	authority, scheme := effectiveRequestHostAndScheme(r)
	return scheme + "://" + authority
}

func (s *oauthServer) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.rateLimiter == nil {
		return true
	}
	key := s.rateKey(r)
	if s.rateLimiter.Allow(key) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func (s *oauthServer) rateKey(r *http.Request) string {
	host := r.RemoteAddr
	return "ip:" + host
}

// registerWellKnownRoutes registers OAuth authorization-server and
// protected-resource metadata endpoints on mux under /.well-known/.
func (s *oauthServer) registerWellKnownRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleOAuthMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOAuthMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/", s.handleProtectedResourceMetadata)
}

// routes returns an http.Handler with OAuth authorization-server endpoints
// under /oauth/. Clients discover these via the metadata served by
// registerWellKnownRoutes, so the paths can change without breaking clients.
func (s *oauthServer) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /oauth/jwks", s.handleOAuthJWKS)
	m.HandleFunc("POST /oauth/register", s.handleOAuthRegister)
	m.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorizeGET)
	m.HandleFunc("POST /oauth/authorize", s.handleOAuthAuthorizePOST)
	m.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	m.HandleFunc("POST /oauth/revoke", s.handleOAuthRevoke)
	return m
}

func (s *oauthServer) handleOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	issuer := s.externalBaseURL(r)
	metadata := oauth.AuthorizationServerMetadata{
		Issuer:                                 issuer,
		AuthorizationEndpoint:                  issuer + "/oauth/authorize",
		TokenEndpoint:                          issuer + "/oauth/token",
		JWKSURI:                                issuer + "/oauth/jwks",
		RegistrationEndpoint:                   issuer + "/oauth/register",
		RevocationEndpoint:                     issuer + "/oauth/revoke",
		ResponseTypesSupported:                 []string{oauth.ResponseTypeCode},
		GrantTypesSupported:                    []string{oauth.GrantAuthorizationCode, oauth.GrantRefreshToken},
		CodeChallengeMethodsSupported:          []string{oauth.CodeChallengeS256},
		TokenEndpointAuthMethodsSupported:      []string{oauth.TokenEndpointAuthNone},
		RevocationEndpointAuthMethodsSupported: []string{oauth.TokenEndpointAuthNone},
		ScopesSupported:                        s.supportedScopes,
		AuthorizationResponseIssuerParameterSupported: true,
	}
	writeJSONResponse(w, &metadata, nil)
}

func (s *oauthServer) handleOAuthJWKS(w http.ResponseWriter, r *http.Request) {
	resp := oauth.JWKSet{Keys: []oauth.JWK{s.tokens.JWK()}}
	writeJSONResponse(w, &resp, nil)
}

func (s *oauthServer) handleOAuthAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		if s.login != nil {
			if loginURL := s.login.LoginStartURL(r); loginURL != "" {
				http.Redirect(w, r, loginURL, http.StatusFound)
				return
			}
		}
		oauth.WriteError(w, http.StatusUnauthorized, "login_required", "log in before authorizing protected resource access")
		return
	}
	if err := s.validateAuthorizeRequest(r); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	values := cloneURLValues(r.URL.Query())
	client := s.oauthClient(values.Get("client_id"))
	scope, _ := s.normalizeScope(values.Get("scope"))
	consentToken, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth consent token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not start consent")
		return
	}
	s.mu.Lock()
	s.consents[consentToken] = oauthConsent{UserID: user.ID, Values: values, ExpiresAt: time.Now().Add(oauthAuthCodeTTL)}
	s.mu.Unlock()
	baseURL := s.externalBaseURL(r)
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	data := oauthConsentTemplateData{
		Action:        baseURL + "/oauth/authorize",
		ConsentToken:  consentToken,
		ClientName:    clientDisplayName(&client),
		ClientID:      client.ID,
		RedirectURI:   values.Get("redirect_uri"),
		Username:      user.Username,
		UserInitial:   userInitial(user.Username),
		ProviderLabel: s.providerLabel(string(user.Provider)),
		Resource:      values.Get("resource"),
		ScopeItems:    s.scopeItems(scope),
	}
	if err := oauthConsentTemplate.Execute(w, data); err != nil {
		slog.WarnContext(r.Context(), "render oauth consent", "err", err)
	}
}

func (s *oauthServer) handleOAuthAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		oauth.WriteError(w, http.StatusUnauthorized, "login_required", "log in before authorizing protected resource access")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	consentToken := r.PostForm.Get("consent_token")
	s.mu.Lock()
	consent, ok := s.consents[consentToken]
	if ok {
		delete(s.consents, consentToken)
	}
	s.mu.Unlock()
	if !ok || consent.UserID != user.ID || time.Now().After(consent.ExpiresAt) {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid or expired consent")
		return
	}
	values := consent.Values
	if err := s.validateAuthorizeForm(r, values); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	switch r.PostForm.Get("decision") {
	case "approve", "":
	case "deny":
		s.recordAudit(r, "", "oauth/authorize", values.Get("client_id"), "deny", "denied", map[string]any{
			"redirectURI": values.Get("redirect_uri"),
			"resource":    values.Get("resource"),
			"scope":       values.Get("scope"),
		})
		s.redirectAuthorizeError(w, r, values, "access_denied", "authorization denied")
		return
	default:
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid consent decision")
		return
	}
	code, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth code", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not authorize client")
		return
	}
	scope, err := s.approveScope(values.Get("scope"), r.PostForm)
	if err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	entry := oauth.Code{UserID: user.ID, ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), CodeChallenge: values.Get("code_challenge"), Resource: values.Get("resource"), Scope: scope, ExpiresAt: time.Now().Add(oauthAuthCodeTTL)}
	s.mu.Lock()
	s.codes[code] = entry
	s.mu.Unlock()
	redirectURL, err := url.Parse(entry.RedirectURI)
	if err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect URI")
		return
	}
	q := redirectURL.Query()
	q.Set("code", code)
	if state := values.Get("state"); state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.externalBaseURL(r))
	redirectURL.RawQuery = q.Encode()
	s.recordAudit(r, "", "oauth/authorize", entry.ClientID, "allow", "approved", map[string]any{
		"redirectURI": entry.RedirectURI,
		"resource":    entry.Resource,
		"scope":       entry.Scope,
	})
	http.Redirect(w, r, redirectURL.String(), http.StatusSeeOther)
}

func (s *oauthServer) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if !s.allowRequest(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if err := s.pruneExpiredRefreshTokens(time.Now()); err != nil {
		slog.WarnContext(r.Context(), "prune oauth refresh tokens", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not prune refresh tokens")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case oauth.GrantAuthorizationCode:
		s.handleOAuthAuthorizationCodeToken(w, r)
	case oauth.GrantRefreshToken:
		s.handleOAuthRefreshToken(w, r)
	default:
		oauth.WriteError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

func (s *oauthServer) handleOAuthAuthorizationCodeToken(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	s.mu.Lock()
	entry, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	s.mu.Unlock()
	if !ok || time.Now().After(entry.ExpiresAt) {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
		return
	}
	if r.PostForm.Get("client_id") != entry.ClientID || r.PostForm.Get("redirect_uri") != entry.RedirectURI {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "client or redirect URI mismatch")
		return
	}
	if resource := r.PostForm.Get("resource"); resource != "" && resource != entry.Resource {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
		return
	}
	if !oauth.VerifyPKCES256(entry.CodeChallenge, r.PostForm.Get("code_verifier")) {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	user, ok := s.findUserByID(entry.UserID)
	if !ok {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	client := s.oauthClient(entry.ClientID)
	grantID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth grant id", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	now := time.Now()
	grant := oauth.Grant{ID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, ClientName: clientDisplayName(&client), Resource: entry.Resource, Scope: entry.Scope, CreatedAt: now, ExpiresAt: now.Add(oauthRefreshTokenTTL)}
	refreshEntry := oauth.RefreshToken{GrantID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: grant.ExpiresAt}
	refreshToken, err := s.issueGrantRefreshToken(&grant, &refreshEntry)
	if err != nil {
		slog.WarnContext(r.Context(), "issue oauth refresh token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	s.recordAudit(r, entry.UserID, "oauth/token", entry.ClientID, "allow", "issued", map[string]any{
		"grantID":  grantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeTokenResponse(w, r, &user, entry.Resource, entry.Scope, grantID, refreshToken)
}

func (s *oauthServer) handleOAuthRefreshToken(w http.ResponseWriter, r *http.Request) {
	refreshToken := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")
	entry, ok := s.validRefreshToken(refreshToken, clientID)
	if !ok {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	user, ok := s.findUserByID(entry.UserID)
	if !ok {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	nextRefreshToken, entry, ok, err := s.rotateRefreshToken(refreshToken, clientID, entry.UserID)
	if err != nil {
		slog.WarnContext(r.Context(), "rotate oauth refresh token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not rotate refresh token")
		return
	}
	if !ok {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	s.recordAudit(r, entry.UserID, "oauth/token", clientID, "allow", "refreshed", map[string]any{
		"grantID":  entry.GrantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeTokenResponse(w, r, &user, entry.Resource, entry.Scope, entry.GrantID, nextRefreshToken)
}

func (s *oauthServer) writeTokenResponse(w http.ResponseWriter, r *http.Request, user *auth.User, resource, scope, grantID, refreshToken string) {
	accessToken, err := s.issueAccessToken(s.externalBaseURL(r), user, resource, scope, grantID)
	if err != nil {
		slog.WarnContext(r.Context(), "issue oauth token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	resp := oauth.TokenResponse{AccessToken: accessToken, TokenType: oauth.TokenTypeBearer, ExpiresIn: int64(oauthAccessTokenTTL.Seconds()), RefreshToken: refreshToken, Scope: scope}
	writeJSONResponse(w, &resp, nil)
}

func (s *oauthServer) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.allowRequest(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	userID, err := s.revokeRefreshToken(r.PostForm.Get("token"), r.PostForm.Get("client_id"))
	if err != nil {
		slog.WarnContext(r.Context(), "revoke oauth refresh token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not revoke refresh token")
		return
	}
	s.recordAudit(r, userID, "oauth/revoke", r.PostForm.Get("client_id"), "allow", "revoked", nil)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func (s *oauthServer) recordAudit(r *http.Request, userID, operation, name, decision, status string, args any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordOAuth(r.Context(), userID, operation, name, decision, status, args)
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

func (s *oauthServer) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, values url.Values, code, description string) {
	redirectURL, err := url.Parse(values.Get("redirect_uri"))
	if err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect URI")
		return
	}
	q := redirectURL.Query()
	q.Set("error", code)
	if description != "" {
		q.Set("error_description", description)
	}
	if state := values.Get("state"); state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.externalBaseURL(r))
	redirectURL.RawQuery = q.Encode()
	http.Redirect(w, r, redirectURL.String(), http.StatusSeeOther)
}

func (s *oauthServer) validateAuthorizeRequest(r *http.Request) error {
	return s.validateAuthorizeForm(r, r.URL.Query())
}

func (s *oauthServer) validateAuthorizeForm(r *http.Request, values url.Values) error {
	if values.Get("response_type") != oauth.ResponseTypeCode {
		return errors.New("response_type must be code")
	}
	client := s.oauthClient(values.Get("client_id"))
	if client.ID == "" {
		return errors.New("unknown client_id")
	}
	redirectURI := values.Get("redirect_uri")
	if !slices.Contains(client.RedirectURIs, redirectURI) {
		return errors.New("redirect_uri is not registered")
	}
	if values.Get("code_challenge_method") != oauth.CodeChallengeS256 || values.Get("code_challenge") == "" {
		return errors.New("S256 PKCE is required")
	}
	resource := values.Get("resource")
	if resource == "" {
		return errors.New("resource is required")
	}
	if resource != s.externalBaseURL(r)+s.resourceURLPath {
		return errors.New("resource must match the protected resource")
	}
	if _, err := s.normalizeScope(values.Get("scope")); err != nil {
		return err
	}
	return nil
}

func (s *oauthServer) issueGrantRefreshToken(grant *oauth.Grant, entry *oauth.RefreshToken) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Grants[grant.ID] = *grant
	s.state.RefreshTokens[oauth.RefreshTokenKey(token)] = *entry
	if err := s.state.Save(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *oauthServer) validRefreshToken(token, clientID string) (oauth.RefreshToken, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.state.RefreshTokens[oauth.RefreshTokenKey(token)]
	if !ok || entry.ClientID != clientID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return oauth.RefreshToken{}, false
	}
	grant, ok := s.state.Grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return oauth.RefreshToken{}, false
	}
	return entry, true
}

func (s *oauthServer) rotateRefreshToken(token, clientID, userID string) (nextToken string, next oauth.RefreshToken, ok bool, err error) {
	nextToken, err = randomToken()
	if err != nil {
		return "", oauth.RefreshToken{}, false, err
	}
	now := time.Now()
	tokenHash := oauth.RefreshTokenKey(token)
	nextTokenHash := oauth.RefreshTokenKey(nextToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.state.RefreshTokens[tokenHash]
	if !ok || entry.ClientID != clientID || entry.UserID != userID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return "", oauth.RefreshToken{}, false, nil
	}
	grant, ok := s.state.Grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return "", oauth.RefreshToken{}, false, nil
	}
	entry.UsedAt = now
	s.state.RefreshTokens[tokenHash] = entry
	nextExpiry := now.Add(oauthRefreshTokenTTL)
	next = oauth.RefreshToken{GrantID: entry.GrantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: nextExpiry}
	s.state.RefreshTokens[nextTokenHash] = next
	grant.LastUsedAt = now
	grant.ExpiresAt = nextExpiry
	s.state.Grants[grant.ID] = grant
	if err := s.state.Save(); err != nil {
		return "", oauth.RefreshToken{}, false, err
	}
	return nextToken, next, true, nil
}

func (s *oauthServer) revokeRefreshToken(token, clientID string) (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tokenHash := oauth.RefreshTokenKey(token)
	entry, ok := s.state.RefreshTokens[tokenHash]
	if !ok || entry.ClientID != clientID || !entry.RevokedAt.IsZero() {
		return "", nil
	}
	entry.RevokedAt = now
	s.state.RefreshTokens[tokenHash] = entry
	if grant, ok := s.state.Grants[entry.GrantID]; ok && grant.RevokedAt.IsZero() {
		grant.RevokedAt = now
		s.state.Grants[entry.GrantID] = grant
		for otherHash := range s.state.RefreshTokens {
			other := s.state.RefreshTokens[otherHash]
			if other.GrantID == entry.GrantID && other.RevokedAt.IsZero() {
				other.RevokedAt = now
				s.state.RefreshTokens[otherHash] = other
			}
		}
	}
	return entry.UserID, s.state.Save()
}

func (s *oauthServer) revokeUserGrant(userID, grantID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.RevokeUserGrant(userID, grantID, time.Now())
}

func (s *oauthServer) listUserGrants(userID string) []oauth.Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.ListUserGrants(userID)
}

func (s *oauthServer) touchGrant(grantID string, now time.Time) (bool, error) {
	if grantID == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.state.Grants[grantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return false, nil
	}
	grant.LastUsedAt = now
	s.state.Grants[grantID] = grant
	if err := s.state.Save(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *oauthServer) pruneExpiredRefreshTokens(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.PruneExpiredRefreshTokens(now) {
		return nil
	}
	return s.state.Save()
}

func (s *oauthServer) issueAccessToken(issuer string, user *auth.User, audience, scope, grantID string) (string, error) {
	return s.tokens.IssueAccessToken(issuer, oauth.User{ID: user.ID, Username: user.Username, Provider: string(user.Provider)}, audience, scope, grantID)
}

func (s *oauthServer) normalizeScope(scope string) (string, error) {
	parts := strings.Fields(scope)
	if len(parts) == 0 {
		return s.supportedScopes[0], nil
	}
	for _, part := range parts {
		if !slices.Contains(s.supportedScopes, part) {
			return "", fmt.Errorf("unsupported scope: %s", part)
		}
	}
	return strings.Join(parts, " "), nil
}

func (s *oauthServer) approveScope(requested string, form url.Values) (string, error) {
	normalizedRequested, err := s.normalizeScope(requested)
	if err != nil {
		return "", err
	}
	if form.Get("scope_form") == "" {
		return normalizedRequested, nil
	}
	allowed := parseScopeSet(normalizedRequested)
	selected := make(map[string]struct{}, len(form["scope"]))
	for _, scope := range form["scope"] {
		if !slices.Contains(s.supportedScopes, scope) {
			return "", fmt.Errorf("unsupported scope: %s", scope)
		}
		if !slices.Contains(allowed, scope) {
			return "", fmt.Errorf("unrequested scope: %s", scope)
		}
		selected[scope] = struct{}{}
	}
	if len(selected) == 0 {
		return "", errors.New("select at least one scope")
	}
	ordered := s.supportedScopes
	approved := make([]string, 0, len(selected))
	for _, scope := range ordered {
		if _, ok := selected[scope]; ok {
			approved = append(approved, scope)
		}
	}
	return strings.Join(approved, " "), nil
}

func parseScopeSet(scope string) []string {
	return strings.Fields(scope)
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func cloneURLValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, entries := range values {
		clone[key] = append([]string(nil), entries...)
	}
	return clone
}

// handleProtectedResourceMetadata writes protected-resource metadata (RFC 9728).
func (s *oauthServer) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/.well-known/oauth-protected-resource" && path != s.resourceMetadataURLPath {
		http.NotFound(w, r)
		return
	}
	metadata := oauth.ProtectedResourceMetadata{
		Resource:             s.externalBaseURL(r) + s.resourceURLPath,
		AuthorizationServers: []string{s.externalBaseURL(r)},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		http.Error(w, "encode metadata", http.StatusInternalServerError)
	}
}

// verifyBearer validates a JWT bearer token against oauthServer's token service
// and grant store.
func (s *oauthServer) verifyBearer(r *http.Request, token string) (*bearerClaims, error) {
	var authUser auth.User
	claims, err := s.tokens.VerifyAccessToken(token, s.externalBaseURL(r), s.externalBaseURL(r)+s.resourceURLPath, time.Now(), s.touchGrant, func(subject string) (oauth.User, bool) {
		user, ok := s.findUserByID(subject)
		if !ok {
			return oauth.User{}, false
		}
		authUser = user
		return oauth.User{ID: user.ID, Username: user.Username, Provider: string(user.Provider)}, true
	})
	if err != nil {
		return nil, err
	}
	return &bearerClaims{BearerClaims: *claims, authUser: authUser}, nil
}

// writeUnauthorized writes a 401 with the bearer challenge so clients can
// discover the authorization server.
func (s *oauthServer) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == s.resourceURLPath && s.resourceMetadataURLPath != "" {
		resourceMetadataURL := s.externalBaseURL(r) + s.resourceMetadataURLPath
		w.Header().Set("WWW-Authenticate", oauth.BearerChallenge(resourceMetadataURL, strings.Join(s.supportedScopes, " ")))
	}
	writeUnauthorizedJSON(w)
}

// listOAuthGrants returns the authenticated user's OAuth client grants.
func (s *oauthServer) listOAuthGrants(ctx context.Context, _ *api.EmptyReq) (*v1.OAuthGrantsResp, error) {
	now := time.Now()
	grants := s.listUserGrants(userIDFromCtx(ctx))
	resp := make([]v1.OAuthGrantResp, len(grants))
	for i := range grants {
		resp[i] = oauthGrantResponse(&grants[i], now)
	}
	return &v1.OAuthGrantsResp{Grants: resp}, nil
}

// revokeOAuthGrant revokes one OAuth client grant for the authenticated user.
func (s *oauthServer) revokeOAuthGrant(ctx context.Context, req *v1.RevokeOAuthGrantReq) (*v1.StatusResp, error) {
	if !s.revokeUserGrant(userIDFromCtx(ctx), req.GrantID) {
		return nil, api.NotFound("OAuth grant")
	}
	if err := s.state.Save(); err != nil {
		return nil, api.InternalError("save OAuth grant revocation: " + err.Error())
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

// grantRoutes returns the handler for OAuth client grant management. Patterns
// are relative to the API version prefix, stripped at mount time.
func (s *oauthServer) grantRoutes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /oauth/grants", handle(s.listOAuthGrants))
	m.HandleFunc("POST /oauth/grants/{grantID}/revoke", handle(s.revokeOAuthGrant))
	return m
}
