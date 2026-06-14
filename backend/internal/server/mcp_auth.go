// MCP OAuth protected-resource metadata and challenge helpers.

package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
)

const (
	mcpProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"
	mcpAuthDefaultScope              = mcpScopeRead + " " + mcpScopeTasksRead + " " + mcpScopeTasksWrite + " " + mcpScopeTasksAdmin + " " + mcpScopeReposWrite
)

func (s *Router) handleMCPProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
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

func (s *Router) isMCPProtectedResourceMetadataPath(path string) bool {
	return path == mcpProtectedResourceMetadataPath || path == mcpProtectedResourceMetadataPath+goModeMCPEndpoint
}

func (s *Router) mcpResourceURL(r *http.Request) string {
	return s.externalBaseURL(r) + goModeMCPEndpoint
}

func (s *Router) externalBaseURL(r *http.Request) string {
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

func (s *Router) mcpResourceMetadataURL(r *http.Request) string {
	return s.externalBaseURL(r) + mcpProtectedResourceMetadataPath + goModeMCPEndpoint
}

func (s *Router) mcpAuthChallenge(r *http.Request) string {
	return mcp.BearerChallenge(s.mcpResourceMetadataURL(r), mcpAuthDefaultScope)
}

func (s *Router) handleMCPAuthenticated(w http.ResponseWriter, r *http.Request) {
	if err := s.validateMCPOrigin(r); err != nil {
		http.Error(w, "forbidden: invalid origin", http.StatusForbidden)
		return
	}
	if !s.authEnabled() {
		if !s.allowMCPRequest(w, r) {
			return
		}
		s.mcpHandlers.HandleMCP(w, r)
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
			s.mcpHandlers.HandleMCP(w, authenticatedRequest)
			return
		}
		s.writeUnauthorized(w, r)
		return
	}
	if _, ok := auth.UserFromContext(r.Context()); ok {
		if !s.allowMCPRequest(w, r) {
			return
		}
		s.mcpHandlers.HandleMCP(w, r)
		return
	}
	if !s.allowMCPRequest(w, r) {
		return
	}
	s.writeUnauthorized(w, r)
}

func (s *Router) allowMCPRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.mcpRateLimiter == nil {
		return true
	}
	if s.mcpRateLimiter.allow(s.mcpRateKey(r)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func (s *Router) validateMCPOrigin(r *http.Request) error {
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

func (s *Router) mcpRateKey(r *http.Request) string {
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

func (s *Router) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.UserFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		s.writeUnauthorized(w, r)
	})
}

func (s *Router) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if s.authEnabled() && r.URL.Path == goModeMCPEndpoint {
		w.Header().Set("WWW-Authenticate", s.mcpAuthChallenge(r))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"authentication required"}}`))
}
