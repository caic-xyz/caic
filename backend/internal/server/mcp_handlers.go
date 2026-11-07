// MCP HTTP handlers: protocol dispatch, subscription ownership, origin validation, and rate limiting.

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
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/oauth/oauthserver"
)

// mcpHandlers owns the MCP HTTP endpoint: protocol dispatch, rate limiting, and
// origin validation. The Router applies remote bearer-token authorization,
// browser-session authorization, or direct loopback access as configured.
type mcpHandlers struct {
	protocol    *mcp.Handler
	rateLimiter *rateLimiter

	// Shared, injected reference (not owned).
	hostState *auth.HostState
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

func taskMCPStateActive(state task.State) bool {
	switch state {
	case task.StatePending, task.StateBranching, task.StateProvisioning,
		task.StateStarting, task.StateRunning, task.StateWaiting,
		task.StateAsking, task.StateHasPlan, task.StatePulling,
		task.StatePushing:
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

// mcpSessionOwner binds released MCP subscription sessions to authenticated
// identities rather than credentials, so refreshing a token preserves the
// subscription while another user or task cannot reuse its session ID.
func mcpSessionOwner(ctx context.Context) (string, error) {
	principal, hasPrincipal := mcpPrincipalFromContext(ctx)
	if hasPrincipal && principal.TaskID != 0 {
		return "task:" + principal.TaskID.String(), nil
	}
	if user, ok := auth.UserFromContext(ctx); ok && user.ID != "" {
		return "user:" + user.ID, nil
	}
	if hasPrincipal && principal.Remote {
		return "", errors.New("authenticated MCP session is missing its user")
	}
	return "local", nil
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
	m.HandleFunc("DELETE /api/caic/v1/mcp", h.handleMCP)
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
	mcpScopeServerAdmin = "caic:server.admin"
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
	mcpScopeServerAdmin: "Read server machine statistics and health",
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

type mcpClientSessionContextKey struct{}

// Conversation IDs are opaque client input; cap their bytes before retaining
// them in focus or subscriber state.
const maxMCPClientSessionBytes = 256

func mcpRequestSession(ctx context.Context, session string) (context.Context, error) {
	if len(session) > maxMCPClientSessionBytes {
		return nil, mcp.ErrInvalidParams("openai/session exceeds the %d-byte limit", maxMCPClientSessionBytes)
	}
	return withMCPClientSession(ctx, session), nil
}

// withMCPClientSession binds the optional MCP conversation session supplied as
// openai/session. Instructions, tools, resources, and subscriptions carry it independently of the
// shared host login session, so tabs do not overwrite each other's focus.
func withMCPClientSession(ctx context.Context, session string) context.Context {
	if session == "" {
		return ctx
	}
	return context.WithValue(ctx, mcpClientSessionContextKey{}, session)
}

func mcpClientSessionFromContext(ctx context.Context) string {
	session, _ := ctx.Value(mcpClientSessionContextKey{}).(string)
	return session
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
