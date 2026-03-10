// Package server provides the HTTP server serving the API and embedded
// frontend.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/kilo"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/cicache"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/github"
	"github.com/caic-xyz/caic/backend/internal/gitlab"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"github.com/maruel/ksid"
)

type repoInfo struct {
	RelPath    string // e.g. "github/caic" — used as API ID.
	AbsPath    string
	BaseBranch string
	Remote     string     // Raw git remote URL (origin).
	ForgeKind  forge.Kind // empty if remote is not a recognized forge
	ForgeOwner string     // empty if remote is not a recognized forge
	ForgeRepo  string     // empty if remote is not a recognized forge
}

// repoCIState holds the live default-branch CI status for one repo.
type repoCIState struct {
	Status cicache.Status
	Checks []v1.ForgeCheck
}

// CreateAuthTokenConfig is the request for POST /v1alpha/auth_tokens.
// See https://github.com/googleapis/go-genai/blob/main/types.go
type CreateAuthTokenConfig struct {
	Uses                 int    `json:"uses"`
	ExpireTime           string `json:"expireTime"`
	NewSessionExpireTime string `json:"newSessionExpireTime"`
}

// AuthToken is the response from POST /v1alpha/auth_tokens.
// See https://github.com/googleapis/go-genai/blob/main/types.go
type AuthToken struct {
	Name string `json:"name"`
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
	GitHubWebhookSecret     []byte // HMAC secret; enables POST /api/v1/github/webhook
	GitHubAppID             int64  // GitHub App ID; used with GitHubAppPrivateKeyPEM
	GitHubAppPrivateKeyPEM  []byte // RSA private key PEM (path or content)

	// GitLab — PAT and OAuth are mutually exclusive.
	GitLabToken             string // PAT; mutually exclusive with GitLabOAuthClientID
	GitLabOAuthClientID     string // OAuth app client ID; mutually exclusive with GitLabToken
	GitLabOAuthClientSecret string
	GitLabOAuthAllowedUsers string // comma-separated; required with OAuth
	GitLabURL               string // default "https://gitlab.com"

	// ExternalURL is the public base URL (e.g. https://caic.example.com).
	// Required for OAuth login and webhook delivery.
	ExternalURL string
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
	if c.ExternalURL != "" {
		u, err := url.Parse(c.ExternalURL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("CAIC_EXTERNAL_URL is not a valid URL: %q", c.ExternalURL)
		}
		if u.Path != "" && u.Path != "/" {
			return fmt.Errorf("CAIC_EXTERNAL_URL must not contain a path: %q", c.ExternalURL)
		}
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
	backend  *mdBackend // container backend for runner creation
	logDir   string
	ciCache  *cicache.Cache
	provider genai.Provider // nil if LLM not configured

	// Agent backends.
	geminiAPIKey string

	// GitHub.
	githubToken         string
	githubOAuth         *auth.ProviderConfig // nil if not configured
	githubAllowedUsers  map[string]struct{}  // nil if GitHub OAuth not configured
	githubWebhookSecret []byte               // nil when webhook not configured
	githubApp           *github.AppClient    // nil when app not configured

	// GitLab.
	gitlabToken        string
	gitlabOAuth        *auth.ProviderConfig // nil if not configured
	gitlabAllowedUsers map[string]struct{}  // nil if GitLab OAuth not configured

	// Auth / session.
	authStore     *auth.Store // nil when auth disabled
	sessionSecret []byte      // nil when auth disabled
	allowedHost   string      // hostname from ExternalURL; empty disables host checking
	usage         *usageFetcher

	// User preferences.
	prefsDir    string   // directory for per-user preference files
	prefsStores sync.Map // map[string]*preferences.Store, keyed by userID

	// Guarded by mu.
	mu           sync.Mutex
	tasks        map[string]*taskEntry
	repoCIStatus map[string]repoCIState // keyed by repoInfo.RelPath
	changed      chan struct{}          // closed on task mutation; replaced under mu
}

// mdBackend adapts *md.Client to task.ContainerBackend.
type mdBackend struct {
	client      *md.Client
	llmProvider string
	llmModel    string
}

func (b *mdBackend) Start(ctx context.Context, dir, branch string, labels []string, opts task.StartOptions) (name, tailscaleFQDN string, err error) {
	slog.Info("md start", "dir", dir, "br", branch, "hns", opts.Harness, "ts", opts.Tailscale, "usb", opts.USB, "dpy", opts.Display)
	harnessMap := map[agent.Harness]md.Harness{
		agent.Claude: md.HarnessClaude,
		agent.Codex:  md.HarnessCodex,
		agent.Gemini: md.HarnessGemini,
		agent.Kilo:   md.HarnessKilo,
	}
	mdHarness, ok := harnessMap[opts.Harness]
	if !ok {
		return "", "", fmt.Errorf("unknown harness %q", opts.Harness)
	}
	harnessPaths := md.HarnessMounts[mdHarness]
	image := opts.DockerImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	client := b.client
	quiet := true
	if opts.LogWriter != nil {
		clientCopy := *b.client
		clientCopy.W = opts.LogWriter
		client = &clientCopy
		quiet = false
	}
	c := client.Container(dir, branch)
	sr, err := c.Start(ctx, &md.StartOpts{Quiet: quiet, BaseImage: image, Labels: labels, AgentPaths: []md.AgentPaths{harnessPaths}, USB: opts.USB, Tailscale: opts.Tailscale, Display: opts.Display})
	if err != nil {
		return "", "", err
	}
	return c.Name, sr.TailscaleFQDN, nil
}

func (b *mdBackend) Diff(ctx context.Context, dir, branch string, args ...string) (string, error) {
	slog.Info("md diff", "dir", dir, "br", branch, "args", args)
	var stdout bytes.Buffer
	if err := b.client.Container(dir, branch).Diff(ctx, &stdout, io.Discard, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func (b *mdBackend) Fetch(ctx context.Context, dir, branch string) error {
	slog.Info("md fetch", "dir", dir, "br", branch)
	return b.client.Container(dir, branch).Fetch(ctx, b.llmProvider, b.llmModel)
}

func (b *mdBackend) Kill(ctx context.Context, dir, branch string) error {
	slog.Info("md kill", "dir", dir, "br", branch)
	return b.client.Container(dir, branch).Kill(ctx)
}

type taskEntry struct {
	task        *task.Task
	result      *task.Result
	done        chan struct{}
	cleanupOnce sync.Once // ensures exactly one cleanup runs per task
	// Webhook callback: when set, post a comment on the originating issue after task terminates.
	webhookInstallationID int64  // 0 when no app configured
	webhookForgeFullName  string // "owner/repo"
	webhookIssueNumber    int    // 0 when not a comment-triggerable event
}

// New creates a new Server. It discovers repos under rootDir, creates a Runner
// per repo, and adopts preexisting containers.
//
// Startup sequence:
//  1. Initialize container client (instant).
//  2. Parallel I/O phase: discover repos, load terminated task logs, and list
//     containers concurrently.
//  3. Runner init phase: create a Runner per repo with container and agent backends
//     (runs parallel within after repos are discovered).
//  4. Adopt containers using pre-fetched list and logs. If a container's relay
//     is alive, auto-attach to resume streaming.
func New(ctx context.Context, rootDir string, cfg *Config) (*Server, error) {
	logDir := cfg.CacheDir
	if logDir == "" {
		return nil, errors.New("CacheDir is required")
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	// container.New is instant; run it serially to simplify.
	mdClient, err := container.New(cfg.TailscaleAPIKey)
	if err != nil {
		return nil, fmt.Errorf("init container library: %w", err)
	}

	// Phase 1: Parallel I/O — repos discovery, logs loading, and container listing.
	type reposResult struct {
		paths []string
		err   error
	}
	type logsResult struct {
		logs []*task.LoadedTask
		err  error
	}
	type containersResult struct {
		containers []*md.Container
		err        error
	}

	repoCh := make(chan reposResult, 1)
	logCh := make(chan logsResult, 1)
	contCh := make(chan containersResult, 1)

	go func() {
		paths, err := gitutil.DiscoverRepos(rootDir, 3)
		repoCh <- reposResult{paths, err}
	}()
	go func() {
		logs, err := task.LoadLogs(logDir)
		logCh <- logsResult{logs, err}
	}()
	go func() {
		containers, err := mdClient.List(ctx)
		contCh <- containersResult{containers, err}
	}()

	repoRes := <-repoCh
	logRes := <-logCh
	contRes := <-contCh

	// Check for errors.
	if repoRes.err != nil {
		return nil, fmt.Errorf("discover repos: %w", repoRes.err)
	}
	if len(repoRes.paths) == 0 {
		return nil, fmt.Errorf("no git repos found under %s", rootDir)
	}

	// Load persistent settings (generates sessionSecret on first run).
	settings, err := loadSettings(filepath.Join(cfg.ConfigDir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	// Initialize auth store and OAuth providers when auth is configured.
	var authStore *auth.Store
	var sessionSecret []byte
	var githubOAuth *auth.ProviderConfig
	var gitlabOAuth *auth.ProviderConfig
	var allowedHost string
	if cfg.ExternalURL != "" {
		u, err := url.Parse(cfg.ExternalURL)
		if err != nil {
			return nil, fmt.Errorf("parse ExternalURL: %w", err)
		}
		allowedHost = u.Hostname()
		secret, err := hexDecode(settings.SessionSecret)
		if err != nil {
			return nil, fmt.Errorf("decode session secret: %w", err)
		}
		sessionSecret = secret
		store, err := auth.Open(filepath.Join(cfg.ConfigDir, "users.json"))
		if err != nil {
			return nil, fmt.Errorf("open users store: %w", err)
		}
		authStore = store
		if cfg.GitHubOAuthClientID != "" && cfg.GitHubOAuthClientSecret != "" {
			c := auth.GitHubConfig(cfg.GitHubOAuthClientID, cfg.GitHubOAuthClientSecret, cfg.ExternalURL)
			githubOAuth = &c
		}
		if cfg.GitLabOAuthClientID != "" && cfg.GitLabOAuthClientSecret != "" {
			c := auth.GitLabConfig(cfg.GitLabOAuthClientID, cfg.GitLabOAuthClientSecret, cfg.GitLabURL, cfg.ExternalURL)
			gitlabOAuth = &c
		}
	}

	githubAllowedUsers := parseAllowedUsers(cfg.GitHubOAuthAllowedUsers)
	gitlabAllowedUsers := parseAllowedUsers(cfg.GitLabOAuthAllowedUsers)

	// Migrate legacy preferences.json to preferences/default.json on first startup in no-auth mode.
	prefsDir := filepath.Join(cfg.ConfigDir, "preferences")
	legacyPrefsPath := filepath.Join(cfg.ConfigDir, "preferences.json")
	if authStore == nil {
		defaultPrefsPath := filepath.Join(prefsDir, "default.json")
		if _, err := os.Stat(legacyPrefsPath); err == nil {
			if _, err := os.Stat(defaultPrefsPath); errors.Is(err, os.ErrNotExist) {
				if mkErr := os.MkdirAll(filepath.Dir(defaultPrefsPath), 0o700); mkErr == nil {
					if data, readErr := os.ReadFile(legacyPrefsPath); readErr == nil { //nolint:gosec // G304: internal config path
						_ = os.WriteFile(defaultPrefsPath, data, 0o600) //nolint:gosec // G703: path is constructed from config dir, not user input
						slog.Info("migrated preferences.json to preferences/default.json")
					}
				}
			}
		}
	}

	backend := &mdBackend{client: mdClient, llmProvider: cfg.LLMProvider, llmModel: cfg.LLMModel}

	cachePath := filepath.Join(cfg.CacheDir, "ci_results.json")
	cache, err := cicache.Open(cachePath)
	if err != nil {
		slog.Warn("cannot open CI cache; falling back to in-memory", "path", cachePath, "err", err)
		cache, _ = cicache.Open("")
	}

	s := &Server{
		ctx:                ctx,
		absRoot:            absRoot,
		runners:            make(map[string]*task.Runner, len(repoRes.paths)),
		mdClient:           mdClient,
		logDir:             logDir,
		prefsDir:           prefsDir,
		authStore:          authStore,
		sessionSecret:      sessionSecret,
		githubOAuth:        githubOAuth,
		gitlabOAuth:        gitlabOAuth,
		githubAllowedUsers: githubAllowedUsers,
		gitlabAllowedUsers: gitlabAllowedUsers,
		allowedHost:        allowedHost,
		usage:              newUsageFetcher(ctx),
		geminiAPIKey:       cfg.GeminiAPIKey,
		githubToken:        cfg.GitHubToken,
		gitlabToken:        cfg.GitLabToken,
		ciCache:            cache,
		backend:            backend,
		tasks:              make(map[string]*taskEntry),
		repoCIStatus:       make(map[string]repoCIState),
		changed:            make(chan struct{}),
	}
	if len(cfg.GitHubWebhookSecret) > 0 {
		s.githubWebhookSecret = cfg.GitHubWebhookSecret
	}
	if cfg.GitHubAppID != 0 && len(cfg.GitHubAppPrivateKeyPEM) > 0 {
		app, err := github.NewAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		s.githubApp = app
	}

	if cfg.LLMProvider != "" {
		if c, ok := providers.All[cfg.LLMProvider]; !ok || c.Factory == nil {
			slog.Warn("unknown LLM provider for title generation", "prov", cfg.LLMProvider)
		} else {
			var opts []genai.ProviderOption
			if cfg.LLMModel != "" {
				opts = append(opts, genai.ProviderOptionModel(cfg.LLMModel))
			} else {
				opts = append(opts, genai.ModelCheap)
			}
			if p, err := c.Factory(ctx, opts...); err != nil {
				slog.Warn("LLM provider init failed", "prov", cfg.LLMProvider, "err", err)
			} else {
				slog.Info("title generation enabled", "prov", p.Name(), "mdl", p.ModelID())
				s.provider = p
			}
		}
	}

	// Phase 2: Runner init (parallel per-repo).
	type repoResult struct {
		info   repoInfo
		runner *task.Runner
	}
	results := make([]repoResult, len(repoRes.paths))
	var wg sync.WaitGroup
	for i, abs := range repoRes.paths {
		wg.Go(func() {
			rel, err := filepath.Rel(absRoot, abs)
			if err != nil {
				rel = filepath.Base(abs)
			}
			branch, err := gitutil.DefaultBranch(ctx, abs, "origin")
			if err != nil {
				slog.Warn("skipping repo, cannot determine default branch", "path", abs, "err", err)
				return
			}
			remote := gitutil.RemoteOriginURL(ctx, abs)
			runner := &task.Runner{
				BaseBranch: branch,
				Dir:        abs,
				LogDir:     logDir,
				Container:  backend,
			}
			if err := runner.Init(ctx); err != nil {
				slog.Warn("runner init failed", "path", abs, "err", err)
			}
			var forgeKind forge.Kind
			var forgeOwner, forgeRepo string
			if rawURL, err := forge.RemoteURL(ctx, abs); err == nil {
				forgeKind, forgeOwner, forgeRepo, _ = forge.ParseRemoteURL(rawURL)
			}
			results[i] = repoResult{
				info: repoInfo{
					RelPath: rel, AbsPath: abs, BaseBranch: branch, Remote: remote,
					ForgeKind: forgeKind, ForgeOwner: forgeOwner, ForgeRepo: forgeRepo,
				},
				runner: runner,
			}
			slog.Info("discovered repo", "path", rel, "br", branch)
		})
	}
	wg.Wait()
	for _, r := range results {
		if r.runner == nil {
			continue
		}
		s.repos = append(s.repos, r.info)
		s.runners[r.info.RelPath] = r.runner
	}

	if len(s.repos) == 0 {
		return nil, fmt.Errorf("no usable git repos found under %s", rootDir)
	}

	// Phase 3: Load terminated tasks from pre-loaded logs.
	if logRes.err != nil {
		slog.Warn("load logs failed", "err", logRes.err)
	} else {
		if err := s.loadTerminatedTasksFrom(logRes.logs); err != nil {
			return nil, fmt.Errorf("load terminated tasks: %w", err)
		}
	}

	// Phase 4: Adopt containers (using pre-fetched list).
	if contRes.err != nil {
		slog.Warn("list containers failed, skipping adoption", "err", contRes.err)
	} else {
		if err := s.adoptContainers(ctx, contRes.containers, logRes.logs); err != nil {
			return nil, fmt.Errorf("adopt containers: %w", err)
		}
	}

	s.watchContainerEvents(ctx)
	go s.discoverKiloModels()
	return s, nil
}

// ListenAndServe starts the HTTP server.
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
	apiMux.HandleFunc("GET /api/v1/server/harnesses", handle(s.listHarnesses))
	apiMux.HandleFunc("GET /api/v1/server/repos", handle(s.listRepos))
	apiMux.HandleFunc("POST /api/v1/server/repos", handle(s.cloneRepo))
	apiMux.HandleFunc("GET /api/v1/server/repos/branches", s.handleListRepoBranches)
	apiMux.HandleFunc("GET /api/v1/tasks", handle(s.listTasks))
	apiMux.HandleFunc("POST /api/v1/tasks", handle(s.createTask))
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/raw_events", s.handleTaskRawEvents)
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/events", s.handleTaskEvents)
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/input", handleWithTask(s, s.sendInput))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/restart", handleWithTask(s, s.restartTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/terminate", handleWithTask(s, s.terminateTask))
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/ci-log", s.handleGetCILog)
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/sync", handleWithTask(s, s.syncTask))
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/diff", s.handleGetDiff)
	apiMux.HandleFunc("GET /api/v1/tasks/{id}/tool/{toolUseID}", s.handleTaskToolInput)
	apiMux.HandleFunc("GET /api/v1/usage", s.handleGetUsage)
	apiMux.HandleFunc("GET /api/v1/voice/token", handle(s.getVoiceToken))
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
	mux.HandleFunc("POST /api/v1/github/webhook", s.handleGitHubWebhook)
	mux.Handle("/api/v1/", protectedAPI)

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
	if s.allowedHost != "" {
		inner = hostCheckMiddleware(s.allowedHost, inner)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		inner.ServeHTTP(rw, r)
		slog.InfoContext(r.Context(), "http",
			"m", r.Method,
			"p", r.URL.Path,
			"s", rw.status,
			"d", roundDuration(time.Since(start)),
			"b", rw.size,
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

func (s *Server) getConfig(_ context.Context, _ *dto.EmptyReq) (*v1.Config, error) {
	cfg := &v1.Config{
		TailscaleAvailable: s.mdClient.TailscaleAPIKey != "",
		USBAvailable:       runtime.GOOS == "linux",
		DisplayAvailable:   true,
	}
	if s.authEnabled() {
		cfg.AuthProviders = s.authProviders()
	}
	return cfg, nil
}

func (s *Server) getPreferences(ctx context.Context, _ *dto.EmptyReq) (*v1.PreferencesResp, error) {
	prefsStore, err := s.prefsForUser(ctx)
	if err != nil {
		return nil, dto.InternalError("open preferences: " + err.Error())
	}
	prefs := prefsStore.Get()
	recent := prefs.RecentRepos(time.Now())
	repos := make([]v1.RepoPrefsResp, len(recent))
	for i, r := range recent {
		repos[i] = v1.RepoPrefsResp{
			Path:       r.Path,
			BaseBranch: r.BaseBranch,
			Harness:    r.Harness,
			Model:      r.Model,
			BaseImage:  r.BaseImage,
		}
	}
	return &v1.PreferencesResp{
		Repositories: repos,
		Harness:      prefs.Harness,
		Models:       prefs.Models,
		BaseImage:    prefs.BaseImage,
	}, nil
}

func (s *Server) listHarnesses(_ context.Context, _ *dto.EmptyReq) (*[]v1.HarnessInfo, error) {
	// Collect unique harness backends from all runners.
	seen := make(map[agent.Harness]agent.Backend)
	for _, r := range s.runners {
		for h, b := range r.Backends {
			seen[h] = b
		}
	}
	out := make([]v1.HarnessInfo, 0, len(seen))
	for h, b := range seen {
		out = append(out, v1.HarnessInfo{Name: string(h), Models: b.Models(), SupportsImages: b.SupportsImages()})
	}
	slices.SortFunc(out, func(a, b v1.HarnessInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	return &out, nil
}

func (s *Server) listRepos(_ context.Context, _ *dto.EmptyReq) (*[]v1.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reposLocked(), nil
}

// reposLocked builds the current repo list including live CI status.
// Must be called with s.mu held.
func (s *Server) reposLocked() *[]v1.Repo {
	out := make([]v1.Repo, len(s.repos))
	for i, r := range s.repos {
		repo := v1.Repo{Path: r.RelPath, BaseBranch: r.BaseBranch, RemoteURL: gitutil.RemoteToHTTPS(r.Remote)}
		if ci, ok := s.repoCIStatus[r.RelPath]; ok {
			repo.DefaultBranchCIStatus = v1.CIStatus(ci.Status)
			repo.DefaultBranchChecks = ci.Checks
		}
		out[i] = repo
	}
	return &out
}

func (s *Server) handleListRepoBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeError(w, dto.BadRequest("repo is required"))
		return
	}
	absPath, ok := s.repoAbsPath(repo)
	if !ok {
		writeError(w, dto.NotFound("repo not found"))
		return
	}
	pairs, err := gitutil.ListBranches(r.Context(), absPath, "origin")
	if err != nil {
		slog.WarnContext(r.Context(), "list branches failed", "repo", repo, "err", err)
	}
	names := make([]string, len(pairs))
	for i, p := range pairs {
		names[i] = p[0]
	}
	writeJSONResponse(w, &v1.RepoBranchesResp{Branches: names}, nil)
}

func (s *Server) cloneRepo(ctx context.Context, req *v1.CloneRepoReq) (*v1.Repo, error) {
	// Derive target relative path.
	targetPath := req.Path
	if targetPath == "" {
		// Extract basename from URL, stripping .git suffix.
		base := filepath.Base(req.URL)
		base = strings.TrimSuffix(base, ".git")
		if base == "" || base == "." || base == "/" {
			return nil, dto.BadRequest("cannot derive repo name from URL; specify path explicitly")
		}
		targetPath = base
	}

	absTarget := filepath.Join(s.absRoot, targetPath)
	// Defense-in-depth: ensure the resolved path is under absRoot.
	if rel, err := filepath.Rel(s.absRoot, absTarget); err != nil || strings.HasPrefix(rel, "..") {
		return nil, dto.BadRequest("path escapes root directory")
	}

	// Check if directory already exists.
	if _, err := os.Stat(absTarget); err == nil {
		return nil, dto.Conflict("directory already exists: " + targetPath)
	}

	// Check if path already registered.
	if _, ok := s.runners[targetPath]; ok {
		return nil, dto.Conflict("repo already registered: " + targetPath)
	}

	// Determine clone depth.
	depth := req.Depth
	if depth == 0 {
		depth = 1
	}

	// Run git clone with timeout.
	cloneCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	args := []string{"clone", "--depth", strconv.Itoa(depth), req.URL, absTarget}
	cmd := exec.CommandContext(cloneCtx, "git", args...) //nolint:gosec // args are validated: depth is an int, URL is user-provided input, absTarget is validated above
	if out, err := cmd.CombinedOutput(); err != nil {
		// Clean up partial clone.
		_ = os.RemoveAll(absTarget)
		slog.Warn("git clone failed", "url", req.URL, "err", err, "out", string(out))
		return nil, dto.InternalError("git clone failed: " + err.Error())
	}

	// Discover repo info.
	branch, err := gitutil.DefaultBranch(ctx, absTarget, "origin")
	if err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, dto.InternalError("cannot determine default branch: " + err.Error())
	}
	remote := gitutil.RemoteOriginURL(ctx, absTarget)

	// Create and init runner.
	runner := &task.Runner{
		BaseBranch: branch,
		Dir:        absTarget,
		LogDir:     s.logDir,
		Container:  s.backend,
	}
	if err := runner.Init(ctx); err != nil {
		_ = os.RemoveAll(absTarget)
		return nil, dto.InternalError("failed to init runner: " + err.Error())
	}

	info := repoInfo{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, Remote: remote}
	s.repos = append(s.repos, info)
	s.runners[targetPath] = runner
	slog.Info("cloned repo", "url", req.URL, "path", targetPath)

	return &v1.Repo{Path: targetPath, BaseBranch: branch, RemoteURL: gitutil.RemoteToHTTPS(remote)}, nil
}

func (s *Server) listTasks(ctx context.Context, _ *dto.EmptyReq) (*[]v1.Task, error) {
	var ownerID string
	if s.authEnabled() {
		if u, ok := auth.UserFromContext(ctx); ok {
			ownerID = u.ID
		}
	}
	s.mu.Lock()
	out := make([]v1.Task, 0, len(s.tasks))
	for _, e := range s.tasks {
		if ownerID != "" && e.task.OwnerID != "" && e.task.OwnerID != ownerID {
			continue
		}
		out = append(out, s.toJSON(e))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out, nil
}

func (s *Server) createTask(ctx context.Context, req *v1.CreateTaskReq) (*v1.CreateTaskResp, error) {
	runner, ok := s.runners[req.Repo]
	if !ok {
		return nil, dto.BadRequest("unknown repo: " + req.Repo)
	}

	harness := toAgentHarness(req.Harness)
	backend, ok := runner.Backends[harness]
	if !ok {
		return nil, dto.BadRequest("unknown harness: " + string(req.Harness))
	}

	if req.Model != "" && !slices.Contains(backend.Models(), req.Model) {
		return nil, dto.BadRequest("unsupported model for " + string(req.Harness) + ": " + req.Model)
	}

	if len(req.InitialPrompt.Images) > 0 && !backend.SupportsImages() {
		return nil, dto.BadRequest(string(req.Harness) + " does not support images")
	}

	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}

	t := &task.Task{ID: ksid.NewID(), InitialPrompt: v1PromptToAgent(req.InitialPrompt), Repo: req.Repo, BaseBranch: req.BaseBranch, Harness: harness, Model: req.Model, DockerImage: req.Image, Tailscale: req.Tailscale, USB: req.USB, Display: req.Display, StartedAt: time.Now().UTC(), OwnerID: ownerID, Provider: s.provider}
	t.SetTitle(req.InitialPrompt.Text)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	// Run in background using the server context, not the request context.
	go func() {
		h, err := runner.Start(s.ctx, t)
		if err != nil {
			result := task.Result{State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
			close(entry.done)
			return
		}
		s.watchSession(entry, runner, h)
	}()

	prefsStore, err := s.prefsForUser(ctx)
	if err != nil {
		return nil, dto.InternalError("open preferences: " + err.Error())
	}
	if err := prefsStore.Update(func(p *preferences.Preferences) {
		p.TouchRepo(req.Repo, &preferences.RepoPrefs{
			BaseBranch: req.BaseBranch,
			Harness:    string(req.Harness),
			Model:      req.Model,
			BaseImage:  req.Image,
		})
	}); err != nil {
		return nil, dto.InternalError("save preferences: " + err.Error())
	}

	return &v1.CreateTaskResp{Status: "accepted", ID: t.ID}, nil
}

// handleTaskRawEvents delegates to handleTaskEvents — both endpoints now
// serve the same backend-neutral EventMessage stream.
func (s *Server) handleTaskRawEvents(w http.ResponseWriter, r *http.Request) {
	s.handleTaskEvents(w, r)
}

// handleTaskEvents streams agent messages as SSE using backend-neutral
// EventMessage DTOs. All tool invocations are emitted as toolUse events.
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	history, live, unsub := entry.task.Subscribe(r.Context())
	defer unsub()

	tracker := newToolTimingTracker(entry.task.Harness)
	idx := 0

	writeEvents := func(events []v1.EventMessage) {
		for i := range events {
			data, err := marshalEvent(&events[i])
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
			idx++
		}
	}

	now := time.Now()
	for _, msg := range filterHistoryForReplay(history) {
		writeEvents(tracker.convertMessage(msg, now))
	}
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	state := entry.task.GetState()
	if state == task.StateTerminated || state == task.StateFailed {
		return
	}

	for msg := range live {
		writeEvents(tracker.convertMessage(msg, time.Now()))
		flusher.Flush()
	}
}

// computeTaskPatch returns a sparse map containing only the fields that differ
// between oldJSON and newJSON, always including "id". Fields present in oldJSON
// but absent in newJSON are set to null so clients can clear them.
func computeTaskPatch(oldJSON, newJSON []byte) (map[string]json.RawMessage, error) {
	var oldMap, newMap map[string]json.RawMessage
	if err := json.Unmarshal(oldJSON, &oldMap); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(newJSON, &newMap); err != nil {
		return nil, err
	}
	patch := map[string]json.RawMessage{"id": newMap["id"]}
	for k, newVal := range newMap {
		if oldVal, ok := oldMap[k]; !ok || !bytes.Equal(oldVal, newVal) {
			patch[k] = newVal
		}
	}
	for k := range oldMap {
		if _, ok := newMap[k]; !ok {
			patch[k] = json.RawMessage("null")
		}
	}
	return patch, nil
}

// handleTaskToolInput returns the full (untruncated) input for a tool call.
// It scans the task's message history for the ToolUseMessage with the given
// toolUseID and returns its Input field.
func (s *Server) handleTaskToolInput(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	toolUseID := r.PathValue("toolUseID")
	if toolUseID == "" {
		writeError(w, dto.BadRequest("toolUseID required"))
		return
	}
	history, _, unsub := entry.task.Subscribe(r.Context())
	unsub()
	for _, msg := range history {
		if tu, ok := msg.(*agent.ToolUseMessage); ok && tu.ToolUseID == toolUseID {
			writeJSONResponse(w, &v1.TaskToolInputResp{ToolUseID: tu.ToolUseID, Input: tu.Input}, nil)
			return
		}
	}
	writeError(w, dto.NotFound("tool use"))
}

// emitTaskListEvent marshals ev and writes it as an SSE message event.
func emitTaskListEvent(w http.ResponseWriter, flusher http.Flusher, ev v1.TaskListEvent) error { //nolint:gocritic // struct size grew with Repos field; refactor not worth it
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	flusher.Flush()
	return nil
}

// handleTaskListEvents streams patch events for the task list as SSE. On first
// iteration it sends a full snapshot; thereafter it sends only upsert/delete
// events for changed or removed tasks. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch runner-internal state transitions.
func (s *Server) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	ciTicker := time.NewTicker(5 * time.Minute)
	defer ciTicker.Stop()

	// Poll CI immediately on connect, then every 5 minutes via ciTicker.
	go s.pollCIForActiveRepos(r.Context())

	// prevByID tracks the last marshalled JSON for each task ID.
	prevByID := map[string][]byte{}
	var prevReposJSON []byte
	first := true

	for {
		s.mu.Lock()
		out := make([]v1.Task, 0, len(s.tasks))
		for _, e := range s.tasks {
			out = append(out, s.toJSON(e))
		}
		repos := s.reposLocked()
		ch := s.changed
		s.mu.Unlock()

		reposJSON, _ := json.Marshal(repos)

		if first {
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "snapshot", Tasks: out}); err != nil {
				slog.Warn("marshal task list snapshot", "err", err)
				return
			}
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repos}); err != nil {
				slog.Warn("marshal repos snapshot", "err", err)
				return
			}
			for i := range out {
				data, _ := json.Marshal(&out[i])
				prevByID[out[i].ID.String()] = data
			}
			prevReposJSON = reposJSON
			first = false
		} else {
			// Emit upserts/patches for new or changed tasks.
			currentIDs := make(map[string]struct{}, len(out))
			for i := range out {
				id := out[i].ID.String()
				currentIDs[id] = struct{}{}
				data, err := json.Marshal(&out[i])
				if err != nil {
					slog.Warn("marshal task", "id", id, "err", err)
					continue
				}
				if !bytes.Equal(data, prevByID[id]) {
					prev := prevByID[id]
					prevByID[id] = data
					if prev == nil {
						// New task: emit full object.
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "upsert", Task: &out[i]}); err != nil {
							slog.Warn("marshal task upsert", "id", id, "err", err)
							return
						}
					} else {
						// Existing task changed: emit only the diff.
						patch, err := computeTaskPatch(prev, data)
						if err != nil {
							slog.Warn("compute task patch", "id", id, "err", err)
							continue
						}
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "patch", Patch: patch}); err != nil {
							slog.Warn("marshal task patch", "id", id, "err", err)
							return
						}
					}
				}
			}
			// Emit deletes for removed tasks.
			for id := range prevByID {
				if _, ok := currentIDs[id]; !ok {
					if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "delete", ID: id}); err != nil {
						slog.Warn("marshal task delete", "id", id, "err", err)
						return
					}
					delete(prevByID, id)
				}
			}
			// Emit repos update when default-branch CI status has changed.
			if !bytes.Equal(reposJSON, prevReposJSON) {
				prevReposJSON = reposJSON
				if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repos}); err != nil {
					slog.Warn("marshal repos update", "err", err)
					return
				}
			}
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		case <-ciTicker.C:
			go s.pollCIForActiveRepos(r.Context())
		}
	}
}

// handleUsageEvents streams usage snapshots as SSE. It reacts to task changes
// immediately and ticks every 5 minutes for window rollovers and OAuth cache
// refreshes. Each message is a single UsageResp JSON object.
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(usageCacheTTL)
	defer ticker.Stop()

	var prev []byte

	for {
		s.mu.Lock()
		resp := computeUsage(s.tasks, time.Now())
		ch := s.changed
		s.mu.Unlock()

		if s.usage != nil {
			if oauth := s.usage.get(); oauth != nil {
				resp.FiveHour.Utilization = oauth.FiveHour.Utilization
				resp.FiveHour.ResetsAt = oauth.FiveHour.ResetsAt
				resp.SevenDay.Utilization = oauth.SevenDay.Utilization
				resp.SevenDay.ResetsAt = oauth.SevenDay.ResetsAt
				resp.ExtraUsage = oauth.ExtraUsage
			}
		}

		data, err := json.Marshal(resp)
		if err == nil && !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
			prev = data
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		}
	}
}

// relayStatus describes the state of the in-container relay daemon, probed
// over SSH when SendInput fails. Combined with the task state and session
// status (from task.SendInput's error), the three values pinpoint why input
// delivery failed:
//
//   - state=waiting session=none  relay=dead → relay died, reconnect failed.
//   - state=waiting session=exited relay=alive → SSH attach exited but relay
//     is still running; reconnect should recover.
//   - state=running session=none  relay=alive → state-machine bug: state says
//     running but no Go-side session object exists.
//   - state=pending session=none  relay=no-container → task never started.
type relayStatus string

const (
	relayAlive       relayStatus = "alive"        // Relay socket exists; daemon is running.
	relayDead        relayStatus = "dead"         // No socket; daemon exited or was never started.
	relayCheckFailed relayStatus = "check-failed" // SSH probe failed (container unreachable).
	relayNoContainer relayStatus = "no-container" // Task has no container yet.
)

// sendInput forwards user input to the agent session. On failure, it probes
// the relay daemon's liveness over SSH and returns diagnostic details in the
// 409 response so the frontend can show the user what went wrong.
//
// The relay probe uses the server context (not the request context) because the
// SSH round-trip may outlive a cancelled HTTP request, and we want the log line
// regardless.
func (s *Server) sendInput(ctx context.Context, entry *taskEntry, req *v1.InputReq) (*v1.StatusResp, error) {
	if len(req.Prompt.Images) > 0 {
		runner := s.runners[entry.task.Repo]
		if b := runner.Backends[entry.task.Harness]; b != nil && !b.SupportsImages() {
			return nil, dto.BadRequest(string(entry.task.Harness) + " does not support images")
		}
	}
	if err := entry.task.SendInput(ctx, v1PromptToAgent(req.Prompt)); err != nil {
		t := entry.task
		rs := relayNoContainer
		if t.Container != "" {
			ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			alive, relayErr := agent.IsRelayRunning(ctx, t.Container) //nolint:contextcheck // diagnostic probe; must outlive request
			cancel()
			switch {
			case relayErr != nil:
				rs = relayCheckFailed
			case alive:
				rs = relayAlive
			default:
				rs = relayDead
			}
		}
		taskState := t.GetState()
		slog.Warn("no active session",
			"task", t.ID,
			"br", t.Branch,
			"ctr", t.Container,
			"state", taskState,
			"relay", rs,
		)
		return nil, dto.Conflict(err.Error()).
			WithDetail("state", taskState.String()).
			WithDetail("relay", string(rs))
	}
	return &v1.StatusResp{Status: "sent"}, nil
}

func (s *Server) restartTask(_ context.Context, entry *taskEntry, req *v1.RestartReq) (*v1.StatusResp, error) {
	t := entry.task
	if state := t.GetState(); state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan {
		return nil, dto.Conflict("task is not waiting or asking")
	}
	prompt := v1PromptToAgent(req.Prompt)
	if prompt.Text == "" {
		// Read the plan file from the container.
		plan, err := agent.ReadPlan(s.ctx, t.Container, t.GetPlanFile()) //nolint:contextcheck // intentionally using server context
		if err != nil {
			return nil, dto.BadRequest("no prompt provided and failed to read plan from container: " + err.Error())
		}
		prompt.Text = plan
	}
	runner := s.runners[t.Repo]
	// Use the server-lifetime context, not the HTTP request context.
	// The new agent session must outlive this request.
	h, err := runner.RestartSession(s.ctx, t, prompt) //nolint:contextcheck // intentionally using server context
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	s.watchSession(entry, runner, h)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	return &v1.StatusResp{Status: "restarted"}, nil
}

// handleGetCILog fetches the log for a specific CI job by jobID.
// The jobID is a required query parameter; the caller knows it from the
// task's ciChecks field. The log is capped at ~8 KB (tail).
func (s *Server) handleGetCILog(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.task
	snap := t.Snapshot()
	info := s.repoInfoFor(t.Repo)
	if info == nil {
		writeError(w, dto.BadRequest("no repo info found"))
		return
	}
	f := s.forgeForInfo(r.Context(), info)
	if f == nil {
		writeError(w, dto.BadRequest("no forge token configured for this repo"))
		return
	}

	jobIDStr := r.URL.Query().Get("jobID")
	if jobIDStr == "" {
		writeError(w, dto.BadRequest("jobID query parameter is required"))
		return
	}
	var jobID int64
	if _, scanErr := fmt.Sscanf(jobIDStr, "%d", &jobID); scanErr != nil || jobID <= 0 {
		writeError(w, dto.BadRequest("invalid jobID"))
		return
	}

	// Find the check by jobID to get owner/repo/name.
	var check *task.CICheck
	for i := range snap.CIChecks {
		if snap.CIChecks[i].JobID == jobID {
			check = &snap.CIChecks[i]
			break
		}
	}
	if check == nil {
		writeError(w, dto.NotFound("no CI check with that jobID"))
		return
	}

	const maxLogBytes = 8192
	jobLog, logErr := f.GetJobLog(r.Context(), check.Owner, check.Repo, jobID, maxLogBytes)
	if logErr != nil {
		slog.Warn("getTaskCILog: fetch job log", "task", t.ID, "jobID", jobID, "err", logErr)
		jobLog = "(log unavailable: " + logErr.Error() + ")"
	}

	if r.URL.Query().Get("raw") == "true" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "Step: %s\n\n%s", check.Name, jobLog)
		return
	}
	writeJSONResponse(w, &v1.CILogResp{StepName: check.Name, Log: jobLog}, nil)
}

func (s *Server) terminateTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.SetState(task.StateTerminating)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	runner := s.runners[entry.task.Repo]
	go s.cleanupTask(entry, runner, task.StateTerminated) //nolint:contextcheck // cleanupTask intentionally uses server context
	return &v1.StatusResp{Status: "terminating"}, nil
}

func (s *Server) syncTask(ctx context.Context, entry *taskEntry, req *v1.SyncReq) (*v1.SyncResp, error) {
	t := entry.task
	switch t.GetState() {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateTerminating, task.StateFailed, task.StateTerminated:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]

	if req.Target == v1.SyncTargetDefault {
		if req.Force {
			return nil, dto.BadRequest("force is not supported for default-branch sync")
		}
		// Look up the base branch for the response.
		baseBranch := runner.BaseBranch
		// Build commit message from task title, falling back to prompt.
		message := t.Title()
		if message == "" {
			message = t.InitialPrompt.Text
		}
		ds, issues, err := runner.SyncToDefault(ctx, t.Branch, t.Container, message)
		if err != nil {
			return nil, dto.InternalError(err.Error())
		}
		status := "synced"
		if len(ds) == 0 {
			status = "empty"
		} else if len(issues) > 0 {
			status = "blocked"
		}
		return &v1.SyncResp{Status: status, Branch: baseBranch, DiffStat: toV1DiffStat(ds), SafetyIssues: toV1SafetyIssues(issues)}, nil
	}

	// Default: push to the task's own branch.
	ds, issues, err := runner.SyncToOrigin(ctx, t.Branch, t.Container, req.Force)
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	status := "synced"
	if len(ds) == 0 {
		status = "empty"
	} else if len(issues) > 0 && !req.Force {
		status = "blocked"
	}
	if status == "synced" {
		if info := s.repoInfoFor(t.Repo); info != nil && s.forgeForInfo(ctx, info) != nil {
			go s.startPRFlow(s.ctx, entry, t.Branch, s.effectiveBaseBranch(t)) //nolint:contextcheck // intentionally using server context; PR flow must outlive request
		}
	}
	return &v1.SyncResp{Status: status, Branch: t.Branch, DiffStat: toV1DiffStat(ds), SafetyIssues: toV1SafetyIssues(issues)}, nil
}

// startPRFlow creates a PR/MR for the synced branch and then launches CI
// monitoring. Runs in a goroutine; logs errors and returns on failure.
func (s *Server) startPRFlow(ctx context.Context, entry *taskEntry, branch, baseBranch string) {
	t := entry.task
	info := s.repoInfoFor(t.Repo)
	if info == nil || info.ForgeOwner == "" {
		return
	}
	f := s.forgeForInfo(ctx, info)
	if f == nil {
		return
	}
	title := t.Title()
	if title == "" {
		title = t.InitialPrompt.Text
	}
	var body string
	if entry.result != nil {
		body = entry.result.AgentResult
	}
	pr, err := f.CreatePR(ctx, info.ForgeOwner, info.ForgeRepo, branch, baseBranch, title, body)
	if err != nil {
		slog.Warn("startPRFlow: create PR", "repo", t.Repo, "branch", branch, "err", err)
		return
	}
	slog.Info("PR created", "task", t.ID, "forge", f.Name(), "owner", info.ForgeOwner, "repo", info.ForgeRepo, "pr", pr.Number)
	t.SetPR(info.ForgeOwner, info.ForgeRepo, pr.Number)
	s.notifyTaskChange()
	go s.monitorCI(ctx, entry, f, info.ForgeOwner, info.ForgeRepo, pr.HeadSHA)
}

// monitorCI polls CI check-runs every 30 s until all checks complete,
// then injects a summary into the agent via SendInput.
func (s *Server) monitorCI(ctx context.Context, entry *taskEntry, f forge.Forge, owner, repo, sha string) {
	t := entry.task

	// Fast path: result already cached (e.g. after a server restart).
	if cached, ok := s.ciCache.Get(owner, repo, sha); ok {
		s.applyMonitorCIResult(ctx, entry, f, owner, repo, sha, cached)
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Stop if the task is no longer waiting (user sent input / terminated).
		st := t.GetState()
		if st != task.StateWaiting && st != task.StateAsking && st != task.StateHasPlan {
			return
		}
		runs, err := f.GetCheckRuns(ctx, owner, repo, sha)
		if err != nil {
			if errors.Is(err, forge.ErrNotFound) {
				return
			}
			slog.Warn("monitorCI: get check-runs", "task", t.ID, "err", err)
			continue
		}
		if len(runs) == 0 {
			continue
		}
		result, done := evaluateCheckRuns(owner, repo, runs)
		if !done {
			t.SetCIStatus(task.CIStatusPending, nil)
			s.notifyTaskChange()
			continue
		}
		if err := s.ciCache.Put(owner, repo, sha, result); err != nil {
			slog.Warn("monitorCI: cache put", "err", err)
		}
		s.applyMonitorCIResult(ctx, entry, f, owner, repo, sha, result)
		return
	}
}

// applyMonitorCIResult updates the task CI status and injects the CI summary
// into the agent.
func (s *Server) applyMonitorCIResult(ctx context.Context, entry *taskEntry, f forge.Forge, owner, repo, sha string, result cicache.Result) {
	t := entry.task
	ciStatus := task.CIStatusSuccess
	var summary string
	if result.Status == cicache.StatusFailure {
		ciStatus = task.CIStatusFailure
		var sb strings.Builder
		var numFailed int
		for _, c := range result.Checks {
			if c.Conclusion != cicache.CheckConclusionSuccess &&
				c.Conclusion != cicache.CheckConclusionNeutral &&
				c.Conclusion != cicache.CheckConclusionSkipped {
				numFailed++
			}
		}
		fmt.Fprintf(&sb, "%s CI: %d check(s) failed:\n", f.Name(), numFailed)
		for _, c := range result.Checks {
			if c.Conclusion != cicache.CheckConclusionSuccess &&
				c.Conclusion != cicache.CheckConclusionNeutral &&
				c.Conclusion != cicache.CheckConclusionSkipped {
				if jobURL := f.CIJobURL(c.Owner, c.Repo, c.RunID, c.JobID); jobURL != "" {
					fmt.Fprintf(&sb, "- %s (%s): %s\n", c.Name, c.Conclusion, jobURL)
				} else {
					fmt.Fprintf(&sb, "- %s (%s)\n", c.Name, c.Conclusion)
				}
			}
		}
		summary = strings.TrimRight(sb.String(), "\n")
	} else {
		summary = fmt.Sprintf("%s CI: all checks passed for %s/%s@%s.", f.Name(), owner, repo, sha[:min(7, len(sha))])
	}
	taskChecks := make([]task.CICheck, len(result.Checks))
	for i, c := range result.Checks {
		taskChecks[i] = task.CICheck{
			Name:       c.Name,
			Owner:      c.Owner,
			Repo:       c.Repo,
			RunID:      c.RunID,
			JobID:      c.JobID,
			Conclusion: forge.CheckRunConclusion(c.Conclusion),
		}
	}
	t.SetCIStatus(ciStatus, taskChecks)
	s.notifyTaskChange()
	if err := t.SendInput(ctx, agent.Prompt{Text: summary}); err != nil {
		slog.Warn("monitorCI: send input", "task", t.ID, "err", err)
	}
}

// evaluateCheckRuns inspects runs for a SHA and returns a cicache.Result plus
// whether all checks have completed (done=true). Only call with len(runs)>0.
func evaluateCheckRuns(owner, repo string, runs []forge.CheckRun) (cicache.Result, bool) {
	for _, r := range runs {
		if r.Status != forge.CheckRunStatusCompleted {
			return cicache.Result{}, false
		}
	}
	checks := make([]cicache.ForgeCheck, 0, len(runs))
	anyFailed := false
	for _, r := range runs {
		checks = append(checks, cicache.ForgeCheck{
			Name:       r.Name,
			Owner:      owner,
			Repo:       repo,
			RunID:      r.RunID,
			JobID:      r.JobID,
			Conclusion: cicache.CheckConclusion(r.Conclusion),
		})
		if r.Conclusion != forge.CheckRunConclusionSuccess &&
			r.Conclusion != forge.CheckRunConclusionNeutral &&
			r.Conclusion != forge.CheckRunConclusionSkipped {
			anyFailed = true
		}
	}
	status := cicache.StatusSuccess
	if anyFailed {
		status = cicache.StatusFailure
	}
	return cicache.Result{Status: status, Checks: checks}, true
}

// pollRepoCIOnce fetches the default branch CI status for a single repo.
// Returns immediately; safe to call from any goroutine with a user context.
func (s *Server) pollRepoCIOnce(ctx context.Context, info repoInfo, f forge.Forge) { //nolint:gocritic // repoInfo passed by value intentionally
	sha, err := f.GetDefaultBranchSHA(ctx, info.ForgeOwner, info.ForgeRepo, info.BaseBranch)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			slog.Warn("pollRepoCIOnce: get SHA", "repo", info.RelPath, "err", err)
		}
		return
	}
	// Cache hit: use stored terminal result directly.
	if cached, ok := s.ciCache.Get(info.ForgeOwner, info.ForgeRepo, sha); ok {
		s.setRepoCIStatus(info.RelPath, cached)
		return
	}
	// Fetch check-runs for the new SHA.
	runs, err := f.GetCheckRuns(ctx, info.ForgeOwner, info.ForgeRepo, sha)
	if err != nil {
		if !errors.Is(err, forge.ErrNotFound) {
			slog.Warn("pollRepoCIOnce: get check-runs", "repo", info.RelPath, "err", err)
		}
		return
	}
	if len(runs) == 0 {
		return
	}
	result, done := evaluateCheckRuns(info.ForgeOwner, info.ForgeRepo, runs)
	if !done {
		// Still pending — store pending status without caching.
		s.setRepoCIStatus(info.RelPath, cicache.Result{Status: cicache.StatusPending})
		return
	}
	if err := s.ciCache.Put(info.ForgeOwner, info.ForgeRepo, sha, result); err != nil {
		slog.Warn("pollRepoCIOnce: cache put", "repo", info.RelPath, "err", err)
	}
	s.setRepoCIStatus(info.RelPath, result)
}

// pollCIForActiveRepos checks the default branch CI status for all repos that
// have active (non-terminal) tasks. Uses the authenticated user's forge token
// from ctx. Called from SSE handlers so no background goroutine is needed.
func (s *Server) pollCIForActiveRepos(ctx context.Context) {
	s.mu.Lock()
	var activeIdx []int
	for i := range s.repos {
		if s.repos[i].ForgeOwner != "" && s.repoHasActiveTasksLocked(s.repos[i].RelPath) {
			activeIdx = append(activeIdx, i)
		}
	}
	s.mu.Unlock()

	for _, i := range activeIdx {
		f := s.forgeForInfo(ctx, &s.repos[i])
		if f == nil {
			continue
		}
		s.pollRepoCIOnce(ctx, s.repos[i], f)
	}
}

// setRepoCIStatus updates the in-memory CI state for a repo and notifies
// SSE subscribers if the status changed.
func (s *Server) setRepoCIStatus(relPath string, result cicache.Result) {
	checks := make([]v1.ForgeCheck, len(result.Checks))
	for i, c := range result.Checks {
		checks[i] = v1.ForgeCheck{Name: c.Name, Owner: c.Owner, Repo: c.Repo, RunID: c.RunID, JobID: c.JobID, Conclusion: v1.CheckConclusion(c.Conclusion)}
	}
	next := repoCIState{Status: result.Status, Checks: checks}
	s.mu.Lock()
	prev := s.repoCIStatus[relPath]
	changed := prev.Status != next.Status
	s.repoCIStatus[relPath] = next
	s.mu.Unlock()
	if changed {
		s.notifyTaskChange()
	}
}

// repoHasActiveTasksLocked returns true if relPath has any non-terminal tasks.
// Must be called with mu held.
func (s *Server) repoHasActiveTasksLocked(relPath string) bool {
	for _, e := range s.tasks {
		if e.task.Repo == relPath && e.result == nil {
			return true
		}
	}
	return false
}

// repoInfoFor returns the repoInfo for relPath, or nil if not found.
// Safe to call without the mutex (s.repos is immutable after construction).
func (s *Server) repoInfoFor(relPath string) *repoInfo {
	for i := range s.repos {
		if s.repos[i].RelPath == relPath {
			return &s.repos[i]
		}
	}
	return nil
}

// forgeForInfo returns the appropriate forge.Forge for the repo's remote, using
// the configured tokens. Returns nil if no matching token is set.
func (s *Server) forgeForInfo(ctx context.Context, info *repoInfo) forge.Forge {
	return s.forgeFor(ctx, info.ForgeKind)
}

// forgeFor returns a Forge client for the given kind.
// In OAuth mode the authenticated user's access token is used.
// In PAT mode (no OAuth) the global token is used.
// Config.Validate ensures these two modes are never mixed.
// Returns nil if no token is available.
func (s *Server) forgeFor(ctx context.Context, kind forge.Kind) forge.Forge {
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == kind && u.AccessToken != "" {
		switch kind {
		case forge.KindGitHub:
			return &github.Client{Token: u.AccessToken}
		case forge.KindGitLab:
			return &gitlab.Client{Token: u.AccessToken}
		}
	}
	switch kind {
	case forge.KindGitHub:
		if s.githubToken != "" {
			return &github.Client{Token: s.githubToken}
		}
	case forge.KindGitLab:
		if s.gitlabToken != "" {
			return &gitlab.Client{Token: s.gitlabToken}
		}
	}
	return nil
}

// prefsForUser returns (lazily opening) the preferences store for the user
// in ctx. In no-auth mode, uses userID="default".
func (s *Server) prefsForUser(ctx context.Context) (*preferences.Store, error) {
	userID := "default"
	if u, ok := auth.UserFromContext(ctx); ok {
		userID = u.ID
	}
	if v, ok := s.prefsStores.Load(userID); ok {
		return v.(*preferences.Store), nil
	}
	path := filepath.Join(s.prefsDir, "preferences", userID+".json")
	store, err := preferences.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open preferences for %s: %w", userID, err)
	}
	actual, _ := s.prefsStores.LoadOrStore(userID, store)
	return actual.(*preferences.Store), nil
}

// hexDecode decodes a hex string to bytes, returning an error if invalid.
func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("odd length hex string")
	}
	b := make([]byte, len(s)/2)
	for i := range b {
		hi, lo := hexNibble(s[2*i]), hexNibble(s[2*i+1])
		if hi < 0 || lo < 0 {
			return nil, fmt.Errorf("invalid hex character at position %d", 2*i)
		}
		b[i] = byte(hi<<4 | lo) //nolint:gosec // G115: hi and lo are 0-15 from hexNibble, no overflow
	}
	return b, nil
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// parseAllowedUsers splits a comma-separated username list into a set.
// Returns nil for an empty input.
func parseAllowedUsers(csv string) map[string]struct{} {
	if csv == "" {
		return nil
	}
	m := make(map[string]struct{})
	for _, u := range strings.Split(csv, ",") {
		if u = strings.TrimSpace(u); u != "" {
			m[strings.ToLower(u)] = struct{}{}
		}
	}
	return m
}

// authEnabled reports whether OAuth authentication is configured.
func (s *Server) authEnabled() bool {
	return s.authStore != nil
}

// authProviders returns the list of configured OAuth provider names.
func (s *Server) authProviders() []string {
	var ps []string
	if s.githubOAuth != nil {
		ps = append(ps, "github")
	}
	if s.gitlabOAuth != nil {
		ps = append(ps, "gitlab")
	}
	return ps
}

func (s *Server) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.task
	if t.Container == "" {
		writeError(w, dto.Conflict("task has no container"))
		return
	}
	runner, ok := s.runners[t.Repo]
	if !ok {
		writeError(w, dto.InternalError("unknown repo"))
		return
	}
	path := r.URL.Query().Get("path")
	diff, err := runner.DiffContent(r.Context(), t.Branch, path)
	if err != nil {
		writeError(w, dto.InternalError(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v1.DiffResp{Diff: diff})
}

func (s *Server) handleGetUsage(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	resp := computeUsage(s.tasks, time.Now())
	s.mu.Unlock()

	if s.usage != nil {
		if oauth := s.usage.get(); oauth != nil {
			resp.FiveHour.Utilization = oauth.FiveHour.Utilization
			resp.FiveHour.ResetsAt = oauth.FiveHour.ResetsAt
			resp.SevenDay.Utilization = oauth.SevenDay.Utilization
			resp.SevenDay.ResetsAt = oauth.SevenDay.ResetsAt
			resp.ExtraUsage = oauth.ExtraUsage
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// getVoiceToken returns a Gemini API credential for the Android voice client.
//
// Currently returns the raw GEMINI_API_KEY (ephemeral=false) because the
// v1alpha ephemeral endpoint produces lower-quality responses. The client uses
// the ephemeral field to decide the WebSocket URL and auth parameter.
//
// TODO(security): Switch back to ephemeral tokens once v1beta supports
// auth_tokens or v1alpha quality improves. See getVoiceTokenEphemeral.
func (s *Server) getVoiceToken(_ context.Context, _ *dto.EmptyReq) (*v1.VoiceTokenResp, error) {
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	slog.Info("voice token", "keylen", len(apiKey), "mode", "raw_key")
	expireTime := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	return &v1.VoiceTokenResp{
		Token:     apiKey,
		ExpiresAt: expireTime,
	}, nil
}

// getVoiceTokenEphemeral creates a short-lived Gemini ephemeral token via
// POST /v1alpha/auth_tokens. Ephemeral tokens are v1alpha only; v1beta
// returns 404. The client must use v1alpha + BidiGenerateContentConstrained
// with ?access_token=.
//
// This path works but produces lower-quality voice responses than the v1beta
// BidiGenerateContent endpoint with a raw key. Kept for future use once Google
// stabilises v1beta ephemeral tokens.
//
// See https://ai.google.dev/gemini-api/docs/ephemeral-tokens
func (s *Server) getVoiceTokenEphemeral(ctx context.Context, _ *dto.EmptyReq) (*v1.VoiceTokenResp, error) { //nolint:unused // kept for future use
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	slog.Info("voice token", "keylen", len(apiKey), "mode", "ephemeral")
	now := time.Now().UTC()
	expireTime := now.Add(30 * time.Minute).Format(time.RFC3339)
	newSessionExpire := now.Add(2 * time.Minute).Format(time.RFC3339)

	reqBody := CreateAuthTokenConfig{
		Uses:                 1,
		ExpireTime:           expireTime,
		NewSessionExpireTime: newSessionExpire,
	}
	bodyBytes, err := json.Marshal(&reqBody)
	if err != nil {
		return nil, dto.InternalError("failed to marshal token request").Wrap(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1alpha/auth_tokens",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, dto.InternalError("failed to create token request").Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, dto.InternalError("failed to fetch ephemeral token").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, dto.InternalError(fmt.Sprintf("Gemini auth_tokens returned %d: %s", resp.StatusCode, string(body)))
	}

	var tokenResp AuthToken
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, dto.InternalError("failed to decode token response").Wrap(err)
	}

	tokenPrefix := tokenResp.Name
	if len(tokenPrefix) > 16 {
		tokenPrefix = tokenPrefix[:16]
	}
	slog.Info("voice token", "prefix", tokenPrefix, "len", len(tokenResp.Name))

	return &v1.VoiceTokenResp{
		Token:     tokenResp.Name,
		ExpiresAt: expireTime,
		Ephemeral: true,
	}, nil
}

// SetRunnerOps overrides container and agent backends on all runners.
func (s *Server) SetRunnerOps(c task.ContainerBackend, backends map[agent.Harness]agent.Backend) {
	for _, r := range s.runners {
		if c != nil {
			r.Container = c
		}
		if backends != nil {
			r.Backends = backends
		}
	}
}

// loadTerminatedTasks loads the last 10 terminated tasks from JSONL logs on disk.
// Exported for testing; New() uses the parallelized variant.
func (s *Server) loadTerminatedTasks() error {
	all, err := task.LoadLogs(s.logDir)
	if err != nil {
		return err
	}
	return s.loadTerminatedTasksFrom(all)
}

// loadTerminatedTasksFrom populates s.tasks from pre-loaded log data. It filters
// to tasks with an explicit caic_result trailer and keeps the most recent 10.
func (s *Server) loadTerminatedTasksFrom(all []*task.LoadedTask) error {
	// Filter to tasks with an explicit caic_result trailer.
	// Log files without a trailer may belong to still-running tasks.
	var terminated []*task.LoadedTask
	for _, lt := range all {
		if lt.Result != nil {
			terminated = append(terminated, lt)
		}
	}
	// LoadLogs returns ascending; reverse for most-recent-first, keep last 10.
	slices.Reverse(terminated)
	if len(terminated) > 10 {
		terminated = terminated[:10]
	}
	if len(terminated) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lt := range terminated {
		t := &task.Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: lt.Prompt},
			Repo:          lt.Repo,
			Harness:       lt.Harness,
			Branch:        lt.Branch,
			StartedAt:     lt.StartedAt,
		}
		t.SetState(lt.State)
		if lt.Title != "" {
			t.SetTitle(lt.Title)
		} else {
			t.SetTitle(lt.Prompt)
		}
		// TODO: Figure out when it was terminated.
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("load messages failed", "repo", lt.Repo, "br", lt.Branch, "err", err)
		}
		if lt.Msgs != nil {
			t.RestoreMessages(lt.Msgs)
		}
		// Backfill result stats from restored messages when the trailer
		// has zero cost (e.g. session exited without a final ResultMessage).
		if lt.Result.CostUSD == 0 {
			lt.Result.CostUSD, lt.Result.NumTurns, lt.Result.Duration, lt.Result.Usage, _ = t.LiveStats()
		}
		done := make(chan struct{})
		close(done)
		entry := &taskEntry{task: t, result: lt.Result, done: done}
		s.tasks[t.ID.String()] = entry
	}
	s.taskChanged()
	slog.Info("loaded terminated tasks from logs", "n", len(terminated))
	return nil
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI.
//
// Flow:
//  1. Map branches from terminated tasks to their IDs so live containers
//     can replace stale entries.
//  2. For each container matching a caic repo, call adoptOne concurrently.
//
// containers and allLogs are pre-loaded to avoid redundant I/O. If containers
// is nil (due to a container client error), adoption is skipped.
func (s *Server) adoptContainers(ctx context.Context, containers []*md.Container, allLogs []*task.LoadedTask) error {
	if containers == nil {
		return nil
	}

	// Map repo+branch loaded from terminated task logs to their ID in
	// s.tasks so we can replace stale entries with live containers.
	// The key is "repo\x00branch" because different repos can share a
	// branch name.
	s.mu.Lock()
	branchID := make(map[string]string, len(s.tasks))
	for id, e := range s.tasks {
		if e.task.Branch != "" {
			branchID[e.task.Repo+"\x00"+e.task.Branch] = id
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, ri := range s.repos {
		repoName := filepath.Base(ri.AbsPath)
		runner := s.runners[ri.RelPath]
		for _, c := range containers {
			branch, ok := container.BranchFromContainer(c.Name, repoName)
			if !ok {
				continue
			}
			wg.Go(func() {
				if err := s.adoptOne(ctx, ri, runner, c, branch, branchID, allLogs); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()
	return errors.Join(errs...)
}

// adoptOne investigates a single container and registers it as a task.
//
// It verifies the container has a "caic" label (proving caic started it),
// restores messages from either the relay output or JSONL logs, checks
// whether the relay is alive, and registers the task. If the relay is
// alive, it spawns a background goroutine to reattach. allLogs is the
// pre-loaded set of JSONL log files (shared across all adoptOne calls).
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, c *md.Container, branch string, branchID map[string]string, allLogs []*task.LoadedTask) error { //nolint:gocritic // repoInfo size increase from GitHub fields; refactor not worth it
	// Only adopt containers that caic started. The caic label is set at
	// container creation and is the authoritative proof of ownership.
	labelVal, err := container.LabelValue(ctx, c.Name, "caic")
	if err != nil {
		return fmt.Errorf("label check for %s: %w", c.Name, err)
	}
	if labelVal == "" {
		slog.Info("skipping non-caic container", "repo", ri.RelPath, "ctr", c.Name, "br", branch)
		return nil
	}
	taskID, err := ksid.Parse(labelVal)
	if err != nil {
		return fmt.Errorf("parse caic label %q on %s: %w", labelVal, c.Name, err)
	}

	// Find the most recent log file for this repo+branch from the pre-loaded
	// logs. Both repo and branch must match: different repos can share a
	// branch name (e.g. "caic-0"), and matching on branch alone would return
	// another repo's log, corrupting the title, prompt, and timestamps.
	var lt *task.LoadedTask
	for i := len(allLogs) - 1; i >= 0; i-- {
		if allLogs[i].Branch == branch && allLogs[i].Repo == ri.RelPath {
			lt = allLogs[i]
			break
		}
	}

	prompt := branch
	var startedAt time.Time
	var stateUpdatedAt time.Time

	// Read the harness from the container label (authoritative), falling
	// back to the log file, then to Claude as the default.
	harnessLabel, _ := container.LabelValue(ctx, c.Name, "harness")
	harnessName := agent.Harness(harnessLabel)
	if harnessName == "" && lt != nil {
		harnessName = lt.Harness
	}
	if harnessName == "" {
		harnessName = agent.Claude
	}

	// Check whether the relay daemon is alive in this container.
	relayAlive, relayErr := agent.IsRelayRunning(ctx, c.Name)
	if relayErr != nil {
		slog.Warn("relay check failed during adopt", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr)
	}

	var relayMsgs []agent.Message
	var relaySize int64
	if relayAlive {
		// Relay is alive — read authoritative output from container.
		relayMsgs, relaySize, relayErr = runner.ReadRelayOutput(ctx, c.Name, harnessName)
		if relayErr != nil {
			slog.Warn("read relay output failed", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr)
			relayAlive = false
		}
	}

	if lt != nil && lt.Prompt != "" {
		prompt = lt.Prompt
		startedAt = lt.StartedAt
		stateUpdatedAt = lt.LastStateUpdateAt
	}

	if stateUpdatedAt.IsZero() {
		stateUpdatedAt = time.Now().UTC()
	}
	t := &task.Task{
		ID:            taskID,
		InitialPrompt: agent.Prompt{Text: prompt},
		Repo:          ri.RelPath,
		Harness:       harnessName,
		Branch:        branch,
		Container:     c.Name,
		StartedAt:     startedAt,
		Tailscale:     c.Tailscale,
		TailscaleFQDN: c.TailscaleFQDN(ctx),
		USB:           c.USB,
		Display:       c.Display,
		Provider:      s.provider,
	}
	t.SetStateAt(task.StateRunning, stateUpdatedAt)
	// Set an immediate fallback title; GenerateTitle is fired async below
	// after messages are restored so the LLM sees the full conversation.
	if lt != nil && lt.Title != "" {
		t.SetTitle(lt.Title)
	} else {
		t.SetTitle(prompt)
	}

	if relayAlive && len(relayMsgs) > 0 {
		// Relay output is authoritative — zero loss. It contains both
		// Claude Code stdout and user inputs (logged by the relay).
		t.RestoreMessages(relayMsgs)
		t.RelayOffset = relaySize
		slog.Info("restored from relay", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(relayMsgs))
	} else if lt != nil {
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("load messages failed", "repo", ri.RelPath, "br", branch, "err", err)
		}
		if len(lt.Msgs) > 0 {
			t.RestoreMessages(lt.Msgs)
			slog.Info("restored from logs", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(lt.Msgs))
		}
	}

	// If the task is still running after message restoration (agent is
	// mid-turn), record now as the turn start. This is the best available
	// approximation on adoption; the real turn start predates the restart.
	t.SetTurnStartedAt(time.Now().UTC())

	// When the relay is dead (agent subprocess already exited) and
	// RestoreMessages didn't infer a terminal turn (no trailing
	// ResultMessage), the task would be stuck as "running" with no
	// session and no way to interact. Transition to StateWaiting so
	// the user can restart with a new prompt or terminate.
	if !relayAlive {
		relayLog := agent.ReadRelayLog(ctx, c.Name, 4096)
		if relayLog != "" {
			slog.Warn("relay log from dead relay", "ctr", c.Name, "br", branch, "log", relayLog)
		}
		if t.GetState() == task.StateRunning {
			t.SetState(task.StateWaiting)
			slog.Warn("adopted with dead relay, marking waiting",
				"repo", ri.RelPath, "br", branch, "ctr", c.Name,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	if oldID, ok := branchID[ri.RelPath+"\x00"+branch]; ok {
		// Replace the stale terminated entry with the live container.
		delete(s.tasks, oldID)
	}
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	slog.Info("adopted container",
		"repo", ri.RelPath, "ctr", c.Name, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Regenerate title async — relay may have new conversation data since the
	// log was written. The fallback title is already set above.
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive adoption

	// Auto-reconnect in background: relay alive → attach; relay dead
	// but SessionID present → --resume. Reconnect handles both paths
	// and reverts to StateWaiting if all strategies fail.
	if relayAlive || t.GetSessionID() != "" {
		strategy := "attach"
		if !relayAlive {
			strategy = "resume"
		}
		slog.Info("auto-reconnect starting", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "st", strategy)
		go func() {
			h, err := runner.Reconnect(ctx, t)
			if err != nil {
				slog.Warn("auto-reconnect failed",
					"repo", t.Repo, "br", t.Branch, "ctr", t.Container,
					"st", strategy, "err", err)
				s.notifyTaskChange()
				return
			}
			slog.Info("auto-reconnect succeeded", "repo", t.Repo, "br", t.Branch, "ctr", t.Container, "st", strategy)
			// Compute host-side diff stat after reconnect. Reconnect
			// replays relay messages which may include stale
			// DiffStatMessages (old relay code diffs against HEAD, not
			// base); the host-side diff captures the full branch diff.
			if ds := runner.BranchDiffStat(ctx, t.Branch); len(ds) > 0 {
				t.SetLiveDiffStat(ds)
			}
			s.notifyTaskChange()
			s.watchSession(entry, runner, h)
		}()
	} else if !relayAlive {
		slog.Warn("adopted orphaned task",
			"repo", ri.RelPath, "br", branch, "ctr", c.Name,
			"state", t.GetState())
	}
	return nil
}

// cleanupTask runs runner.Cleanup exactly once per task (guarded by
// entry.cleanupOnce), stores the result, notifies SSE, and closes entry.done.
func (s *Server) cleanupTask(entry *taskEntry, runner *task.Runner, reason task.State) {
	entry.cleanupOnce.Do(func() {
		result := runner.Cleanup(s.ctx, entry.task, reason)
		s.mu.Lock()
		entry.result = &result
		s.taskChanged()
		s.mu.Unlock()
		close(entry.done)
		// Post a comment back to GitHub if this was a webhook-triggered task.
		if entry.webhookIssueNumber > 0 && entry.webhookForgeFullName != "" {
			go s.postWebhookComment(entry)
		}
	})
}

// watchSession monitors a single active session. When the session's SSH
// process exits, it transitions the task to StateWaiting (the container and
// relay daemon may still be alive — see Flow 2 in the relay shutdown protocol
// in package agent). If entry.done fires first, the goroutine exits silently.
func (s *Server) watchSession(entry *taskEntry, runner *task.Runner, h *task.SessionHandle) {
	_ = runner // kept for interface consistency; may be used for future auto-reconnect
	go func() {
		done := h.Session.Done()
		select {
		case <-done:
			// Session died. Check if this handle is still the task's current
			// handle (restart may have replaced it). If stale, exit silently.
			current := entry.task.SessionDone()
			if current != done {
				return
			}
			t := entry.task
			t.DetachSession()
			result, sessionErr := h.Session.Wait()
			attrs := []any{"repo", t.Repo, "br", t.Branch, "ctr", t.Container}
			if result != nil {
				attrs = append(attrs, "result", result.Subtype)
			}
			if sessionErr != nil {
				attrs = append(attrs, "err", sessionErr)
				slog.Warn("session exited with error", attrs...)
			} else {
				slog.Info("session exited", attrs...)
			}
			// Only transition Running→Waiting. If addMessage() already set
			// Asking (agent asked a question) or the task is Terminating,
			// don't clobber that state.
			t.SetStateIf(task.StateRunning, task.StateWaiting)
			s.notifyTaskChange()
		case <-entry.done:
		}
	}()
}

// watchContainerEvents starts a single goroutine that listens for Docker
// container die events and triggers cleanup for the corresponding task.
func (s *Server) watchContainerEvents(ctx context.Context) {
	go func() {
		for {
			ch, err := container.WatchEvents(ctx, "caic")
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("docker events failed, retrying in 5s", "err", err)
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
			for ev := range ch {
				s.handleContainerDeath(ev.Name) //nolint:contextcheck // handleContainerDeath intentionally uses server context
			}
			// Stream ended. Reconnect unless context cancelled.
			if ctx.Err() != nil {
				return
			}
			slog.Warn("docker events stream ended, reconnecting in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// discoverKiloModels fetches available models from the OpenRouter API and
// updates all runners' Kilo backends. Falls back to defaults on any error.
func (s *Server) discoverKiloModels() {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://openrouter.ai/api/v1/models")
	if err != nil {
		slog.Warn("kilo: fetch OpenRouter models failed", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("kilo: OpenRouter non-200", "st", resp.StatusCode)
		return
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		slog.Warn("kilo: decode OpenRouter response failed", "err", err)
		return
	}
	models := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	if len(models) == 0 {
		slog.Warn("kilo: OpenRouter returned no models")
		return
	}
	for _, r := range s.runners {
		if b, ok := r.Backends[agent.Kilo].(*kilo.Backend); ok {
			b.SetModels(kilo.SortModels(models))
		}
	}
	slog.Info("kilo: models discovered", "n", len(models))
}

// handleContainerDeath looks up a task by container name and triggers cleanup.
func (s *Server) handleContainerDeath(containerName string) {
	s.mu.Lock()
	var found *taskEntry
	var runner *task.Runner
	for _, e := range s.tasks {
		if e.task.Container == containerName {
			found = e
			runner = s.runners[e.task.Repo]
			break
		}
	}
	s.mu.Unlock()
	if found == nil || runner == nil {
		return
	}
	slog.Info("container died, cleaning up task", "ctr", containerName, "task", found.task.ID, "br", found.task.Branch)
	go s.cleanupTask(found, runner, task.StateFailed)
}

// getTask looks up a task by the {id} path parameter.
// When auth is enabled, returns 403 if the task belongs to a different user.
func (s *Server) getTask(r *http.Request) (*taskEntry, error) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tasks[id]
	if !ok {
		return nil, dto.NotFound("task")
	}
	if s.authEnabled() {
		if u, ok := auth.UserFromContext(r.Context()); ok {
			if entry.task.OwnerID != "" && entry.task.OwnerID != u.ID {
				return nil, dto.Forbidden("task")
			}
		}
	}
	return entry, nil
}

// taskChanged closes the current changed channel and replaces it. Must be
// called while holding s.mu.
func (s *Server) taskChanged() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// notifyTaskChange signals that task data may have changed.
func (s *Server) notifyTaskChange() {
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
}

func (s *Server) repoURL(rel string) string {
	for _, r := range s.repos {
		if r.RelPath == rel {
			return gitutil.RemoteToHTTPS(r.Remote)
		}
	}
	return ""
}

func (s *Server) repoAbsPath(rel string) (string, bool) {
	for _, r := range s.repos {
		if r.RelPath == rel {
			return r.AbsPath, true
		}
	}
	return "", false
}

// tailscaleURL returns the Tailscale URL for the task, or "true" if enabled
// but FQDN not yet known, or "" if disabled.
func tailscaleURL(t *task.Task) string {
	if t.TailscaleFQDN != "" {
		return "https://" + t.TailscaleFQDN
	}
	if t.Tailscale {
		return "true"
	}
	return ""
}

// effectiveBaseBranch returns the branch the task was forked from: the task's
// own override if set, otherwise the runner's configured default.
func (s *Server) effectiveBaseBranch(t *task.Task) string {
	if t.BaseBranch != "" {
		return t.BaseBranch
	}
	if runner, ok := s.runners[t.Repo]; ok {
		return runner.BaseBranch
	}
	return ""
}

func (s *Server) toJSON(e *taskEntry) v1.Task {
	// Read all volatile fields in a single locked snapshot to avoid
	// data races with addMessage/RestoreMessages.
	snap := e.task.Snapshot()
	j := v1.Task{
		ID:             e.task.ID,
		InitialPrompt:  e.task.InitialPrompt.Text,
		Title:          snap.Title,
		Repo:           e.task.Repo,
		RemoteURL:      s.repoURL(e.task.Repo),
		BaseBranch:     s.effectiveBaseBranch(e.task),
		Branch:         e.task.Branch,
		Container:      e.task.Container,
		State:          snap.State.String(),
		StateUpdatedAt: float64(snap.StateUpdatedAt.UnixMilli()) / 1e3,
		Harness:        toV1Harness(e.task.Harness),
		Model:          snap.Model,
		AgentVersion:   snap.AgentVersion,
		SessionID:      snap.SessionID,
		InPlanMode:     snap.InPlanMode,
		PlanContent:    snap.PlanContent,
		Tailscale:      tailscaleURL(e.task),
		USB:            e.task.USB,
		Display:        e.task.Display,
		CostUSD:        snap.CostUSD,
		NumTurns:       snap.NumTurns,
		Duration:       snap.Duration.Seconds(),
	}
	if !e.task.StartedAt.IsZero() {
		j.StartedAt = float64(e.task.StartedAt.UnixMilli()) / 1e3
	}
	if !snap.TurnStartedAt.IsZero() {
		j.TurnStartedAt = float64(snap.TurnStartedAt.UnixMilli()) / 1e3
	}
	j.CumulativeInputTokens = snap.Usage.InputTokens
	j.CumulativeOutputTokens = snap.Usage.OutputTokens
	j.CumulativeCacheCreationInputTokens = snap.Usage.CacheCreationInputTokens
	j.CumulativeCacheReadInputTokens = snap.Usage.CacheReadInputTokens
	// Active tokens = last API call's context window fill (not the per-query sum).
	j.ActiveInputTokens = snap.LastAPIUsage.InputTokens + snap.LastAPIUsage.CacheCreationInputTokens
	j.ActiveCacheReadTokens = snap.LastAPIUsage.CacheReadInputTokens
	if r := s.runners[e.task.Repo]; r != nil {
		if b := r.Backends[e.task.Harness]; b != nil {
			j.ContextWindowLimit = b.ContextWindowLimit(snap.Model)
		}
	}
	if e.result != nil {
		j.DiffStat = toV1DiffStat(e.result.DiffStat)
		j.Result = e.result.AgentResult
		if e.result.Err != nil {
			j.Error = e.result.Err.Error()
		}
	} else {
		j.DiffStat = toV1DiffStat(snap.DiffStat)
	}
	j.ForgeOwner = snap.ForgeOwner
	j.ForgeRepo = snap.ForgeRepo
	j.ForgePR = snap.ForgePR
	j.CIStatus = v1.CIStatus(snap.CIStatus)
	if len(snap.CIChecks) > 0 {
		j.CIChecks = make([]v1.ForgeCheck, len(snap.CIChecks))
		for i, c := range snap.CIChecks {
			j.CIChecks[i] = v1.ForgeCheck{
				Name:       c.Name,
				Owner:      c.Owner,
				Repo:       c.Repo,
				RunID:      c.RunID,
				JobID:      c.JobID,
				Conclusion: v1.CheckConclusion(c.Conclusion),
			}
		}
	}
	if s.authStore != nil && e.task.OwnerID != "" {
		if u, ok := s.authStore.FindByID(e.task.OwnerID); ok {
			j.Owner = u.Username
		}
	}
	return j
}

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Flush implements http.Flusher so SSE handlers can flush through the wrapper.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.NewResponseController
// can discover interfaces like http.Flusher.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// roundDuration rounds d to 3 significant digits with minimum 1us precision.
func roundDuration(d time.Duration) time.Duration {
	for t := 100 * time.Second; t >= 100*time.Microsecond; t /= 10 {
		if d >= t {
			return d.Round(t / 100)
		}
	}
	return d.Round(time.Microsecond)
}
