// Package server provides the HTTP router serving the API and embedded frontend.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/gomode"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

type fakeCIHook func(ctx context.Context, t *task.Task)

// Router is the HTTP router for the caic web UI. It owns HTTP routing,
// middleware, and route handler concerns only. Application services and
// long-lived automation (tasks, runtime, forge, bot/CI) are owned by
// internal/app and reached through the handler concerns below.
type Router struct {
	// Immutable after construction.

	// server-lifetime context; outlives individual HTTP requests.
	ctx context.Context

	// Route handler concerns.
	authHandlers                  *authHandlers
	ciHandlers                    *ciHandlers
	runtimeProcesses              *RuntimeProcesses
	goModeHandler                 http.Handler
	serverConfigHandlers          *serverConfigHandlers
	taskHTTPHandlers              *taskHTTPHandlers
	mcpHandlers                   *mcp.Handler
	mcpOAuth                      *mcpOAuthServer
	mcpOAuthPrivateKeyPEM         []byte
	mcpOAuthKeyID                 string
	mcpOAuthRefreshTokenStorePath string
	mcpAudit                      *mcpAuditStore
	mcpRateLimiter                *rateLimiter
	usageHandlers                 *usageHandlers
	voiceHandlers                 *voiceHandlers
	webFetchHandlers              *webFetchHandlers

	// Forge webhook delivery. Established by New (and the test constructors) and
	// never nil thereafter; owns the webhook secrets and the App owner allowlist.
	webhooks *WebhookHandlers

	// Auth / session middleware deps.
	authStore     *auth.Store     // nil when auth disabled
	sessionSecret []byte          // nil when auth disabled
	hostState     *auth.HostState // non-nil when ExternalURL is set (static or auto)

	// mcpDisabled refuses all MCP endpoint requests. Set by Serve when auth is
	// disabled and the listener binds a non-loopback address, so an exposed
	// server never serves MCP without authentication.
	mcpDisabled bool

	// IP geolocation.
	ipgeoChecker *ipgeo.Checker

	// Profiling (opt-in).
	pprof bool
}

// SetFakeCI injects a fake CI simulation hook for smoke and e2e tests.
func (s *Router) SetFakeCI(f func(context.Context, *task.Task)) {
	if s.taskHTTPHandlers != nil && s.taskHTTPHandlers.service != nil {
		s.taskHTTPHandlers.service.fakeCI = f
	}
}

// Serve starts the HTTP server on an already-open listener and blocks until
// ctx is cancelled. Opening the listener early (before calling New) lets the
// caller detect port conflicts at startup instead of after lengthy
// initialisation.
func (s *Router) Serve(ctx context.Context, ln net.Listener) error {
	if !s.authEnabled() && !hostIsLoopback(hostOnly(ln.Addr().String())) {
		s.mcpDisabled = true
		slog.WarnContext(ctx, "MCP endpoint disabled: no OAuth login configured and the server binds a non-loopback address; configure OAuth login or bind to localhost to enable MCP",
			"addr", ln.Addr())
	}
	handler, err := s.buildHandler()
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	shutdownBase := context.WithoutCancel(ctx)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(shutdownBase, 5*time.Second)
		_ = srv.Shutdown(shutdownCtx)
		shutdownCancel()
	}()
	slog.Info("listening", "addr", ln.Addr())
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

// SetUsageFetchers replaces the provider usage fetchers used by the usage
// endpoints. Intended for e2e tests to inject fake fetchers that return
// canned data without real API credentials.
func (s *Router) SetUsageFetchers(fetchers []usage.ProviderFetcher) {
	s.usageHandlers.fetchers = fetchers
}

// buildHandler assembles the full HTTP handler. Extracted from Serve so that
// route registration can be tested without a listener.
func (s *Router) buildHandler() (http.Handler, error) {
	serverConfig := s.serverConfigHandlers
	if err := s.ensureMCPOAuthServer(); err != nil {
		return nil, err
	}

	// Auth routes (exempt from RequireUser).
	authMux := http.NewServeMux()
	authMux.HandleFunc("GET /api/caic/v1/server/config", handle(serverConfig.getConfig))
	authMux.HandleFunc("GET /api/caic/v1/server/version", handle(serverConfig.getVersion))
	authMux.HandleFunc("GET /api/caic/v1/auth/github/start", s.authHandlers.handleStart("github"))
	authMux.HandleFunc("GET /api/caic/v1/auth/github/callback", s.authHandlers.handleCallback("github"))
	authMux.HandleFunc("GET /api/caic/v1/auth/gitlab/start", s.authHandlers.handleStart("gitlab"))
	authMux.HandleFunc("GET /api/caic/v1/auth/gitlab/callback", s.authHandlers.handleCallback("gitlab"))
	authMux.HandleFunc("GET /api/caic/v1/auth/me", s.authHandlers.handleGetMe)
	authMux.HandleFunc("POST /api/caic/v1/auth/logout", s.authHandlers.handleLogout)

	// Protected routes.
	apiMux := http.NewServeMux()
	runtimeProcesses := s.runtimeProcesses
	taskRoutes := s.taskHTTPHandlers
	apiMux.HandleFunc("GET /api/caic/v1/server/preferences", handle(serverConfig.getPreferences))
	apiMux.HandleFunc("POST /api/caic/v1/server/preferences", handle(serverConfig.updatePreferences))
	apiMux.HandleFunc("GET /api/caic/v1/server/mcp-grants", handle(s.listMCPGrants))
	apiMux.HandleFunc("POST /api/caic/v1/server/mcp-grants/{grantID}/revoke", handle(s.revokeMCPGrant))
	apiMux.HandleFunc("GET /api/caic/v1/server/harnesses", handle(serverConfig.listHarnesses))
	apiMux.HandleFunc("GET /api/caic/v1/server/caches", handle(serverConfig.listCaches))
	apiMux.HandleFunc("GET /api/caic/v1/server/cache-sizes", handle(serverConfig.getCacheSizes))
	apiMux.HandleFunc("GET /api/caic/v1/server/repos", handle(serverConfig.listRepos))
	apiMux.HandleFunc("POST /api/caic/v1/server/repos", handle(serverConfig.cloneRepo))
	apiMux.HandleFunc("POST /api/caic/v1/server/update", handle(serverConfig.triggerUpdate))
	apiMux.HandleFunc("GET /api/caic/v1/server/repos/branches", serverConfig.handleListRepoBranches)
	apiMux.HandleFunc("POST /api/caic/v1/bot/fix-ci", handle(s.ciHandlers.fixCI))
	apiMux.HandleFunc("POST /api/caic/v1/bot/fix-pr", handle(s.ciHandlers.fixPR))
	apiMux.HandleFunc("GET /api/caic/v1/tasks", handle(taskRoutes.service.listTasks))
	apiMux.HandleFunc("POST /api/caic/v1/tasks", handle(taskRoutes.service.createTask))
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/raw_events", taskRoutes.handleTaskRawEvents)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/events", taskRoutes.handleTaskEvents)
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/input", handleWithTask(taskRoutes, taskRoutes.service.sendInput))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/restart", handleWithTask(taskRoutes, taskRoutes.service.restartTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/clear-context", handleWithTask(taskRoutes, taskRoutes.service.clearContext))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/compact", handleWithTask(taskRoutes, taskRoutes.service.compactContext))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/fork", handleWithTask(taskRoutes, taskRoutes.service.forkTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/stop", handleWithTask(taskRoutes, taskRoutes.service.stopTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/purge", handleWithTask(taskRoutes, taskRoutes.service.purgeTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/revive", handleWithTask(taskRoutes, taskRoutes.service.reviveTask))
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/ci-log", s.ciHandlers.handleGetCILog)
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/sync", handleWithTask(taskRoutes, taskRoutes.service.syncTask))
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/diff", taskRoutes.handleGetDiff)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/vnc/ws", taskRoutes.handleVNCWebSocket)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/processes", runtimeProcesses.HandleGetProcesses)
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/processes/{pid}/signal", runtimeProcesses.HandleSignalProcess)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/tool/{toolUseID}", taskRoutes.handleTaskToolInput)
	apiMux.HandleFunc("GET /api/caic/v1/usage", s.usageHandlers.handleGetUsage)
	apiMux.Handle("/api/voicegateway/v1/", s.voiceHandlers.handler())
	apiMux.HandleFunc("POST /api/caic/v1/web/fetch", handle(s.webFetchHandlers.webFetch))
	apiMux.HandleFunc("GET /api/caic/v1/server/tasks/events", taskRoutes.handleTaskListEvents)
	apiMux.HandleFunc("GET /api/caic/v1/server/usage/events", s.usageHandlers.handleEvents)

	// Combine: auth routes first, then protected API routes (gated by RequireUser when auth enabled).
	var protectedAPI http.Handler = apiMux
	if s.authEnabled() {
		protectedAPI = s.requireUser(apiMux)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/caic/v1/auth/", authMux)
	mux.HandleFunc("GET "+mcpProtectedResourceMetadataPath, s.handleMCPProtectedResourceMetadata)
	mux.HandleFunc("GET "+mcpProtectedResourceMetadataPath+"/", s.handleMCPProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleMCPOAuthMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleMCPOAuthMetadata)
	mux.HandleFunc("GET "+mcpOAuthJWKSPath, s.handleMCPOAuthJWKS)
	mux.HandleFunc("POST "+mcpOAuthRegisterPath, s.handleMCPOAuthRegister)
	mux.HandleFunc("GET "+mcpOAuthAuthorizePath, s.handleMCPOAuthAuthorize)
	mux.HandleFunc("POST "+mcpOAuthAuthorizePath, s.handleMCPOAuthAuthorize)
	mux.HandleFunc("POST "+mcpOAuthTokenPath, s.handleMCPOAuthToken)
	mux.HandleFunc("POST "+mcpOAuthRevokePath, s.handleMCPOAuthRevoke)
	// The MCP endpoint is left unregistered when auth is disabled on a
	// non-loopback listener (set by Serve), so an exposed server never serves
	// MCP without authentication. Unregistered /api/ paths answer 404 via the
	// static handler rather than falling back to the SPA.
	if !s.mcpDisabled {
		mux.HandleFunc("POST "+goModeMCPEndpoint, s.handleMCPAuthenticated)
		// Released Streamable HTTP clients (Claude Code, Codex) issue a GET to
		// probe for a server-initiated SSE stream; the handler answers 405 (caic
		// is stateless) so they fall back to plain POST request/response.
		mux.HandleFunc("GET "+goModeMCPEndpoint, s.handleMCPAuthenticated)
	}
	mux.HandleFunc("GET /api/caic/v1/server/config", handle(serverConfig.getConfig))
	mux.HandleFunc("GET /api/caic/v1/server/version", handle(serverConfig.getVersion))
	mux.Handle("/api/gomode/v1/", s.goModeHandler)
	mux.HandleFunc("POST /webhooks/github", s.webhooks.HandleGitHub)
	mux.HandleFunc("POST /webhooks/gitlab", s.webhooks.HandleGitLab)
	mux.Handle("/api/caic/v1/", protectedAPI)
	mux.Handle("/api/voicegateway/v1/", protectedAPI)

	// Profiling (opt-in via -pprof / CAIC_PPROF).
	if s.pprof {
		registerPprof(mux)
		slog.Info("pprof enabled", "url", "/debug/pprof/")
	}

	// Serve embedded provider logos (static, no auth).
	logosFS, err := fs.Sub(frontend.Logos, "logos")
	if err != nil {
		return nil, err
	}
	mux.Handle("/logos/", http.StripPrefix("/logos/", http.FileServer(http.FS(logosFS))))

	// Unmatched API paths must not fall through to the SPA: this subtree is more
	// specific than "/", so any /api/ request without a registered route (an
	// unknown or disabled endpoint) gets 404 instead of index.html. Registered
	// subtrees like /api/caic/v1/ are more specific still and take precedence.
	mux.Handle("/api/", http.NotFoundHandler())

	// Serve embedded frontend with SPA fallback and precompressed variants.
	dist, err := fs.Sub(frontend.Files, "dist")
	if err != nil {
		return nil, err
	}
	mux.HandleFunc("/", newStaticHandler(dist))

	// Middleware chain: logging → host check → auth → decompress → compress → mux.
	var inner http.Handler = mux
	inner = compressMiddleware(inner)
	inner = decompressMiddleware(inner)
	inner = auth.Middleware(s.authStore, s.sessionSecret)(inner)
	if s.hostState != nil {
		inner = s.hostState.Middleware(inner)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := ipgeo.GetClientIP(r)
		origin, allowed := s.ipgeoChecker.CheckOrigin(clientIP)
		if !allowed {
			http.Error(w, fmt.Sprintf("forbidden: origin %s (%s) not allowed", clientIP, origin), http.StatusForbidden)
			slog.Info("http blocked", "m", r.Method, "p", r.URL.Path, "s", http.StatusForbidden, "ip", clientIP, "origin", origin)
			return
		}
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		inner.ServeHTTP(rw, r)
		logFn := slog.InfoContext
		if rw.status < 300 {
			logFn = slog.DebugContext
		}
		logFn(r.Context(), "http",
			"m", r.Method,
			"p", r.URL.Path,
			"s", rw.status,
			"d", roundDuration(time.Since(start)),
			"b", rw.size,
			"ip", clientIP,
			"origin", origin,
		)
	}), nil
}

// authEnabled reports whether OAuth authentication is configured.
func (s *Router) authEnabled() bool {
	return s.authStore != nil
}

// hostOnly returns the host portion of a host:port address, or the input when
// it carries no port.
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// hostIsLoopback reports whether host refers to the local machine only. An
// empty host, "localhost", and any loopback IP are loopback; a routable IP or
// any other hostname is not.
func hostIsLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Dependencies contains already-constructed server dependencies. The caller
// (internal/app) owns the lifetime of the long-lived automation services
// (Bot, CIService, and their adapters); the router only routes requests to them.
type Dependencies struct {
	Repos                         *repos.Service
	Tailscale                     bool
	Preferences                   *preferences.Store
	AuthStore                     *auth.Store
	SessionSecret                 []byte
	MCPOAuthPrivateKeyPEM         []byte
	MCPOAuthKeyID                 string
	MCPOAuthRefreshTokenStorePath string
	MCPAuditLogPath               string
	GitHubOAuth                   *auth.ProviderConfig
	GitLabOAuth                   *auth.ProviderConfig
	HostState                     *auth.HostState
	UsageFetchers                 []usage.ProviderFetcher
	VoiceBridge                   *voicertc.Bridge
	VoiceGateway                  VoiceGatewayConfig
	Forge                         *forgemanager.Manager
	CICache                       *forgecache.Cache
	ProcessBackend                runtime.Backend
	TaskManager                   *tasks.Manager
	Provider                      genai.Provider
	IPGeoChecker                  *ipgeo.Checker

	// App-owned automation services, routed to by HTTP handlers and webhooks.
	Bot        *bot.Bot
	CIService  *ci.Service
	TaskClient bot.Client // creates bot-driven tasks for manual CI repair
	Warnings   *WarningStore
	CacheSizes *CacheSizeStore

	GitHubAllowedUsers     map[string]struct{}
	GitLabAllowedUsers     map[string]struct{}
	GitHubWebhookSecret    []byte
	GitLabWebhookSecret    []byte
	GitHubAppAllowedOwners map[string]struct{}
	Pprof                  bool
}

// New creates a new HTTP router from already-assembled dependencies. It wires
// the injected services into HTTP handler concerns and the webhook endpoint; it
// does not construct or own application services.
func New(ctx context.Context, d Dependencies) (*Router, error) { //nolint:gocritic // Dependencies is a startup value bag.
	if d.ProcessBackend == nil {
		return nil, errors.New("process backend is required")
	}
	voice := &voiceHandlers{bridge: d.VoiceBridge, gateway: d.VoiceGateway}
	voiceMetadata := voice.metadata()
	goModeSettings := newGoModeSettings(voiceMetadata, d.AuthStore != nil)
	goModeHandler, err := gomode.NewHandler(&goModeSettings)
	if err != nil {
		return nil, err
	}
	webFetch := &webFetchHandlers{}
	taskService := &taskAPIService{
		ctx:       ctx,
		taskMgr:   d.TaskManager,
		prefs:     d.Preferences,
		repos:     d.Repos,
		forge:     d.Forge,
		ciService: d.CIService,
		authStore: d.AuthStore,
	}
	s := &Router{
		ctx:              ctx,
		authHandlers:     &authHandlers{store: d.AuthStore, sessionSecret: d.SessionSecret, hostState: d.HostState, githubOAuth: d.GitHubOAuth, gitlabOAuth: d.GitLabOAuth, githubAllowedUsers: d.GitHubAllowedUsers, gitlabAllowedUsers: d.GitLabAllowedUsers},
		ciHandlers:       &ciHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, provider: d.Provider, taskClient: d.TaskClient, authStore: d.AuthStore},
		goModeHandler:    goModeHandler,
		runtimeProcesses: &RuntimeProcesses{taskMgr: d.TaskManager, backend: d.ProcessBackend, notifyChange: notifyChangeFn(d.TaskManager)},
		serverConfigHandlers: &serverConfigHandlers{
			serverCtx:          ctx,
			tailscaleAvailable: d.Tailscale,
			forge:              d.Forge,
			prefs:              d.Preferences,
			repos:              d.Repos,
			taskMgr:            d.TaskManager,
			cacheSizes:         d.CacheSizes,
			authStore:          d.AuthStore,
			githubOAuth:        d.GitHubOAuth,
			gitlabOAuth:        d.GitLabOAuth,
			voiceGateway:       voiceMetadata,
		},
		taskHTTPHandlers:              &taskHTTPHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, ciService: d.CIService, authStore: d.AuthStore, warnings: d.Warnings, service: taskService},
		usageHandlers:                 &usageHandlers{taskMgr: d.TaskManager, fetchers: d.UsageFetchers},
		voiceHandlers:                 voice,
		webFetchHandlers:              webFetch,
		mcpOAuthPrivateKeyPEM:         d.MCPOAuthPrivateKeyPEM,
		mcpOAuthKeyID:                 d.MCPOAuthKeyID,
		mcpOAuthRefreshTokenStorePath: d.MCPOAuthRefreshTokenStorePath,
		mcpAudit:                      &mcpAuditStore{path: d.MCPAuditLogPath},
		mcpRateLimiter:                newRateLimiter(120, time.Minute),
		authStore:                     d.AuthStore,
		sessionSecret:                 d.SessionSecret,
		hostState:                     d.HostState,
		pprof:                         d.Pprof,
		ipgeoChecker:                  d.IPGeoChecker,
	}
	mcpRegistry := &caicToolRegistry{serverConfig: s.serverConfigHandlers, tasks: taskService, ci: s.ciHandlers, usage: s.usageHandlers, audit: s.mcpAudit}
	s.mcpHandlers = &mcp.Handler{
		Registry:   mcpRegistry,
		ServerInfo: mcp.Implementation{Name: "caic", Title: "caic", Version: autoupdate.Version},
	}
	s.runtimeProcesses.authEnabled = s.authEnabled
	// The webhook concern owns the forge webhook secrets and the GitHub App
	// owner allowlist, and dispatches to the app-owned bot and CI service.
	s.webhooks = &WebhookHandlers{
		serverCtx:        ctx,
		githubSecret:     d.GitHubWebhookSecret,
		gitlabSecret:     d.GitLabWebhookSecret,
		appAllowedOwners: d.GitHubAppAllowedOwners,
		bot:              d.Bot,
		ciService:        d.CIService,
		ciCache:          d.CICache,
		forge:            d.Forge,
		taskMgr:          d.TaskManager,
		repos:            d.Repos,
		prefs:            d.Preferences,
	}
	return s, nil
}

// notifyChangeFn returns the task-change notifier for taskMgr, or nil.
func notifyChangeFn(taskMgr *tasks.Manager) func() {
	if taskMgr == nil {
		return nil
	}
	return taskMgr.NotifyTaskChange
}
