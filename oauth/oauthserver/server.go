// OAuth authorization-server HTTP handlers.

// Package oauthserver is a generic OAuth 2.0 authorization server.
//
// The caller plugs in session management, consent UI rendering, and
// token storage via interfaces (SessionManager, AuthorizationUI,
// Storage). Dependency-free.
package oauthserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

// IntrospectionPrincipal identifies an authenticated introspection caller.
// ClientID restricts the caller to tokens issued to that OAuth client. An
// empty ClientID authorizes a trusted protected resource to inspect any token.
type IntrospectionPrincipal struct {
	ClientID string
}

// IntrospectionAuthenticator authenticates an introspection caller.
type IntrospectionAuthenticator func(*http.Request) (IntrospectionPrincipal, bool)

// SessionManager manages user sessions for the authorization server.
// The caller implements browser login, user lookup, and session teardown.
type SessionManager interface {
	CurrentUser(ctx context.Context) (oauth.User, bool)
	AttachUser(ctx context.Context, u oauth.User) context.Context
	FindUser(id string) (oauth.User, bool)
	EndSession(ctx context.Context, r *http.Request, u oauth.User) (redirectURL string)
}

// AuthorizationUI renders the OAuth authorization user interface.
type AuthorizationUI interface {
	LoginStartURL(r *http.Request) string
	ProviderLabel(provider string) string
	RenderOAuthConsent(w http.ResponseWriter, data *ConsentPageData) error
}

// AuditRecorder records OAuth authorization-server decisions.
type AuditRecorder interface {
	RecordOAuth(ctx context.Context, userID, operation, name, decision, status string, args any)
}

// RateLimiter allows or rejects a rate-limit key.
//
// Keys are opaque strings built by the oauth package: "ip:<addr>" for
// per-IP limits and "client:<client_id>" for per-client limits. Introspection
// uses the authenticated client identity when one is scoped and otherwise
// falls back to the remote address. The implementation chooses the window
// size and request budget per key.
type RateLimiter interface {
	Allow(key string) bool
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
	KeyPEM          []byte
	KeyID           string
	Issuer          string
	AccessTokenTTL  time.Duration
	AuthCodeTTL     time.Duration
	RefreshTokenTTL time.Duration
	DPoPNonceTTL    time.Duration
	// RefreshTokenStorePath is exclusively owned by one Server until Close.
	// Deployments must not share the same JSON state path across processes.
	RefreshTokenStorePath string

	ResourceURLPath         string
	ResourceMetadataURLPath string
	ClientIDPrefix          string
	SupportedScopes         []string
	DefaultScopes           []string
	ScopeLabels             map[string]string

	Session           SessionManager
	UI                AuthorizationUI
	Audit             AuditRecorder
	RateLimiter       RateLimiter
	IntrospectionAuth IntrospectionAuthenticator
}

// Server stores OAuth state for remote clients.
//
// Clients use Authorization Code with PKCE S256. Authorization requires a web
// session, so unauthenticated browser requests can be sent through a caller
// supplied login flow before consent is shown. Access tokens are resource-scoped
// JWTs. Refresh tokens are resource-scoped opaque tokens, persisted as hashes,
// and rotated on every refresh grant.
type Server struct {
	mu          sync.Mutex
	state       *Store
	parRequests map[string]ConsentParams // short-lived pushed authorization requests (RFC 9126)
	tokens      *AccessTokenService

	supportedScopes []string
	defaultScopes   []string
	scopeLabels     map[string]string

	accessTokenTTL          time.Duration
	authCodeTTL             time.Duration
	refreshTokenTTL         time.Duration
	dpopNonceTTL            time.Duration
	dpopNonceKey            [sha256.Size]byte
	resourceURLPath         string
	resourceMetadataURLPath string
	clientIDPrefix          string
	issuer                  string
	resourceURL             string

	session           SessionManager
	ui                AuthorizationUI
	audit             AuditRecorder
	rateLimiter       RateLimiter
	introspectionAuth IntrospectionAuthenticator

	releaseStore func()
}

type dpopBinding struct {
	jkt            string
	jti            string
	iat            int64
	nonce          string
	nonceExpiresAt time.Time
}

// NewServer returns an OAuth authorization server.
func NewServer(c ServerConfig) (*Server, error) { //nolint:gocritic // ServerConfig is a startup value bag and the public constructor shape is intentional.
	issuer, err := validateIssuer(c.Issuer)
	if err != nil {
		return nil, err
	}
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
	dpopNonceKey := sha256.Sum256(append([]byte("caic oauth dpop nonce\x00"), c.KeyPEM...))
	tokens, err := NewAccessTokenService(c.KeyPEM, c.KeyID, accessTokenTTL)
	if err != nil {
		return nil, err
	}
	if c.Session == nil {
		return nil, errors.New("oauth: Session is required")
	}
	if c.UI == nil {
		return nil, errors.New("oauth: UI is required")
	}
	releaseStore, err := claimStore(c.RefreshTokenStorePath)
	if err != nil {
		return nil, err
	}
	state, err := LoadStore(c.RefreshTokenStorePath)
	if err != nil {
		releaseStore()
		return nil, err
	}
	resourceURL := issuer + c.ResourceURLPath
	if err := state.transact(func(next *storeFile) bool {
		return containResourceState(next, resourceURL, time.Now())
	}); err != nil {
		releaseStore()
		return nil, fmt.Errorf("contain oauth resource state: %w", err)
	}
	return &Server{
		state:                   state,
		parRequests:             map[string]ConsentParams{},
		tokens:                  tokens,
		supportedScopes:         c.SupportedScopes,
		defaultScopes:           c.DefaultScopes,
		scopeLabels:             c.ScopeLabels,
		accessTokenTTL:          accessTokenTTL,
		authCodeTTL:             authCodeTTL,
		refreshTokenTTL:         refreshTokenTTL,
		dpopNonceTTL:            dpopNonceTTL,
		dpopNonceKey:            dpopNonceKey,
		resourceURLPath:         c.ResourceURLPath,
		resourceMetadataURLPath: c.ResourceMetadataURLPath,
		clientIDPrefix:          c.ClientIDPrefix,
		issuer:                  issuer,
		resourceURL:             resourceURL,
		session:                 c.Session,
		ui:                      c.UI,
		audit:                   c.Audit,
		rateLimiter:             c.RateLimiter,
		introspectionAuth:       c.IntrospectionAuth,
		releaseStore:            releaseStore,
	}, nil
}

// Close releases exclusive ownership of the configured durable state path.
func (s *Server) Close() {
	s.mu.Lock()
	release := s.releaseStore
	s.releaseStore = nil
	s.mu.Unlock()
	if release != nil {
		release()
	}
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
	m.HandleFunc("POST /oauth/par", s.handleOAuthPAR)
	m.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorizeGET)
	m.HandleFunc("POST /oauth/authorize", s.handleOAuthAuthorizePOST)
	m.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	m.HandleFunc("POST /oauth/revoke", s.handleOAuthRevoke)
	m.HandleFunc("GET /oauth/end-session", s.handleOAuthEndSession)
	m.HandleFunc("POST /oauth/device_authorization", s.handleOAuthDeviceAuthorization)
	m.HandleFunc("GET /oauth/device", s.handleOAuthDevicePage)
	m.HandleFunc("POST /oauth/device", s.handleOAuthDeviceApprove)
	return m
}

type bearerClaimsContextKey struct{}

// NewBearerClaimsContext returns a context with claims attached.
func NewBearerClaimsContext(ctx context.Context, claims *oauth.BearerClaims) context.Context {
	return context.WithValue(ctx, bearerClaimsContextKey{}, claims)
}

// BearerClaimsFromContext returns verified bearer-token claims from ctx.
func BearerClaimsFromContext(ctx context.Context) (*oauth.BearerClaims, bool) {
	claims, ok := ctx.Value(bearerClaimsContextKey{}).(*oauth.BearerClaims)
	return claims, ok && claims != nil
}

// BearerAuth verifies OAuth bearer tokens. On success, bearer claims and the
// verified user are set in the request context using the configured callbacks.
func (s *Server) BearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token := authorizationCredential(r)
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
		switch strings.ToLower(scheme) {
		case strings.ToLower(oauth.TokenTypeBearer):
			if claims.Confirmation != nil {
				s.writeUnauthorized(w, r)
				return
			}
		case strings.ToLower(DPoPTokenType):
			if claims.Confirmation == nil || claims.Confirmation.JKT == "" {
				s.writeUnauthorizedDPoP(w, r, "access token is not bound to a dpop key")
				return
			}
			proofHeader, proofClaims, err := DPoPProof(r)
			if err != nil {
				s.writeUnauthorizedDPoP(w, r, "missing or invalid dpop proof")
				return
			}
			proofJKT, err := JWKThumbprint(&proofHeader.JWK)
			if err != nil || subtle.ConstantTimeCompare([]byte(proofJKT), []byte(claims.Confirmation.JKT)) != 1 {
				s.writeUnauthorizedDPoP(w, r, "dpop proof key does not match token binding")
				return
			}
			binding := dpopBinding{jkt: proofJKT, jti: proofClaims.JTI, iat: proofClaims.IAT, nonce: proofClaims.Nonce}
			expectedHTU := s.issuer + r.URL.EscapedPath()
			if err := VerifyDPoPProof(r, expectedHTU, proofHeader, proofClaims, defaultDPoPMaxAge, token, func(nonce string) bool {
				binding.nonceExpiresAt = s.validateDPoPNonce(nonce)
				return !binding.nonceExpiresAt.IsZero()
			}, nil); err != nil {
				s.writeUnauthorizedDPoP(w, r, "invalid dpop proof")
				return
			}
			fresh, err := s.reserveDPoPBinding(binding)
			if err != nil || !fresh {
				s.writeUnauthorizedDPoP(w, r, "dpop proof has already been used")
				return
			}
		default:
			s.writeUnauthorized(w, r)
			return
		}
		ctx := s.newUserContext(r.Context(), claims.User)
		ctx = NewBearerClaimsContext(ctx, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func authorizationCredential(r *http.Request) (scheme, token string) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", ""
	}
	return scheme, strings.TrimSpace(token)
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
	revoked := false
	err := s.state.transact(func(next *storeFile) bool {
		revoked = revokeUserGrant(next, userID, grantID, time.Now())
		return revoked
	})
	return revoked, err
}

// RevokeAllUserGrants revokes all grants and refresh tokens for a user, then saves durable state.
func (s *Server) RevokeAllUserGrants(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.transact(func(next *storeFile) bool {
		return revokeAllUserGrants(next, userID, time.Now())
	})
}

func (s *Server) currentRequestUser(ctx context.Context) (oauth.User, bool) {
	return s.session.CurrentUser(ctx)
}

func (s *Server) newUserContext(ctx context.Context, u oauth.User) context.Context {
	return s.session.AttachUser(ctx, u)
}

func (s *Server) findUserByID(id string) (oauth.User, bool) {
	return s.session.FindUser(id)
}

func (s *Server) providerLabel(provider string) string {
	return s.ui.ProviderLabel(provider)
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
	issuer := s.issuer
	metadata := oauth.AuthorizationServerMetadata{
		Issuer:                                 issuer,
		AuthorizationEndpoint:                  issuer + "/oauth/authorize",
		TokenEndpoint:                          issuer + "/oauth/token",
		JWKSURI:                                issuer + "/oauth/jwks",
		RegistrationEndpoint:                   issuer + "/oauth/register",
		RevocationEndpoint:                     issuer + "/oauth/revoke",
		ResponseTypesSupported:                 []string{oauth.ResponseTypeCode},
		GrantTypesSupported:                    []string{oauth.GrantAuthorizationCode, oauth.GrantRefreshToken, "urn:ietf:params:oauth:grant-type:device_code"},
		CodeChallengeMethodsSupported:          []string{oauth.CodeChallengeS256},
		TokenEndpointAuthMethodsSupported:      []string{oauth.TokenEndpointAuthNone},
		RevocationEndpointAuthMethodsSupported: []string{oauth.TokenEndpointAuthNone},
		ScopesSupported:                        s.supportedScopes,
		EndSessionEndpoint:                     issuer + "/oauth/end-session",
		AuthorizationResponseIssuerParameterSupported: true,
		PushedAuthorizationRequestEndpoint:            issuer + "/oauth/par",
		RequirePushedAuthorizationRequests:            false,
		DPoPSigningAlgValuesSupported:                 []string{"RS256", "ES256", "EdDSA"},
	}
	writeJSONResponse(w, &metadata)
}

func (s *Server) handleOAuthJWKS(w http.ResponseWriter, r *http.Request) {
	resp := oauth.JWKSet{Keys: s.tokens.JWK()}
	writeJSONResponse(w, &resp)
}

func (s *Server) handleOAuthAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		if loginURL := s.ui.LoginStartURL(r); loginURL != "" {
			if !strings.HasPrefix(loginURL, "/") || strings.HasPrefix(loginURL, "//") {
				http.Error(w, "oauth login unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Location", loginURL)
			w.WriteHeader(http.StatusFound)
			return
		}
		oauth.WriteError(w, http.StatusUnauthorized, "login_required", "log in before authorizing protected resource access")
		return
	}
	// Check for RFC 9126 pushed authorization request.
	if requestURI := r.URL.Query().Get("request_uri"); requestURI != "" {
		s.mu.Lock()
		par, ok := s.parRequests[requestURI]
		if ok {
			delete(s.parRequests, requestURI)
		}
		s.mu.Unlock()
		if !ok || time.Now().After(par.ExpiresAt) {
			oauth.WriteError(w, http.StatusBadRequest, "invalid_request_uri", "invalid or expired request_uri")
			return
		}
		values := url.Values{}
		for k, v := range par.Params {
			values.Set(k, v)
		}
		if err := s.validateAuthorizeForm(values); err != nil {
			oauth.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		s.renderConsent(w, r, user, values)
		return
	}
	if err := s.validateAuthorizeRequest(r); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.renderConsent(w, r, user, cloneURLValues(r.URL.Query()))
}

// renderConsent stores consent parameters and renders the consent page.
func (s *Server) renderConsent(w http.ResponseWriter, r *http.Request, user oauth.User, values url.Values) {
	client := s.oauthClient(values.Get("client_id"))
	scope, _ := s.normalizeScope(values.Get("scope"))
	consentToken, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth consent token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not start consent")
		return
	}
	s.mu.Lock()
	params := make(map[string]string)
	for k, vals := range values {
		if len(vals) > 0 {
			params[k] = vals[0]
		}
	}
	err = s.state.transact(func(next *storeFile) bool {
		next.Consents[oauth.RefreshTokenKey(consentToken)] = ConsentParams{UserID: user.ID, Params: params, ExpiresAt: time.Now().Add(s.authCodeTTL)}
		return true
	})
	s.mu.Unlock()
	if err != nil {
		slog.WarnContext(r.Context(), "save oauth consent", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not start consent")
		return
	}
	data := ConsentPageData{
		Action:        s.issuer + "/oauth/authorize",
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
	if err := s.ui.RenderOAuthConsent(w, &data); err != nil {
		slog.WarnContext(r.Context(), "render oauth consent", "err", err)
	}
}

// handleOAuthPAR handles RFC 9126 pushed authorization requests.
func (s *Server) handleOAuthPAR(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	values := r.PostForm
	if err := s.validateAuthorizeForm(values); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	requestURI, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate par request_uri", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not generate request_uri")
		return
	}
	requestURI = "urn:ietf:params:oauth:request_uri:" + requestURI
	params := make(map[string]string)
	for k, vals := range values {
		if len(vals) > 0 {
			params[k] = vals[0]
		}
	}
	s.mu.Lock()
	s.parRequests[requestURI] = ConsentParams{
		Params:    params,
		ExpiresAt: time.Now().Add(90 * time.Second),
	}
	s.mu.Unlock()
	resp := oauth.PARResponse{
		RequestURI: requestURI,
		ExpiresIn:  90,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.WarnContext(r.Context(), "encode par response", "err", err)
	}
}

func (s *Server) handleOAuthAuthorizePOST(w http.ResponseWriter, r *http.Request) {
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
	consentHash := oauth.RefreshTokenKey(consentToken)
	s.mu.Lock()
	var c ConsentParams
	var consentFound bool
	err := s.state.transact(func(next *storeFile) bool {
		c, consentFound = next.Consents[consentHash]
		if consentFound {
			delete(next.Consents, consentHash)
		}
		return consentFound
	})
	s.mu.Unlock()
	if err != nil {
		slog.WarnContext(r.Context(), "consume oauth consent", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not consume consent")
		return
	}
	if !consentFound || c.UserID != user.ID || time.Now().After(c.ExpiresAt) {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid or expired consent")
		return
	}
	values := url.Values{}
	for k, v := range c.Params {
		values.Set(k, v)
	}
	if err := s.validateAuthorizeForm(values); err != nil {
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
	entry := Code{UserID: user.ID, ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), CodeChallenge: values.Get("code_challenge"), Resource: values.Get("resource"), Scope: scope, ExpiresAt: time.Now().Add(s.authCodeTTL)}
	s.mu.Lock()
	err = s.state.transact(func(next *storeFile) bool {
		next.Codes[oauth.RefreshTokenKey(code)] = entry
		return true
	})
	if err != nil {
		slog.WarnContext(r.Context(), "save oauth code", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not authorize client")
		s.mu.Unlock()
		return
	}
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
	q.Set("iss", s.issuer)
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
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if !s.rateLimitClient(w, r, r.PostForm.Get("client_id")) {
		return
	}
	binding := dpopBinding{}
	if r.Header.Get(DPoPTokenType) != "" {
		proofHeader, proofClaims, err := DPoPProof(r)
		if err != nil {
			s.writeInvalidDPoPProof(w, r, "malformed dpop proof")
			return
		}
		jkt, err := JWKThumbprint(&proofHeader.JWK)
		if err != nil {
			s.writeInvalidDPoPProof(w, r, "invalid dpop proof key")
			return
		}
		if err := VerifyDPoPProof(r, s.issuer+"/oauth/token", proofHeader, proofClaims, defaultDPoPMaxAge, "", func(nonce string) bool {
			binding.nonceExpiresAt = s.validateDPoPNonce(nonce)
			return !binding.nonceExpiresAt.IsZero()
		}, nil); err != nil {
			s.writeInvalidDPoPProof(w, r, "invalid dpop proof")
			return
		}
		binding.jkt, binding.jti, binding.iat, binding.nonce = jkt, proofClaims.JTI, proofClaims.IAT, proofClaims.Nonce
	}

	// RFC 8628 device_code grant: handle before pruning so expired entries
	// are correctly reported as expired_token rather than invalid_grant.
	if r.PostForm.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
		s.handleOAuthDeviceCodeToken(w, r, binding)
		return
	}

	if err := s.pruneExpiredRefreshTokens(time.Now()); err != nil {
		slog.WarnContext(r.Context(), "prune oauth refresh tokens", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not prune refresh tokens")
		return
	}

	switch r.PostForm.Get("grant_type") {
	case oauth.GrantAuthorizationCode:
		s.handleOAuthAuthorizationCodeToken(w, r, binding)
	case oauth.GrantRefreshToken:
		s.handleOAuthRefreshToken(w, r, binding)
	default:
		oauth.WriteError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

func (s *Server) handleOAuthAuthorizationCodeToken(w http.ResponseWriter, r *http.Request, binding dpopBinding) { //nolint:gocritic // Immutable proof values are passed together to preserve their binding.
	dpopJKT := binding.jkt
	code := r.PostForm.Get("code")
	codeHash := oauth.RefreshTokenKey(code)
	s.mu.Lock()
	entry, ok := s.state.Codes[codeHash]
	s.mu.Unlock()
	if !ok || time.Now().After(entry.ExpiresAt) {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
		return
	}
	if entry.Resource != s.resourceURL {
		s.mu.Lock()
		err := s.state.transact(func(next *storeFile) bool {
			current, found := next.Codes[codeHash]
			if !found || current != entry {
				return false
			}
			delete(next.Codes, codeHash)
			return true
		})
		s.mu.Unlock()
		if err != nil {
			slog.WarnContext(r.Context(), "discard oauth code for foreign resource", "err", err)
			oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not reject authorization code")
			return
		}
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "authorization code resource is no longer valid")
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
	grantID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth grant id", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	now := time.Now()
	grant := Grant{ID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, CreatedAt: now, ExpiresAt: now.Add(s.refreshTokenTTL)}
	refreshEntry := RefreshToken{GrantID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, DPoPJKT: dpopJKT, ExpiresAt: grant.ExpiresAt}
	refreshToken, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth refresh token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	response, err := s.issueTokenResponse(user, entry.Resource, entry.Scope, grantID, refreshToken, dpopJKT)
	if err != nil {
		slog.WarnContext(r.Context(), "sign oauth authorization-code token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	redeemed := false
	proofRejected := false
	s.mu.Lock()
	err = s.state.transact(func(next *storeFile) bool {
		current, found := next.Codes[codeHash]
		if !found || current != entry {
			return false
		}
		client, found := next.Clients[entry.ClientID]
		if !found {
			return false
		}
		if !reserveDPoPBinding(next, binding, time.Now()) {
			proofRejected = true
			return false
		}
		grant.ClientName = clientDisplayName(&client)
		delete(next.Codes, codeHash)
		next.Grants[grant.ID] = grant
		next.RefreshTokens[oauth.RefreshTokenKey(refreshToken)] = refreshEntry
		redeemed = true
		return true
	})
	s.mu.Unlock()
	if err != nil {
		slog.WarnContext(r.Context(), "exchange oauth authorization code", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not exchange authorization code")
		return
	}
	if proofRejected {
		s.writeInvalidDPoPProof(w, r, "dpop proof has already been used")
		return
	}
	if !redeemed {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
		return
	}
	s.recordAudit(r, entry.UserID, "oauth/token", entry.ClientID, "allow", "issued", map[string]any{
		"grantID":  grantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeTokenResponse(w, &response)
}

func (s *Server) handleOAuthRefreshToken(w http.ResponseWriter, r *http.Request, binding dpopBinding) { //nolint:gocritic // Immutable proof values are passed together to preserve their binding.
	dpopJKT := binding.jkt
	refreshToken := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")
	s.mu.Lock()
	entry, candidate := s.state.RefreshTokens[oauth.RefreshTokenKey(refreshToken)]
	grant, grantOK := s.state.Grants[entry.GrantID]
	s.mu.Unlock()
	if candidate && subtle.ConstantTimeCompare([]byte(entry.DPoPJKT), []byte(dpopJKT)) != 1 {
		s.writeInvalidDPoPProof(w, r, "dpop proof key does not match refresh token binding")
		return
	}
	candidate = candidate && entry.ClientID == clientID && entry.Resource == s.resourceURL && entry.UsedAt.IsZero() && entry.RevokedAt.IsZero() && time.Now().Before(entry.ExpiresAt) && grantOK && grant.Resource == s.resourceURL && grant.RevokedAt.IsZero() && time.Now().Before(grant.ExpiresAt)
	var response oauth.TokenResponse
	nextRefreshToken := ""
	if candidate {
		user, found := s.findUserByID(entry.UserID)
		if !found {
			oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
			return
		}
		var err error
		nextRefreshToken, err = randomToken()
		if err == nil {
			response, err = s.issueTokenResponse(user, entry.Resource, entry.Scope, entry.GrantID, nextRefreshToken, entry.DPoPJKT)
		}
		if err != nil {
			slog.WarnContext(r.Context(), "prepare oauth refresh response", "err", err)
			oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
			return
		}
	}
	result, exchanged, err := s.exchangeRefreshToken(refreshToken, clientID, entry.UserID, nextRefreshToken, binding)
	if err != nil {
		slog.WarnContext(r.Context(), "exchange oauth refresh token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not rotate refresh token")
		return
	}
	if result == refreshExchangeReused {
		s.recordAudit(r, exchanged.UserID, "oauth/token", clientID, "deny", "reuse_detected", map[string]any{"grantID": exchanged.GrantID})
	}
	if result == refreshExchangeDPoPRejected {
		s.writeInvalidDPoPProof(w, r, "dpop proof has already been used")
		return
	}
	if result != refreshExchangeRotated {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	s.recordAudit(r, exchanged.UserID, "oauth/token", clientID, "allow", "refreshed", map[string]any{
		"grantID":  exchanged.GrantID,
		"resource": exchanged.Resource,
		"scope":    exchanged.Scope,
	})
	s.writeTokenResponse(w, &response)
}

func (s *Server) issueTokenResponse(user oauth.User, resource, scope, grantID, refreshToken, dpopJKT string) (oauth.TokenResponse, error) {
	if resource != s.resourceURL {
		return oauth.TokenResponse{}, errors.New("oauth: token resource does not match configured protected resource")
	}
	var accessToken string
	var err error
	if dpopJKT == "" {
		accessToken, err = s.tokens.IssueAccessToken(s.issuer, user, resource, scope, grantID)
	} else {
		accessToken, err = s.tokens.IssueDPoPAccessToken(s.issuer, user, resource, scope, grantID, dpopJKT)
	}
	if err != nil {
		return oauth.TokenResponse{}, err
	}
	tokenType := oauth.TokenTypeBearer
	if dpopJKT != "" {
		tokenType = DPoPTokenType
	}
	return oauth.TokenResponse{AccessToken: accessToken, TokenType: tokenType, ExpiresIn: int64(s.accessTokenTTL.Seconds()), RefreshToken: refreshToken, Scope: scope}, nil
}

func (s *Server) writeTokenResponse(w http.ResponseWriter, response *oauth.TokenResponse) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSONResponse(w, response)
}

func (s *Server) handleOAuthIntrospect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if s.introspectionAuth == nil {
		oauth.WriteError(w, http.StatusUnauthorized, "invalid_client", "introspection authentication required")
		return
	}
	principal, ok := s.introspectionAuth(r)
	if !ok {
		oauth.WriteError(w, http.StatusUnauthorized, "invalid_client", "introspection authentication required")
		return
	}
	if !s.rateLimitClient(w, r, principal.ClientID) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		writeJSONResponse(w, oauth.IntrospectionResponse{Active: false})
		return
	}
	token := r.PostForm.Get("token")
	hint := r.PostForm.Get("token_type_hint")
	if token == "" {
		writeJSONResponse(w, oauth.IntrospectionResponse{Active: false})
		return
	}

	switch hint {
	case "refresh_token":
		s.introspectRefreshToken(w, token, principal)
	default:
		s.introspectAccessToken(w, token, principal)
	}
}

// introspectAccessToken introspects a JWT access token.
func (s *Server) introspectAccessToken(w http.ResponseWriter, token string, principal IntrospectionPrincipal) {
	claims, err := s.tokens.VerifyAccessToken(token, s.issuer, s.resourceURL, time.Now(), s.touchGrant, s.session)
	if err != nil || (principal.ClientID != "" && claims.ClientID != principal.ClientID) {
		writeJSONResponse(w, oauth.IntrospectionResponse{Active: false})
		return
	}
	writeJSONResponse(w, oauth.IntrospectionResponse{
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
func (s *Server) introspectRefreshToken(w http.ResponseWriter, token string, principal IntrospectionPrincipal) {
	now := time.Now()
	tokenHash := oauth.RefreshTokenKey(token)
	s.mu.Lock()
	entry, ok := s.state.RefreshTokens[tokenHash]
	if !ok || (principal.ClientID != "" && entry.ClientID != principal.ClientID) || !entry.RevokedAt.IsZero() || !entry.UsedAt.IsZero() || now.After(entry.ExpiresAt) {
		s.mu.Unlock()
		writeJSONResponse(w, oauth.IntrospectionResponse{Active: false})
		return
	}
	grant, grantOK := s.state.Grants[entry.GrantID]
	s.mu.Unlock()
	if !grantOK || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		writeJSONResponse(w, oauth.IntrospectionResponse{Active: false})
		return
	}
	writeJSONResponse(w, oauth.IntrospectionResponse{
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
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
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
		userID, err = s.revokeAccessToken(token)
	default:
		userID, err = s.revokeRefreshToken(token, clientID)
		if err == nil && userID == "" {
			userID, err = s.revokeAccessToken(token)
		}
	}
	if err != nil {
		slog.WarnContext(r.Context(), "revoke oauth token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not revoke token")
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
func (s *Server) revokeAccessToken(token string) (string, error) {
	claims, verr := s.tokens.verifyClaims(token, s.issuer, s.resourceURL, time.Now())
	if verr != nil {
		return "", nil //nolint:nilerr // RFC 7009 requires the endpoint not to disclose invalid tokens.
	}
	if claims.GrantID == "" {
		return claims.Subject, nil // no grant to revoke
	}
	now := time.Now()
	s.mu.Lock()
	err := s.state.transact(func(next *storeFile) bool {
		grant, ok := next.Grants[claims.GrantID]
		if !ok || !grant.RevokedAt.IsZero() {
			return false
		}
		grant.RevokedAt = now
		next.Grants[claims.GrantID] = grant
		for tokenHash := range next.RefreshTokens {
			entry := next.RefreshTokens[tokenHash]
			if entry.GrantID == claims.GrantID && entry.RevokedAt.IsZero() {
				entry.RevokedAt = now
				next.RefreshTokens[tokenHash] = entry
			}
		}
		return true
	})
	s.mu.Unlock()
	return claims.Subject, err
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
	q.Set("iss", s.issuer)
	redirectURL.RawQuery = q.Encode()
	http.Redirect(w, r, redirectURL.String(), http.StatusSeeOther)
}

func (s *Server) validateAuthorizeRequest(r *http.Request) error {
	return s.validateAuthorizeForm(r.URL.Query())
}

// validateAuthorizeForm validates OAuth authorization request parameters.
//
// redirect_uri validation uses exact string match per RFC 6819 §4.1.2 and
// RFC 9700 §4.1.2: redirect URIs must be compared using simple string
// comparison as defined in [RFC3986] Section 6.2.1.
func (s *Server) validateAuthorizeForm(values url.Values) error {
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
	if resource != s.resourceURL {
		return errors.New("resource must match the protected resource")
	}
	if _, err := s.normalizeScope(values.Get("scope")); err != nil {
		return err
	}
	return nil
}

type refreshExchangeResult uint8

const (
	refreshExchangeUnknown refreshExchangeResult = iota
	refreshExchangeRotated
	refreshExchangeReused
	refreshExchangeDPoPRejected
)

func (s *Server) exchangeRefreshToken(token, clientID, userID, nextToken string, binding dpopBinding) (refreshExchangeResult, RefreshToken, error) { //nolint:gocritic // Immutable proof values are passed together to preserve their binding.
	now := time.Now()
	tokenHash := oauth.RefreshTokenKey(token)
	nextTokenHash := oauth.RefreshTokenKey(nextToken)
	result := refreshExchangeUnknown
	var exchanged RefreshToken
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.state.transact(func(state *storeFile) bool {
		entry, found := state.RefreshTokens[tokenHash]
		if !found {
			return false
		}
		exchanged = entry
		grant, grantFound := state.Grants[entry.GrantID]
		if entry.Resource != s.resourceURL || (grantFound && grant.Resource != s.resourceURL) {
			if grantFound {
				return revokeGrant(state, entry.GrantID, now)
			}
			if entry.RevokedAt.IsZero() {
				entry.RevokedAt = now
				state.RefreshTokens[tokenHash] = entry
				return true
			}
			return false
		}
		if entry.ClientID != clientID {
			return false
		}
		if !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() {
			result = refreshExchangeReused
			return revokeGrant(state, entry.GrantID, now)
		}
		if !grantFound || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) || now.After(entry.ExpiresAt) || userID == "" || entry.UserID != userID || nextToken == "" {
			return false
		}
		if !reserveDPoPBinding(state, binding, now) {
			result = refreshExchangeDPoPRejected
			return false
		}
		entry.UsedAt = now
		state.RefreshTokens[tokenHash] = entry
		nextExpiry := now.Add(s.refreshTokenTTL)
		exchanged = RefreshToken{GrantID: entry.GrantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, DPoPJKT: entry.DPoPJKT, ExpiresAt: nextExpiry}
		state.RefreshTokens[nextTokenHash] = exchanged
		grant.LastUsedAt = now
		grant.ExpiresAt = nextExpiry
		state.Grants[grant.ID] = grant
		result = refreshExchangeRotated
		return true
	})
	return result, exchanged, err
}

func (s *Server) revokeRefreshToken(token, clientID string) (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tokenHash := oauth.RefreshTokenKey(token)
	var userID string
	err := s.state.transact(func(next *storeFile) bool {
		entry, ok := next.RefreshTokens[tokenHash]
		if !ok || entry.ClientID != clientID || !entry.RevokedAt.IsZero() {
			return false
		}
		userID = entry.UserID
		return revokeGrant(next, entry.GrantID, now)
	})
	return userID, err
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
	if grant.Resource != s.resourceURL {
		err := s.state.transact(func(next *storeFile) bool {
			return revokeGrant(next, grantID, now)
		})
		return false, "", err
	}
	// Bearer validation is deliberately read-only. Refresh rotation records the
	// durable last-use timestamp without rewriting the complete store on every
	// protected-resource request.
	return true, grant.ClientID, nil
}

func (s *Server) pruneExpiredRefreshTokens(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.transact(func(next *storeFile) bool {
		return pruneExpiredStore(next, now)
	})
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
	return s.state.transact(func(next *storeFile) bool {
		next.Clients[client.ID] = *client
		return true
	})
}

func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimitIP(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req oauth.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration JSON")
		return
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = oauth.TokenEndpointAuthNone
	}
	if method != oauth.TokenEndpointAuthNone {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, redirectURI := range req.RedirectURIs {
		if !validRedirectURI(redirectURI) {
			oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be https or localhost http")
			return
		}
	}
	clientID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth client id", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	now := time.Now()
	client := Client{ID: s.clientIDPrefix + clientID, Name: req.ClientName, RedirectURIs: req.RedirectURIs, TokenEndpointAuthMethod: method, CreatedAt: now}
	if err := s.registerClient(&client); err != nil {
		slog.WarnContext(r.Context(), "save oauth client registration", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	issuer := s.issuer
	regToken, err := s.tokens.IssueRegistrationAccessToken(issuer, client.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "issue registration access token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue registration access token")
		return
	}
	resp := oauth.RegisterResponse{
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
	token := oauth.BearerToken(r)
	if token == "" {
		return errors.New("missing registration access token")
	}
	audience := s.issuer + "/oauth/register"
	tokenClientID, err := s.tokens.VerifyRegistrationAccessToken(token, s.issuer, audience, time.Now())
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
		oauth.WriteError(w, http.StatusNotFound, "invalid_client", "client not found")
		return
	}
	if err := s.verifyRegistrationAccessToken(r, clientID); err != nil {
		oauth.WriteError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}
	resp := oauth.RegisterResponse{
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
		oauth.WriteError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req oauth.UpdateClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration JSON")
		return
	}
	s.mu.Lock()
	client, ok := s.state.Clients[clientID]
	if !ok {
		s.mu.Unlock()
		oauth.WriteError(w, http.StatusNotFound, "invalid_client", "client not found")
		return
	}
	if req.ClientName != nil {
		client.Name = *req.ClientName
	}
	if req.RedirectURIs != nil {
		for _, uri := range *req.RedirectURIs {
			if !validRedirectURI(uri) {
				s.mu.Unlock()
				oauth.WriteError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be https or localhost http")
				return
			}
		}
		client.RedirectURIs = *req.RedirectURIs
	}
	if req.TokenEndpointAuthMethod != nil {
		method := *req.TokenEndpointAuthMethod
		if method != oauth.TokenEndpointAuthNone {
			s.mu.Unlock()
			oauth.WriteError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
			return
		}
		client.TokenEndpointAuthMethod = method
	}
	err := s.state.transact(func(next *storeFile) bool {
		next.Clients[clientID] = client
		return true
	})
	if err != nil {
		s.mu.Unlock()
		slog.WarnContext(r.Context(), "save oauth client update", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not update client")
		return
	}
	s.mu.Unlock()

	issuer := s.issuer
	regToken, err := s.tokens.IssueRegistrationAccessToken(issuer, clientID)
	if err != nil {
		slog.WarnContext(r.Context(), "issue registration access token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue registration access token")
		return
	}
	resp := oauth.RegisterResponse{
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
		oauth.WriteError(w, http.StatusUnauthorized, "invalid_client", "client not found")
		return
	}
	if err := s.verifyRegistrationAccessToken(r, clientID); err != nil {
		oauth.WriteError(w, http.StatusUnauthorized, "invalid_token", err.Error())
		return
	}
	s.mu.Lock()
	err := s.state.transact(func(next *storeFile) bool {
		delete(next.Clients, clientID)
		for key, code := range next.Codes {
			if code.ClientID == clientID {
				delete(next.Codes, key)
			}
		}
		for key, consent := range next.Consents {
			if consent.Params["client_id"] == clientID {
				delete(next.Consents, key)
			}
		}
		for key, deviceCode := range next.DeviceCodes {
			if deviceCode != nil && deviceCode.ClientID == clientID {
				delete(next.DeviceCodes, key)
			}
		}
		for key := range next.RefreshTokens {
			token := next.RefreshTokens[key]
			if token.ClientID == clientID {
				delete(next.RefreshTokens, key)
			}
		}
		for key := range next.Grants {
			grant := next.Grants[key]
			if grant.ClientID == clientID {
				delete(next.Grants, key)
			}
		}
		return true
	})
	s.mu.Unlock()
	if err != nil {
		slog.WarnContext(r.Context(), "save oauth client delete", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not delete client")
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

func validateIssuer(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.ForceQuery || strings.ContainsAny(raw, "?#") || (u.Path != "" && u.Path != "/") {
		return "", errors.New("oauth: Issuer must be an absolute origin URL")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1")) {
		return "", errors.New("oauth: Issuer must use HTTPS except for loopback development origins")
	}
	schemeEnd := strings.IndexByte(raw, ':')
	return raw[:schemeEnd] + "://" + u.Host, nil
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
	metadata := oauth.ProtectedResourceMetadata{
		Resource:             s.resourceURL,
		AuthorizationServers: []string{s.issuer},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		http.Error(w, "encode metadata", http.StatusInternalServerError)
	}
}

func (s *Server) verifyBearer(_ *http.Request, token string) (*oauth.BearerClaims, error) {
	return s.tokens.VerifyAccessToken(token, s.issuer, s.resourceURL, time.Now(), s.touchGrant, s.session)
}

func (s *Server) issueDPoPNonce() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	payload := strconv.FormatInt(time.Now().Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(random[:])
	mac := hmac.New(sha256.New, s.dpopNonceKey[:])
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(append([]byte(payload), mac.Sum(nil)...)), nil
}

func (s *Server) validateDPoPNonce(nonce string) time.Time {
	if nonce == "" {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) <= sha256.Size {
		return time.Time{}
	}
	payload, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, s.dpopNonceKey[:])
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return time.Time{}
	}
	now := time.Now()
	timestamp, _, found := strings.Cut(string(payload), ".")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || !found {
		return time.Time{}
	}
	issuedAt := time.Unix(seconds, 0)
	expiresAt := issuedAt.Add(s.dpopNonceTTL)
	if issuedAt.After(now.Add(defaultDPoPMaxAge)) || !now.Before(expiresAt) {
		return time.Time{}
	}
	return expiresAt
}

func reserveDPoPBinding(next *storeFile, binding dpopBinding, now time.Time) bool { //nolint:gocritic // Immutable proof values are passed together to preserve their binding.
	if binding.jkt == "" {
		return true
	}
	if binding.jti == "" {
		return false
	}
	proofKey := oauth.RefreshTokenKey(binding.jkt + "\x00" + binding.jti)
	pruneExpiredStore(next, now)
	if expiresAt, found := next.DPoPProofs[proofKey]; found && now.Before(expiresAt) {
		return false
	}
	if len(next.DPoPProofs) >= maxDPoPJTIEntries {
		return false
	}
	if binding.nonce != "" {
		nonceKey := oauth.RefreshTokenKey(binding.nonce)
		if _, used := next.DPoPNonces[nonceKey]; used || len(next.DPoPNonces) >= maxDPoPJTIEntries {
			return false
		}
		next.DPoPNonces[nonceKey] = binding.nonceExpiresAt
	}
	deadline := time.Unix(binding.iat, 0).Add(defaultDPoPMaxAge + time.Second)
	if !now.Before(deadline) {
		return false
	}
	next.DPoPProofs[proofKey] = deadline
	return true
}

func (s *Server) reserveDPoPBinding(binding dpopBinding) (bool, error) { //nolint:gocritic // Immutable proof values are passed together to preserve their binding.
	reserved := false
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.state.transact(func(next *storeFile) bool {
		reserved = reserveDPoPBinding(next, binding, time.Now())
		return reserved
	})
	return reserved, err
}

func (s *Server) writeInvalidDPoPProof(w http.ResponseWriter, r *http.Request, description string) {
	if nonce, err := s.issueDPoPNonce(); err != nil {
		slog.ErrorContext(r.Context(), "issue dpop nonce", "err", err)
	} else {
		w.Header().Set("DPoP-Nonce", nonce)
	}
	oauth.WriteError(w, http.StatusBadRequest, "invalid_dpop_proof", description)
}

func (s *Server) writeUnauthorizedDPoP(w http.ResponseWriter, r *http.Request, description string) {
	if nonce, err := s.issueDPoPNonce(); err != nil {
		slog.ErrorContext(r.Context(), "issue dpop nonce", "err", err)
	} else {
		w.Header().Set("DPoP-Nonce", nonce)
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`DPoP error="invalid_token", error_description=%q`, description))
	writeUnauthorizedJSON(w)
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == s.resourceURLPath && s.resourceMetadataURLPath != "" {
		resourceMetadataURL := s.issuer + s.resourceMetadataURLPath
		w.Header().Set("WWW-Authenticate", oauth.BearerChallenge(resourceMetadataURL, strings.Join(s.supportedScopes, " ")))
	}
	writeUnauthorizedJSON(w)
}

func (s *Server) handleOAuthEndSession(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		if loginURL := s.ui.LoginStartURL(r); loginURL != "" {
			http.Redirect(w, r, loginURL, http.StatusFound) //nolint:gosec // loginURL produced by the caller's AuthorizationUI
			return
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

	callerRedirect := s.session.EndSession(r.Context(), r, user)

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

func (s *Server) handleOAuthDeviceAuthorization(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if clientID == "" {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "missing client_id")
		return
	}
	client := s.oauthClient(clientID)
	if client.ID == "" {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}
	scope, err := s.normalizeScope(r.PostForm.Get("scope"))
	if err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	deviceCode, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate device code", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not generate device code")
		return
	}
	userCode := generateUserCode()
	now := time.Now()
	expiresAt := now.Add(10 * time.Minute)

	dc := &DeviceCode{
		DeviceCode:  deviceCode,
		UserCode:    userCode,
		UserCodeKey: oauth.RefreshTokenKey(userCode),
		ClientID:    clientID,
		Scope:       scope,
		Status:      "pending",
		ExpiresAt:   expiresAt,
		IssuedAt:    now,
	}
	s.mu.Lock()
	err = s.state.transact(func(next *storeFile) bool {
		next.DeviceCodes[oauth.RefreshTokenKey(deviceCode)] = dc
		return true
	})
	if err != nil {
		s.mu.Unlock()
		slog.WarnContext(r.Context(), "save device code", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not save device code")
		return
	}
	s.mu.Unlock()

	issuer := s.issuer
	resp := oauth.DeviceAuthorizationResponse{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         issuer + "/oauth/device",
		VerificationURIComplete: issuer + "/oauth/device?user_code=" + userCode,
		ExpiresIn:               600,
		Interval:                5,
	}
	writeJSONResponse(w, &resp)
}

func (s *Server) handleOAuthDevicePage(w http.ResponseWriter, r *http.Request) {
	userCode := r.URL.Query().Get("user_code")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Simple inline HTML; user_code is strictly uppercase alphanumeric.
	escaped := html.EscapeString(userCode)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>Device Authorization</title></head><body>
		<h1>Device Authorization</h1>
		<form method="post" action="/oauth/device">
			<label for="user_code">Enter the code shown on your device:</label>
			<input type="text" name="user_code" id="user_code" autocomplete="off" value="` + escaped + `" maxlength="8" pattern="[A-Z0-9]{8}" required>
			<button type="submit">Authorize</button>
		</form>
	</body></html>`))
}

func (s *Server) handleOAuthDeviceApprove(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentRequestUser(r.Context())
	if !ok {
		if loginURL := s.ui.LoginStartURL(r); loginURL != "" {
			http.Redirect(w, r, loginURL, http.StatusFound) //nolint:gosec // loginURL produced by the caller's AuthorizationUI
			return
		}
		oauth.WriteError(w, http.StatusUnauthorized, "login_required", "log in before authorizing device")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	userCode := r.PostForm.Get("user_code")
	if userCode == "" {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "missing user_code")
		return
	}
	s.mu.Lock()
	var dc *DeviceCode
	userCodeKey := oauth.RefreshTokenKey(userCode)
	err := s.state.transact(func(next *storeFile) bool {
		for _, d := range next.DeviceCodes {
			if d != nil && d.UserCodeKey == userCodeKey && d.Status == "pending" && time.Now().Before(d.ExpiresAt) {
				dc = d
				d.Status = "approved"
				d.UserID = user.ID
				return true
			}
		}
		return false
	})
	if dc == nil && err == nil {
		s.mu.Unlock()
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid or expired user code")
		return
	}
	if err != nil {
		s.mu.Unlock()
		slog.WarnContext(r.Context(), "save device approval", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not approve device")
		return
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<html><body><p>Device authorized. You may close this page.</p></body></html>`))
}

func (s *Server) handleOAuthDeviceCodeToken(w http.ResponseWriter, r *http.Request, binding dpopBinding) { //nolint:gocritic // Immutable proof values are passed together to preserve their binding.
	dpopJKT := binding.jkt
	deviceCode := r.PostForm.Get("device_code")
	clientID := r.PostForm.Get("client_id")
	if deviceCode == "" || clientID == "" {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_request", "missing device_code or client_id")
		return
	}
	codeHash := oauth.RefreshTokenKey(deviceCode)
	s.mu.Lock()
	dc, ok := s.state.DeviceCodes[codeHash]
	if !ok || dc.ClientID != clientID {
		s.mu.Unlock()
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid device_code")
		return
	}
	if time.Now().After(dc.ExpiresAt) {
		err := s.state.transact(func(next *storeFile) bool {
			delete(next.DeviceCodes, codeHash)
			return true
		})
		s.mu.Unlock()
		if err != nil {
			slog.WarnContext(r.Context(), "delete expired device code", "err", err)
			oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not consume device code")
			return
		}
		oauth.WriteError(w, http.StatusBadRequest, "expired_token", "device_code expired")
		return
	}
	if binding.jkt != "" {
		reserved := false
		err := s.state.transact(func(next *storeFile) bool {
			reserved = reserveDPoPBinding(next, binding, time.Now())
			return reserved
		})
		if err != nil {
			s.mu.Unlock()
			slog.WarnContext(r.Context(), "reserve device dpop proof", "err", err)
			oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not validate dpop proof")
			return
		}
		if !reserved {
			s.mu.Unlock()
			s.writeInvalidDPoPProof(w, r, "dpop proof has already been used")
			return
		}
	}
	switch dc.Status {
	case "pending":
		s.mu.Unlock()
		oauth.WriteError(w, http.StatusBadRequest, "authorization_pending", "user has not yet authorized the device")
		return
	case "denied":
		err := s.state.transact(func(next *storeFile) bool {
			delete(next.DeviceCodes, codeHash)
			return true
		})
		s.mu.Unlock()
		if err != nil {
			slog.WarnContext(r.Context(), "consume denied device code", "err", err)
			oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not consume device code")
			return
		}
		oauth.WriteError(w, http.StatusBadRequest, "access_denied", "user denied the authorization request")
		return
	case "approved":
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "unknown device code status")
		return
	}

	user, ok := s.findUserByID(dc.UserID)
	if !ok {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	grantID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate device grant id", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue token")
		return
	}
	now := time.Now()
	grant := Grant{ID: grantID, UserID: dc.UserID, ClientID: dc.ClientID, Resource: s.resourceURL, Scope: dc.Scope, CreatedAt: now, ExpiresAt: now.Add(s.refreshTokenTTL)}
	refreshEntry := RefreshToken{GrantID: grantID, UserID: dc.UserID, ClientID: dc.ClientID, Resource: s.resourceURL, Scope: dc.Scope, DPoPJKT: dpopJKT, ExpiresAt: grant.ExpiresAt}
	refreshToken, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate device refresh token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	response, err := s.issueTokenResponse(user, grant.Resource, dc.Scope, grantID, refreshToken, dpopJKT)
	if err != nil {
		slog.WarnContext(r.Context(), "sign device access token", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	consumed := false
	s.mu.Lock()
	err = s.state.transact(func(next *storeFile) bool {
		current, found := next.DeviceCodes[codeHash]
		client, clientFound := next.Clients[dc.ClientID]
		if !found || current == nil || current.Status != "approved" || current.UserID != dc.UserID || current.ClientID != dc.ClientID || !clientFound {
			return false
		}
		grant.ClientName = clientDisplayName(&client)
		delete(next.DeviceCodes, codeHash)
		next.Grants[grant.ID] = grant
		next.RefreshTokens[oauth.RefreshTokenKey(refreshToken)] = refreshEntry
		consumed = true
		return true
	})
	s.mu.Unlock()
	if err != nil {
		slog.WarnContext(r.Context(), "exchange device code", "err", err)
		oauth.WriteError(w, http.StatusInternalServerError, "server_error", "could not exchange device code")
		return
	}
	if !consumed {
		oauth.WriteError(w, http.StatusBadRequest, "invalid_grant", "invalid device_code")
		return
	}
	s.writeTokenResponse(w, &response)
}

func generateUserCode() string {
	const chars = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	var code [8]byte
	for i := range code {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		code[i] = chars[n.Int64()]
	}
	return string(code[:])
}

func writeUnauthorizedJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"authentication required"}}`))
}
