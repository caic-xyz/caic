// Package server provides the HTTP server serving the API and embedded
// frontend.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/server/voicertc"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/md"
	"github.com/maruel/genai"
)

type repoInfo struct {
	RelPath          string // e.g. "github/caic" — used as API ID.
	AbsPath          string
	BaseBranch       string
	BaseBranchRemote string     // Git remote name (e.g. "origin") used to determine BaseBranch.
	Remote           string     // Raw git remote URL (origin).
	ForgeKind        forge.Kind // empty if remote is not a recognized forge
	ForgeOwner       string     // empty if remote is not a recognized forge
	ForgeRepo        string     // empty if remote is not a recognized forge
}

// githubAppClient is the interface used by the server to interact with a GitHub App.
// Abstracted so that tests can substitute a stub.
type githubAppClient interface {
	ForgeClient(ctx context.Context, installationID int64) (forge.Forge, error)
	DeleteInstallation(ctx context.Context, installationID int64) error
	RepoInstallation(ctx context.Context, owner, repo string) (int64, error)
	PostComment(ctx context.Context, installationID int64, owner, repo string, issueNumber int, body string) error
}

// Config bundles values read once at startup from config.toml, environment
// variables, and CLI flags, then threaded into the server.
type Config struct {
	// Directories.
	ConfigDir string // persistent server state, e.g. ~/.config/caic
	CacheDir  string // logs and cache files, e.g. ~/.cache/caic

	// Agent backends.
	HarnessEnv      map[string][]string // per-harness KEY=VALUE env vars for containers
	GeminiAPIKey    string              // required for Gemini Live audio
	TailscaleAPIKey string              // required for Tailscale networking inside containers
	Runtime         string              // container runtime: "docker" or "podman" (default: "docker")

	// LLM features (title generation, commit descriptions).
	LLMProvider string
	LLMModel    string

	// GitHub — PAT for server-level API access (forges, autoupdate); OAuth for user login.
	GitHubToken             string // PAT for GitHub API access
	GitHubOAuthClientID     string // OAuth app client ID
	GitHubOAuthClientSecret string
	GitHubOAuthAllowedUsers string // comma-separated; required with OAuth
	GitHubWebhookSecret     []byte // HMAC secret; enables POST /webhooks/github
	GitHubAppID             int64  // GitHub App ID; used with GitHubAppPrivateKeyPEM
	GitHubAppPrivateKeyPEM  []byte // RSA private key PEM (path or content)
	GitHubAppAllowedOwners  string // comma-separated; if set, reject installs from other owners

	// GitLab — PAT and OAuth are mutually exclusive.
	GitLabToken             string // PAT; mutually exclusive with GitLabOAuthClientID
	GitLabOAuthClientID     string // OAuth app client ID; mutually exclusive with GitLabToken
	GitLabOAuthClientSecret string
	GitLabOAuthAllowedUsers string // comma-separated; required with OAuth
	GitLabURL               string // default "https://gitlab.com"
	GitLabWebhookSecret     []byte // X-Gitlab-Token secret; enables POST /webhooks/gitlab

	// ExternalURL is the public base URL (e.g. https://caic.example.com).
	// "auto" (the default) locks the hostname from the first FQDN request.
	// Required for OAuth login and webhook delivery.
	ExternalURL string

	// WebRTC voice bridge.
	WebRTCPort int // UDP port for ICE; 0 = ephemeral; -1 = disabled

	// Profiling.
	Pprof bool // expose /debug/pprof/* endpoints

	// IP geolocation (optional).
	// IPGeoDB is the path to a MaxMind MMDB file (e.g. GeoLite2-Country.mmdb).
	// When set, country codes are resolved and logged for every request.
	IPGeoDB string
	// IPGeoAllowlist is a comma-separated list of permitted country codes and
	// special values "local" and "tailscale". When set, requests from IPs that
	// do not resolve to an allowed value are rejected with 403. Requires
	// IPGeoDB when any token is not "local" or "tailscale".
	IPGeoAllowlist string

	// Skip warmup of base images at startup. Used by e2e fake mode to avoid
	// pulling Docker images that aren't needed for testing.
	SkipWarmup bool
}

// Validate returns an error if the configuration is invalid.
func (c *Config) Validate() error {
	if (c.GitHubOAuthClientID == "") != (c.GitHubOAuthClientSecret == "") {
		return errors.New("github.oauth_client_id and github.oauth_client_secret must both be set or both be unset")
	}
	if (c.GitLabOAuthClientID == "") != (c.GitLabOAuthClientSecret == "") {
		return errors.New("gitlab.oauth_client_id and gitlab.oauth_client_secret must both be set or both be unset")
	}
	oauthConfigured := c.GitHubOAuthClientID != "" || c.GitLabOAuthClientID != ""
	if oauthConfigured && c.ExternalURL == "" {
		return errors.New("external_url is required when OAuth login is configured")
	}
	if c.ExternalURL != "" && !strings.EqualFold(c.ExternalURL, "auto") {
		u, err := url.Parse(c.ExternalURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("external_url is not a valid URL: %q", c.ExternalURL)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("external_url must not contain a path: %q", c.ExternalURL)
		}
		// Normalize: strip trailing slash to avoid double-slash in redirect URIs.
		c.ExternalURL = strings.TrimRight(c.ExternalURL, "/")
		if oauthConfigured && u.Scheme != "https" {
			return errors.New("external_url must use https:// when OAuth login is configured")
		}
	}
	if c.GitLabURL != "" {
		u, err := url.Parse(c.GitLabURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("gitlab.url is not a valid URL: %q", c.GitLabURL)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("gitlab.url must not contain a path: %q", c.GitLabURL)
		}
	}
	if c.GitLabToken != "" && c.GitLabOAuthClientID != "" {
		return errors.New("gitlab.token and gitlab.oauth_client_id are mutually exclusive: " +
			"remove gitlab.token when using GitLab OAuth login")
	}
	if c.GitHubOAuthClientID != "" && c.GitHubOAuthAllowedUsers == "" {
		return errors.New("github.oauth_allowed_users is required when GitHub OAuth login is configured")
	}
	if c.GitLabOAuthClientID != "" && c.GitLabOAuthAllowedUsers == "" {
		return errors.New("gitlab.oauth_allowed_users is required when GitLab OAuth login is configured")
	}
	if c.Runtime != "" && c.Runtime != "docker" && c.Runtime != "podman" {
		return fmt.Errorf("core.runtime must be \"docker\" or \"podman\", got %q", c.Runtime)
	}
	return nil
}

// Server is the HTTP server for the caic web UI.
type Server struct {
	// Immutable after construction.

	// Core infrastructure.
	ctx       context.Context // server-lifetime context; outlives individual HTTP requests
	absRoot   string          // absolute path to the root repos directory
	repoReg   *repoRegistry   // owns the managed-repo set and per-repo CI status (self-locking)
	taskMgr   *tasks.Manager  // task orchestration layer
	mdClient  *md.Client
	backend   *container.Backend // container backend for runner creation
	logDir    string
	cacheDir  string
	ciCache   *forgecache.Cache
	provider  genai.Provider // nil if LLM not configured
	Bot       *bot.Bot
	ciService *ci.Service // handles forge event-driven task automation

	// Profiling.
	pprof bool

	// Agent backends.
	geminiAPIKey string
	voiceBridge  *voicertc.Bridge

	// Forge client management (throttles, App client, installation cache).
	forge *forgeManager

	// GitHub.
	githubOAuth            *auth.ProviderConfig // nil if not configured
	githubAllowedUsers     map[string]struct{}  // nil if GitHub OAuth not configured
	githubWebhookSecret    []byte               // nil when webhook not configured
	githubAppAllowedOwners map[string]struct{}  // nil = allow all; rejects installs from other owners

	// GitLab.
	gitlabWebhookSecret []byte               // nil when GitLab webhook not configured
	gitlabOAuth         *auth.ProviderConfig // nil if not configured
	gitlabAllowedUsers  map[string]struct{}  // nil if GitLab OAuth not configured

	// Auth / session.
	authStore     *auth.Store     // nil when auth disabled
	sessionSecret []byte          // nil when auth disabled
	hostState     *auth.HostState // non-nil when ExternalURL is set (static or auto)
	usageFetchers []usage.ProviderFetcher

	// IP geolocation.
	ipgeoChecker *ipgeo.Checker

	// User preferences — all users in a single file.
	prefs *preferences.Store

	mu       sync.Mutex
	warnings []serverWarning // ring buffer of recent CI warnings for SSE clients; guarded by mu

	// Fake hooks injected during e2e testing.
	fakeProcesses func(ctx context.Context, containerName string) ([]task.ProcessInfo, error)
	fakeSignal    func(ctx context.Context, containerName string, pid int, sig string) error
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

// buildHandler assembles the full HTTP handler. Extracted from Serve so that
// route registration can be tested without a listener.
func (s *Server) buildHandler() (http.Handler, error) {
	// Auth routes (exempt from RequireUser).
	authMux := http.NewServeMux()
	authMux.HandleFunc("GET /api/v1/server/config", handle(s.getConfig))
	authMux.HandleFunc("GET /api/v1/server/version", handle(s.getVersion))
	authMux.HandleFunc("GET /api/v1/auth/github/start", s.handleAuthStart("github"))
	authMux.HandleFunc("GET /api/v1/auth/github/callback", s.handleAuthCallback("github"))
	authMux.HandleFunc("GET /api/v1/auth/gitlab/start", s.handleAuthStart("gitlab"))
	authMux.HandleFunc("GET /api/v1/auth/gitlab/callback", s.handleAuthCallback("gitlab"))
	authMux.HandleFunc("GET /api/v1/auth/me", s.handleGetMe)
	authMux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	// Protected routes.
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("GET /api/v1/server/preferences", handle(s.getPreferences))
	apiMux.HandleFunc("POST /api/v1/server/preferences", handle(s.updatePreferences))
	apiMux.HandleFunc("GET /api/v1/server/harnesses", handle(s.listHarnesses))
	apiMux.HandleFunc("GET /api/v1/server/caches", handle(s.listCaches))
	apiMux.HandleFunc("GET /api/v1/server/repos", handle(s.listRepos))
	apiMux.HandleFunc("POST /api/v1/server/repos", handle(s.cloneRepo))
	apiMux.HandleFunc("POST /api/v1/server/update", handle(s.triggerUpdate))
	apiMux.HandleFunc("GET /api/v1/server/repos/branches", s.handleListRepoBranches)
	apiMux.HandleFunc("POST /api/v1/bot/fix-ci", handle(s.botFixCI))
	apiMux.HandleFunc("POST /api/v1/bot/fix-pr", handle(s.botFixPR))
	apiMux.HandleFunc("GET /api/v1/tasks", handle(s.listTasks))
	apiMux.HandleFunc("POST /api/v1/tasks", handle(s.createTask))
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/raw_events", s.handleTaskRawEvents)
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/events", s.handleTaskEvents)
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/input", handleWithTask(s, s.sendInput))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/restart", handleWithTask(s, s.restartTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/clear-context", handleWithTask(s, s.clearContext))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/compact", handleWithTask(s, s.compactContext))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/fork", handleWithTask(s, s.forkTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/stop", handleWithTask(s, s.stopTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/purge", handleWithTask(s, s.purgeTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/revive", handleWithTask(s, s.reviveTask))
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/ci-log", s.handleGetCILog)
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/sync", handleWithTask(s, s.syncTask))
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/diff", s.handleGetDiff)
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/vnc/ws", s.handleVNCWebSocket)
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/processes", s.handleGetProcesses)
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/processes/{pid}/signal", s.handleSignalProcess)
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/tool/{toolUseID}", s.handleTaskToolInput)
	apiMux.HandleFunc("GET /api/v1/usage", s.handleGetUsage)
	apiMux.HandleFunc("GET /api/v1/voice/token", handle(s.getVoiceToken))
	apiMux.HandleFunc("POST /api/v1/voice/rtc/offer", handle(s.voiceRTCOffer))
	apiMux.HandleFunc("DELETE /api/v1/voice/rtc/{sessionID}", s.handleVoiceRTCClose)
	apiMux.HandleFunc("POST /api/v1/web/fetch", handle(s.webFetch))
	apiMux.HandleFunc("GET /api/v1/server/tasks/events", s.handleTaskListEvents)
	apiMux.HandleFunc("GET /api/v1/server/usage/events", s.handleUsageEvents)

	// Combine: auth routes first, then protected API routes (gated by RequireUser when auth enabled).
	var protectedAPI http.Handler = apiMux
	if s.authEnabled() {
		protectedAPI = auth.RequireUser(apiMux)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authMux)
	mux.HandleFunc("GET /api/v1/server/config", handle(s.getConfig))
	mux.HandleFunc("GET /api/v1/server/version", handle(s.getVersion))
	mux.HandleFunc("POST /webhooks/github", s.handleGitHubWebhook)
	mux.HandleFunc("POST /webhooks/gitlab", s.handleGitLabWebhook)
	mux.Handle("/api/v1/", protectedAPI)

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

func statsToEvent(cs *task.ContainerStats) v1.EventMessage {
	return v1.EventMessage{
		Kind: v1.EventKindStats,
		Ts:   cs.Ts.UnixMilli(),
		Stats: &v1.EventStats{
			Ts:         cs.Ts.UnixMilli(),
			CPUPerc:    cs.CPUPerc,
			MemUsed:    cs.MemUsed,
			MemLimit:   cs.MemLimit,
			MemPerc:    cs.MemPerc,
			NetRx:      cs.NetRx,
			NetTx:      cs.NetTx,
			BlockRead:  cs.BlockRead,
			BlockWrite: cs.BlockWrite,
			DiskUsed:   cs.DiskUsed,
		},
	}
}
