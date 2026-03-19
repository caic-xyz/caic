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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
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
	ciCache  *forgecache.Cache
	provider genai.Provider // nil if LLM not configured
	bot      *bot.Bot       // handles forge event-driven task automation

	// Agent backends.
	geminiAPIKey string

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
	authStore     *auth.Store    // nil when auth disabled
	sessionSecret []byte         // nil when auth disabled
	allowedHost   string         // hostname from ExternalURL; empty disables static host checking
	autoHostLock  *autoHostState // non-nil when ExternalURL is "auto"
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
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/stop", handleWithTask(s, s.stopTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/purge", handleWithTask(s, s.purgeTask))
	apiMux.HandleFunc("POST /api/v1/tasks/{id}/revive", handleWithTask(s, s.reviveTask))
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
	mux.HandleFunc("POST /webhooks/github", s.handleGitHubWebhook)
	mux.HandleFunc("POST /webhooks/gitlab", s.handleGitLabWebhook)
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
	switch {
	case s.allowedHost != "":
		inner = hostCheckMiddleware(s.allowedHost, inner)
	case s.autoHostLock != nil:
		inner = autoHostCheckMiddleware(s.autoHostLock, inner)
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
		GitHubAppEnabled:   s.forge.githubApp != nil,
	}
	if s.authEnabled() {
		cfg.AuthProviders = s.authProviders()
	}
	return cfg, nil
}

func (s *Server) getPreferences(ctx context.Context, _ *dto.EmptyReq) (*v1.PreferencesResp, error) {
	prefs := s.prefs.Get(userIDFromCtx(ctx))
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
	cacheMappings := make([]v1.CacheMappingResp, len(prefs.Settings.CacheMappings))
	for i, m := range prefs.Settings.CacheMappings {
		cacheMappings[i] = v1.CacheMappingResp{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
		}
	}
	return &v1.PreferencesResp{
		Repositories: repos,
		Harness:      prefs.Harness,
		Models:       prefs.Models,
		Settings: v1.UserSettings{
			AutoFixOnCIFailure: prefs.Settings.AutoFixOnCIFailure,
			AutoFixOnPROpen:    prefs.Settings.AutoFixOnPROpen,
			BaseImage:          prefs.Settings.BaseImage,
			UseDefaultCaches:   prefs.Settings.UseDefaultCaches,
			WellKnownCaches:    prefs.Settings.WellKnownCaches,
			CacheMappings:      cacheMappings,
		},
	}, nil
}

func (s *Server) updatePreferences(ctx context.Context, req *v1.UpdatePreferencesReq) (*v1.PreferencesResp, error) {
	if err := s.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
		p.Settings.AutoFixOnCIFailure = req.Settings.AutoFixOnCIFailure
		p.Settings.AutoFixOnPROpen = req.Settings.AutoFixOnPROpen
		p.Settings.BaseImage = req.Settings.BaseImage
		p.Settings.UseDefaultCaches = req.Settings.UseDefaultCaches
		p.Settings.WellKnownCaches = req.Settings.WellKnownCaches
		if req.Settings.CacheMappings != nil {
			p.Settings.CacheMappings = make([]preferences.CacheMapping, len(req.Settings.CacheMappings))
			for i, m := range req.Settings.CacheMappings {
				p.Settings.CacheMappings[i] = preferences.CacheMapping{
					HostPath:      m.HostPath,
					ContainerPath: m.ContainerPath,
				}
			}
		}
	}); err != nil {
		return nil, dto.InternalError("save preferences: " + err.Error())
	}
	// Return the updated preferences.
	return s.getPreferences(ctx, nil)
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

func (s *Server) listCaches(_ context.Context, _ *dto.EmptyReq) (*v1.WellKnownCachesResp, error) {
	harnessMounts := make([]string, 0, len(md.HarnessMounts))
	for _, hp := range md.HarnessMounts {
		for _, p := range hp.HomePaths {
			harnessMounts = append(harnessMounts, "~/"+p)
		}
	}
	slices.Sort(harnessMounts)
	harnessMounts = slices.Compact(harnessMounts)

	wellKnown := make([]v1.WellKnownCache, 0, len(md.WellKnownCaches))
	for name, mounts := range md.WellKnownCaches {
		containerPaths := make([]string, len(mounts))
		for i, m := range mounts {
			containerPaths[i] = m.ContainerPath
		}
		wellKnown = append(wellKnown, v1.WellKnownCache{
			Name:        name,
			Description: mounts[0].Description,
			Mounts:      containerPaths,
		})
	}
	slices.SortFunc(wellKnown, func(a, b v1.WellKnownCache) int {
		return strings.Compare(a.Name, b.Name)
	})

	return &v1.WellKnownCachesResp{
		HarnessMounts: harnessMounts,
		WellKnown:     wellKnown,
	}, nil
}

func (s *Server) listRepos(_ context.Context, _ *dto.EmptyReq) (*[]v1.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reposLocked(), nil
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
	args := []string{"clone", "--depth", strconv.Itoa(depth), "--recurse-submodules", "--shallow-submodules", req.URL, absTarget}
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

	var cloneForgeKind forge.Kind
	var cloneForgeOwner, cloneForgeRepo string
	if rawURL, err := forge.RemoteURL(ctx, absTarget); err == nil {
		cloneForgeKind, cloneForgeOwner, cloneForgeRepo, _ = forge.ParseRemoteURL(rawURL)
	}
	info := repoInfo{RelPath: targetPath, AbsPath: absTarget, BaseBranch: branch, Remote: remote, ForgeKind: cloneForgeKind, ForgeOwner: cloneForgeOwner, ForgeRepo: cloneForgeRepo}
	s.repos = append(s.repos, info)
	s.runners[targetPath] = runner
	slog.Info("cloned repo", "url", req.URL, "path", targetPath)

	return &v1.Repo{Path: targetPath, BaseBranch: branch, RemoteURL: gitutil.RemoteToHTTPS(remote), Forge: v1.Forge(cloneForgeKind)}, nil
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
