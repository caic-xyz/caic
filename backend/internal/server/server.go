// Package server provides the HTTP server serving the API and embedded
// frontend.
package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

type fakeCIHook func(ctx context.Context, t *task.Task)

// RepoInfo describes a repository managed by the server.
type RepoInfo struct {
	RelPath          string // e.g. "github/caic" — used as API ID.
	AbsPath          string
	BaseBranch       string
	BaseBranchRemote string     // Git remote name (e.g. "origin") used to determine BaseBranch.
	Remote           string     // Raw git remote URL (origin).
	ForgeKind        forge.Kind // empty if remote is not a recognized forge
	ForgeOwner       string     // empty if remote is not a recognized forge
	ForgeRepo        string     // empty if remote is not a recognized forge
}

// GitHubAppClient is the interface used by the server to interact with a GitHub App.
// Abstracted so that tests can substitute a stub.
type GitHubAppClient interface {
	ForgeClient(ctx context.Context, installationID int64) (forge.Forge, error)
	DeleteInstallation(ctx context.Context, installationID int64) error
	RepoInstallation(ctx context.Context, owner, repo string) (int64, error)
	PostComment(ctx context.Context, installationID int64, owner, repo string, issueNumber int, body string) error
}

// Server is the HTTP server for the caic web UI.
type Server struct {
	// Immutable after construction.

	// Core infrastructure.
	ctx            context.Context // server-lifetime context; outlives individual HTTP requests
	absRoot        string          // absolute path to the root repos directory
	repoReg        *repoRegistry   // owns the managed-repo set and per-repo CI status (self-locking)
	taskMgr        *tasks.Manager  // task orchestration layer
	runtimeBackend runtime.Backend // runtime backend used by route-level runtime operations
	agentBackends  map[agent.Harness]agent.Backend
	harnessEnv     map[string][]string
	logDir         string
	cacheDir       string
	ciCache        *forgecache.Cache
	provider       genai.Provider // nil if LLM not configured
	Bot            *bot.Bot
	ciService      *ci.Service // handles forge event-driven task automation
	botClient      *BotClient
	ciAdapter      *CIAdapter

	// Profiling.
	pprof              bool
	tailscaleAvailable bool

	// Agent backends.
	geminiAPIKey string
	voiceBridge  *voicertc.Bridge
	voiceGateway VoiceGatewayConfig

	// Forge client management (throttles, App client, installation cache).
	forge *ForgeManager

	// GitHub.
	githubOAuth        *auth.ProviderConfig // nil if not configured
	githubAllowedUsers map[string]struct{}  // nil if GitHub OAuth not configured

	// GitLab.
	gitlabOAuth        *auth.ProviderConfig // nil if not configured
	gitlabAllowedUsers map[string]struct{}  // nil if GitLab OAuth not configured

	// Forge webhook delivery. Established by New (and the test constructors) and
	// never nil thereafter; owns the webhook secrets and the App owner allowlist.
	webhooks *WebhookHandlers

	// Auth / session.
	authStore     *auth.Store     // nil when auth disabled
	sessionSecret []byte          // nil when auth disabled
	hostState     *auth.HostState // non-nil when ExternalURL is set (static or auto)
	usageFetchers []usage.ProviderFetcher
	fakeCI        fakeCIHook // nil outside smoke/e2e tests.

	// IP geolocation.
	ipgeoChecker *ipgeo.Checker

	// User preferences — all users in a single file.
	prefs *preferences.Store

	warnings *warningStore
}

// SetFakeCI injects a fake CI simulation hook for smoke and e2e tests.
func (s *Server) SetFakeCI(f func(context.Context, *task.Task)) {
	s.fakeCI = f
}

// Serve starts the HTTP server on an already-open listener and blocks until
// ctx is cancelled. Opening the listener early (before calling New) lets the
// caller detect port conflicts at startup instead of after lengthy
// initialisation.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
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
		if s.voiceBridge != nil {
			s.voiceBridge.CloseAll()
		}
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

func (s *Server) maybeFakeCI(t *task.Task) {
	if s.fakeCI == nil {
		return
	}
	s.fakeCI(s.ctx, t)
}

// buildHandler assembles the full HTTP handler. Extracted from Serve so that
// route registration can be tested without a listener.
func (s *Server) buildHandler() (http.Handler, error) {
	s.initConcernAdapters()

	// Auth routes (exempt from RequireUser).
	authMux := http.NewServeMux()
	authMux.HandleFunc("GET /api/caic/v1/server/config", handle(s.getConfig))
	authMux.HandleFunc("GET /api/caic/v1/server/version", handle(s.getVersion))
	authMux.HandleFunc("GET /api/caic/v1/auth/github/start", s.handleAuthStart("github"))
	authMux.HandleFunc("GET /api/caic/v1/auth/github/callback", s.handleAuthCallback("github"))
	authMux.HandleFunc("GET /api/caic/v1/auth/gitlab/start", s.handleAuthStart("gitlab"))
	authMux.HandleFunc("GET /api/caic/v1/auth/gitlab/callback", s.handleAuthCallback("gitlab"))
	authMux.HandleFunc("GET /api/caic/v1/auth/me", s.handleGetMe)
	authMux.HandleFunc("POST /api/caic/v1/auth/logout", s.handleLogout)

	// Protected routes.
	apiMux := http.NewServeMux()
	runtimeProcesses := &RuntimeProcesses{
		taskMgr:      s.taskMgr,
		backend:      s.runtimeBackend,
		authEnabled:  s.authEnabled,
		notifyChange: s.taskMgr.NotifyTaskChange,
	}
	apiMux.HandleFunc("GET /api/caic/v1/server/preferences", handle(s.getPreferences))
	apiMux.HandleFunc("POST /api/caic/v1/server/preferences", handle(s.updatePreferences))
	apiMux.HandleFunc("GET /api/caic/v1/server/harnesses", handle(s.listHarnesses))
	apiMux.HandleFunc("GET /api/caic/v1/server/caches", handle(s.listCaches))
	apiMux.HandleFunc("GET /api/caic/v1/server/repos", handle(s.listRepos))
	apiMux.HandleFunc("POST /api/caic/v1/server/repos", handle(s.cloneRepo))
	apiMux.HandleFunc("POST /api/caic/v1/server/update", handle(s.triggerUpdate))
	apiMux.HandleFunc("GET /api/caic/v1/server/repos/branches", s.handleListRepoBranches)
	apiMux.HandleFunc("POST /api/caic/v1/bot/fix-ci", handle(s.botFixCI))
	apiMux.HandleFunc("POST /api/caic/v1/bot/fix-pr", handle(s.botFixPR))
	apiMux.HandleFunc("GET /api/caic/v1/tasks", handle(s.listTasks))
	apiMux.HandleFunc("POST /api/caic/v1/tasks", handle(s.createTask))
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/raw_events", s.handleTaskRawEvents)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/events", s.handleTaskEvents)
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/input", handleWithTask(s, s.sendInput))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/restart", handleWithTask(s, s.restartTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/clear-context", handleWithTask(s, s.clearContext))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/compact", handleWithTask(s, s.compactContext))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/fork", handleWithTask(s, s.forkTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/stop", handleWithTask(s, s.stopTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/purge", handleWithTask(s, s.purgeTask))
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/revive", handleWithTask(s, s.reviveTask))
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/ci-log", s.handleGetCILog)
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/sync", handleWithTask(s, s.syncTask))
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/diff", s.handleGetDiff)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/vnc/ws", s.handleVNCWebSocket)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/processes", runtimeProcesses.HandleGetProcesses)
	apiMux.HandleFunc("POST /api/caic/v1/tasks/{id}/processes/{pid}/signal", runtimeProcesses.HandleSignalProcess)
	apiMux.HandleFunc("GET /api/caic/v1/tasks/{id}/tool/{toolUseID}", s.handleTaskToolInput)
	apiMux.HandleFunc("GET /api/caic/v1/usage", s.handleGetUsage)
	apiMux.HandleFunc("GET /api/voicegateway/v1/voice/token", handle(s.getVoiceToken))
	apiMux.HandleFunc("POST /api/voicegateway/v1/voice/rtc/offer", handle(s.voiceRTCOffer))
	apiMux.HandleFunc("POST /api/voicegateway/v1/voice/rtc/{sessionID}", s.handleVoiceRTCClose)
	apiMux.HandleFunc("POST /api/caic/v1/web/fetch", handle(s.webFetch))
	apiMux.HandleFunc("GET /api/caic/v1/server/tasks/events", s.handleTaskListEvents)
	apiMux.HandleFunc("GET /api/caic/v1/server/usage/events", s.handleUsageEvents)

	// Combine: auth routes first, then protected API routes (gated by RequireUser when auth enabled).
	var protectedAPI http.Handler = apiMux
	if s.authEnabled() {
		protectedAPI = auth.RequireUser(apiMux)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/caic/v1/auth/", authMux)
	mux.HandleFunc("GET /api/caic/v1/server/config", handle(s.getConfig))
	mux.HandleFunc("GET /api/caic/v1/server/version", handle(s.getVersion))
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
