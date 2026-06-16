// caic OAuth authorization server issuing MCP access tokens to remote clients.

package server

import (
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
	"github.com/caic-xyz/caic/backend/internal/mcp"
)

const (
	mcpOAuthAccessTokenTTL  = time.Hour
	mcpOAuthAuthCodeTTL     = 10 * time.Minute
	mcpOAuthRefreshTokenTTL = 30 * 24 * time.Hour
)

//go:embed mcp_consent.html
var mcpConsentHTML string

var (
	mcpOAuthConsentTemplate = template.Must(template.New("mcp-oauth-consent").Parse(mcpConsentHTML))
	mcpScopeLabels          = map[string]string{
		mcpScopeRead:       "Use basic MCP tools including usage and non-task resources",
		mcpScopeTasksRead:  "Read task information",
		mcpScopeTasksWrite: "Create and manage tasks",
		mcpScopeTasksAdmin: "Administer tasks (cancel, delete)",
		mcpScopeReposWrite: "Manage repositories",
	}
)

// mcpConsentTemplateData is the server-rendered OAuth consent page model.
type mcpConsentTemplateData struct {
	Action        string
	ConsentToken  string
	ClientName    string
	ClientID      string
	RedirectURI   string
	Username      string
	UserInitial   string
	ProviderLabel string
	Resource      string
	ScopeItems    []mcpScopeItem
}

// mcpScopeItem holds a scope identifier and its human-readable label.
type mcpScopeItem struct {
	ID      string
	Label   string
	Checked bool
}

func mcpScopeItems(scope string) []mcpScopeItem {
	parts := strings.Fields(scope)
	if len(parts) == 0 {
		parts = []string{mcpScopeRead}
	}
	items := make([]mcpScopeItem, 0, len(parts))
	for _, p := range parts {
		label := mcpScopeLabels[p]
		if label == "" {
			label = p
		}
		items = append(items, mcpScopeItem{ID: p, Label: label, Checked: mcpScopeDefaultChecked(p)})
	}
	return items
}

func mcpScopeDefaultChecked(scope string) bool {
	return scope == mcpScopeRead || scope == mcpScopeTasksRead
}

func mcpUserInitial(username string) string {
	for _, r := range strings.TrimSpace(username) {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func mcpProviderLabel(provider string) string {
	switch provider {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "":
		return "unknown provider"
	default:
		return provider
	}
}

// mcpOAuthServer stores caic's OAuth state for remote MCP clients.
//
// Clients use Authorization Code with PKCE S256. Authorization always requires
// a caic web session, so an unauthenticated browser request is sent through caic
// login before consent is shown. Access tokens are caic-scoped JWTs. Refresh
// tokens are caic-scoped opaque tokens, persisted as hashes, and rotated on
// every refresh grant. Dynamic client registrations and user grants are durable
// so clients survive restarts and users can revoke individual MCP client grants
// from settings.
type mcpOAuthServer struct {
	mu                    sync.Mutex
	clients               map[string]mcpOAuthClient
	codes                 map[string]mcpOAuthCode
	consents              map[string]mcpOAuthConsent
	refreshTokens         map[string]mcpOAuthRefreshToken
	grants                map[string]mcpOAuthGrant
	refreshTokenStorePath string
	key                   *rsa.PrivateKey
	kid                   string
}

type mcpOAuthClient struct {
	ID                      string    `json:"id"`
	Name                    string    `json:"name"`
	RedirectURIs            []string  `json:"redirectURIs"`
	TokenEndpointAuthMethod string    `json:"tokenEndpointAuthMethod"`
	CreatedAt               time.Time `json:"createdAt"`
}

type mcpOAuthConsent struct {
	UserID    string
	Values    url.Values
	ExpiresAt time.Time
}

type mcpOAuthCode struct {
	UserID        string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	Scope         string
	ExpiresAt     time.Time
}

type mcpOAuthRefreshToken struct {
	GrantID   string    `json:"grantID,omitempty"`
	UserID    string    `json:"userID"`
	ClientID  string    `json:"clientID"`
	Resource  string    `json:"resource"`
	Scope     string    `json:"scope"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedAt    time.Time `json:"usedAt,omitzero"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

type mcpOAuthGrant struct {
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

type mcpOAuthRefreshTokenFile struct {
	Version int                          `json:"version"`
	Clients []mcpOAuthClient             `json:"clients,omitempty"`
	Tokens  []mcpOAuthRefreshTokenRecord `json:"tokens"`
	Grants  []mcpOAuthGrant              `json:"grants,omitempty"`
}

type mcpOAuthRefreshTokenRecord struct {
	mcpOAuthRefreshToken

	TokenHash string `json:"tokenHash"`
}

func newMCPOAuthServer(keyPEM []byte, kid, refreshTokenStorePath string) (*mcpOAuthServer, error) {
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
	clients, refreshTokens, grants, err := loadMCPOAuthRefreshTokens(refreshTokenStorePath)
	if err != nil {
		return nil, err
	}
	return &mcpOAuthServer{clients: clients, codes: map[string]mcpOAuthCode{}, consents: map[string]mcpOAuthConsent{}, refreshTokens: refreshTokens, grants: grants, refreshTokenStorePath: refreshTokenStorePath, key: key, kid: kid}, nil
}

func (s *mcpServer) ensureMCPOAuthServer() error {
	if !s.authEnabled() || s.oauth != nil {
		return nil
	}
	oauthServer, err := newMCPOAuthServer(s.privateKeyPEM, s.keyID, s.refreshTokenStorePath)
	if err != nil {
		return err
	}
	s.oauth = oauthServer
	return nil
}

func (s *mcpServer) handleMCPOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.NotFound(w, r)
		return
	}
	issuer := s.externalBaseURL(r)
	metadata := mcp.OAuthAuthorizationServerMetadata{
		Issuer:                                 issuer,
		AuthorizationEndpoint:                  issuer + "/oauth/authorize",
		TokenEndpoint:                          issuer + "/oauth/token",
		JWKSURI:                                issuer + "/oauth/jwks",
		RegistrationEndpoint:                   issuer + "/oauth/register",
		RevocationEndpoint:                     issuer + "/oauth/revoke",
		ResponseTypesSupported:                 []string{mcp.OAuthResponseTypeCode},
		GrantTypesSupported:                    []string{mcp.OAuthGrantAuthorizationCode, mcp.OAuthGrantRefreshToken},
		CodeChallengeMethodsSupported:          []string{mcp.OAuthCodeChallengeS256},
		TokenEndpointAuthMethodsSupported:      []string{mcp.OAuthTokenEndpointAuthNone},
		RevocationEndpointAuthMethodsSupported: []string{mcp.OAuthTokenEndpointAuthNone},
		ScopesSupported:                        supportedMCPOAuthScopes(),
		AuthorizationResponseIssuerParameterSupported: true,
	}
	writeJSONResponse(w, &metadata, nil)
}

func (s *mcpServer) handleMCPOAuthJWKS(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	pub := s.oauth.key.PublicKey
	resp := mcp.JWKSet{Keys: []mcp.JWK{mcp.RSAJWK(s.oauth.kid, &pub)}}
	writeJSONResponse(w, &resp, nil)
}

func (s *mcpServer) handleMCPOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	if !s.allowMCPRequest(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req mcp.OAuthRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration JSON")
		return
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = mcp.OAuthTokenEndpointAuthNone
	}
	if method != mcp.OAuthTokenEndpointAuthNone {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, redirectURI := range req.RedirectURIs {
		if !validOAuthRedirectURI(redirectURI) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must be https or localhost http")
			return
		}
	}
	clientID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth client id", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	now := time.Now()
	client := mcpOAuthClient{ID: "caic_" + clientID, Name: req.ClientName, RedirectURIs: req.RedirectURIs, TokenEndpointAuthMethod: method, CreatedAt: now}
	if err := s.oauth.registerClient(&client); err != nil {
		slog.WarnContext(r.Context(), "save oauth client registration", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	resp := mcp.OAuthRegisterResponse{ClientID: client.ID, ClientIDIssuedAt: now.Unix(), ClientName: client.Name, RedirectURIs: client.RedirectURIs, TokenEndpointAuthMethod: method}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		slog.WarnContext(r.Context(), "encode oauth registration response", "err", err)
	}
}

func (s *mcpServer) handleMCPOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		if r.Method == http.MethodGet {
			if loginURL := s.mcpLoginStartURL(r); loginURL != "" {
				http.Redirect(w, r, loginURL, http.StatusFound)
				return
			}
		}
		writeOAuthError(w, http.StatusUnauthorized, "login_required", "log in to caic before authorizing MCP access")
		return
	}
	if r.Method == http.MethodGet {
		if err := s.validateAuthorizeRequest(r); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		values := cloneURLValues(r.URL.Query())
		client := s.oauthClient(values.Get("client_id"))
		scope, _ := normalizeMCPScope(values.Get("scope"))
		consentToken, err := randomToken()
		if err != nil {
			slog.WarnContext(r.Context(), "generate oauth consent token", "err", err)
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not start consent")
			return
		}
		s.oauth.mu.Lock()
		s.oauth.consents[consentToken] = mcpOAuthConsent{UserID: user.ID, Values: values, ExpiresAt: time.Now().Add(mcpOAuthAuthCodeTTL)}
		s.oauth.mu.Unlock()
		baseURL := s.externalBaseURL(r)
		writeMCPConsentHeaders(w)
		data := mcpConsentTemplateData{
			Action:        baseURL + "/oauth/authorize",
			ConsentToken:  consentToken,
			ClientName:    clientDisplayName(&client),
			ClientID:      client.ID,
			RedirectURI:   values.Get("redirect_uri"),
			Username:      user.Username,
			UserInitial:   mcpUserInitial(user.Username),
			ProviderLabel: mcpProviderLabel(string(user.Provider)),
			Resource:      values.Get("resource"),
			ScopeItems:    mcpScopeItems(scope),
		}
		if err := mcpOAuthConsentTemplate.Execute(w, data); err != nil {
			slog.WarnContext(r.Context(), "render oauth consent", "err", err)
		}
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	consentToken := r.PostForm.Get("consent_token")
	s.oauth.mu.Lock()
	consent, ok := s.oauth.consents[consentToken]
	if ok {
		delete(s.oauth.consents, consentToken)
	}
	s.oauth.mu.Unlock()
	if !ok || consent.UserID != user.ID || time.Now().After(consent.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid or expired consent")
		return
	}
	values := consent.Values
	if err := s.validateAuthorizeForm(r, values); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	switch r.PostForm.Get("decision") {
	case "approve", "":
	case "deny":
		s.recordMCPAuthAudit(r, "", "oauth/authorize", values.Get("client_id"), "deny", "denied", map[string]any{
			"redirectURI": values.Get("redirect_uri"),
			"resource":    values.Get("resource"),
			"scope":       values.Get("scope"),
		})
		s.redirectAuthorizeError(w, r, values, "access_denied", "authorization denied")
		return
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid consent decision")
		return
	}
	code, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate oauth code", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not authorize client")
		return
	}
	scope, err := approveMCPScope(values.Get("scope"), r.PostForm)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	entry := mcpOAuthCode{UserID: user.ID, ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), CodeChallenge: values.Get("code_challenge"), Resource: values.Get("resource"), Scope: scope, ExpiresAt: time.Now().Add(mcpOAuthAuthCodeTTL)}
	s.oauth.mu.Lock()
	s.oauth.codes[code] = entry
	s.oauth.mu.Unlock()
	redirectURL, err := url.Parse(entry.RedirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect URI")
		return
	}
	q := redirectURL.Query()
	q.Set("code", code)
	if state := values.Get("state"); state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.externalBaseURL(r))
	redirectURL.RawQuery = q.Encode()
	s.recordMCPAuthAudit(r, "", "oauth/authorize", entry.ClientID, "allow", "approved", map[string]any{
		"redirectURI": entry.RedirectURI,
		"resource":    entry.Resource,
		"scope":       entry.Scope,
	})
	http.Redirect(w, r, redirectURL.String(), http.StatusSeeOther)
}

func (s *mcpServer) mcpLoginStartURL(r *http.Request) string {
	if s.authHandlers == nil {
		return ""
	}
	provider := ""
	if s.authHandlers.githubOAuth != nil && s.authHandlers.githubOAuth.RedirectURI() != "" {
		provider = "github"
	} else if s.authHandlers.gitlabOAuth != nil && s.authHandlers.gitlabOAuth.RedirectURI() != "" {
		provider = "gitlab"
	}
	if provider == "" {
		return ""
	}
	values := url.Values{"next": {r.URL.RequestURI()}}
	return "/auth/" + provider + "/start?" + values.Encode()
}

func (s *mcpServer) handleMCPOAuthToken(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	if !s.allowMCPRequest(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if err := s.oauth.pruneExpiredRefreshTokens(time.Now()); err != nil {
		slog.WarnContext(r.Context(), "prune mcp oauth refresh tokens", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not prune refresh tokens")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case mcp.OAuthGrantAuthorizationCode:
		s.handleMCPOAuthAuthorizationCodeToken(w, r)
	case mcp.OAuthGrantRefreshToken:
		s.handleMCPOAuthRefreshToken(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

func (s *mcpServer) handleMCPOAuthAuthorizationCodeToken(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	s.oauth.mu.Lock()
	entry, ok := s.oauth.codes[code]
	if ok {
		delete(s.oauth.codes, code)
	}
	s.oauth.mu.Unlock()
	if !ok || time.Now().After(entry.ExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
		return
	}
	if r.PostForm.Get("client_id") != entry.ClientID || r.PostForm.Get("redirect_uri") != entry.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client or redirect URI mismatch")
		return
	}
	if resource := r.PostForm.Get("resource"); resource != "" && resource != entry.Resource {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
		return
	}
	if !mcp.VerifyPKCES256(entry.CodeChallenge, r.PostForm.Get("code_verifier")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	user, ok := s.authStore.FindByID(entry.UserID)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	client := s.oauthClient(entry.ClientID)
	grantID, err := randomToken()
	if err != nil {
		slog.WarnContext(r.Context(), "generate mcp oauth grant id", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	now := time.Now()
	grant := mcpOAuthGrant{ID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, ClientName: clientDisplayName(&client), Resource: entry.Resource, Scope: entry.Scope, CreatedAt: now, ExpiresAt: now.Add(mcpOAuthRefreshTokenTTL)}
	refreshEntry := mcpOAuthRefreshToken{GrantID: grantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: grant.ExpiresAt}
	refreshToken, err := s.oauth.issueGrantRefreshToken(&grant, &refreshEntry)
	if err != nil {
		slog.WarnContext(r.Context(), "issue mcp oauth refresh token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	s.recordMCPAuthAudit(r, entry.UserID, "oauth/token", entry.ClientID, "allow", "issued", map[string]any{
		"grantID":  grantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeMCPOAuthTokenResponse(w, r, &user, entry.Resource, entry.Scope, grantID, refreshToken)
}

func (s *mcpServer) handleMCPOAuthRefreshToken(w http.ResponseWriter, r *http.Request) {
	refreshToken := r.PostForm.Get("refresh_token")
	clientID := r.PostForm.Get("client_id")
	entry, ok := s.oauth.validRefreshToken(refreshToken, clientID)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	user, ok := s.authStore.FindByID(entry.UserID)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}
	nextRefreshToken, entry, ok, err := s.oauth.rotateRefreshToken(refreshToken, clientID, entry.UserID)
	if err != nil {
		slog.WarnContext(r.Context(), "rotate mcp oauth refresh token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not rotate refresh token")
		return
	}
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	s.recordMCPAuthAudit(r, entry.UserID, "oauth/token", clientID, "allow", "refreshed", map[string]any{
		"grantID":  entry.GrantID,
		"resource": entry.Resource,
		"scope":    entry.Scope,
	})
	s.writeMCPOAuthTokenResponse(w, r, &user, entry.Resource, entry.Scope, entry.GrantID, nextRefreshToken)
}

func (s *mcpServer) writeMCPOAuthTokenResponse(w http.ResponseWriter, r *http.Request, user *auth.User, resource, scope, grantID, refreshToken string) {
	accessToken, err := s.oauth.issueAccessToken(s.externalBaseURL(r), user, resource, scope, grantID)
	if err != nil {
		slog.WarnContext(r.Context(), "issue mcp oauth token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	resp := mcp.OAuthTokenResponse{AccessToken: accessToken, TokenType: mcp.OAuthTokenTypeBearer, ExpiresIn: int64(mcpOAuthAccessTokenTTL.Seconds()), RefreshToken: refreshToken, Scope: scope}
	writeJSONResponse(w, &resp, nil)
}

func (s *mcpServer) handleMCPOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.oauth == nil {
		http.NotFound(w, r)
		return
	}
	if !s.allowMCPRequest(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	userID, err := s.oauth.revokeRefreshToken(r.PostForm.Get("token"), r.PostForm.Get("client_id"))
	if err != nil {
		slog.WarnContext(r.Context(), "revoke mcp oauth refresh token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not revoke refresh token")
		return
	}
	s.recordMCPAuthAudit(r, userID, "oauth/revoke", r.PostForm.Get("client_id"), "allow", "revoked", nil)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func (s *mcpServer) recordMCPAuthAudit(r *http.Request, userID, operation, name, decision, status string, args any) {
	if s.audit == nil {
		return
	}
	s.audit.record(r.Context(), &mcpAuditEvent{
		UserID:    userID,
		Operation: operation,
		Name:      name,
		Args:      auditValueSummary(args),
		Decision:  decision,
		Status:    status,
	})
}

func (s *mcpServer) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, values url.Values, code, description string) {
	redirectURL, err := url.Parse(values.Get("redirect_uri"))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "invalid redirect URI")
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

func (s *mcpServer) validateAuthorizeRequest(r *http.Request) error {
	return s.validateAuthorizeForm(r, r.URL.Query())
}

func (s *mcpServer) validateAuthorizeForm(r *http.Request, values url.Values) error {
	if values.Get("response_type") != mcp.OAuthResponseTypeCode {
		return errors.New("response_type must be code")
	}
	client := s.oauthClient(values.Get("client_id"))
	if client.ID == "" {
		return errors.New("unknown client_id")
	}
	redirectURI := values.Get("redirect_uri")
	if !hasString(client.RedirectURIs, redirectURI) {
		return errors.New("redirect_uri is not registered")
	}
	if values.Get("code_challenge_method") != mcp.OAuthCodeChallengeS256 || values.Get("code_challenge") == "" {
		return errors.New("S256 PKCE is required")
	}
	resource := values.Get("resource")
	if resource == "" {
		return errors.New("resource is required")
	}
	if resource != s.mcpResourceURL(r) {
		return errors.New("resource must match the caic MCP endpoint")
	}
	if _, err := normalizeMCPScope(values.Get("scope")); err != nil {
		return err
	}
	return nil
}

func (s *mcpServer) oauthClient(id string) mcpOAuthClient {
	if s.oauth == nil {
		return mcpOAuthClient{}
	}
	s.oauth.mu.Lock()
	defer s.oauth.mu.Unlock()
	return s.oauth.clients[id]
}

func (s *mcpOAuthServer) registerClient(client *mcpOAuthClient) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[client.ID] = *client
	return s.saveRefreshTokensLocked()
}

func (s *mcpOAuthServer) issueGrantRefreshToken(grant *mcpOAuthGrant, entry *mcpOAuthRefreshToken) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[grant.ID] = *grant
	s.refreshTokens[mcpOAuthRefreshTokenKey(token)] = *entry
	if err := s.saveRefreshTokensLocked(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *mcpOAuthServer) validRefreshToken(token, clientID string) (mcpOAuthRefreshToken, bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.refreshTokens[mcpOAuthRefreshTokenKey(token)]
	if !ok || entry.ClientID != clientID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return mcpOAuthRefreshToken{}, false
	}
	grant, ok := s.grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return mcpOAuthRefreshToken{}, false
	}
	return entry, true
}

func (s *mcpOAuthServer) rotateRefreshToken(token, clientID, userID string) (nextToken string, next mcpOAuthRefreshToken, ok bool, err error) {
	nextToken, err = randomToken()
	if err != nil {
		return "", mcpOAuthRefreshToken{}, false, err
	}
	now := time.Now()
	tokenHash := mcpOAuthRefreshTokenKey(token)
	nextTokenHash := mcpOAuthRefreshTokenKey(nextToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.refreshTokens[tokenHash]
	if !ok || entry.ClientID != clientID || entry.UserID != userID || !entry.UsedAt.IsZero() || !entry.RevokedAt.IsZero() || now.After(entry.ExpiresAt) {
		return "", mcpOAuthRefreshToken{}, false, nil
	}
	grant, ok := s.grants[entry.GrantID]
	if !ok || !grant.RevokedAt.IsZero() || now.After(grant.ExpiresAt) {
		return "", mcpOAuthRefreshToken{}, false, nil
	}
	entry.UsedAt = now
	s.refreshTokens[tokenHash] = entry
	nextExpiry := now.Add(mcpOAuthRefreshTokenTTL)
	next = mcpOAuthRefreshToken{GrantID: entry.GrantID, UserID: entry.UserID, ClientID: entry.ClientID, Resource: entry.Resource, Scope: entry.Scope, ExpiresAt: nextExpiry}
	s.refreshTokens[nextTokenHash] = next
	grant.LastUsedAt = now
	grant.ExpiresAt = nextExpiry
	s.grants[grant.ID] = grant
	if err := s.saveRefreshTokensLocked(); err != nil {
		return "", mcpOAuthRefreshToken{}, false, err
	}
	return nextToken, next, true, nil
}

func (s *mcpOAuthServer) revokeRefreshToken(token, clientID string) (string, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	tokenHash := mcpOAuthRefreshTokenKey(token)
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

func (s *mcpOAuthServer) revokeUserGrant(userID, grantID string) bool {
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

func (s *mcpOAuthServer) listUserGrants(userID string) []mcpOAuthGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	grants := make([]mcpOAuthGrant, 0, len(s.grants))
	for id := range s.grants {
		grant := s.grants[id]
		if grant.UserID == userID {
			grants = append(grants, grant)
		}
	}
	slices.SortFunc(grants, func(a, b mcpOAuthGrant) int {
		if cmp := b.CreatedAt.Compare(a.CreatedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ID, b.ID)
	})
	return grants
}

func (s *mcpOAuthServer) touchGrant(grantID string, now time.Time) (bool, error) {
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

func (s *mcpOAuthServer) saveRefreshTokens() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveRefreshTokensLocked()
}

func (s *mcpOAuthServer) pruneExpiredRefreshTokens(now time.Time) error {
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

func (s *mcpOAuthServer) saveRefreshTokensLocked() error {
	if s.refreshTokenStorePath == "" {
		return nil
	}
	records := make([]mcpOAuthRefreshTokenRecord, 0, len(s.refreshTokens))
	for tokenHash := range s.refreshTokens {
		records = append(records, mcpOAuthRefreshTokenRecord{TokenHash: tokenHash, mcpOAuthRefreshToken: s.refreshTokens[tokenHash]})
	}
	slices.SortFunc(records, func(a, b mcpOAuthRefreshTokenRecord) int {
		return strings.Compare(a.TokenHash, b.TokenHash)
	})
	grants := slices.Collect(maps.Values(s.grants))
	slices.SortFunc(grants, func(a, b mcpOAuthGrant) int {
		return strings.Compare(a.ID, b.ID)
	})
	clients := slices.Collect(maps.Values(s.clients))
	slices.SortFunc(clients, func(a, b mcpOAuthClient) int {
		return strings.Compare(a.ID, b.ID)
	})
	return saveMCPOAuthRefreshTokens(s.refreshTokenStorePath, clients, records, grants)
}

func loadMCPOAuthRefreshTokens(path string) (clients map[string]mcpOAuthClient, refreshTokens map[string]mcpOAuthRefreshToken, grants map[string]mcpOAuthGrant, err error) {
	clients = map[string]mcpOAuthClient{}
	refreshTokens = map[string]mcpOAuthRefreshToken{}
	grants = map[string]mcpOAuthGrant{}
	if path == "" {
		return clients, refreshTokens, grants, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is app-controlled persistent state.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return clients, refreshTokens, grants, nil
		}
		return nil, nil, nil, fmt.Errorf("read mcp oauth refresh tokens: %w", err)
	}
	var file mcpOAuthRefreshTokenFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, nil, fmt.Errorf("parse mcp oauth refresh tokens: %w", err)
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
		refreshTokens[record.TokenHash] = record.mcpOAuthRefreshToken
	}
	for tokenHash := range refreshTokens {
		token := refreshTokens[tokenHash]
		if token.GrantID == "" {
			grantID := "legacy:" + tokenHash
			token.GrantID = grantID
			refreshTokens[tokenHash] = token
			if _, ok := grants[grantID]; !ok {
				grants[grantID] = mcpOAuthGrant{ID: grantID, UserID: token.UserID, ClientID: token.ClientID, ClientName: token.ClientID, Resource: token.Resource, Scope: token.Scope, CreatedAt: token.ExpiresAt.Add(-mcpOAuthRefreshTokenTTL), ExpiresAt: token.ExpiresAt, RevokedAt: token.RevokedAt}
			}
		}
	}
	return clients, refreshTokens, grants, nil
}

func saveMCPOAuthRefreshTokens(path string, clients []mcpOAuthClient, records []mcpOAuthRefreshTokenRecord, grants []mcpOAuthGrant) error {
	file := mcpOAuthRefreshTokenFile{Version: 3, Clients: clients, Tokens: records, Grants: grants}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp oauth refresh tokens: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create mcp oauth refresh token dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write mcp oauth refresh tokens: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename mcp oauth refresh tokens: %w", err)
	}
	return nil
}

func mcpOAuthRefreshTokenKey(token string) string {
	digest := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (s *mcpOAuthServer) issueAccessToken(issuer string, user *auth.User, audience, scope, grantID string) (string, error) {
	now := time.Now()
	headerJSON, err := json.Marshal(map[string]string{"alg": mcp.JWTAlgRS256, "typ": "JWT", "kid": s.kid})
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
		"exp":      now.Add(mcpOAuthAccessTokenTTL).Unix(),
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

func supportedMCPOAuthScopes() []string {
	return []string{mcpScopeRead, mcpScopeTasksRead, mcpScopeTasksWrite, mcpScopeTasksAdmin, mcpScopeReposWrite}
}

func normalizeMCPScope(scope string) (string, error) {
	parts := strings.Fields(scope)
	if len(parts) == 0 {
		return mcpScopeRead, nil
	}
	for _, part := range parts {
		if _, ok := mcpSupportedScopes[part]; !ok {
			return "", fmt.Errorf("unsupported scope: %s", part)
		}
	}
	return strings.Join(parts, " "), nil
}

func approveMCPScope(requested string, form url.Values) (string, error) {
	normalizedRequested, err := normalizeMCPScope(requested)
	if err != nil {
		return "", err
	}
	if form.Get("scope_form") == "" {
		return normalizedRequested, nil
	}
	allowed := parseMCPScopeSet(normalizedRequested)
	selected := make(map[string]struct{}, len(form["scope"]))
	for _, scope := range form["scope"] {
		if _, ok := mcpSupportedScopes[scope]; !ok {
			return "", fmt.Errorf("unsupported scope: %s", scope)
		}
		if _, ok := allowed[scope]; !ok {
			return "", fmt.Errorf("unrequested scope: %s", scope)
		}
		selected[scope] = struct{}{}
	}
	if len(selected) == 0 {
		return "", errors.New("select at least one scope")
	}
	ordered := supportedMCPOAuthScopes()
	approved := make([]string, 0, len(selected))
	for _, scope := range ordered {
		if _, ok := selected[scope]; ok {
			approved = append(approved, scope)
		}
	}
	return strings.Join(approved, " "), nil
}

func cloneURLValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, entries := range values {
		clone[key] = append([]string(nil), entries...)
	}
	return clone
}

func validOAuthRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
}

func hasString(values []string, needle string) bool {
	return slices.Contains(values, needle)
}

func clientDisplayName(client *mcpOAuthClient) string {
	if client.Name != "" {
		return client.Name
	}
	if client.ID != "" {
		return client.ID
	}
	return "remote MCP client"
}

func randomToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func writeMCPConsentHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	mcp.WriteOAuthError(w, status, code, description)
}
