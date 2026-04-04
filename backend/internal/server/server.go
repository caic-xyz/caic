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
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/server/voicertc"
	"github.com/caic-xyz/caic/backend/internal/task"
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

// Config bundles environment-derived values read once at startup and threaded
// into the server instead of calling os.Getenv at runtime.
type Config struct {
	// Directories.
	ConfigDir string // persistent server state, e.g. ~/.config/caic
	CacheDir  string // logs and cache files, e.g. ~/.cache/caic

	// Agent backends.
	GeminiAPIKey    string // required for Gemini Live audio
	TailscaleAPIKey string // required for Tailscale networking inside containers

	// LLM features (title generation, commit descriptions).
	LLMProvider string
	LLMModel    string

	// GitHub — PAT and OAuth are mutually exclusive; App is independent.
	GitHubToken             string // PAT; mutually exclusive with GitHubOAuthClientID
	GitHubOAuthClientID     string // OAuth app client ID; mutually exclusive with GitHubToken
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

	// WebRTC voice bridge (optional).
	WebRTCPort int // UDP port for ICE; 0 disables WebRTC

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
}

// Validate returns an error if the configuration is invalid.
func (c *Config) Validate() error {
	if (c.GitHubOAuthClientID == "") != (c.GitHubOAuthClientSecret == "") {
		return errors.New("GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET must both be set or both be unset")
	}
	if (c.GitLabOAuthClientID == "") != (c.GitLabOAuthClientSecret == "") {
		return errors.New("GITLAB_OAUTH_CLIENT_ID and GITLAB_OAUTH_CLIENT_SECRET must both be set or both be unset")
	}
	oauthConfigured := c.GitHubOAuthClientID != "" || c.GitLabOAuthClientID != ""
	if oauthConfigured && c.ExternalURL == "" {
		return errors.New("CAIC_EXTERNAL_URL is required when OAuth login is configured")
	}
	if c.ExternalURL != "" && !strings.EqualFold(c.ExternalURL, "auto") {
		u, err := url.Parse(c.ExternalURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("CAIC_EXTERNAL_URL is not a valid URL: %q", c.ExternalURL)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("CAIC_EXTERNAL_URL must not contain a path: %q", c.ExternalURL)
		}
		// Normalize: strip trailing slash to avoid double-slash in redirect URIs.
		c.ExternalURL = strings.TrimRight(c.ExternalURL, "/")
		if oauthConfigured && u.Scheme != "https" {
			return errors.New("CAIC_EXTERNAL_URL must use https:// when OAuth login is configured")
		}
	}
	if c.GitLabURL != "" {
		u, err := url.Parse(c.GitLabURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("GITLAB_URL is not a valid URL: %q", c.GitLabURL)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("GITLAB_URL must not contain a path: %q", c.GitLabURL)
		}
	}
	if c.GitHubToken != "" && c.GitHubOAuthClientID != "" {
		return errors.New("GITHUB_TOKEN and GITHUB_OAUTH_CLIENT_ID are mutually exclusive: " +
			"remove GITHUB_TOKEN when using GitHub OAuth login")
	}
	if c.GitLabToken != "" && c.GitLabOAuthClientID != "" {
		return errors.New("GITLAB_TOKEN and GITLAB_OAUTH_CLIENT_ID are mutually exclusive: " +
			"remove GITLAB_TOKEN when using GitLab OAuth login")
	}
	if c.GitHubOAuthClientID != "" && c.GitHubOAuthAllowedUsers == "" {
		return errors.New("GITHUB_OAUTH_ALLOWED_USERS is required when GitHub OAuth login is configured")
	}
	if c.GitLabOAuthClientID != "" && c.GitLabOAuthAllowedUsers == "" {
		return errors.New("GITLAB_OAUTH_ALLOWED_USERS is required when GitLab OAuth login is configured")
	}
	return nil
}

// Server is the HTTP server for the caic web UI.
type Server struct {
	// Immutable after construction.

	// Core infrastructure.
	ctx      context.Context // server-lifetime context; outlives individual HTTP requests
	absRoot  string          // absolute path to the root repos directory
	repos    []repoInfo
	runners  map[string]*task.Runner // keyed by RelPath
	mdClient *md.Client
	backend  *container.Backend // container backend for runner creation
	logDir   string
	ciCache  *forgecache.Cache
	provider genai.Provider // nil if LLM not configured
	bot      *bot.Bot       // handles forge event-driven task automation

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
	usage         *usageFetcher

	// IP geolocation.
	ipgeoChecker *ipgeo.Checker

	// User preferences — all users in a single file.
	prefs *preferences.Store

	// Guarded by mu.
	mu           sync.Mutex
	tasks        map[string]*taskEntry
	repoCIStatus map[string]repoCIState // keyed by repoInfo.RelPath
	changed      chan struct{}          // closed on task mutation; replaced under mu
	warnings     []serverWarning        // append-only ring buffer; capped at maxWarnings
	warningSeq   uint64                 // monotonic sequence counter for warnings
}

type taskEntry struct {
	task        *task.Task
	result      *task.Result
	done        chan struct{}
	cleanupOnce sync.Once // ensures exactly one cleanup runs per task
	// CI monitoring: set when a PR is created; used by webhook handlers to
	// find the task waiting for CI results.
	monitorBranch string // branch being monitored (e.g. "caic-123"); empty when no CI monitoring active
}

// buildHandler assembles the full HTTP handler. Extracted from ListenAndServe
// so that route registration can be tested without a listener.
func (s *Server) buildHandler() (http.Handler, error) {
	// Auth routes (exempt from RequireUser).
	authMux := http.NewServeMux()
	authMux.HandleFunc("GET /api/v1/server/config", handle(s.getConfig))
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
	mux.HandleFunc("POST /webhooks/github", s.handleGitHubWebhook)
	mux.HandleFunc("POST /webhooks/gitlab", s.handleGitLabWebhook)
	mux.Handle("/api/v1/", protectedAPI)

	// Profiling (opt-in via -pprof / CAIC_PPROF).
	if s.pprof {
		registerPprof(mux)
		slog.Info("pprof enabled", "url", "/debug/pprof/")
	}

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
			slog.Info("http blocked", "m", r.Method, "p", r.URL.Path, "s", http.StatusForbidden, "ip", clientIP, "cc", cc) //nolint:gosec // G706: request metadata logged for audit; not used in security decisions
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

// ListenAndServe starts the HTTP server on addr and blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	handler, err := s.buildHandler()
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	shutdownDone := make(chan struct{})
	go func() { //nolint:gosec // G118: goroutine intentionally uses Background; parent ctx is already cancelled at shutdown
		defer close(shutdownDone)
		<-ctx.Done()
		if s.voiceBridge != nil {
			s.voiceBridge.CloseAll()
		}
		// Use Background because the parent ctx is already cancelled.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx) //nolint:contextcheck // parent ctx is already cancelled at shutdown time
		shutdownCancel()
	}()
	slog.Info("listening", "addr", addr)
	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

// pollStats polls container resource stats every 5 seconds for all active tasks.
func (s *Server) pollStats(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pushStats(ctx)
		}
	}
}

func (s *Server) pushStats(ctx context.Context) {
	s.mu.Lock()
	type entry struct {
		task *task.Task
		name string
	}
	var active []entry
	for _, e := range s.tasks {
		t := e.task
		name := t.Container
		if name == "" {
			continue
		}
		st := t.GetState()
		if st == task.StatePurged || st == task.StateFailed || st == task.StateStopped || st == task.StateStopping {
			continue
		}
		active = append(active, entry{task: t, name: name})
	}
	s.mu.Unlock()
	if len(active) == 0 {
		return
	}
	names := make([]string, len(active))
	for i, e := range active {
		names[i] = e.name
	}
	statsMap, err := md.StatsAll(ctx, s.mdClient.Runtime, names)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range active {
		cs, ok := statsMap[e.name]
		if !ok {
			continue
		}
		e.task.PushStats(&task.ContainerStats{
			Ts:         now,
			CPUPerc:    cs.CPUPerc,
			MemUsed:    cs.MemUsed,
			MemLimit:   cs.MemLimit,
			MemPerc:    cs.MemPerc,
			NetRx:      cs.NetRx,
			NetTx:      cs.NetTx,
			BlockRead:  cs.BlockRead,
			BlockWrite: cs.BlockWrite,
			DiskUsed:   cs.DiskUsed,
		})
	}
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
