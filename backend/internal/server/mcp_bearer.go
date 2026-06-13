// MCP bearer token verification and request context.

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
	"net/http"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
)

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

func (s *Router) verifyMCPBearer(r *http.Request, token string) (*auth.User, *mcpPrincipal, error) {
	if s.mcpOAuth == nil {
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
	if header.Alg != mcp.JWTAlgRS256 || header.KID != s.mcpOAuth.kid {
		return nil, nil, errors.New("unsupported token header")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, fmt.Errorf("decode token signature: %w", err)
	}
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&s.mcpOAuth.key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
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
