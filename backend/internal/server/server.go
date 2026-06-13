// Package server provides the HTTP router serving the API and embedded frontend.
package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
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
	authHandlers          *authHandlers
	ciHandlers            *ciHandlers
	runtimeProcesses      *RuntimeProcesses
	goModeHandler         http.Handler
	serverConfigHandlers  *serverConfigHandlers
	taskHTTPHandlers      *taskHTTPHandlers
	mcpHandlers           *mcp.Handler
	mcpOAuth              *mcpOAuthServer
	mcpOAuthPrivateKeyPEM []byte
	mcpOAuthKeyID         string
	mcpRateLimiter        *rateLimiter
	usageHandlers         *usageHandlers
	voiceHandlers         *voiceHandlers
	webFetchHandlers      *webFetchHandlers

	// Forge webhook delivery. Established by New (and the test constructors) and
	// never nil thereafter; owns the webhook secrets and the App owner allowlist.
	webhooks *WebhookHandlers

	// Auth / session middleware deps.
	authStore     *auth.Store     // nil when auth disabled
	sessionSecret []byte          // nil when auth disabled
	hostState     *auth.HostState // non-nil when ExternalURL is set (static or auto)

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
	mux.HandleFunc("POST "+goModeMCPEndpoint, s.handleMCPAuthenticated)
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
		cc := s.ipgeoChecker.CountryCode(clientIP)
		if !s.ipgeoChecker.IsAllowed(clientIP) {
			http.Error(w, "forbidden: country not allowed", http.StatusForbidden)
			slog.Info("http blocked", "m", r.Method, "p", r.URL.Path, "s", http.StatusForbidden, "ip", clientIP, "cc", cc)
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
			"cc", cc,
		)
	}), nil
}
