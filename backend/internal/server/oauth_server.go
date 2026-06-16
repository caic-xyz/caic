// OAuth authorization server: authorization code grant with PKCE (RFC 7636),
// authorization server metadata (RFC 8414), protected resource metadata
// (RFC 9728), JWKS (RFC 7517), dynamic client registration (RFC 7591), token
// revocation (RFC 7009), OpenID Connect Discovery, and a consent page.

package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	mu                    sync.Mutex
	clients               map[string]oauthClient
	codes                 map[string]oauthCode
	consents              map[string]oauthConsent
	refreshTokens         map[string]oauthRefreshToken
	grants                map[string]oauthGrant
	refreshTokenStorePath string
	key                   *rsa.PrivateKey
	kid                   string

	supportedScopes []string // canonical ordered scope list.
	defaultScopes   []string // pre-checked on the consent form.

	scopeLabels map[string]string // human-readable labels for supported scopes.

	resourceURLPath         string
	resourceMetadataURLPath string
	clientIDPrefix          string

	// Shared dependencies (not owned).
	authStore    *auth.Store
	hostState    *auth.HostState
	authHandlers *authHandlers
	audit        *auditStore
	rateLimiter  *rateLimiter
}

// oauthConsent holds an in-progress user consent session.
type oauthConsent struct {
	UserID    string
	Values    url.Values
	ExpiresAt time.Time
}

// oauthCode is an issued authorization code with PKCE binding.
type oauthCode struct {
	UserID        string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	Scope         string
	ExpiresAt     time.Time
}

// oauthRefreshToken is an opaque refresh token persisted by hash.
type oauthRefreshToken struct {
	GrantID   string    `json:"grantID,omitempty"`
	UserID    string    `json:"userID"`
	ClientID  string    `json:"clientID"`
	Resource  string    `json:"resource"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedAt    time.Time `json:"usedAt,omitzero"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// oauthGrant ties a user authorization grant to a client and token.
type oauthGrant struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userID"`
	ClientID   string    `json:"clientID"`
	ClientName string    `json:"clientName"`
	Resource   string    `json:"resource"`
	Scope      string    `json:"scope"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitzero"`
	ExpiresAt  time.Time `json:"expiresAt"`
	RevokedAt  time.Time `json:"revokedAt,omitzero"`
}

// oauthRefreshTokenFile is the on-disk format for durable OAuth state.
type oauthRefreshTokenFile struct {
	Version int                       `json:"version"`
	Clients []oauthClient             `json:"clients,omitempty"`
	Tokens  []oauthRefreshTokenRecord `json:"tokens"`
	Grants  []oauthGrant              `json:"grants,omitempty"`
}

// oauthRefreshTokenRecord pairs a persisted refresh token with its hash key.
type oauthRefreshTokenRecord struct {
	oauthRefreshToken

	TokenHash string `json:"tokenHash"`
}

func userInitial(username string) string {
	for _, r := range strings.TrimSpace(username) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func newOAuthServer(keyPEM []byte, kid, refreshTokenStorePath, resourceURLPath, resourceMetadataURLPath, clientIDPrefix string, supportedScopes, defaultScopes []string, scopeLabels map[string]string, authStore *auth.Store, hostState *auth.HostState, authHandlers *authHandlers, audit *auditStore, rateLimiter *rateLimiter) (*oauthServer, error) {
	var key *rsa.PrivateKey
	if len(keyPEM) > 0 {
		block, _ := pem.Decode(keyPEM)
		if block == nil {
			return nil, errors.New("decode oauth signing key PEM")
		}
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse oauth signing key: %w", err)
		}
		key = parsed
	} else {
		generated, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("generate oauth signing key: %w", err)
		}
		key = generated
	}
	if kid == "" {
		generatedKID, err := randomToken()
		if err != nil {
			return nil, fmt.Errorf("generate oauth key id: %w", err)
		}
		kid = generatedKID
	}
	clients, refreshTokens, grants, err := loadOAuthRefreshTokens(refreshTokenStorePath)
	if err != nil {
		return nil, err
	}
	return &oauthServer{
		clients:                 clients,
		codes:                   map[string]oauthCode{},
		consents:                map[string]oauthConsent{},
		refreshTokens:           refreshTokens,
		grants:                  grants,
		refreshTokenStorePath:   refreshTokenStorePath,
		key:                     key,
		kid:                     kid,
		supportedScopes:         supportedScopes,
		defaultScopes:           defaultScopes,
		authStore:               authStore,
		hostState:               hostState,
		authHandlers:            authHandlers,
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
	clients, tokens, grants, err := loadOAuthRefreshTokens(path)
	if err != nil {
		return err
	}
	s.refreshTokenStorePath = path
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients = clients
	s.refreshTokens = tokens
	s.grants = grants
	return nil
}

// bearerClaims holds the verified bearer token claims.
type bearerClaims struct {
	User     auth.User
	Subject  string
	Username string
	Issuer   string
	Audience string
	Scopes   []string
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
			if _, ok := auth.UserFromContext(r.Context()); ok {
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
		ctx := auth.NewContext(r.Context(), &claims.User)
		ctx = newBearerClaimsContext(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
	if s.hostState != nil {
		if externalURL := s.hostState.ExternalURL(); externalURL != "" {
			return strings.TrimRight(externalURL, "/")
		}
	}
	authority, scheme := effectiveRequestHostAndScheme(r)
	return scheme + "://" + authority
}

func (s *oauthServer) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.rateLimiter == nil {
		return true
	}
	key := s.rateKey(r)
	if s.rateLimiter.allow(key) {
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
	pub := s.key.PublicKey
	resp := oauth.JWKSet{Keys: []oauth.JWK{oauth.RSAJWK(s.kid, &pub)}}
	writeJSONResponse(w, &resp, nil)
}

func (s *oauthServer) handleOAuthAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		if s.authHandlers != nil {
			if loginURL := s.authHandlers.loginStartURL(r); loginURL != "" {
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
		ProviderLabel: s.authHandlers.providerLabel(string(user.Provider)),
		Resource:      values.Get("resource"),
		ScopeItems:    s.scopeItems(scope),
	}
	if err := oauthConsentTemplate.Execute(w, data); err != nil {
		slog.WarnContext(r.Context(), "render oauth consent", "err", err)
	}
}

func (s *oauthServer) handleOAuthAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.UserFromContext(r.Context())
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
	entry := oauthCode{UserID: user.ID, ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), CodeChallenge: values.Get("code_challenge"), Resource: values.Get("resource"), Scope: scope, ExpiresAt: time.Now().Add(oauthAuthCodeTTL)}
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
	user, ok := s.authStore.FindByID(entry.UserID)
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
	grant := oauthGrant{ID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, ClientName: clientDisplayName(&client), Resource: entry.Resource, Scope: entry.Scope, CreatedAt: now, ExpiresAt: now.Add(oauthRefreshTokenTTL)}
	refreshEntry := oauthRefreshToken{GrantID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: grant.ExpiresAt}
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
	user, ok := s.authStore.FindByID(entry.UserID)
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
	s.audit.record(r.Context(), &auditEvent{
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

func (s *oauthServer) issueGrantRefreshToken(grant *oauthGrant, entry *oauthRefreshToken) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[grant.ID] = *grant
	s.refreshTokens[oauthRefreshTokenKey(token)] = *entry
	if err := s.saveRefreshTokensLocked(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *oauthServer) validRefreshToken(token, clientID string) (oauthRefreshToken, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.refreshTokens[oauthRefreshTokenKey(token)]
	if !ok || entry.ClientID != clientID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return oauthRefreshToken{}, false
	}
	grant, ok := s.grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return oauthRefreshToken{}, false
	}
	return entry, true
}

func (s *oauthServer) rotateRefreshToken(token, clientID, userID string) (nextToken string, next oauthRefreshToken, ok bool, err error) {
	nextToken, err = randomToken()
	if err != nil {
		return "", oauthRefreshToken{}, false, err
	}
	now := time.Now()
	tokenHash := oauthRefreshTokenKey(token)
	nextTokenHash := oauthRefreshTokenKey(nextToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.refreshTokens[tokenHash]
	if !ok || entry.ClientID != clientID || entry.UserID != userID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return "", oauthRefreshToken{}, false, nil
	}
	grant, ok := s.grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return "", oauthRefreshToken{}, false, nil
	}
	entry.UsedAt = now
	s.refreshTokens[tokenHash] = entry
	nextExpiry := now.Add(oauthRefreshTokenTTL)
	next = oauthRefreshToken{GrantID: entry.GrantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: nextExpiry}
	s.refreshTokens[nextTokenHash] = next
	grant.LastUsedAt = now
	grant.ExpiresAt = nextExpiry
	s.grants[grant.ID] = grant
	if err := s.saveRefreshTokensLocked(); err != nil {
		return "", oauthRefreshToken{}, false, err
	}
	return nextToken, next, true, nil
}

func (s *oauthServer) revokeRefreshToken(token, clientID string) (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tokenHash := oauthRefreshTokenKey(token)
	entry, ok := s.refreshTokens[tokenHash]
	if !ok || entry.ClientID != clientID || !entry.RevokedAt.IsZero() {
		return "", nil
	}
	entry.RevokedAt = now
	s.refreshTokens[tokenHash] = entry
	if grant, ok := s.grants[entry.GrantID]; ok && grant.RevokedAt.IsZero() {
		grant.RevokedAt = now
		s.grants[entry.GrantID] = grant
		for otherHash := range s.refreshTokens {
			other := s.refreshTokens[otherHash]
			if other.GrantID == entry.GrantID && other.RevokedAt.IsZero() {
				other.RevokedAt = now
				s.refreshTokens[otherHash] = other
			}
		}
	}
	return entry.UserID, s.saveRefreshTokensLocked()
}

func (s *oauthServer) revokeUserGrant(userID, grantID string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[grantID]
	if !ok || grant.UserID != userID {
		return false
	}
	if grant.RevokedAt.IsZero() {
		grant.RevokedAt = now
		s.grants[grantID] = grant
		for tokenHash := range s.refreshTokens {
			entry := s.refreshTokens[tokenHash]
			if entry.GrantID == grantID && entry.RevokedAt.IsZero() {
				entry.RevokedAt = now
				s.refreshTokens[tokenHash] = entry
			}
		}
	}
	return true
}

func (s *oauthServer) listUserGrants(userID string) []oauthGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	grants := make([]oauthGrant, 0, len(s.grants))
	for id := range s.grants {
		grant := s.grants[id]
		if grant.UserID == userID {
			grants = append(grants, grant)
		}
	}
	slices.SortFunc(grants, func(a, b oauthGrant) int {
		if cmp := b.CreatedAt.Compare(a.CreatedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ID, b.ID)
	})
	return grants
}

func (s *oauthServer) touchGrant(grantID string, now time.Time) (bool, error) {
	if grantID == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[grantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return false, nil
	}
	grant.LastUsedAt = now
	s.grants[grantID] = grant
	if err := s.saveRefreshTokensLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *oauthServer) saveRefreshTokens() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveRefreshTokensLocked()
}

func (s *oauthServer) pruneExpiredRefreshTokens(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for token := range s.refreshTokens {
		if now.After(s.refreshTokens[token].ExpiresAt) {
			delete(s.refreshTokens, token)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveRefreshTokensLocked()
}

func (s *oauthServer) saveRefreshTokensLocked() error {
	if s.refreshTokenStorePath == "" {
		return nil
	}
	records := make([]oauthRefreshTokenRecord, 0, len(s.refreshTokens))
	for tokenHash := range s.refreshTokens {
		records = append(records, oauthRefreshTokenRecord{TokenHash: tokenHash, oauthRefreshToken: s.refreshTokens[tokenHash]})
	}
	slices.SortFunc(records, func(a, b oauthRefreshTokenRecord) int {
		return strings.Compare(a.TokenHash, b.TokenHash)
	})
	grants := slices.Collect(maps.Values(s.grants))
	slices.SortFunc(grants, func(a, b oauthGrant) int {
		return strings.Compare(a.ID, b.ID)
	})
	clients := slices.Collect(maps.Values(s.clients))
	slices.SortFunc(clients, func(a, b oauthClient) int {
		return strings.Compare(a.ID, b.ID)
	})
	return saveOAuthRefreshTokens(s.refreshTokenStorePath, clients, records, grants)
}

func (s *oauthServer) issueAccessToken(issuer string, user *auth.User, audience, scope, grantID string) (string, error) {
	now := time.Now()
	headerJSON, err := json.Marshal(map[string]string{"alg": oauth.JWTAlgRS256, "typ": "JWT", "kid": s.kid})
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"iss":      issuer,
		"sub":      user.ID,
		"aud":      audience,
		"username": user.Username,
		"scope":    scope,
		"grant_id": grantID,
		"iat":      now.Unix(),
		"nbf":      now.Unix(),
		"exp":      now.Add(oauthAccessTokenTTL).Unix(),
		"typ":      "access_token",
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
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

func loadOAuthRefreshTokens(path string) (clients map[string]oauthClient, refreshTokens map[string]oauthRefreshToken, grants map[string]oauthGrant, err error) {
	clients = map[string]oauthClient{}
	refreshTokens = map[string]oauthRefreshToken{}
	grants = map[string]oauthGrant{}
	if path == "" {
		return clients, refreshTokens, grants, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is app-controlled persistent state.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return clients, refreshTokens, grants, nil
		}
		return nil, nil, nil, fmt.Errorf("read oauth refresh tokens: %w", err)
	}
	var file oauthRefreshTokenFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, nil, fmt.Errorf("parse oauth refresh tokens: %w", err)
	}
	now := time.Now()
	for i := range file.Clients {
		client := file.Clients[i]
		if client.ID == "" {
			continue
		}
		clients[client.ID] = client
	}
	for i := range file.Grants {
		grant := file.Grants[i]
		if grant.ID == "" || now.After(grant.ExpiresAt) {
			continue
		}
		grants[grant.ID] = grant
	}
	for i := range file.Tokens {
		record := &file.Tokens[i]
		if record.TokenHash == "" || now.After(record.ExpiresAt) {
			continue
		}
		refreshTokens[record.TokenHash] = record.oauthRefreshToken
	}
	for tokenHash := range refreshTokens {
		token := refreshTokens[tokenHash]
		if token.GrantID == "" {
			grantID := "legacy:" + tokenHash
			token.GrantID = grantID
			refreshTokens[tokenHash] = token
			if _, ok := grants[grantID]; !ok {
				grants[grantID] = oauthGrant{ID: grantID, UserID: token.UserID, ClientID: token.ClientID, ClientName: token.ClientID, Resource: token.Resource, Scope: token.Scope, CreatedAt: token.ExpiresAt.Add(-oauthRefreshTokenTTL), ExpiresAt: token.ExpiresAt, RevokedAt: token.RevokedAt}
			}
		}
	}
	return clients, refreshTokens, grants, nil
}

func saveOAuthRefreshTokens(path string, clients []oauthClient, records []oauthRefreshTokenRecord, grants []oauthGrant) error {
	file := oauthRefreshTokenFile{Version: 3, Clients: clients, Tokens: records, Grants: grants}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal oauth refresh tokens: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create oauth refresh token dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write oauth refresh tokens: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename oauth refresh tokens: %w", err)
	}
	return nil
}

func oauthRefreshTokenKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
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

// verifyBearer validates a JWT bearer token against oauthServer's key material
// and grant store.
func (s *oauthServer) verifyBearer(r *http.Request, token string) (*bearerClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid bearer token format")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode token header: %w", err)
	}
	var header oauth.JWTHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse token header: %w", err)
	}
	if header.Alg != oauth.JWTAlgRS256 || header.KID != s.kid {
		return nil, errors.New("unsupported token header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode token signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&s.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, errors.New("invalid token signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode token payload: %w", err)
	}
	var claims oauth.AccessTokenClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("parse token claims: %w", err)
	}
	now := time.Now().Unix()
	if claims.Issuer != s.externalBaseURL(r) {
		return nil, errors.New("invalid token issuer")
	}
	if claims.Audience != s.externalBaseURL(r)+s.resourceURLPath {
		return nil, errors.New("invalid token audience")
	}
	if claims.Type != "access_token" {
		return nil, errors.New("invalid token type")
	}
	if claims.NotBefore > now || claims.Expiry <= now {
		return nil, errors.New("token is not valid now")
	}
	if claims.GrantID != "" {
		active, err := s.touchGrant(claims.GrantID, time.Now())
		if err != nil {
			return nil, fmt.Errorf("touch token grant: %w", err)
		}
		if !active {
			return nil, errors.New("token grant is not active")
		}
	}
	user, ok := s.authStore.FindByID(claims.Subject)
	if !ok {
		return nil, errors.New("token subject is unknown")
	}
	return &bearerClaims{
		User:     user,
		Subject:  claims.Subject,
		Username: claims.Username,
		Issuer:   claims.Issuer,
		Audience: claims.Audience,
		Scopes:   parseScopeSet(claims.Scope),
	}, nil
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
	if err := s.saveRefreshTokens(); err != nil {
		return nil, api.InternalError("save OAuth grant revocation: " + err.Error())
	}
	return &v1.StatusResp{Status: "ok"}, nil
}

// oauthGrantResponse converts an internal oauthGrant to an API response.
func oauthGrantResponse(grant *oauthGrant, now time.Time) v1.OAuthGrantResp {
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
