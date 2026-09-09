// MCP HTTP handlers: protocol dispatch, origin validation, and rate limiting.

package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/oauth/oauthserver"
)

// mcpHandlers owns the MCP HTTP endpoint: protocol dispatch, rate limiting, and
// origin validation. The Router applies remote bearer-token authorization,
// browser-session authorization, or direct loopback access as configured.
type mcpHandlers struct {
	protocol    *mcp.Handler
	rateLimiter *rateLimiter

	// Shared, injected reference (not owned).
	hostState          *auth.HostState
	taskMgr            *taskmgr.Manager
	taskMCPTokenIssuer *auth.TaskMCPTokenIssuer
}

// handleMCP is the MCP endpoint handler. Origin validation and rate limiting
// are applied here; configured bearer or session authentication is applied
// upstream by the Router.
func (h *mcpHandlers) handleMCP(w http.ResponseWriter, r *http.Request) {
	r = requestWithMCPPrincipal(r)
	if err := h.validateMCPOrigin(r); err != nil {
		http.Error(w, "forbidden: invalid origin", http.StatusForbidden)
		return
	}
	if !h.allowMCPRequest(w, r) {
		return
	}
	h.protocol.HandleMCP(w, r)
}

// withTaskMCPAuth accepts server-issued task credentials before normal browser
// or OAuth authentication. All non-task requests use normalAuth unchanged.
func (h *mcpHandlers) withTaskMCPAuth(normalAuth func(http.Handler) http.Handler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token, ok := taskMCPBearerToken(r); ok {
			principal, valid := h.taskMCPPrincipal(token)
			if !valid {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(newMCPPrincipalContext(r.Context(), principal)))
			return
		}
		normalAuth(next).ServeHTTP(w, r)
	})
}

func taskMCPBearerToken(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token, ok && strings.HasPrefix(token, auth.TaskMCPTokenPrefix)
}

func (h *mcpHandlers) taskMCPPrincipal(token string) (*mcpPrincipal, bool) {
	if h.taskMCPTokenIssuer == nil || h.taskMgr == nil {
		return nil, false
	}
	taskID, ok := h.taskMCPTokenIssuer.Verify(token)
	if !ok {
		return nil, false
	}
	id, err := ksid.Parse(taskID)
	if err != nil || id == 0 {
		return nil, false
	}
	entry, ok := h.taskMgr.GetEntry(taskID)
	if !ok {
		return nil, false
	}
	t := entry.Task()
	if !t.CaicMCPEnabled || !taskMCPStateActive(t.GetState()) {
		return nil, false
	}
	return &mcpPrincipal{TaskID: id, Remote: true}, true
}

func taskMCPStateActive(state taskslog.State) bool {
	switch state {
	case taskslog.StatePending, taskslog.StateBranching, taskslog.StateProvisioning,
		taskslog.StateStarting, taskslog.StateRunning, taskslog.StateWaiting,
		taskslog.StateAsking, taskslog.StateHasPlan, taskslog.StatePulling,
		taskslog.StatePushing:
		return true
	default:
		return false
	}
}

func requestWithMCPPrincipal(r *http.Request) *http.Request {
	if _, ok := mcpPrincipalFromContext(r.Context()); ok {
		return r
	}
	claims, ok := oauthserver.BearerClaimsFromContext(r.Context())
	if !ok {
		return r
	}
	principal := &mcpPrincipal{
		Subject:  claims.Subject,
		Username: claims.Username,
		Issuer:   claims.Issuer,
		Audience: claims.Audience,
		Scopes:   claims.Scopes,
		Remote:   true,
	}
	return r.WithContext(newMCPPrincipalContext(r.Context(), principal))
}

func (h *mcpHandlers) allowMCPRequest(w http.ResponseWriter, r *http.Request) bool {
	if h.rateLimiter == nil {
		return true
	}
	if h.rateLimiter.Allow(h.mcpRateKey(r)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	return false
}

func (h *mcpHandlers) validateMCPOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme == "" || originURL.Host == "" || originURL.Path != "" {
		return errors.New("invalid origin")
	}
	baseURL, err := url.Parse(externalBaseURL(h.hostState, r))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return errors.New("invalid server origin")
	}
	if originURL.Scheme != baseURL.Scheme || !strings.EqualFold(originURL.Host, baseURL.Host) {
		return errors.New("origin mismatch")
	}
	return nil
}

func (h *mcpHandlers) mcpRateKey(r *http.Request) string {
	if p, ok := mcpPrincipalFromContext(r.Context()); ok && p.TaskID != 0 {
		return "task:" + p.TaskID.String()
	}
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

// endpointRoutes returns an http.Handler with the JSON-RPC endpoint at
// /api/caic/v1/mcp. The Router applies remote bearer authentication when MCP
// OAuth is enabled, browser-session authentication when login uses an automatic
// external URL, or direct mounting for an auth-disabled loopback server.
func (h *mcpHandlers) endpointRoutes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/caic/v1/mcp", h.handleMCP)
	m.HandleFunc("GET /api/caic/v1/mcp", h.handleMCP)
	return m
}

// MCP OAuth scope constants define the authorization scopes for MCP clients.
const (
	mcpScopeRead        = "caic:mcp.read"
	mcpScopeTasksRead   = "caic:tasks.read"
	mcpScopeTasksCreate = "caic:tasks.create"
	mcpScopeTasksWrite  = "caic:tasks.write"
	mcpScopeTasksAdmin  = "caic:tasks.admin"
	mcpScopeReposWrite  = "caic:repos.write"
)

// mcpScopeLabels maps MCP scope identifiers to human-readable labels
// shown on the OAuth consent form.
var mcpScopeLabels = map[string]string{
	mcpScopeRead:        "Use basic MCP tools including usage and non-task resources",
	mcpScopeTasksRead:   "Read task information",
	mcpScopeTasksCreate: "Create coding tasks",
	mcpScopeTasksWrite:  "Interact with and manage existing tasks",
	mcpScopeTasksAdmin:  "Administer tasks (cancel, delete)",
	mcpScopeReposWrite:  "Manage repositories",
}

type mcpPrincipalContextKey struct{}

// mcpPrincipal stores MCP protocol identity shared by the endpoint (which
// adapts OAuth bearer claims) and mcpRegistry (which checks scopes on
// tool/resource access).
type mcpPrincipal struct {
	TaskID   ksid.ID
	Subject  string
	Username string
	Issuer   string
	Audience string
	Scopes   []string
	Remote   bool
}

func newMCPPrincipalContext(ctx context.Context, p *mcpPrincipal) context.Context {
	return context.WithValue(ctx, mcpPrincipalContextKey{}, p)
}

func mcpPrincipalFromContext(ctx context.Context) (*mcpPrincipal, bool) {
	p, ok := ctx.Value(mcpPrincipalContextKey{}).(*mcpPrincipal)
	return p, ok && p != nil
}

func taskMCPTaskID(ctx context.Context) (ksid.ID, bool) {
	p, ok := mcpPrincipalFromContext(ctx)
	if !ok || p.TaskID == 0 {
		return 0, false
	}
	return p.TaskID, true
}

func mcpHasScope(ctx context.Context, scope string) bool {
	if p, ok := mcpPrincipalFromContext(ctx); ok && p.TaskID != 0 {
		return scope == mcpScopeTasksCreate
	}
	if _, ok := auth.UserFromContext(ctx); !ok {
		return true
	}
	p, ok := mcpPrincipalFromContext(ctx)
	if !ok || !p.Remote {
		return true
	}
	ok = slices.Contains(p.Scopes, scope)
	return ok
}

// externalBaseURL constructs the server's external base URL from hostState or
// the request. Used by both mcpHandlers (origin validation) and oauthserver.Server
// (metadata, challenge, bearer verification).
func externalBaseURL(hostState *auth.HostState, r *http.Request) string {
	if hostState != nil {
		if externalURL := hostState.ExternalURL(r); externalURL != "" {
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
