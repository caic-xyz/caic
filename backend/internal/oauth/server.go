// OAuth authorization-server HTTP handlers.

package oauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// BaseURLFunc returns the request's external base URL.
type BaseURLFunc func(*http.Request) string

// CurrentUserFunc returns the authenticated resource owner from ctx.
type CurrentUserFunc func(context.Context) (User, bool)

// AttachUserFunc returns a context with user attached for downstream handlers.
type AttachUserFunc func(context.Context, User) context.Context

// LoginAdapter supplies product login URLs and provider display labels.
type LoginAdapter interface {
	LoginStartURL(r *http.Request) string
	ProviderLabel(provider string) string
}

// AuditRecorder records OAuth authorization-server decisions.
type AuditRecorder interface {
	RecordOAuth(ctx context.Context, userID, operation, name, decision, status string, args any)
}

// RateLimiter allows or rejects a rate-limit key.
//
// Keys are opaque strings built by the oauth package: "ip:<addr>" for
// per-IP limits (register, introspect) and "client:<client_id>" for
// per-client limits (token, revoke). The implementation chooses the window
// size and request budget per key.
type RateLimiter interface {
	Allow(key string) bool
}

// EndSessionFunc tears down the caller's session for a user.
// Called by the end-session endpoint after grants are revoked.
// Returns a post-logout URL to redirect to, or empty string for a default page.
type EndSessionFunc func(ctx context.Context, r *http.Request, user User) (redirectURL string)

// ConsentRenderer renders an OAuth consent page.
type ConsentRenderer interface {
	RenderOAuthConsent(w http.ResponseWriter, data *ConsentPageData) error
}

// ConsentPageData is the server-rendered OAuth consent page model.
type ConsentPageData struct {
	Action        string
	ConsentToken  string
	ClientName    string
	ClientID      string
	RedirectURI   string
	Username      string
	UserInitial   string
	ProviderLabel string
	Resource      string
	ScopeItems    []ScopeItem
}

// ScopeItem holds a scope identifier and its human-readable label.
type ScopeItem struct {
	ID      string
	Label   string
	Checked bool
}

// ServerConfig configures an OAuth authorization server.
//
// The package owns protocol behavior. Callers inject product seams for login,
// users, audit, rate limiting, base URL discovery, and consent rendering so
// package oauth never depends on caic auth, MCP, API, or UI packages.
type ServerConfig struct {
	KeyPEM                []byte
	KeyID                 string
	AccessTokenTTL        time.Duration
	AuthCodeTTL           time.Duration
	RefreshTokenTTL       time.Duration
	DPoPNonceTTL          time.Duration
	RefreshTokenStorePath string

	ResourceURLPath         string
	ResourceMetadataURLPath string
	ClientIDPrefix          string
	SupportedScopes         []string
	DefaultScopes           []string
	ScopeLabels             map[string]string

	BaseURL     BaseURLFunc
	CurrentUser CurrentUserFunc
	AttachUser  AttachUserFunc
	UserLookup  UserLookupFunc
	Login       LoginAdapter
	Audit       AuditRecorder
	RateLimiter RateLimiter
	Renderer    ConsentRenderer
	EndSession  EndSessionFunc
}

// Server stores OAuth state for remote clients.
//
// Clients use Authorization Code with PKCE S256. Authorization requires a web
// session, so unauthenticated browser requests can be sent through a caller
// supplied login flow before consent is shown. Access tokens are resource-scoped
// JWTs. Refresh tokens are resource-scoped opaque tokens, persisted as hashes,
// and rotated on every refresh grant.
type Server struct {
	mu     sync.Mutex
	state  *Store
	tokens *AccessTokenService

	supportedScopes []string
	defaultScopes   []string
	scopeLabels     map[string]string

	accessTokenTTL          time.Duration
	authCodeTTL             time.Duration
	refreshTokenTTL         time.Duration
	dpopNonceTTL            time.Duration
	resourceURLPath         string
	resourceMetadataURLPath string
	clientIDPrefix          string

	dpopNonces *DPoPNonceManager

	baseURL     BaseURLFunc
	currentUser CurrentUserFunc
	attachUser  AttachUserFunc
	userLookup  UserLookupFunc
	login       LoginAdapter
	audit       AuditRecorder
	rateLimiter RateLimiter
	renderer    ConsentRenderer
	endSession  EndSessionFunc
}

// NewServer returns an OAuth authorization server.
func NewServer(c ServerConfig) (*Server, error) { //nolint:gocritic // ServerConfig is a startup value bag and the public constructor shape is intentional.
	accessTokenTTL := c.AccessTokenTTL
	if accessTokenTTL == 0 {
		accessTokenTTL = time.Hour
	}
	authCodeTTL := c.AuthCodeTTL
	if authCodeTTL == 0 {
		authCodeTTL = 10 * time.Minute
	}
	refreshTokenTTL := c.RefreshTokenTTL
	if refreshTokenTTL == 0 {
		refreshTokenTTL = 30 * 24 * time.Hour
	}
	dpopNonceTTL := c.DPoPNonceTTL
	if dpopNonceTTL == 0 {
		dpopNonceTTL = defaultDPoPNonceTTL
	}
	tokens, err := NewAccessTokenService(c.KeyPEM, c.KeyID, accessTokenTTL)
	if err != nil {
		return nil, err
	}
	state, err := LoadStore(c.RefreshTokenStorePath)
	if err != nil {
		return nil, err
	}
	return &Server{
		state:                   state,
		tokens:                  tokens,
		dpopNonces:              NewDPoPNonceManager(dpopNonceTTL),
		supportedScopes:         c.SupportedScopes,
		defaultScopes:           c.DefaultScopes,
		scopeLabels:             c.ScopeLabels,
		accessTokenTTL:          accessTokenTTL,
		authCodeTTL:             authCodeTTL,
		refreshTokenTTL:         refreshTokenTTL,
		dpopNonceTTL:            dpopNonceTTL,
		resourceURLPath:         c.ResourceURLPath,
		resourceMetadataURLPath: c.ResourceMetadataURLPath,
		clientIDPrefix:          c.ClientIDPrefix,
		baseURL:                 c.BaseURL,
		currentUser:             c.CurrentUser,
		attachUser:              c.AttachUser,
		userLookup:              c.UserLookup,
		login:                   c.Login,
		audit:                   c.Audit,
		rateLimiter:             c.RateLimiter,
		renderer:                c.Renderer,
		endSession:              c.EndSession,
	}, nil
}

// RegisterWellKnownRoutes registers OAuth metadata endpoints on mux.
func (s *Server) RegisterWellKnownRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleOAuthMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOAuthMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/", s.handleProtectedResourceMetadata)
}

// Routes returns OAuth authorization-server endpoint routes.
func (s *Server) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /oauth/jwks", s.handleOAuthJWKS)
	m.HandleFunc("POST /oauth/register", s.handleOAuthRegister)
	m.HandleFunc("GET /oauth/register/{clientID}", s.handleOAuthRegisterRead)
	m.HandleFunc("PUT /oauth/register/{clientID}", s.handleOAuthRegisterUpdate)
	m.HandleFunc("DELETE /oauth/register/{clientID}", s.handleOAuthRegisterDelete)
	m.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorizeGET)
	m.HandleFunc("POST /oauth/authorize", s.handleOAuthAuthorizePOST)
	m.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	m.HandleFunc("POST /oauth/revoke", s.handleOAuthRevoke)
	m.HandleFunc("POST /oauth/introspect", s.handleOAuthIntrospect)
	m.HandleFunc("GET /oauth/end-session", s.handleOAuthEndSession)
	return m
}

type bearerClaimsContextKey struct{}

// NewBearerClaimsContext returns a context with claims attached.
func NewBearerClaimsContext(ctx context.Context, claims *BearerClaims) context.Context {
	return context.WithValue(ctx, bearerClaimsContextKey{}, claims)
}

// BearerClaimsFromContext returns verified bearer-token claims from ctx.
func BearerClaimsFromContext(ctx context.Context) (*BearerClaims, bool) {
	claims, ok := ctx.Value(bearerClaimsContextKey{}).(*BearerClaims)
	return claims, ok && claims != nil
}

// BearerAuth verifies OAuth bearer tokens. On success, bearer claims and the
// verified user are set in the request context using the configured callbacks.
func (s *Server) BearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, scheme := s.extractAuthToken(r)
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

		// DPoP scheme: proof validation and key confirmation.
		if scheme == DPoPTokenType {
			if claims.Confirmation == nil || claims.Confirmation.JKT == "" {
				s.writeUnauthorizedDPoP(w, r, "access token not bound to a dpop key")
				return
			}
			proofHeader, proofClaims, err := DPoPProof(r)
			if err != nil {
				s.writeUnauthorizedDPoP(w, r, "missing or invalid dpop proof")
				return
			}
			if err := VerifyDPoPProof(r, proofHeader, proofClaims, defaultDPoPMaxAge, token, s.dpopNonces.Validate); err != nil {
				s.writeUnauthorizedDPoP(w, r, err.Error())
				return
			}
			proofJKT, err := JWKThumbprint(&proofHeader.JWK)
			if err != nil {
				s.writeUnauthorizedDPoP(w, r, "invalid dpop proof key")
				return
			}
			if subtle.ConstantTimeCompare([]byte(proofJKT), []byte(claims.Confirmation.JKT)) != 1 {
				s.writeUnauthorizedDPoP(w, r, "dpop proof key does not match token binding")
				return
			}
		}

		ctx := s.newUserContext(r.Context(), claims.User)
		ctx = NewBearerClaimsContext(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ListUserGrants returns a user's grants, newest first.
func (s *Server) ListUserGrants(userID string) []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.ListUserGrants(userID)
}

// RevokeUserGrant revokes one user's grant and all refresh tokens for it, then saves durable state.
func (s *Server) RevokeUserGrant(userID, grantID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.RevokeUserGrant(userID, grantID, time.Now()) {
		return false, nil
	}
	if err := s.state.Save(); err != nil {
		return true, err
	}
	return true, nil
}

// RevokeAllUserGrants revokes all grants and refresh tokens for a user, then saves durable state.
func (s *Server) RevokeAllUserGrants(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.RevokeAllUserGrants(userID, time.Now()) {
		return nil
	}
	return s.state.Save()
}

func (s *Server) currentRequestUser(ctx context.Context) (User, bool) {
	if s.currentUser == nil {
		return User{}, false
	}
	return s.currentUser(ctx)
}

func (s *Server) newUserContext(ctx context.Context, u User) context.Context {
	if s.attachUser == nil {
		return ctx
	}
	return s.attachUser(ctx, u)
}

func (s *Server) findUserByID(id string) (User, bool) {
	if s.userLookup == nil {
		return User{}, false
	}
	return s.userLookup(id)
}

func (s *Server) providerLabel(provider string) string {
	if s.login != nil {
		return s.login.ProviderLabel(provider)
	}
	if provider == "" {
		return "unknown provider"
	}
	return provider
}

func (s *Server) scopeItems(scope string) []ScopeItem {
	parts := strings.Fields(scope)
	if len(parts) == 0 && len(s.supportedScopes) > 0 {
		parts = []string{s.supportedScopes[0]}
	}
	items := make([]ScopeItem, 0, len(parts))
	for _, p := range parts {
		label := s.scopeLabels[p]
		if label == "" {
			label = p
		}
		items = append(items, ScopeItem{ID: p, Label: label, Checked: slices.Contains(s.defaultScopes, p)})
	}
	return items
}

func (s *Server) externalBaseURL(r *http.Request) string {
	if s.baseURL != nil {
		return s.baseURL(r)
	}
	authority, scheme := effectiveRequestHostAndScheme(r)
	return scheme + "://" + authority
}

// rateLimit rejects the request with 429 if the limiter is configured and
// the key is disallowed. Returns true when allowed (or no limiter).
func (s *Server) rateLimit(w http.ResponseWriter, key string) bool {
	if s.rateLimiter == nil {
		return true
	}
	if s.rateLimiter.Allow(key) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

// rateLimitIP rate-limits the request by remote IP.
func (s *Server) rateLimitIP(w http.ResponseWriter, r *http.Request) bool {
	return s.rateLimit(w, "ip:"+r.RemoteAddr)
}

// rateLimitClient rate-limits the request by client_id, falling back to
// remote IP when clientID is empty.
func (s *Server) rateLimitClient(w http.ResponseWriter, r *http.Request, clientID string) bool {
	if clientID != "" {
		return s.rateLimit(w, "client:"+clientID)
	}
	return s.rateLimitIP(w, r)
}

func (s *Server) handleOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	issuer := s.externalBaseURL(r)
	metadata := AuthorizationServerMetadata{
		Issuer:                                        issuer,
		AuthorizationEndpoint:                         issuer + "/oauth/authorize",
		TokenEndpoint:                                 issuer + "/oauth/token",
		JWKSURI:                                       issuer + "/oauth/jwks",
		RegistrationEndpoint:                          issuer + "/oauth/register",
		RevocationEndpoint:                            issuer + "/oauth/revoke",
		ResponseTypesSupported:                        []string{ResponseTypeCode},
		GrantTypesSupported:                           []string{GrantAuthorizationCode, GrantRefreshToken},
		CodeChallengeMethodsSupported:                 []string{CodeChallengeS256},
		TokenEndpointAuthMethodsSupported:             []string{TokenEndpointAuthNone},
		RevocationEndpointAuthMethodsSupported:        []string{TokenEndpointAuthNone},
		ScopesSupported:                               s.supportedScopes,
		IntrospectionEndpoint:                         issuer + "/oauth/introspect",
		IntrospectionEndpointAuthMethodsSupported:     []string{TokenEndpointAuthNone},
		DPoPSigningAlgValuesSupported:                 []string{"RS256", "ES256", "EdDSA"},
		EndSessionEndpoint:                            issuer + "/oauth/end-session",
		AuthorizationResponseIssuerParameterSupported: true,
	}
	writeJSONResponse(w, &metadata)
}

func (s *Server) handleOAuthJWKS(w http.ResponseWriter, r *http.Request) {
	resp := JWKSet{Keys: s.tokens.JWK()}
	writeJSONResponse(w, &resp)
}

func (s *Server) handleOAuthAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		if s.login != nil {
			if loginURL := s.login.LoginStartURL(r); loginURL != "" {
				if !strings.HasPrefix(loginURL, "/") || strings.HasPrefix(loginURL, "//") {
					http.Error(w, "oauth login unavailable", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Location", loginURL)
				w.WriteHeader(http.StatusFound)
				return
			}
		}
		WriteError(w, http.StatusUnauthorized, "login_required", "log in before authorizing protected resource access")
		return
	}
	if err := s.validateAuthorizeRequest(r); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	values := cloneURLValues(r.URL.Query())
	client := s.oauthClient(values.Get("client_id"))
	scope, _ := s.normalizeScope(values.Get("scope"))
	consentToken, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth consent token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not start consent")
		return
	}
	s.mu.Lock()
	params := make(map[string]string)
	for k, vals := range values {
		if len(vals) > 0 {
			params[k] = vals[0]
		}
	}
	s.state.Consents[consentToken] = ConsentParams{UserID: user.ID, Params: params, ExpiresAt: time.Now().Add(s.authCodeTTL)}
	if err := s.state.Save(); err != nil {
		slog.WarnContext(r.Context(), "save oauth consent", "err", err)
	}
	s.mu.Unlock()
	if s.renderer == nil {
		WriteError(w, http.StatusInternalServerError, "server_error", "consent renderer is not configured")
		return
	}
	data := ConsentPageData{
		Action:        s.externalBaseURL(r) + "/oauth/authorize",
		ConsentToken:  consentToken,
		ClientName:    clientDisplayName(&client),
		ClientID:      client.ID,
		RedirectURI:   values.Get("redirect_uri"),
		Username:      user.Username,
		UserInitial:   userInitial(user.Username),
		ProviderLabel: s.providerLabel(user.Provider),
		Resource:      values.Get("resource"),
		ScopeItems:    s.scopeItems(scope),
	}
	if err := s.renderer.RenderOAuthConsent(w, &data); err != nil {
		slog.WarnContext(r.Context(), "render oauth consent", "err", err)
	}
}

func (s *Server) handleOAuthAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		WriteError(w, http.StatusUnauthorized, "login_required", "log in before authorizing protected resource access")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	consentToken := r.PostForm.Get("consent_token")
	s.mu.Lock()
	c, ok := s.state.Consents[consentToken]
	if ok {
		delete(s.state.Consents, consentToken)
		if err := s.state.Save(); err != nil {
			slog.WarnContext(r.Context(), "save oauth consent deletion", "err", err)
		}
	}
	s.mu.Unlock()
	if !ok || c.UserID != user.ID || time.Now().After(c.ExpiresAt) {
		WriteError(w, http.StatusBadRequest, "invalid_request", "invalid or expired consent")
		return
	}
	values := url.Values{}
	for k, v := range c.Params {
		values.Set(k, v)
	}
	if err := s.validateAuthorizeForm(r, values); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
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
		WriteError(w, http.StatusBadRequest, "invalid_request", "invalid consent decision")
		return
	}
	code, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth code", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not authorize client")
		return
	}
	scope, err := s.approveScope(values.Get("scope"), r.PostForm)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	entry := Code{UserID: user.ID, ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), CodeChallenge: values.Get("code_challenge"), Resource: values.Get("resource"), Scope: scope, ExpiresAt: time.Now().Add(s.authCodeTTL)}
	s.mu.Lock()
	s.state.Codes[code] = entry
	if err := s.state.Save(); err != nil {
		slog.WarnContext(r.Context(), "save oauth code", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not authorize client")
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	redirectURL, err := url.Parse(entry.RedirectURI)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect URI")
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

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if !s.rateLimitClient(w, r, r.PostForm.Get("client_id")) {
		return
	}
	if err := s.pruneExpiredRefreshTokens(time.Now()); err != nil {
		slog.WarnContext(r.Context(), "prune oauth refresh tokens", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not prune refresh tokens")
		return
	}

	// Extract and validate DPoP proof if present.
	dpopJKT := ""
	if proofHeader, proofClaims, err := DPoPProof(r); err == nil {
		jkt, err := JWKThumbprint(&proofHeader.JWK)
		if err != nil {
			slog.WarnContext(r.Context(), "dpop proof thumbprint", "err", err)
			WriteError(w, http.StatusBadRequest, "invalid_dpop_proof", "invalid dpop proof key")
			return
		}
		if err := VerifyDPoPProofTokenEndpoint(r, proofHeader, proofClaims, defaultDPoPMaxAge, s.dpopNonces.Validate); err != nil {
			slog.WarnContext(r.Context(), "dpop proof validation", "err", err)
			w.Header().Set("DPoP-Nonce", s.dpopNonces.Issue())
			WriteError(w, http.StatusBadRequest, "invalid_dpop_proof", err.Error())
			return
		}
		dpopJKT = jkt
	}

	switch r.PostForm.Get("grant_type") {
	case GrantAuthorizationCode:
		s.handleOAuthAuthorizationCodeToken(w, r, dpopJKT)
	case GrantRefreshToken:
		s.handleOAuthRefreshToken(w, r, dpopJKT)
	default:
		WriteError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

func (s *Server) handleOAuthAuthorizationCodeToken(w http.ResponseWriter, r *http.Request, dpopJKT string) {
	code := r.PostForm.Get("code")
	s.mu.Lock()
	entry, ok := s.state.Codes[code]
	if ok {
		delete(s.state.Codes, code)
		if err := s.state.Save(); err != nil {
			slog.WarnContext(r.Context(), "save oauth code deletion", "err", err)
		}
	}
	s.mu.Unlock()
	if !ok || time.Now().After(entry.ExpiresAt) {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
		return
	}
	if r.PostForm.Get("client_id") != entry.ClientID || r.PostForm.Get("redirect_uri") != entry.RedirectURI {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "client or redirect URI mismatch")
		return
	}
	if resource := r.PostForm.Get("resource"); resource != "" && resource != entry.Resource {
		WriteError(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
		return
	}
	if !VerifyPKCES256(entry.CodeChallenge, r.PostForm.Get("code_verifier")) {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	user, ok := s.findUserByID(entry.UserID)
	if !ok {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	client := s.oauthClient(entry.ClientID)
	grantID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth grant id", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	now := time.Now()
	grant := Grant{ID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, ClientName: clientDisplayName(&client), Resource: entry.Resource, Scope: entry.Scope, CreatedAt: now, ExpiresAt: now.Add(s.refreshTokenTTL)}
	refreshEntry := RefreshToken{GrantID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: grant.ExpiresAt}
	refreshToken, err := s.issueGrantRefreshToken(&grant, &refreshEntry)
	if err != nil {
		slog.WarnContext(r.Context(), "issue oauth refresh token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	s.recordAudit(r, entry.UserID, "oauth/token", entry.ClientID, "allow", "issued", map[string]any{
		"grantID":  grantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeTokenResponse(w, r, user, entry.Resource, entry.Scope, grantID, refreshToken, dpopJKT)
}

func (s *Server) handleOAuthRefreshToken(w http.ResponseWriter, r *http.Request, dpopJKT string) {
	refreshToken := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")
	entry, ok := s.validRefreshToken(refreshToken, clientID)
	if !ok {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	user, ok := s.findUserByID(entry.UserID)
	if !ok {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	nextRefreshToken, entry, ok, err := s.rotateRefreshToken(refreshToken, clientID, entry.UserID)
	if err != nil {
		slog.WarnContext(r.Context(), "rotate oauth refresh token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not rotate refresh token")
		return
	}
	if !ok {
		WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	s.recordAudit(r, entry.UserID, "oauth/token", clientID, "allow", "refreshed", map[string]any{
		"grantID":  entry.GrantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeTokenResponse(w, r, user, entry.Resource, entry.Scope, entry.GrantID, nextRefreshToken, dpopJKT)
}

func (s *Server) writeTokenResponse(w http.ResponseWriter, r *http.Request, user User, resource, scope, grantID, refreshToken, dpopJKT string) {
	var accessToken string
	var err error
	if dpopJKT != "" {
		accessToken, err = s.tokens.IssueDPoPAccessToken(s.externalBaseURL(r), user, resource, scope, grantID, dpopJKT)
	} else {
		accessToken, err = s.tokens.IssueAccessToken(s.externalBaseURL(r), user, resource, scope, grantID)
	}
	if err != nil {
		slog.WarnContext(r.Context(), "issue oauth token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	tokenType := TokenTypeBearer
	if dpopJKT != "" {
		tokenType = DPoPTokenType
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	nonce := s.dpopNonces.Issue()
	w.Header().Set("DPoP-Nonce", nonce)
	resp := TokenResponse{AccessToken: accessToken, TokenType: tokenType, ExpiresIn: int64(s.accessTokenTTL.Seconds()), RefreshToken: refreshToken, Scope: scope}
	writeJSONResponse(w, &resp)
}

func (s *Server) handleOAuthIntrospect(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimitIP(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		writeJSONResponse(w, IntrospectionResponse{Active: false})
		return
	}
	token := r.PostForm.Get("token")
	hint := r.PostForm.Get("token_type_hint")
	if token == "" {
		writeJSONResponse(w, IntrospectionResponse{Active: false})
		return
	}

	switch hint {
	case "refresh_token":
		s.introspectRefreshToken(w, token)
	default:
		s.introspectAccessToken(w, r, token)
	}
}

// introspectAccessToken introspects a JWT access token.
func (s *Server) introspectAccessToken(w http.ResponseWriter, r *http.Request, token string) {
	claims, err := s.tokens.VerifyAccessToken(token, s.externalBaseURL(r), s.externalBaseURL(r)+s.resourceURLPath, time.Now(), s.touchGrant, s.findUserByID)
	if err != nil {
		writeJSONResponse(w, IntrospectionResponse{Active: false})
		return
	}
	writeJSONResponse(w, IntrospectionResponse{
		Active:       true,
		Scope:        strings.Join(claims.Scopes, " "),
		ClientID:     claims.ClientID,
		TokenType:    accessTokenType,
		Iat:          claims.Iat,
		Exp:          claims.Exp,
		Sub:          claims.Subject,
		Username:     claims.Username,
		Iss:          claims.Issuer,
		Aud:          claims.Audience,
		Confirmation: claims.Confirmation,
	})
}

// introspectRefreshToken introspects a refresh token by looking it up
// in the store and returning grant-level information.
func (s *Server) introspectRefreshToken(w http.ResponseWriter, token string) {
	now := time.Now()
	tokenHash := RefreshTokenKey(token)
	s.mu.Lock()
	entry, ok := s.state.RefreshTokens[tokenHash]
	if !ok || !entry.RevokedAt.IsZero() || !entry.UsedAt.IsZero() || now.After(entry.ExpiresAt) {
		s.mu.Unlock()
		writeJSONResponse(w, IntrospectionResponse{Active: false})
		return
	}
	grant, grantOK := s.state.Grants[entry.GrantID]
	s.mu.Unlock()
	if !grantOK || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		writeJSONResponse(w, IntrospectionResponse{Active: false})
		return
	}
	writeJSONResponse(w, IntrospectionResponse{
		Active:    true,
		Scope:     grant.Scope,
		ClientID:  grant.ClientID,
		TokenType: "refresh_token",
		Exp:       grant.ExpiresAt.Unix(),
		Sub:       grant.UserID,
		Iat:       grant.CreatedAt.Unix(),
	})
}

func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if !s.rateLimitClient(w, r, r.PostForm.Get("client_id")) {
		return
	}
	token := r.PostForm.Get("token")
	clientID := r.PostForm.Get("client_id")
	hint := r.PostForm.Get("token_type_hint")

	var userID string
	var err error

	switch hint {
	case "refresh_token":
		userID, err = s.revokeRefreshToken(token, clientID)
	case "access_token":
		userID = s.revokeAccessToken(token, r)
	default:
		userID, err = s.revokeRefreshToken(token, clientID)
		if userID == "" {
			userID = s.revokeAccessToken(token, r)
		}
	}
	if err != nil {
		slog.WarnContext(r.Context(), "revoke oauth token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not revoke token")
		return
	}
	s.recordAudit(r, userID, "oauth/revoke", clientID, "allow", "revoked", nil)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
}

// revokeAccessToken verifies a JWT access token and revokes its grant.
// Always returns an empty userID on verification failure to avoid leaking
// token validity — per RFC 7009 the revoke endpoint must always return 200.
func (s *Server) revokeAccessToken(token string, r *http.Request) (userID string) {
	claims, verr := s.tokens.verifyClaims(token, s.externalBaseURL(r), s.externalBaseURL(r)+s.resourceURLPath, time.Now())
	if verr != nil {
		return "" // token invalid, but don't leak that
	}
	if claims.GrantID == "" {
		return claims.Subject // no grant to revoke
	}
	now := time.Now()
	s.mu.Lock()
	grant, ok := s.state.Grants[claims.GrantID]
	if ok && grant.RevokedAt.IsZero() {
		grant.RevokedAt = now
		s.state.Grants[claims.GrantID] = grant
		// Also revoke all refresh tokens for this grant.
		for tokenHash := range s.state.RefreshTokens {
			entry := s.state.RefreshTokens[tokenHash]
			if entry.GrantID == claims.GrantID && entry.RevokedAt.IsZero() {
				entry.RevokedAt = now
				s.state.RefreshTokens[tokenHash] = entry
			}
		}
		_ = s.state.Save()
	}
	s.mu.Unlock()
	return claims.Subject
}

func (s *Server) recordAudit(r *http.Request, userID, operation, name, decision, status string, args any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordOAuth(r.Context(), userID, operation, name, decision, status, args)
}

func (s *Server) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, values url.Values, code, description string) {
	redirectURL, err := url.Parse(values.Get("redirect_uri"))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect URI")
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

func (s *Server) validateAuthorizeRequest(r *http.Request) error {
	return s.validateAuthorizeForm(r, r.URL.Query())
}

func (s *Server) validateAuthorizeForm(r *http.Request, values url.Values) error {
	if values.Get("response_type") != ResponseTypeCode {
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
	if values.Get("code_challenge_method") != CodeChallengeS256 || values.Get("code_challenge") == "" {
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

func (s *Server) issueGrantRefreshToken(grant *Grant, entry *RefreshToken) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Grants[grant.ID] = *grant
	s.state.RefreshTokens[RefreshTokenKey(token)] = *entry
	if err := s.state.Save(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Server) validRefreshToken(token, clientID string) (RefreshToken, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.state.RefreshTokens[RefreshTokenKey(token)]
	if !ok || entry.ClientID != clientID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return RefreshToken{}, false
	}
	grant, ok := s.state.Grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return RefreshToken{}, false
	}
	return entry, true
}

func (s *Server) rotateRefreshToken(token, clientID, userID string) (nextToken string, next RefreshToken, ok bool, err error) {
	nextToken, err = randomToken()
	if err != nil {
		return "", RefreshToken{}, false, err
	}
	now := time.Now()
	tokenHash := RefreshTokenKey(token)
	nextTokenHash := RefreshTokenKey(nextToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.state.RefreshTokens[tokenHash]
	if !ok || entry.ClientID != clientID || entry.UserID != userID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return "", RefreshToken{}, false, nil
	}
	grant, ok := s.state.Grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return "", RefreshToken{}, false, nil
	}
	entry.UsedAt = now
	s.state.RefreshTokens[tokenHash] = entry
	nextExpiry := now.Add(s.refreshTokenTTL)
	next = RefreshToken{GrantID: entry.GrantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, DPoPJKT: entry.DPoPJKT, ExpiresAt: nextExpiry}
	s.state.RefreshTokens[nextTokenHash] = next
	grant.LastUsedAt = now
	grant.ExpiresAt = nextExpiry
	s.state.Grants[grant.ID] = grant
	if err := s.state.Save(); err != nil {
		return "", RefreshToken{}, false, err
	}
	return nextToken, next, true, nil
}

func (s *Server) revokeRefreshToken(token, clientID string) (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tokenHash := RefreshTokenKey(token)
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

func (s *Server) touchGrant(grantID string, now time.Time) (active bool, clientID string, err error) {
	if grantID == "" {
		return true, "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.state.Grants[grantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return false, "", nil
	}
	grant.LastUsedAt = now
	s.state.Grants[grantID] = grant
	if err := s.state.Save(); err != nil {
		return false, "", err
	}
	return true, grant.ClientID, nil
}

func (s *Server) pruneExpiredRefreshTokens(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.PruneExpiredRefreshTokens(now) {
		return nil
	}
	return s.state.Save()
}

func (s *Server) normalizeScope(scope string) (string, error) {
	parts := strings.Fields(scope)
	if len(parts) == 0 {
		if len(s.supportedScopes) == 0 {
			return "", errors.New("no supported scopes configured")
		}
		return s.supportedScopes[0], nil
	}
	for _, part := range parts {
		if !slices.Contains(s.supportedScopes, part) {
			return "", fmt.Errorf("unsupported scope: %s", part)
		}
	}
	return strings.Join(parts, " "), nil
}

func (s *Server) approveScope(requested string, form url.Values) (string, error) {
	normalizedRequested, err := s.normalizeScope(requested)
	if err != nil {
		return "", err
	}
	if form.Get("scope_form") == "" {
		return normalizedRequested, nil
	}
	allowed := strings.Fields(normalizedRequested)
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
	approved := make([]string, 0, len(selected))
	for _, scope := range s.supportedScopes {
		if _, ok := selected[scope]; ok {
			approved = append(approved, scope)
		}
	}
	return strings.Join(approved, " "), nil
}

func (s *Server) oauthClient(id string) Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Clients[id]
}

func (s *Server) registerClient(client *Client) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Clients[client.ID] = *client
	return s.state.Save()
}

func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimitIP(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration JSON")
		return
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = TokenEndpointAuthNone
	}
	if method != TokenEndpointAuthNone {
		WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
		return
	}
	if len(req.RedirectURIs) == 0 {
		WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, redirectURI := range req.RedirectURIs {
		if !validRedirectURI(redirectURI) {
			WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be https or localhost http")
			return
		}
	}
	clientID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth client id", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	now := time.Now()
	client := Client{ID: s.clientIDPrefix + clientID, Name: req.ClientName, RedirectURIs: req.RedirectURIs, TokenEndpointAuthMethod: method, CreatedAt: now}
	if err := s.registerClient(&client); err != nil {
		slog.WarnContext(r.Context(), "save oauth client registration", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	issuer := s.externalBaseURL(r)
	regToken, err := s.tokens.IssueRegistrationAccessToken(issuer, client.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "issue registration access token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not issue registration access token")
		return
	}
	resp := RegisterResponse{
		ClientID:                client.ID,
		ClientIDIssuedAt:        now.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		TokenEndpointAuthMethod: method,
		RegistrationAccessToken: regToken,
		RegistrationClientURI:   issuer + "/oauth/register/" + client.ID,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		slog.WarnContext(r.Context(), "encode oauth registration response", "err", err)
	}
}

// verifyRegistrationAccessToken extracts and validates a registration access token from the request.
// Returns nil if the token is valid and its subject matches clientID.
func (s *Server) verifyRegistrationAccessToken(r *http.Request, clientID string) error {
	token := BearerToken(r)
	if token == "" {
		return errors.New("missing registration access token")
	}
	audience := s.externalBaseURL(r) + "/oauth/register"
	tokenClientID, err := s.tokens.VerifyRegistrationAccessToken(token, s.externalBaseURL(r), audience, time.Now())
	if err != nil {
		return fmt.Errorf("invalid registration access token: %w", err)
	}
	if tokenClientID != clientID {
		return errors.New("registration access token does not match client")
	}
	return nil
}

// handleOAuthRegisterRead returns client registration metadata (RFC 7592 §2.1).
func (s *Server) handleOAuthRegisterRead(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	client := s.oauthClient(clientID)
	if client.ID == "" {
		WriteError(w, http.StatusNotFound, "invalid_client", "client not found")
		return
	}
	if err := s.verifyRegistrationAccessToken(r, clientID); err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}
	resp := RegisterResponse{
		ClientID:                client.ID,
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		TokenEndpointAuthMethod: client.TokenEndpointAuthMethod,
	}
	writeJSONResponse(w, &resp)
}

// handleOAuthRegisterUpdate updates client registration metadata (RFC 7592 §2.2).
func (s *Server) handleOAuthRegisterUpdate(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	if err := s.verifyRegistrationAccessToken(r, clientID); err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req UpdateClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration JSON")
		return
	}
	s.mu.Lock()
	client, ok := s.state.Clients[clientID]
	if !ok {
		s.mu.Unlock()
		WriteError(w, http.StatusNotFound, "invalid_client", "client not found")
		return
	}
	if req.ClientName != nil {
		client.Name = *req.ClientName
	}
	if req.RedirectURIs != nil {
		for _, uri := range *req.RedirectURIs {
			if !validRedirectURI(uri) {
				s.mu.Unlock()
				WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be https or localhost http")
				return
			}
		}
		client.RedirectURIs = *req.RedirectURIs
	}
	if req.TokenEndpointAuthMethod != nil {
		method := *req.TokenEndpointAuthMethod
		if method != TokenEndpointAuthNone {
			s.mu.Unlock()
			WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
			return
		}
		client.TokenEndpointAuthMethod = method
	}
	s.state.Clients[clientID] = client
	if err := s.state.Save(); err != nil {
		s.mu.Unlock()
		slog.WarnContext(r.Context(), "save oauth client update", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not update client")
		return
	}
	s.mu.Unlock()

	issuer := s.externalBaseURL(r)
	regToken, err := s.tokens.IssueRegistrationAccessToken(issuer, clientID)
	if err != nil {
		slog.WarnContext(r.Context(), "issue registration access token", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not issue registration access token")
		return
	}
	resp := RegisterResponse{
		ClientID:                client.ID,
		ClientIDIssuedAt:        client.CreatedAt.Unix(),
		ClientName:              client.Name,
		RedirectURIs:            client.RedirectURIs,
		TokenEndpointAuthMethod: client.TokenEndpointAuthMethod,
		RegistrationAccessToken: regToken,
		RegistrationClientURI:   issuer + "/oauth/register/" + clientID,
	}
	writeJSONResponse(w, &resp)
}

// handleOAuthRegisterDelete deletes a client registration (RFC 7592 §2.3).
func (s *Server) handleOAuthRegisterDelete(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	client := s.oauthClient(clientID)
	if client.ID == "" {
		// RFC 7592 §2.3: respond with errors as in §2.2.
		WriteError(w, http.StatusUnauthorized, "invalid_client", "client not found")
		return
	}
	if err := s.verifyRegistrationAccessToken(r, clientID); err != nil {
		WriteError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}
	s.mu.Lock()
	delete(s.state.Clients, clientID)
	err := s.state.Save()
	s.mu.Unlock()
	if err != nil {
		slog.WarnContext(r.Context(), "save oauth client delete", "err", err)
		WriteError(w, http.StatusInternalServerError, "server_error", "could not delete client")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNoContent)
}

func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func clientDisplayName(client *Client) string {
	if client.Name != "" {
		return client.Name
	}
	if client.ID != "" {
		return client.ID
	}
	return "remote OAuth client"
}

func userInitial(username string) string {
	for _, r := range strings.TrimSpace(username) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func cloneURLValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, entries := range values {
		clone[key] = append([]string(nil), entries...)
	}
	return clone
}

func writeJSONResponse(w http.ResponseWriter, output any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(output); err != nil {
		slog.Warn("failed to encode JSON response", "err", err)
	}
}

func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/.well-known/oauth-protected-resource" && path != s.resourceMetadataURLPath {
		http.NotFound(w, r)
		return
	}
	metadata := ProtectedResourceMetadata{
		Resource:             s.externalBaseURL(r) + s.resourceURLPath,
		AuthorizationServers: []string{s.externalBaseURL(r)},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		http.Error(w, "encode metadata", http.StatusInternalServerError)
	}
}

func (s *Server) verifyBearer(r *http.Request, token string) (*BearerClaims, error) {
	return s.tokens.VerifyAccessToken(token, s.externalBaseURL(r), s.externalBaseURL(r)+s.resourceURLPath, time.Now(), s.touchGrant, s.findUserByID)
}

// extractAuthToken extracts the access token from an Authorization header,
// supporting both "Bearer" and "DPoP" schemes. Returns the token and scheme.
func (s *Server) extractAuthToken(r *http.Request) (token, scheme string) {
	header := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(header, "DPoP "):
		return strings.TrimSpace(strings.TrimPrefix(header, "DPoP ")), DPoPTokenType
	case strings.HasPrefix(header, "Bearer "):
		return BearerToken(r), TokenTypeBearer
	default:
		return "", ""
	}
}

func (s *Server) writeUnauthorizedDPoP(w http.ResponseWriter, _ *http.Request, description string) {
	nonce := s.dpopNonces.Issue()
	w.Header().Set("DPoP-Nonce", nonce)
	// Per RFC 9449 §9.3, include error and error_description with DPoP scheme.
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`DPoP error="invalid_token", error_description=%q`, description))
	writeUnauthorizedJSON(w)
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == s.resourceURLPath && s.resourceMetadataURLPath != "" {
		resourceMetadataURL := s.externalBaseURL(r) + s.resourceMetadataURLPath
		w.Header().Set("WWW-Authenticate", BearerChallenge(resourceMetadataURL, strings.Join(s.supportedScopes, " ")))
	}
	writeUnauthorizedJSON(w)
}

func (s *Server) handleOAuthEndSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		if s.login != nil {
			if loginURL := s.login.LoginStartURL(r); loginURL != "" {
				http.Redirect(w, r, loginURL, http.StatusFound) //nolint:gosec // loginURL produced by the caller's LoginAdapter
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><p>You are not logged in.</p></body></html>`))
		return
	}

	if err := s.RevokeAllUserGrants(user.ID); err != nil {
		slog.WarnContext(r.Context(), "revoke all user grants on logout", "err", err)
	}

	postLogoutRedirectURI := r.URL.Query().Get("post_logout_redirect_uri")
	clientID := r.URL.Query().Get("client_id")
	if postLogoutRedirectURI != "" && clientID != "" {
		client := s.oauthClient(clientID)
		if client.ID != "" && slices.Contains(client.RedirectURIs, postLogoutRedirectURI) {
			// Valid — will redirect after session teardown.
		} else {
			postLogoutRedirectURI = ""
		}
	}

	callerRedirect := ""
	if s.endSession != nil {
		callerRedirect = s.endSession(r.Context(), r, user)
	}

	redirectTo := callerRedirect
	if redirectTo == "" {
		redirectTo = postLogoutRedirectURI
	}
	if redirectTo != "" {
		q := ""
		if state := r.URL.Query().Get("state"); state != "" {
			separator := "?"
			if strings.Contains(redirectTo, "?") {
				separator = "&"
			}
			q = separator + "state=" + url.QueryEscape(state)
		}
		http.Redirect(w, r, redirectTo+q, http.StatusFound) //nolint:gosec // redirectTo is validated against client registration
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<html><body><p>You have been logged out.</p></body></html>`))
}

func writeUnauthorizedJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"authentication required"}}`))
}

func effectiveRequestHostAndScheme(r *http.Request) (authority, scheme string) {
	authority = r.Host
	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		authority = forwardedHost
	}
	scheme = "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return authority, scheme
}
