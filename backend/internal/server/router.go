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
	"github.com/caic-xyz/caic/backend/internal/httplog"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/oauth"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

// Router is the HTTP router for the caic web UI. It owns HTTP routing,
// middleware, and route handler concerns only. Application services and
// long-lived automation (tasks, runtime, forge, bot/CI) are owned by
// internal/app and reached through the handler concerns below.
type Router struct {
	// Immutable after construction.

	// server-lifetime context; outlives individual HTTP requests.
	ctx context.Context

	// Route handler concerns.
	authHandlers     *authHandlers
	ciHandlers       *ciHandlers
	runtimeProcesses *runtimeProcessHandlers
	goModeHandler    http.Handler
	serverHandlers   *serverHandlers
	taskHandlers     *taskHandlers
	mcpHandlers      *mcpHandlers
	oauthServer      *oauth.Server
	usageHandlers    *usageHandlers
	voiceHandlers    *voiceHandlers
	webFetchHandlers *webFetchHandlers

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

// Serve starts the HTTP server on an already-open listener and blocks until
// ctx is cancelled. Opening the listener early (before calling New) lets the
// caller detect port conflicts at startup instead of after lengthy
// initialisation.
func (r *Router) Serve(ctx context.Context, ln net.Listener) error {
	if !r.authEnabled() && !hostIsLoopback(hostOnly(ln.Addr().String())) {
		r.mcpDisabled = true
		slog.WarnContext(ctx, "MCP endpoint disabled: no OAuth login configured and the server binds a non-loopback address; configure OAuth login or bind to localhost to enable MCP",
			"addr", ln.Addr())
	}
	handler, err := r.buildHandler()
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

// buildAPIHandler assembles the protected API mux with all route concerns
// and a 404 catch-all for unmatched /api/ paths. It returns the mux wrapped
// in RequireUser when auth is enabled.
func (r *Router) buildAPIHandler() http.Handler {
	apiMux := http.NewServeMux()
	m := func(suffix string, h http.Handler) {
		mountPrefix(apiMux, "/api/caic/v1", "/api/caic/v1"+suffix, h)
	}
	m("/tasks", r.taskHandlers.routes())
	m("/usage", r.usageHandlers.routes())
	m("/server", r.serverHandlers.routes())
	m("/processes", r.runtimeProcesses.routes())
	m("/ci", r.ciHandlers.routes())
	m("/web", r.webFetchHandlers.routes())
	if r.oauthServer != nil {
		m("/oauth/grants", oauthGrantRoutes(r.oauthServer))
	}
	mountPrefix(apiMux, "", "/api/voicegateway/v1", r.voiceHandlers.handler())
	apiMux.Handle("/api/", http.NotFoundHandler())
	apiMux.Handle("/api", http.NotFoundHandler())

	if r.authEnabled() {
		return auth.RequireUser(apiMux)
	}
	return apiMux
}

// buildHandler assembles the full HTTP handler. Extracted from Serve so that
// route registration can be tested without a listener.
func (r *Router) buildHandler() (http.Handler, error) {
	// Sync shared references (tests may mutate after construction).
	r.mcpHandlers.hostState = r.hostState

	// --- Root mux ---
	//
	// Public routes (no session required) are registered directly on the root
	// mux. The protected API subtree is mounted at /api/ and gated by
	// RequireUser.
	//
	// Invariant: every route under /api/ requires a credential.
	//   - /api/caic/v1/mcp is bearer-authenticated (MCP token), not session.
	//   - /api/caic/v1/server/{config,version} are mirrored at /server-info/
	//     for pre-login access; the /api/ variants are session-gated.
	mux := http.NewServeMux()

	// --- Public: OAuth login flow ---
	//
	// /auth/github/start, /auth/github/callback, /auth/gitlab/start,
	// /auth/gitlab/callback, /auth/me, /auth/logout.
	// handleGetMe and handleLogout check the session internally and return 404
	// when unauthenticated.
	mountPrefix(mux, "", "/auth", r.authHandlers.routes())

	// OAuth routes only when auth is configured.
	if r.oauthServer != nil {
		r.oauthServer.RegisterWellKnownRoutes(mux)
		mountPrefix(mux, "", "/oauth", r.oauthServer.Routes())
	}
	if !r.mcpDisabled {
		mcpHandler := r.mcpHandlers.endpointRoutes()
		if r.oauthServer != nil {
			mcpHandler = r.oauthServer.BearerAuth(mcpHandler)
		}
		mountPrefix(mux, "", "/api/caic/v1/mcp", mcpHandler)
	}

	// --- Public: server discovery (read before login) ---
	//
	// The /api/caic/v1/server/{config,version} variants are gated by the
	// protected API subtree below and require a session. /server-info/ provides
	// the same data without authentication for pre-login bootstrap.
	mountPrefix(mux, "", "/server-info", r.serverHandlers.discoveryRoutes())

	// --- Public: Go Mode bootstrap manifest ---
	//
	// RFC 8615 well-known discovery document, registered before the
	// /.well-known/ catch-all below so its exact path takes precedence.
	mux.Handle("/.well-known/gomode.json", r.goModeHandler)

	// --- Public: webhooks (HMAC-authenticated, not session) ---
	mountPrefix(mux, "", "/webhooks", r.webhooks.routes())

	// --- Protected API subtrees ---
	//
	// All /api/ routes go through RequireUser when auth is enabled. Exact-path
	// registrations (e.g. /api/caic/v1/mcp for MCP bearer auth) are registered
	// directly on the root mux and take precedence over this subtree mount.
	//
	// apiMux owns the route concerns, the version-prefix handling, and the 404
	// catch-all for unmatched /api/ paths.
	mux.Handle("/api/", r.buildAPIHandler())

	// Profiling (opt-in via -pprof / CAIC_PPROF).
	if r.pprof {
		registerPprof(mux)
		slog.Info("pprof enabled", "url", "/debug/pprof/")
	}

	// Serve embedded provider logos (static, no auth).
	logosFS, err := fs.Sub(frontend.Logos, "logos")
	if err != nil {
		return nil, err
	}
	mux.Handle("/logos/", http.StripPrefix("/logos/", http.FileServer(http.FS(logosFS))))

	// Unmatched static/well-known paths must not fall through to the SPA.
	// /api/ is handled inside buildAPIHandler; /webhooks/ is owned by its
	// sub-mux.
	mux.Handle("/.well-known/", http.NotFoundHandler())
	mux.Handle("/static/", http.NotFoundHandler())

	// Serve embedded frontend with SPA fallback and precompressed variants.
	dist, err := fs.Sub(frontend.Files, "dist")
	if err != nil {
		return nil, err
	}
	mux.HandleFunc("/", newStaticHandler(dist))

	// Middleware chain: log context → logging → IP origin check → host check → auth → decompress → compress → mux.
	var inner http.Handler = mux
	inner = compressMiddleware(inner)
	inner = decompressMiddleware(inner)
	inner = auth.Middleware(r.authStore, r.sessionSecret)(inner)
	if r.hostState != nil {
		inner = r.hostState.Middleware(inner)
	}
	inner = r.ipgeoMiddleware(inner)
	inner = httplog.Handler{Handler: inner, Attrs: r.httpLogAttrs}
	inner = httpLogContextMiddleware(inner)
	return inner, nil
}

// mountPrefix mounts a handler at prefix on mux, stripping base from the URL
// path before dispatching. It registers both the exact path and the subtree
// (prefix + "/") so the bare collection path and its sub-routes both reach
// the same handler.
func mountPrefix(mux *http.ServeMux, base, prefix string, h http.Handler) {
	h = http.StripPrefix(base, h)
	mux.Handle(prefix, h)
	mux.Handle(prefix+"/", h)
}

func (r *Router) ipgeoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.ipgeoChecker == nil {
			next.ServeHTTP(w, req)
			return
		}
		logCtx := httpLogContextFromRequest(req)
		clientIP := logCtx.clientIP
		origin, allowed := r.ipgeoChecker.CheckOrigin(clientIP)
		logCtx.origin = origin
		if !allowed {
			http.Error(w, fmt.Sprintf("forbidden: origin %s (%s) not allowed", clientIP, origin), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func httpLogContextMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logCtx := &httpLogContext{clientIP: ipgeo.GetClientIP(r)}
		r = r.WithContext(context.WithValue(r.Context(), httpLogContextKey{}, logCtx))
		next.ServeHTTP(w, r)
	})
}

func (r *Router) httpLogAttrs(req *http.Request) []slog.Attr {
	logCtx := httpLogContextFromRequest(req)
	return []slog.Attr{
		slog.String("ip", logCtx.clientIP),
		slog.String("origin", logCtx.origin),
	}
}

func httpLogContextFromRequest(r *http.Request) *httpLogContext {
	logCtx, _ := r.Context().Value(httpLogContextKey{}).(*httpLogContext)
	if logCtx == nil {
		return &httpLogContext{clientIP: ipgeo.GetClientIP(r)}
	}
	return logCtx
}

type httpLogContextKey struct{}

type httpLogContext struct {
	clientIP string
	origin   string
}

// authEnabled reports whether OAuth authentication is configured.
func (r *Router) authEnabled() bool {
	return r.authStore != nil
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
	Repos                      *repos.Service
	Tailscale                  bool
	Preferences                *preferences.Store
	AuthStore                  *auth.Store
	SessionSecret              []byte
	OAuthPrivateKeyPEM         []byte
	OAuthKeyID                 string
	OAuthRefreshTokenStorePath string
	AuditLogPath               string
	GitHubOAuth                *auth.ProviderConfig
	GitLabOAuth                *auth.ProviderConfig
	HostState                  *auth.HostState
	UsageFetchers              []usage.ProviderFetcher
	VoiceBridge                *voicertc.Bridge
	VoiceGateway               VoiceGatewayConfig
	Forge                      *forgemanager.Manager
	CICache                    *forgecache.Cache
	ProcessBackend             runtime.Backend
	TaskManager                *tasks.Manager
	Provider                   genai.Provider
	IPGeoChecker               *ipgeo.Checker

	// App-owned automation services, routed to by HTTP handlers and webhooks.
	Bot        *bot.Bot
	CIService  *ci.Service
	TaskClient bot.Client // creates bot-driven tasks for manual CI repair
	Warnings   *WarningStore
	CacheSizes *CacheSizeStore
	FakeCI     FakeCIHook // optional fake CI simulation hook for smoke/e2e tests

	GitHubAllowedUsers     []string
	GitLabAllowedUsers     []string
	GitHubWebhookSecret    []byte
	GitLabWebhookSecret    []byte
	GitHubAppAllowedOwners []string
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
	svc := &taskService{
		ctx:       ctx,
		taskMgr:   d.TaskManager,
		prefs:     d.Preferences,
		repos:     d.Repos,
		forge:     d.Forge,
		ciService: d.CIService,
		authStore: d.AuthStore,
		fakeCI:    d.FakeCI,
	}
	audit := &auditStore{path: d.AuditLogPath}
	rateLimiter := newRateLimiter(120, time.Minute)

	s := &Router{
		ctx:              ctx,
		authHandlers:     &authHandlers{store: d.AuthStore, sessionSecret: d.SessionSecret, hostState: d.HostState, githubOAuth: d.GitHubOAuth, gitlabOAuth: d.GitLabOAuth, githubAllowedUsers: d.GitHubAllowedUsers, gitlabAllowedUsers: d.GitLabAllowedUsers},
		ciHandlers:       &ciHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, provider: d.Provider, taskClient: d.TaskClient, authStore: d.AuthStore},
		goModeHandler:    goModeHandler,
		runtimeProcesses: &runtimeProcessHandlers{taskMgr: d.TaskManager, backend: d.ProcessBackend, notifyChange: notifyChangeFn(d.TaskManager)},
		serverHandlers: &serverHandlers{
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
		taskHandlers:     &taskHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, ciService: d.CIService, authStore: d.AuthStore, warnings: d.Warnings, service: svc},
		usageHandlers:    &usageHandlers{taskMgr: d.TaskManager, fetchers: d.UsageFetchers},
		voiceHandlers:    voice,
		webFetchHandlers: webFetch,
		authStore:        d.AuthStore,
		sessionSecret:    d.SessionSecret,
		hostState:        d.HostState,
		pprof:            d.Pprof,
		ipgeoChecker:     d.IPGeoChecker,
	}

	// OAuth server — only when auth is configured.
	if d.AuthStore != nil {
		var err error
		s.oauthServer, err = oauth.NewServer(oauth.ServerConfig{
			KeyPEM:                  d.OAuthPrivateKeyPEM,
			KeyID:                   d.OAuthKeyID,
			AccessTokenTTL:          time.Hour,
			AuthCodeTTL:             10 * time.Minute,
			RefreshTokenTTL:         30 * 24 * time.Hour,
			RefreshTokenStorePath:   d.OAuthRefreshTokenStorePath,
			ResourceURLPath:         "/api/caic/v1/mcp",
			ResourceMetadataURLPath: "/.well-known/oauth-protected-resource/api/caic/v1/mcp",
			ClientIDPrefix:          "caic_",
			SupportedScopes:         []string{mcpScopeRead, mcpScopeTasksRead, mcpScopeTasksWrite, mcpScopeTasksAdmin, mcpScopeReposWrite},
			DefaultScopes:           []string{mcpScopeRead, mcpScopeTasksRead},
			ScopeLabels:             mcpScopeLabels,
			BaseURL:                 func(r *http.Request) string { return externalBaseURL(s.hostState, r) },
			CurrentUser:             oauthCurrentUser,
			AttachUser:              oauthAttachUser(d.AuthStore),
			UserLookup:              oauthUserLookup(d.AuthStore),
			Login:                   s.authHandlers,
			Audit:                   audit,
			RateLimiter:             rateLimiter,
			Renderer:                oauthConsentRenderer{},
		})
		if err != nil {
			return nil, err
		}
	}

	s.mcpHandlers = &mcpHandlers{
		rateLimiter: rateLimiter,
		hostState:   d.HostState,
	}
	registry := &mcpRegistry{serverConfig: s.serverHandlers, tasks: svc, ci: s.ciHandlers, usage: s.usageHandlers, audit: audit}
	s.mcpHandlers.protocol = &mcp.Handler{
		Registry:   registry,
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
