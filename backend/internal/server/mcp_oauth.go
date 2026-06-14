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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
)

const (
	mcpOAuthAuthorizePath = "/api/caic/v1/oauth/authorize"
	mcpOAuthTokenPath     = "/api/caic/v1/oauth/token" //nolint:gosec // OAuth token endpoint path, not a credential.
	mcpOAuthRegisterPath  = "/api/caic/v1/oauth/register"
	mcpOAuthJWKSPath      = "/api/caic/v1/oauth/jwks"

	mcpOAuthAccessTokenTTL = time.Hour
	mcpOAuthAuthCodeTTL    = 10 * time.Minute
)

//go:embed mcp_consent.html
var mcpConsentHTML string

var (
	mcpOAuthConsentTemplate = template.Must(template.New("mcp-oauth-consent").Parse(mcpConsentHTML))
	mcpScopeLabels          = map[string]string{
		mcpScopeRead:       "Use basic MCP tools including usage, web search/fetch, and non-task resources",
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
	ID    string
	Label string
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
		items = append(items, mcpScopeItem{ID: p, Label: label})
	}
	return items
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

type mcpOAuthServer struct {
	mu       sync.Mutex
	clients  map[string]mcpOAuthClient
	codes    map[string]mcpOAuthCode
	consents map[string]mcpOAuthConsent
	key      *rsa.PrivateKey
	kid      string
}

type mcpOAuthClient struct {
	ID                      string
	Name                    string
	RedirectURIs            []string
	TokenEndpointAuthMethod string
	CreatedAt               time.Time
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

func newMCPOAuthServer(keyPEM []byte, kid string) (*mcpOAuthServer, error) {
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
	return &mcpOAuthServer{clients: map[string]mcpOAuthClient{}, codes: map[string]mcpOAuthCode{}, consents: map[string]mcpOAuthConsent{}, key: key, kid: kid}, nil
}

func (s *Router) ensureMCPOAuthServer() error {
	if !s.authEnabled() || s.mcpOAuth != nil {
		return nil
	}
	oauthServer, err := newMCPOAuthServer(s.mcpOAuthPrivateKeyPEM, s.mcpOAuthKeyID)
	if err != nil {
		return err
	}
	s.mcpOAuth = oauthServer
	return nil
}

func (s *Router) handleMCPOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.NotFound(w, r)
		return
	}
	issuer := s.externalBaseURL(r)
	metadata := mcp.OAuthAuthorizationServerMetadata{
		Issuer:                            issuer,
		AuthorizationEndpoint:             issuer + mcpOAuthAuthorizePath,
		TokenEndpoint:                     issuer + mcpOAuthTokenPath,
		JWKSURI:                           issuer + mcpOAuthJWKSPath,
		RegistrationEndpoint:              issuer + mcpOAuthRegisterPath,
		ResponseTypesSupported:            []string{mcp.OAuthResponseTypeCode},
		GrantTypesSupported:               []string{mcp.OAuthGrantAuthorizationCode},
		CodeChallengeMethodsSupported:     []string{mcp.OAuthCodeChallengeS256},
		TokenEndpointAuthMethodsSupported: []string{mcp.OAuthTokenEndpointAuthNone},
		ScopesSupported:                   supportedMCPOAuthScopes(),
		AuthorizationResponseIssuerParameterSupported: true,
	}
	writeJSONResponse(w, &metadata, nil)
}

func (s *Router) handleMCPOAuthJWKS(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.mcpOAuth == nil {
		http.NotFound(w, r)
		return
	}
	pub := s.mcpOAuth.key.PublicKey
	resp := mcp.JWKSet{Keys: []mcp.JWK{mcp.RSAJWK(s.mcpOAuth.kid, &pub)}}
	writeJSONResponse(w, &resp, nil)
}

func (s *Router) handleMCPOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.mcpOAuth == nil {
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
	s.mcpOAuth.mu.Lock()
	s.mcpOAuth.clients[client.ID] = client
	s.mcpOAuth.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	resp := mcp.OAuthRegisterResponse{ClientID: client.ID, ClientIDIssuedAt: now.Unix(), ClientName: client.Name, RedirectURIs: client.RedirectURIs, TokenEndpointAuthMethod: method}
	writeJSONResponse(w, &resp, nil)
}

func (s *Router) handleMCPOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.mcpOAuth == nil {
		http.NotFound(w, r)
		return
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
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
		s.mcpOAuth.mu.Lock()
		s.mcpOAuth.consents[consentToken] = mcpOAuthConsent{UserID: user.ID, Values: values, ExpiresAt: time.Now().Add(mcpOAuthAuthCodeTTL)}
		s.mcpOAuth.mu.Unlock()
		writeMCPConsentHeaders(w)
		data := mcpConsentTemplateData{
			Action:        mcpOAuthAuthorizePath,
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
	s.mcpOAuth.mu.Lock()
	consent, ok := s.mcpOAuth.consents[consentToken]
	if ok {
		delete(s.mcpOAuth.consents, consentToken)
	}
	s.mcpOAuth.mu.Unlock()
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
	scope, err := normalizeMCPScope(values.Get("scope"))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	entry := mcpOAuthCode{UserID: user.ID, ClientID: values.Get("client_id"), RedirectURI: values.Get("redirect_uri"), CodeChallenge: values.Get("code_challenge"), Resource: values.Get("resource"), Scope: scope, ExpiresAt: time.Now().Add(mcpOAuthAuthCodeTTL)}
	s.mcpOAuth.mu.Lock()
	s.mcpOAuth.codes[code] = entry
	s.mcpOAuth.mu.Unlock()
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
	http.Redirect(w, r, redirectURL.String(), http.StatusSeeOther)
}

func (s *Router) handleMCPOAuthToken(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.mcpOAuth == nil {
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
	if r.PostForm.Get("grant_type") != mcp.OAuthGrantAuthorizationCode {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	code := r.PostForm.Get("code")
	s.mcpOAuth.mu.Lock()
	entry, ok := s.mcpOAuth.codes[code]
	if ok {
		delete(s.mcpOAuth.codes, code)
	}
	s.mcpOAuth.mu.Unlock()
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
	accessToken, err := s.mcpOAuth.issueAccessToken(s.externalBaseURL(r), &user, entry.Resource, entry.Scope)
	if err != nil {
		slog.WarnContext(r.Context(), "issue mcp oauth token", "err", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	resp := mcp.OAuthTokenResponse{AccessToken: accessToken, TokenType: mcp.OAuthTokenTypeBearer, ExpiresIn: int64(mcpOAuthAccessTokenTTL.Seconds()), Scope: entry.Scope}
	writeJSONResponse(w, &resp, nil)
}

func (s *Router) redirectAuthorizeError(w http.ResponseWriter, r *http.Request, values url.Values, code, description string) {
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

func (s *Router) validateAuthorizeRequest(r *http.Request) error {
	return s.validateAuthorizeForm(r, r.URL.Query())
}

func (s *Router) validateAuthorizeForm(r *http.Request, values url.Values) error {
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

func (s *Router) oauthClient(id string) mcpOAuthClient {
	if s.mcpOAuth == nil {
		return mcpOAuthClient{}
	}
	s.mcpOAuth.mu.Lock()
	defer s.mcpOAuth.mu.Unlock()
	return s.mcpOAuth.clients[id]
}

func (s *mcpOAuthServer) issueAccessToken(issuer string, user *auth.User, audience, scope string) (string, error) {
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
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	mcp.WriteOAuthError(w, status, code, description)
}
