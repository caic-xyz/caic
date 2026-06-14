// MCP HTTP endpoint: protected-resource metadata, bearer verification, and client grant management.

package server

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// mcpServer owns the MCP HTTP endpoint: protocol dispatch, the caic OAuth
// authorization server, bearer verification, rate limiting, and audit. It is
// the Router's MCP route-handler concern. The auth/host references are shared
// with the rest of the router (referenced, not owned); everything else is
// MCP-specific state.
type mcpServer struct {
	protocol    *mcp.Handler
	oauth       *mcpOAuthServer
	audit       *mcpAuditStore
	rateLimiter *rateLimiter

	privateKeyPEM         []byte
	keyID                 string
	refreshTokenStorePath string

	// Shared, injected references (not owned).
	authStore    *auth.Store
	hostState    *auth.HostState
	authHandlers *authHandlers
}

// authEnabled reports whether OAuth authentication is configured.
func (s *mcpServer) authEnabled() bool {
	return s.authStore != nil
}

const (
	mcpProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"
	mcpAuthDefaultScope              = mcpScopeRead + " " + mcpScopeTasksRead + " " + mcpScopeTasksWrite + " " + mcpScopeTasksAdmin + " " + mcpScopeReposWrite
)

// handleMCPProtectedResourceMetadata writes caic's MCP protected-resource
// metadata.
//
// MCP auth is caic-scoped: provider tokens may authenticate a caic web user or
// authorize forge operations, but they are not accepted as MCP bearer tokens and
// are not forwarded from inbound MCP requests to upstream APIs. The protected
// resource URL is always the external base URL plus the MCP endpoint, and the
// authorization server URL is always the external base URL.
func (s *mcpServer) handleMCPProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || !s.isMCPProtectedResourceMetadataPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	metadata := mcp.ProtectedResourceMetadata{
		Resource:             s.mcpResourceURL(r),
		AuthorizationServers: []string{s.externalBaseURL(r)},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		http.Error(w, "encode metadata", http.StatusInternalServerError)
	}
}

func (s *mcpServer) isMCPProtectedResourceMetadataPath(path string) bool {
	return path == mcpProtectedResourceMetadataPath || path == mcpProtectedResourceMetadataPath+goModeMCPEndpoint
}

func (s *mcpServer) mcpResourceURL(r *http.Request) string {
	return s.externalBaseURL(r) + goModeMCPEndpoint
}

func (s *mcpServer) externalBaseURL(r *http.Request) string {
	if s.hostState != nil {
		if externalURL := s.hostState.ExternalURL(); externalURL != "" {
			return strings.TrimRight(externalURL, "/")
		}
	}
	authority, scheme := effectiveRequestHostAndScheme(r)
	return scheme + "://" + authority
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

func (s *mcpServer) mcpResourceMetadataURL(r *http.Request) string {
	return s.externalBaseURL(r) + mcpProtectedResourceMetadataPath + goModeMCPEndpoint
}

func (s *mcpServer) mcpAuthChallenge(r *http.Request) string {
	return mcp.BearerChallenge(s.mcpResourceMetadataURL(r), mcpAuthDefaultScope)
}

func (s *mcpServer) handleMCPAuthenticated(w http.ResponseWriter, r *http.Request) {
	if err := s.validateMCPOrigin(r); err != nil {
		http.Error(w, "forbidden: invalid origin", http.StatusForbidden)
		return
	}
	if !s.authEnabled() {
		if !s.allowMCPRequest(w, r) {
			return
		}
		s.protocol.HandleMCP(w, r)
		return
	}
	if token := mcp.BearerToken(r); token != "" {
		user, principal, err := s.verifyMCPBearer(r, token)
		if err == nil {
			ctx := auth.NewContext(r.Context(), user)
			ctx = newMCPPrincipalContext(ctx, principal)
			authenticatedRequest := r.WithContext(ctx)
			if !s.allowMCPRequest(w, authenticatedRequest) {
				return
			}
			s.protocol.HandleMCP(w, authenticatedRequest)
			return
		}
		s.writeUnauthorized(w, r)
		return
	}
	if _, ok := auth.UserFromContext(r.Context()); ok {
		if !s.allowMCPRequest(w, r) {
			return
		}
		s.protocol.HandleMCP(w, r)
		return
	}
	if !s.allowMCPRequest(w, r) {
		return
	}
	s.writeUnauthorized(w, r)
}

func (s *mcpServer) allowMCPRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.rateLimiter == nil {
		return true
	}
	if s.rateLimiter.allow(s.mcpRateKey(r)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func (s *mcpServer) validateMCPOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" || originURL.Path != "" {
		return errors.New("invalid origin")
	}
	baseURL, err := url.Parse(s.externalBaseURL(r))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return errors.New("invalid server origin")
	}
	if originURL.Scheme != baseURL.Scheme || !strings.EqualFold(originURL.Host, baseURL.Host) {
		return errors.New("origin mismatch")
	}
	return nil
}

func (s *mcpServer) mcpRateKey(r *http.Request) string {
	if p, ok := mcpPrincipalFromContext(r.Context()); ok && p.Subject != "" {
		return "sub:" + p.Subject
	}
	if u, ok := auth.UserFromContext(r.Context()); ok {
		return "user:" + u.ID
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return "ip:" + host
	}
	return "ip:" + r.RemoteAddr
}

// writeUnauthorized answers an unauthenticated MCP request with a 401 carrying
// the MCP bearer challenge so clients can discover the authorization server.
func (s *mcpServer) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if s.authEnabled() && r.URL.Path == goModeMCPEndpoint {
		w.Header().Set("WWW-Authenticate", s.mcpAuthChallenge(r))
	}
	writeUnauthorizedJSON(w)
}

const (
	mcpScopeRead       = "caic:mcp.read"
	mcpScopeTasksRead  = "caic:tasks.read"
	mcpScopeTasksWrite = "caic:tasks.write"
	mcpScopeTasksAdmin = "caic:tasks.admin"
	mcpScopeReposWrite = "caic:repos.write"
)

var mcpSupportedScopes = map[string]struct{}{
	mcpScopeRead:       {},
	mcpScopeTasksRead:  {},
	mcpScopeTasksWrite: {},
	mcpScopeTasksAdmin: {},
	mcpScopeReposWrite: {},
}

type mcpPrincipalContextKey struct{}

type mcpPrincipal struct {
	Subject  string
	Username string
	Issuer   string
	Audience string
	Scopes   map[string]struct{}
	Remote   bool
}

func newMCPPrincipalContext(ctx context.Context, p *mcpPrincipal) context.Context {
	return context.WithValue(ctx, mcpPrincipalContextKey{}, p)
}

func mcpPrincipalFromContext(ctx context.Context) (*mcpPrincipal, bool) {
	p, ok := ctx.Value(mcpPrincipalContextKey{}).(*mcpPrincipal)
	return p, ok && p != nil
}

func mcpHasScope(ctx context.Context, scope string) bool {
	if _, ok := auth.UserFromContext(ctx); !ok {
		return true
	}
	p, ok := mcpPrincipalFromContext(ctx)
	if !ok || !p.Remote {
		return true
	}
	_, ok = p.Scopes[scope]
	return ok
}

func (s *mcpServer) verifyMCPBearer(r *http.Request, token string) (*auth.User, *mcpPrincipal, error) {
	if s.oauth == nil {
		return nil, nil, errors.New("MCP OAuth server is not initialized")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, errors.New("invalid bearer token format")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, fmt.Errorf("decode token header: %w", err)
	}
	var header mcp.JWTHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, nil, fmt.Errorf("parse token header: %w", err)
	}
	if header.Alg != mcp.JWTAlgRS256 || header.KID != s.oauth.kid {
		return nil, nil, errors.New("unsupported token header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, fmt.Errorf("decode token signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&s.oauth.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		return nil, nil, errors.New("invalid token signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, fmt.Errorf("decode token payload: %w", err)
	}
	var claims mcp.AccessTokenClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, nil, fmt.Errorf("parse token claims: %w", err)
	}
	now := time.Now().Unix()
	if claims.Issuer != s.externalBaseURL(r) {
		return nil, nil, errors.New("invalid token issuer")
	}
	if claims.Audience != s.mcpResourceURL(r) {
		return nil, nil, errors.New("invalid token audience")
	}
	if claims.Type != "access_token" {
		return nil, nil, errors.New("invalid token type")
	}
	if claims.NotBefore > now || claims.Expiry <= now {
		return nil, nil, errors.New("token is not valid now")
	}
	if claims.GrantID != "" {
		active, err := s.oauth.touchGrant(claims.GrantID, time.Now())
		if err != nil {
			return nil, nil, fmt.Errorf("touch token grant: %w", err)
		}
		if !active {
			return nil, nil, errors.New("token grant is not active")
		}
	}
	user, ok := s.authStore.FindByID(claims.Subject)
	if !ok {
		return nil, nil, errors.New("token subject is unknown")
	}
	principal := &mcpPrincipal{
		Subject:  claims.Subject,
		Username: claims.Username,
		Issuer:   claims.Issuer,
		Audience: claims.Audience,
		Scopes:   parseMCPScopeSet(claims.Scope),
		Remote:   true,
	}
	return &user, principal, nil
}

func parseMCPScopeSet(scope string) map[string]struct{} {
	parts := strings.Fields(scope)
	scopes := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		scopes[part] = struct{}{}
	}
	return scopes
}

func (s *mcpServer) listMCPGrants(ctx context.Context, _ *api.EmptyReq) (*v1.MCPGrantsResp, error) {
	if !s.authEnabled() || s.oauth == nil {
		return &v1.MCPGrantsResp{}, nil
	}
	now := time.Now()
	grants := s.oauth.listUserGrants(userIDFromCtx(ctx))
	resp := make([]v1.MCPGrantResp, len(grants))
	for i := range grants {
		resp[i] = mcpGrantResponse(&grants[i], now)
	}
	return &v1.MCPGrantsResp{Grants: resp}, nil
}

func (s *mcpServer) revokeMCPGrant(ctx context.Context, req *v1.RevokeMCPGrantReq) (*v1.StatusResp, error) {
	if !s.authEnabled() || s.oauth == nil {
		return nil, api.NotFound("MCP grant")
	}
	if !s.oauth.revokeUserGrant(userIDFromCtx(ctx), req.GrantID) {
		return nil, api.NotFound("MCP grant")
	}
	if err := s.oauth.saveRefreshTokens(); err != nil {
		return nil, api.InternalError("save MCP grant revocation: " + err.Error())
	}
	return &v1.StatusResp{Status: "ok"}, nil
}

func mcpGrantResponse(grant *mcpOAuthGrant, now time.Time) v1.MCPGrantResp {
	status := v1.MCPGrantStatusActive
	if !grant.RevokedAt.IsZero() {
		status = v1.MCPGrantStatusRevoked
	} else if now.After(grant.ExpiresAt) {
		status = v1.MCPGrantStatusExpired
	}
	return v1.MCPGrantResp{
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
