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
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
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
	if ipgeo.ParseAllowlist(c.IPGeoAllowlist).NeedsDB() && c.IPGeoDB == "" {
		return errors.New("CAIC_IPGEO_DB is required when CAIC_IPGEO_ALLOWLIST contains country codes")
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

	// Throttle transports for forge API calls. GitHub OAuth, PAT, and App tokens
	// each have separate GitHub-side rate-limit buckets and must not share a throttle.
	// OAuth is per-user (separate buckets per authenticated user); guarded by mu.
	githubOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	githubPATThrottle    http.RoundTripper
	githubAppThrottle    http.RoundTripper
	gitlabOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	gitlabPATThrottle    http.RoundTripper

	// GitHub.
	githubToken            string
	githubOAuth            *auth.ProviderConfig // nil if not configured
	githubAllowedUsers     map[string]struct{}  // nil if GitHub OAuth not configured
	githubWebhookSecret    []byte               // nil when webhook not configured
	githubApp              githubAppClient      // nil when app not configured
	githubAppAllowedOwners map[string]struct{}  // nil = allow all; rejects installs from other owners

	// GitLab.
	gitlabToken         string
	gitlabWebhookSecret []byte               // nil when GitLab webhook not configured
	gitlabOAuth         *auth.ProviderConfig // nil if not configured
	gitlabAllowedUsers  map[string]struct{}  // nil if GitLab OAuth not configured

	// Auth / session.
	authStore     *auth.Store // nil when auth disabled
	sessionSecret []byte      // nil when auth disabled
	allowedHost   string      // hostname from ExternalURL; empty disables host checking
	usage         *usageFetcher

	// IP geolocation.
	ipgeoChecker   *ipgeo.Checker   // nil when CAIC_IPGEO_DB not set
	ipgeoAllowlist *ipgeo.Allowlist // nil when CAIC_IPGEO_ALLOWLIST not set

	// User preferences — all users in a single file.
	prefs *preferences.Store

	// Guarded by mu.
	mu                  sync.Mutex
	tasks               map[string]*taskEntry
	repoCIStatus        map[string]repoCIState // keyed by repoInfo.RelPath
	changed             chan struct{}          // closed on task mutation; replaced under mu
	githubInstallations map[string]int64       // owner (lowercase) → installation ID
}

// mdBackend adapts *md.Client to task.ContainerBackend.
type mdBackend struct {
	client   *md.Client
	provider genai.Provider // nil if LLM not configured

	mu                sync.Mutex
	pendingContainers map[string]*md.Container // keyed by container name
}

func (b *mdBackend) mdStartOpts(labels []string, opts *task.StartOptions) (client *md.Client, mdOpts *md.StartOpts) {
	harnessMap := map[agent.Harness]md.Harness{
		agent.Claude: md.HarnessClaude,
		agent.Codex:  md.HarnessCodex,
		agent.Gemini: md.HarnessGemini,
		agent.Kilo:   md.HarnessKilo,
	}
	mdHarness := harnessMap[opts.Harness]
	harnessPaths := md.HarnessMounts[mdHarness]
	image := opts.DockerImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	client = b.client
	mdOpts = &md.StartOpts{
		Quiet:      opts.LogWriter == nil,
		BaseImage:  image,
		Labels:     labels,
		AgentPaths: []md.AgentPaths{harnessPaths},
		USB:        opts.USB,
		Tailscale:  opts.Tailscale,
		Display:    opts.Display,
	}
	return client, mdOpts
}

func (b *mdBackend) Launch(ctx context.Context, repos []md.Repo, labels []string, opts *task.StartOptions) error {
	if len(repos) > 0 {
		slog.Info("md", "phase", "launch", "dir", repos[0].GitRoot, "br", repos[0].Branch, "hns", opts.Harness)
	} else {
		slog.Info("md", "phase", "launch", "hns", opts.Harness)
	}
	if _, ok := map[agent.Harness]md.Harness{
		agent.Claude: md.HarnessClaude,
		agent.Codex:  md.HarnessCodex,
		agent.Gemini: md.HarnessGemini,
		agent.Kilo:   md.HarnessKilo,
	}[opts.Harness]; !ok {
		return fmt.Errorf("unknown harness %q", opts.Harness)
	}
	client, mdOpts := b.mdStartOpts(labels, opts)
	c := client.Container(repos...)
	if opts.LogWriter != nil {
		c.W = opts.LogWriter
	}
	if err := c.Launch(ctx, mdOpts); err != nil {
		return err
	}
	b.mu.Lock()
	if b.pendingContainers == nil {
		b.pendingContainers = make(map[string]*md.Container)
	}
	b.pendingContainers[c.Name] = c
	b.mu.Unlock()
	return nil
}

func (b *mdBackend) Connect(ctx context.Context, repos []md.Repo, opts *task.StartOptions) (name, tailscaleFQDN string, err error) {
	if len(repos) > 0 {
		slog.Info("md", "phase", "connect", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	// Derive container name from repos (deterministic, same as Launch used).
	tmpClient := b.client
	c := tmpClient.Container(repos...)
	b.mu.Lock()
	if stored, ok := b.pendingContainers[c.Name]; ok {
		c = stored
		delete(b.pendingContainers, c.Name)
	}
	b.mu.Unlock()
	_, mdOpts := b.mdStartOpts(nil, opts)
	sr, err := c.Connect(ctx, mdOpts)
	if err != nil {
		return "", "", err
	}
	return c.Name, sr.TailscaleFQDN, nil
}

func (b *mdBackend) Diff(ctx context.Context, repo md.Repo, args ...string) (string, error) {
	slog.Info("md diff", "dir", repo.GitRoot, "br", repo.Branch, "args", args)
	var stdout bytes.Buffer
	if err := b.client.Container(repo).Diff(ctx, 0, &stdout, io.Discard, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func (b *mdBackend) Fetch(ctx context.Context, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md fetch", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	}
	ct := b.client.Container(repos...)
	for i := range repos {
		if err := ct.Fetch(ctx, i, b.provider); err != nil {
			return err
		}
	}
	return nil
}

func (b *mdBackend) Stop(ctx context.Context, name string) error {
	slog.Info("md stop", "name", name)
	ct := b.client.Container()
	ct.Name = name
	return ct.Stop(ctx)
}

func (b *mdBackend) Purge(ctx context.Context, name string, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md purge", "dir", repos[0].GitRoot, "br", repos[0].Branch)
	} else {
		slog.Info("md purge", "name", name)
	}
	ct := b.client.Container(repos...)
	if len(repos) == 0 {
		ct.Name = name
	}
	return ct.Purge(ctx)
}

func (b *mdBackend) Revive(ctx context.Context, name string, repos []md.Repo) error {
	if len(repos) > 0 {
		slog.Info("md revive", "dir", repos[0].GitRoot, "br", repos[0].Branch, "ctr", name)
	} else {
		slog.Info("md revive", "name", name)
	}
	ct := b.client.Container(repos...)
	if len(repos) == 0 {
		ct.Name = name
	}
	return ct.Revive(ctx)
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

// New creates a new Server. It discovers repos under rootDir, creates a Runner
// per repo, and adopts preexisting containers.
//
// Startup sequence:
//  1. Initialize container client (instant).
//  2. Parallel I/O phase: discover repos, load purged task logs, and list
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
	mdClient.DigestCacheTTL = warmupInterval

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

	prefsPath := filepath.Join(cfg.ConfigDir, "preferences.json")
	prefsStore, err := preferences.Open(prefsPath)
	if err != nil {
		return nil, fmt.Errorf("open preferences: %w", err)
	}

	backend := &mdBackend{client: mdClient}

	cachePath := filepath.Join(cfg.CacheDir, "ci_results.json")
	cache, err := forgecache.Open(cachePath)
	if err != nil {
		slog.Warn("cannot open CI cache; falling back to in-memory", "path", cachePath, "err", err)
		cache, _ = forgecache.Open("")
	}

	s := &Server{
		ctx:                  ctx,
		absRoot:              absRoot,
		runners:              make(map[string]*task.Runner, len(repoRes.paths)),
		mdClient:             mdClient,
		logDir:               logDir,
		prefs:                prefsStore,
		authStore:            authStore,
		sessionSecret:        sessionSecret,
		githubOAuth:          githubOAuth,
		gitlabOAuth:          gitlabOAuth,
		githubAllowedUsers:   githubAllowedUsers,
		gitlabAllowedUsers:   gitlabAllowedUsers,
		allowedHost:          allowedHost,
		usage:                newUsageFetcher(ctx),
		geminiAPIKey:         cfg.GeminiAPIKey,
		githubToken:          cfg.GitHubToken,
		gitlabToken:          cfg.GitLabToken,
		githubOAuthThrottles: make(map[string]http.RoundTripper),
		githubPATThrottle:    newThrottle(),
		githubAppThrottle:    newThrottle(),
		gitlabOAuthThrottles: make(map[string]http.RoundTripper),
		gitlabPATThrottle:    newThrottle(),
		ciCache:              cache,
		backend:              backend,
		tasks:                make(map[string]*taskEntry),
		repoCIStatus:         make(map[string]repoCIState),
		changed:              make(chan struct{}),
		githubInstallations:  make(map[string]int64),
	}
	s.githubWebhookSecret = cfg.GitHubWebhookSecret
	s.gitlabWebhookSecret = cfg.GitLabWebhookSecret
	if cfg.GitHubAppID != 0 && len(cfg.GitHubAppPrivateKeyPEM) > 0 {
		app, err := github.NewAppClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPEM, s.githubAppThrottle)
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		s.githubApp = app
		if cfg.GitHubAppAllowedOwners != "" {
			s.githubAppAllowedOwners = parseAllowedUsers(cfg.GitHubAppAllowedOwners)
		}
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
				slog.Info("title", "prov", p.Name(), "mdl", p.ModelID())
				s.provider = p
				backend.provider = p
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
			slog.Debug("discovered repo", "path", rel, "br", branch)
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

	// Wire the bot with the server as its client.
	// Eventually we may want to use a clearer observer pattern.
	s.bot = bot.New(ctx, s)

	// Always register a no-repo runner (keyed by "") for tasks that don't
	// need a git repository.
	noRepoRunner := &task.Runner{LogDir: logDir, Container: backend}
	_ = noRepoRunner.Init(ctx) // populates Backends; no-op for no-repo (no branches to scan)
	s.runners[""] = noRepoRunner

	// Phase 3: Load purged tasks from pre-loaded logs.
	if logRes.err != nil {
		slog.Warn("load logs failed", "err", logRes.err)
	} else {
		if err := s.loadPurgedTasksFrom(logRes.logs); err != nil {
			return nil, fmt.Errorf("load purged tasks: %w", err)
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

	// Resume bot comment watchers for adopted tasks with pending forge issues.
	s.bot.ResumePendingComments()

	if cfg.IPGeoDB != "" {
		checker, err := ipgeo.Open(cfg.IPGeoDB)
		if err != nil {
			return nil, fmt.Errorf("open ipgeo db: %w", err)
		}
		s.ipgeoChecker = checker
		s.ipgeoAllowlist = ipgeo.ParseAllowlist(cfg.IPGeoAllowlist)
		slog.Info("ipgeo", "path", cfg.IPGeoDB, "list", cfg.IPGeoAllowlist)
	}

	s.watchContainerEvents(ctx)
	go s.warmupImages()
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
	if s.allowedHost != "" {
		inner = hostCheckMiddleware(s.allowedHost, inner)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := ipgeo.GetClientIP(r)
		cc := s.ipgeoChecker.CountryCode(clientIP)
		if !s.ipgeoAllowlist.Allowed(cc) {
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
		GitHubAppEnabled:   s.githubApp != nil,
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

// reposLocked builds the current repo list including live CI status.
// Must be called with s.mu held.
func (s *Server) reposLocked() *[]v1.Repo {
	out := make([]v1.Repo, len(s.repos))
	for i, r := range s.repos {
		repo := v1.Repo{Path: r.RelPath, BaseBranch: r.BaseBranch, RemoteURL: gitutil.RemoteToHTTPS(r.Remote), Forge: v1.Forge(r.ForgeKind)}
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
	// Resolve primary runner (first repo, or no-repo).
	var primaryRunner *task.Runner
	if len(req.Repos) > 0 {
		r, ok := s.runners[req.Repos[0].Name]
		if !ok {
			return nil, dto.BadRequest("unknown repo: " + req.Repos[0].Name)
		}
		primaryRunner = r
	} else {
		r, ok := s.runners[""]
		if !ok {
			return nil, dto.InternalError("no-repo runner not available")
		}
		primaryRunner = r
	}

	// Validate and resolve extra repo runners.
	extraRunners := make([]*task.Runner, 0, max(0, len(req.Repos)-1))
	for _, rs := range req.Repos[min(1, len(req.Repos)):] {
		er, ok := s.runners[rs.Name]
		if !ok {
			return nil, dto.BadRequest("unknown extra repo: " + rs.Name)
		}
		extraRunners = append(extraRunners, er)
	}

	harness := toAgentHarness(req.Harness)
	backend, ok := primaryRunner.Backends[harness]
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

	// Build RepoMount slice — GitRoot filled immediately from runner.Dir.
	mounts := make([]task.RepoMount, len(req.Repos))
	for i, rs := range req.Repos {
		r := s.runners[rs.Name]
		mounts[i] = task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: r.Dir}
	}

	t := &task.Task{
		ID:            ksid.NewID(),
		InitialPrompt: v1PromptToAgent(req.InitialPrompt),
		Repos:         mounts,
		Harness:       harness,
		Model:         req.Model,
		DockerImage:   req.Image,
		Tailscale:     req.Tailscale,
		USB:           req.USB,
		Display:       req.Display,
		StartedAt:     time.Now().UTC(),
		OwnerID:       ownerID,
		Provider:      s.provider,
	}
	t.SetTitle(req.InitialPrompt.Text)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	// Run in background using the server context, not the request context.
	go func() {
		// Allocate branches for extra repos before starting the container.
		for i, er := range extraRunners {
			branch, err := er.AllocateBranch(s.ctx)
			if err != nil {
				result := task.Result{State: task.StateFailed, Err: fmt.Errorf("allocate branch for extra repo: %w", err)}
				s.mu.Lock()
				entry.result = &result
				s.taskChanged()
				s.mu.Unlock()
				close(entry.done)
				return
			}
			t.Repos[i+1].Branch = branch
		}

		h, err := primaryRunner.Start(s.ctx, t)
		if err != nil {
			result := task.Result{State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
			close(entry.done)
			return
		}
		s.watchSession(entry, primaryRunner, h)
	}()

	go s.maybeFakeCI(t)

	if len(req.Repos) > 0 {
		if err := s.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
			p.TouchRepo(req.Repos[0].Name, &preferences.RepoPrefs{
				BaseBranch: req.Repos[0].BaseBranch,
				Harness:    string(req.Harness),
				Model:      req.Model,
				BaseImage:  req.Image,
			})
		}); err != nil {
			return nil, dto.InternalError("save preferences: " + err.Error())
		}
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
	if state == task.StatePurged || state == task.StateFailed {
		return
	}

	for msg := range live {
		writeEvents(tracker.convertMessage(msg, time.Now()))
		flusher.Flush()
	}
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

	// With GitHub App configured, CI updates arrive via check_suite webhooks;
	// use a nil channel so the ticker case is never selected.
	var ciTickerC <-chan time.Time
	if s.githubApp == nil {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		ciTickerC = t.C
	}

	// Seed CI status immediately on connect (once); subsequent updates come from
	// webhooks (App) or the ciTicker (polling).
	go s.pollCIForActiveRepos(context.WithoutCancel(r.Context()))

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
		case <-ciTickerC:
			go s.pollCIForActiveRepos(context.WithoutCancel(r.Context()))
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

// sendInput forwards user input to the agent session. On failure, it probes
// the relay daemon's liveness over SSH and returns diagnostic details in the
// 409 response so the frontend can show the user what went wrong.
//
// The relay probe uses the server context (not the request context) because the
// SSH round-trip may outlive a cancelled HTTP request, and we want the log line
// regardless.
func (s *Server) sendInput(ctx context.Context, entry *taskEntry, req *v1.InputReq) (*v1.StatusResp, error) {
	if len(req.Prompt.Images) > 0 {
		primaryName := ""
		if p := entry.task.Primary(); p != nil {
			primaryName = p.Name
		}
		runner := s.runners[primaryName]
		if b := runner.Backends[entry.task.Harness]; b != nil && !b.SupportsImages() {
			return nil, dto.BadRequest(string(entry.task.Harness) + " does not support images")
		}
	}
	if err := entry.task.SendInput(ctx, v1PromptToAgent(req.Prompt)); err != nil {
		t := entry.task
		rs := relayNoContainer
		if t.Container != "" {
			probeCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			alive, relayErr := agent.IsRelayRunning(probeCtx, t.Container) //nolint:contextcheck // diagnostic probe; must outlive request
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
		var primaryBranchLog string
		if p := t.Primary(); p != nil {
			primaryBranchLog = p.Branch
		}
		slog.Warn("no active session",
			"task", t.ID,
			"br", primaryBranchLog,
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
	primaryName := ""
	if p := t.Primary(); p != nil {
		primaryName = p.Name
	}
	runner := s.runners[primaryName]
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

func (s *Server) stopTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.SetState(task.StateStopping)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	stopPrimaryName := ""
	if p := entry.task.Primary(); p != nil {
		stopPrimaryName = p.Name
	}
	runner := s.runners[stopPrimaryName]
	go func() {
		runner.StopTask(s.ctx, entry.task)
		s.mu.Lock()
		s.taskChanged()
		s.mu.Unlock()
	}()
	return &v1.StatusResp{Status: "stopping"}, nil
}

func (s *Server) purgeTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan && state != task.StateRunning && state != task.StateStopping && state != task.StateStopped {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.SetState(task.StatePurging)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	purgePrimaryName := ""
	if p := entry.task.Primary(); p != nil {
		purgePrimaryName = p.Name
	}
	runner := s.runners[purgePrimaryName]
	go s.cleanupTask(entry, runner, task.StatePurged)
	return &v1.StatusResp{Status: "purging"}, nil
}

func (s *Server) reviveTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateStopped {
		return nil, dto.Conflict("task is not stopped")
	}
	revivePrimaryName := ""
	if p := entry.task.Primary(); p != nil {
		revivePrimaryName = p.Name
	}
	runner := s.runners[revivePrimaryName]
	entry.task.SetState(task.StateProvisioning)
	s.mu.Lock()
	// Reset done channel so watchSession works on the revived task.
	entry.done = make(chan struct{})
	entry.result = nil
	entry.cleanupOnce = sync.Once{}
	s.taskChanged()
	s.mu.Unlock()
	go func() {
		h, err := runner.ReviveTask(s.ctx, entry.task)
		if err != nil {
			slog.Warn("revive failed", "task", entry.task.ID, "err", err)
			return
		}
		s.watchSession(entry, runner, h)
	}()
	return &v1.StatusResp{Status: "provisioning"}, nil
}

func (s *Server) syncTask(ctx context.Context, entry *taskEntry, req *v1.SyncReq) (*v1.SyncResp, error) {
	t := entry.task
	switch t.GetState() {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateStopping, task.StateStopped, task.StatePurging, task.StateFailed, task.StatePurged:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StatePulling, task.StatePushing:
	}
	syncPrimaryName := ""
	syncPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		syncPrimaryName = p.Name
		syncPrimaryBranch = p.Branch
	}
	runner := s.runners[syncPrimaryName]

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
		ds, issues, err := runner.SyncToDefault(ctx, syncPrimaryBranch, t.Container, message, t.ExtraMDRepos())
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
	ds, issues, err := runner.SyncToOrigin(ctx, syncPrimaryBranch, t.Container, req.Force, t.ExtraMDRepos())
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	status := "synced"
	if len(ds) == 0 {
		status = "empty"
	} else if len(issues) > 0 && !req.Force {
		status = "blocked"
	}
	resp := &v1.SyncResp{Status: status, Branch: syncPrimaryBranch, DiffStat: toV1DiffStat(ds), SafetyIssues: toV1SafetyIssues(issues)}
	if status != "blocked" {
		if info := s.repoInfoFor(syncPrimaryName); info != nil {
			if f := s.forgeForInfo(ctx, info); f != nil {
				prNumber, err := s.startPRFlow(ctx, entry, f, info, syncPrimaryBranch, s.effectiveBaseBranch(t))
				if err != nil {
					slog.Warn("sync: create PR", "repo", info.ForgeRepo, "branch", syncPrimaryBranch, "err", err)
				} else {
					resp.PRNumber = prNumber
				}
			} else {
				slog.Warn("sync: no forge client available, skipping PR flow", "repo", syncPrimaryName, "forge", info.ForgeKind)
			}
		} else {
			slog.Warn("sync: repo not found in server list, skipping PR flow", "repo", syncPrimaryName)
		}
	}
	return resp, nil
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
	diffPrimaryName := ""
	diffPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		diffPrimaryName = p.Name
		diffPrimaryBranch = p.Branch
	}
	runner, ok := s.runners[diffPrimaryName]
	if !ok {
		writeError(w, dto.InternalError("unknown repo"))
		return
	}
	path := r.URL.Query().Get("path")
	diff, err := runner.DiffContent(r.Context(), diffPrimaryBranch, path)
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

// loadPurgedTasks loads the last 5 purged tasks per repository from JSONL logs on disk.
// Exported for testing; New() uses the parallelized variant.
func (s *Server) loadPurgedTasks() error {
	all, err := task.LoadLogs(s.logDir)
	if err != nil {
		return err
	}
	return s.loadPurgedTasksFrom(all)
}

// loadPurgedTasksFrom populates s.tasks from pre-loaded log data. It keeps
// tasks updated within the last 7 days and limits the result to the 5 most
// recent per repository. Tasks without a caic_result trailer get a synthetic
// result; their state is inferred from messages and finalised in the setup
// loop. adoptContainers removes all stale entries for any branch that has a
// live container, so no-trailer tasks never duplicate adopted ones.
func (s *Server) loadPurgedTasksFrom(all []*task.LoadedTask) error {
	// Include all tasks updated within the last 7 days, with or without a
	// caic_result trailer. Trailer-less tasks (interrupted or still-running)
	// are deduplicated by adoptContainers which sweeps all stale entries for
	// each branch it adopts.
	var purged []*task.LoadedTask
	now := time.Now().UTC()
	for _, lt := range all {
		if now.Sub(lt.LastStateUpdateAt) > 7*24*time.Hour {
			continue
		}
		if lt.Result == nil {
			lt.Result = &task.Result{State: task.StateFailed}
		}
		purged = append(purged, lt)
	}
	// LoadLogs returns ascending; reverse for most-recent-first, keep last 5 per repo.
	slices.Reverse(purged)
	perRepo := make(map[string]int)
	kept := purged[:0]
	for _, lt := range purged {
		key := ""
		if p := lt.Primary(); p != nil {
			key = p.Name
		}
		if perRepo[key] < 5 {
			perRepo[key]++
			kept = append(kept, lt)
		}
	}
	purged = kept
	if len(purged) == 0 {
		slog.Info("no purged tasks to load", "candidates", len(all))
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lt := range purged {
		taskID := ksid.NewID()
		if len(lt.TaskID) == 13 {
			if parsed, parseErr := ksid.Parse(lt.TaskID); parseErr == nil && parsed != 0 {
				taskID = parsed
			}
		}
		t := &task.Task{
			ID:            taskID,
			InitialPrompt: agent.Prompt{Text: lt.Prompt},
			Repos:         lt.Repos, // GitRoot is empty for purged tasks
			Harness:       lt.Harness,
			StartedAt:     lt.StartedAt,
		}
		t.SetState(lt.State)
		if lt.Title != "" {
			t.SetTitle(lt.Title)
		} else {
			t.SetTitle(lt.Prompt)
		}
		// TODO: Figure out when it was purged.
		if err := lt.LoadMessages(); err != nil {
			ltPrimary := lt.Primary()
			ltRepo, ltBranch := "", ""
			if ltPrimary != nil {
				ltRepo = ltPrimary.Name
				ltBranch = ltPrimary.Branch
			}
			slog.Warn("load messages failed", "repo", ltRepo, "br", ltBranch, "err", err)
		}
		if lt.Msgs != nil {
			t.RestoreMessages(lt.Msgs)
		}
		// For tasks without a caic_result trailer (lt.State == StateRunning
		// sentinel), any state RestoreMessages inferred is unreliable — the
		// task may have been purged or interrupted without a trailer.
		// Force StateFailed; adoptContainers replaces this entry with the
		// correct live state if the container is still running.
		if lt.State == task.StateRunning {
			t.SetState(task.StateFailed)
		}
		// SetPR after LoadMessages: the header-only tail scan may miss
		// caic_pr when the record is beyond the 64 KiB window; the full
		// parse in LoadMessages always finds it.
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
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
	slog.Info("loaded purged tasks from logs", "n", len(purged))
	return nil
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI.
//
// Flow:
//  1. Map branches from purged tasks to their IDs so live containers
//     can replace stale entries.
//  2. For each container matching a caic repo, call adoptOne concurrently.
//
// containers and allLogs are pre-loaded to avoid redundant I/O. If containers
// is nil (due to a container client error), adoption is skipped.
func (s *Server) adoptContainers(ctx context.Context, containers []*md.Container, allLogs []*task.LoadedTask) error {
	if containers == nil {
		return nil
	}

	// Map repo+branch loaded from purged task logs to their ID in
	// s.tasks so we can replace stale entries with live containers.
	// The key is "repo\x00branch" because different repos can share a
	// branch name.
	s.mu.Lock()
	// Map repo+branch → all stale task IDs so adoptOne can remove every
	// matching entry (there may be multiple log files per branch when a
	// branch was reused or when trailer-less tasks were loaded alongside
	// properly-purged ones with the same branch).
	branchIDs := make(map[string][]string, len(s.tasks))
	for id, e := range s.tasks {
		if p := e.task.Primary(); p != nil && p.Branch != "" {
			key := p.Name + "\x00" + p.Branch
			branchIDs[key] = append(branchIDs[key], id)
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	claimed := make(map[string]bool, len(containers))
	for _, ri := range s.repos {
		repoName := filepath.Base(ri.AbsPath)
		runner := s.runners[ri.RelPath]
		for _, c := range containers {
			branch, ok := container.BranchFromContainer(c.Name, repoName)
			if !ok {
				continue
			}
			claimed[c.Name] = true
			wg.Go(func() {
				if err := s.adoptOne(ctx, ri, runner, c, branch, branchIDs, allLogs); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()

	// Adopt no-repo containers. md names them "md-agent-<hex>" when started
	// with no repos (md.Client.Container with zero Repo arguments).
	if noRepoRunner := s.runners[""]; noRepoRunner != nil {
		for _, c := range containers {
			if claimed[c.Name] || !strings.HasPrefix(c.Name, "md-agent-") {
				continue
			}
			wg.Go(func() {
				if err := s.adoptOne(ctx, repoInfo{}, noRepoRunner, c, "", branchIDs, allLogs); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
		wg.Wait()
	}

	return errors.Join(errs...)
}

// adoptOne investigates a single container and registers it as a task.
//
// It verifies the container has a "caic" label (proving caic started it),
// restores messages from either the relay output or JSONL logs, checks
// whether the relay is alive, and registers the task. If the relay is
// alive, it spawns a background goroutine to reattach. allLogs is the
// pre-loaded set of JSONL log files (shared across all adoptOne calls).
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, c *md.Container, branch string, branchIDs map[string][]string, allLogs []*task.LoadedTask) error { //nolint:gocritic // repoInfo size increase from GitHub fields; refactor not worth it
	// Only adopt containers that caic started. The caic label is set at
	// container creation and is the authoritative proof of ownership.
	labelVal, err := container.LabelValue(ctx, c.Name, "caic")
	if err != nil {
		return fmt.Errorf("label check for %s: %w", c.Name, err)
	}
	if labelVal == "" {
		slog.Info("container", "msg", "skipping non-caic", "repo", ri.RelPath, "ctr", c.Name, "br", branch)
		return nil
	}
	taskID, err := ksid.Parse(labelVal)
	if err != nil {
		return fmt.Errorf("parse caic label %q on %s: %w", labelVal, c.Name, err)
	}

	// Exited containers are adopted as stopped tasks. The user can
	// explicitly revive them via the UI or API when ready.
	isExited := c.State == "exited"
	if isExited {
		slog.Info("container", "msg", "adopting exited container as stopped", "ctr", c.Name, "br", branch)
	}

	// Find the log file for this task. For repo-based tasks, match by repo+branch
	// (most recent first) since different repos can share branch names. For no-repo
	// tasks (branch==""), match by task ID parsed from the filename, which is the
	// only reliable disambiguator when multiple no-repo tasks share the same empty
	// repo+branch values.
	taskIDStr := taskID.String()
	var lt *task.LoadedTask
	for i := len(allLogs) - 1; i >= 0; i-- {
		log := allLogs[i]
		if branch == "" && ri.RelPath == "" {
			if log.TaskID == taskIDStr {
				lt = log
				break
			}
		} else {
			lp := log.Primary()
			if lp != nil && lp.Branch == branch && lp.Name == ri.RelPath {
				lt = log
				break
			}
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
	// Skip for exited containers — can't exec into them.
	var relayAlive bool
	var relayMsgs []agent.Message
	var relaySize int64
	var relayDiag string
	if !isExited {
		var relayErr error
		relayAlive, relayDiag, relayErr = agent.RelayStatus(ctx, c.Name)
		if relayErr != nil {
			slog.Warn("relay", "msg", "check failed during adopt", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr, "diag", relayDiag)
		}
		if relayAlive {
			// Relay is alive — read authoritative output from container.
			relayMsgs, relaySize, relayErr = runner.ReadRelayOutput(ctx, c.Name, harnessName)
			if relayErr != nil {
				slog.Warn("relay", "msg", "read output failed", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "err", relayErr)
				relayAlive = false
			}
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
	var adoptRepos []task.RepoMount
	if ri.RelPath != "" {
		// Primary mount from repoInfo; extra mounts from log.
		adoptRepos = []task.RepoMount{{Name: ri.RelPath, GitRoot: ri.AbsPath, Branch: branch}}
		if lt != nil {
			for _, lm := range lt.Repos[1:] {
				gitRoot := ""
				if er, ok := s.runners[lm.Name]; ok {
					gitRoot = er.Dir
				}
				adoptRepos = append(adoptRepos, task.RepoMount{Name: lm.Name, BaseBranch: lm.BaseBranch, Branch: lm.Branch, GitRoot: gitRoot})
			}
		}
	}
	var forgeIssue int
	if lt != nil {
		forgeIssue = lt.ForgeIssue
	}
	t := &task.Task{
		ID:            taskID,
		InitialPrompt: agent.Prompt{Text: prompt},
		Repos:         adoptRepos,
		Harness:       harnessName,
		Container:     c.Name,
		StartedAt:     startedAt,
		Tailscale:     c.Tailscale,
		TailscaleFQDN: c.TailscaleFQDN(ctx),
		USB:           c.USB,
		Display:       c.Display,
		Provider:      s.provider,
		ForgeIssue:    forgeIssue,
	}
	t.SetStateAt(task.StateRunning, stateUpdatedAt)
	// Set an immediate fallback title; GenerateTitle is fired async below
	// after messages are restored so the LLM sees the full conversation.
	if lt != nil && lt.Title != "" {
		t.SetTitle(lt.Title)
	} else {
		t.SetTitle(prompt)
	}
	switch {
	case lt != nil && lt.ForgePR > 0:
		// Restore PR created during a previous session (persisted in log).
		t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
	case forgeIssue > 0 && ri.ForgeOwner != "":
		// Ensure forge owner/repo are set so the bot can resolve a commenter.
		t.SetPR(ri.ForgeOwner, ri.ForgeRepo, 0)
	case ri.ForgeOwner != "" && branch != "" && ri.ForgeKind != "":
		// Query the forge for an existing PR created outside of caic.
		f := s.forgeForInfo(ctx, &ri)
		if f == nil && s.authStore != nil {
			if u, ok := s.authStore.FindByProvider(ri.ForgeKind); ok {
				f = s.forgeFor(auth.NewContext(ctx, &u), ri.ForgeKind)
			}
		}
		if f != nil {
			pr, err := f.FindPRByBranch(ctx, ri.ForgeOwner, ri.ForgeRepo, branch)
			if err == nil && pr.Number > 0 {
				slog.Info("adopt: found external PR", "repo", ri.RelPath, "br", branch, "pr", pr.Number)
				t.SetPR(ri.ForgeOwner, ri.ForgeRepo, pr.Number)
			}
		}
	}

	// Restore messages from relay or logs.
	if relayAlive && len(relayMsgs) > 0 {
		// Relay output is authoritative — zero loss. It contains both
		// Claude Code stdout and user inputs (logged by the relay).
		t.RestoreMessages(relayMsgs)
		t.RelayOffset = relaySize
		slog.Debug("relay", "msg", "restored from", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(relayMsgs))
	} else if lt != nil {
		if err := lt.LoadMessages(); err != nil {
			slog.Warn("load messages failed", "repo", ri.RelPath, "br", branch, "err", err)
		}
		if len(lt.Msgs) > 0 {
			t.RestoreMessages(lt.Msgs)
			slog.Warn("relay", "msg", "restored from log", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "msgs", len(lt.Msgs))
		}
	}

	// The header-only tail scan may miss caic_pr when the record is beyond
	// the 64 KiB window. If the PR is still unset, do a full parse of the
	// log to recover it. This covers both the relay-alive path (where
	// LoadMessages was skipped) and the log-restore path.
	if lt != nil && t.GetPR() == 0 {
		if lt.ForgePR == 0 {
			// Full parse not yet done; trigger it for PR metadata only.
			_ = lt.LoadMessages()
		}
		if lt.ForgePR > 0 {
			t.SetPR(lt.ForgeOwner, lt.ForgeRepo, lt.ForgePR)
		}
	}

	// If the task is still running after message restoration (agent is
	// mid-turn), record now as the turn start. This is the best available
	// approximation on adoption; the real turn start predates the restart.
	if !isExited {
		t.SetTurnStartedAt(time.Now().UTC())
	}

	// Exited containers are always stopped — user must revive explicitly.
	if isExited {
		t.SetState(task.StateStopped)
	} else if !relayAlive {
		// Relay is dead but container is running. Read relay log for
		// diagnostics, then mark waiting so the user can restart or
		// we can auto-reconnect via --resume.
		relayLog := agent.ReadRelayLog(ctx, c.Name, 4096)
		if relayLog != "" {
			slog.Warn("relay", "msg", "log from dead relay", "ctr", c.Name, "br", branch, "diag", relayDiag, "log", relayLog)
		}
		if t.GetState() == task.StateRunning {
			t.SetState(task.StateWaiting)
			slog.Warn("relay", "msg", "dead, marking waiting",
				"repo", ri.RelPath, "br", branch, "ctr", c.Name,
				"sess", t.GetSessionID(), "msgs", len(t.Messages()))
		}
	}

	// Track whether we've already registered the task entry (happens for external PRs).
	entryRegistered := false
	entry := &taskEntry{task: t, done: make(chan struct{})}

	// Register entry and start CI monitoring if a PR was found (either from logs or external).
	if t.GetPR() > 0 && ri.ForgeOwner != "" && ri.ForgeKind != "" {
		// The adoption context has no authenticated user. Try the general
		// lookup first (PAT / GitHub App), then fall back to a stored
		// OAuth token from the auth store (most recently seen user for
		// this forge provider).
		f := s.forgeForInfo(ctx, &ri)
		if f == nil && s.authStore != nil {
			if u, ok := s.authStore.FindByProvider(ri.ForgeKind); ok {
				f = s.forgeFor(auth.NewContext(ctx, &u), ri.ForgeKind)
			}
		}
		slog.Info("adopt: CI monitoring", "task", t.ID, "pr", t.GetPR(), "forgeKind", ri.ForgeKind, "forgeOwner", ri.ForgeOwner, "hasForge", f != nil)
		if f != nil {
			s.mu.Lock()
			if ri.RelPath != "" || branch != "" {
				for _, oldID := range branchIDs[ri.RelPath+"\x00"+branch] {
					delete(s.tasks, oldID)
				}
			}
			s.tasks[t.ID.String()] = entry
			s.taskChanged()
			s.mu.Unlock()
			entryRegistered = true
			// Get the PR head SHA for CI monitoring.
			pr := t.Snapshot().ForgePR
			if pr > 0 {
				sha, err := f.GetDefaultBranchSHA(ctx, ri.ForgeOwner, ri.ForgeRepo, branch)
				if err != nil {
					slog.Warn("adopt: GetDefaultBranchSHA failed", "task", t.ID, "branch", branch, "err", err)
				} else {
					slog.Info("adopt: starting monitorCI", "task", t.ID, "branch", branch, "sha", sha)
					s.mu.Lock()
					entry.monitorBranch = branch
					s.mu.Unlock()
					go s.monitorCI(s.ctx, entry, f, ri.ForgeOwner, ri.ForgeRepo, sha) //nolint:contextcheck // CI monitoring must outlive the request
				}
			}
		}
	}

	if !entryRegistered {
		s.mu.Lock()
		if ri.RelPath != "" || branch != "" {
			for _, oldID := range branchIDs[ri.RelPath+"\x00"+branch] {
				delete(s.tasks, oldID)
			}
		}
		s.tasks[t.ID.String()] = entry
		s.taskChanged()
		s.mu.Unlock()
	}

	slog.Info("container", "msg", "adopted",
		"repo", ri.RelPath, "ctr", c.Name, "br", branch,
		"relay", relayAlive, "state", t.GetState(), "sess", t.GetSessionID())

	// Only regenerate title if a new turn was completed since the log was
	// written (relay captured ResultMessages beyond what the log has).
	// Count results in the restored messages; if the relay has more than the
	// log, a turn happened while the server was down and the title is stale.
	if needsTitleRegen(t, lt) {
		go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive adoption
	}

	// Auto-reconnect in background: relay alive → attach; relay dead
	// → restart relay via --resume (requires a session ID).
	// Skip reconnect for stopped tasks — container is not running.
	if t.GetState() != task.StateStopped && (relayAlive || t.GetSessionID() != "") {
		strategy := "attach"
		if !relayAlive {
			strategy = "resume"
		}
		slog.Debug("container", "msg", "auto-reconnect starting", "repo", ri.RelPath, "br", branch, "ctr", c.Name, "st", strategy)
		go func() {
			tlog := slog.With("repo", ri.RelPath, "br", branch, "ctr", t.Container)
			h, err := runner.Reconnect(ctx, t, true)
			if err != nil {
				tlog.Warn("auto-reconnect failed", "st", strategy, "err", err)
				s.notifyTaskChange()
				return
			}
			// If --resume exits immediately (previous session complete),
			// start a fresh idle relay so the task can accept prompts.
			h, err = runner.EnsureSession(ctx, t, h, tlog)
			if err != nil {
				tlog.Warn("ensure session failed", "err", err)
				t.SetState(task.StateWaiting)
				s.notifyTaskChange()
				return
			}
			tlog.Debug("auto-reconnect succeeded", "st", strategy)
			// Compute host-side diff stat after reconnect. Reconnect
			// replays relay messages which may include stale
			// DiffStatMessages (old relay code diffs against HEAD, not
			// base); the host-side diff captures the full branch diff.
			var adoptPrimaryBranch string
			if p := t.Primary(); p != nil {
				adoptPrimaryBranch = p.Branch
			}
			if ds := runner.BranchDiffStat(ctx, adoptPrimaryBranch, t.ExtraMDRepos()); len(ds) > 0 {
				t.SetLiveDiffStat(ds)
			}
			s.notifyTaskChange()
			s.watchSession(entry, runner, h)
		}()
	} else if !relayAlive && t.GetState() != task.StateStopped {
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
	})
}

// watchSession monitors a single active session. When the session's SSH
// process exits, it transitions the task to StateWaiting (the container and
// relay daemon may still be alive — see Flow 2 in the relay shutdown protocol
// in package agent). If entry.done fires first, the goroutine exits silently.
func (s *Server) watchSession(entry *taskEntry, runner *task.Runner, h *task.SessionHandle) {
	_ = runner // kept for interface consistency
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
			// Close the dispatch goroutine. CloseMsgCh is idempotent so this
			// is safe even if StopTask races and closes MsgCh concurrently.
			h.CloseMsgCh()
			<-h.DispatchDone
			if h.LogW != nil {
				_ = h.LogW.Close()
			}
			watchPrimaryName := ""
			watchPrimaryBranch := ""
			if p := t.Primary(); p != nil {
				watchPrimaryName = p.Name
				watchPrimaryBranch = p.Branch
			}
			attrs := []any{"repo", watchPrimaryName, "br", watchPrimaryBranch, "ctr", t.Container}
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
			// Asking (agent asked a question) or the task is Purging,
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
				s.handleContainerDeath(ev.Name)
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

// warmupInterval controls how often warmupImages re-checks for new base image
// versions. It also sets DigestCacheTTL so that container starts between
// warmup cycles reuse the cached digest instead of hitting the registry.
const warmupInterval = 6 * time.Hour

// warmupImages periodically calls md.Client.Warmup for the default base image
// and any custom images configured in user preferences. This ensures the image
// is pulled and the md-user layer is built before a task needs it.
func (s *Server) warmupImages() {
	// Run immediately on startup, then every warmupInterval.
	ticker := time.NewTicker(warmupInterval)
	defer ticker.Stop()
	for {
		images := []string{md.DefaultBaseImage + ":latest"}
		for _, img := range s.prefs.BaseImages() {
			if !slices.Contains(images, img) {
				images = append(images, img)
			}
		}
		for _, img := range images {
			built, err := s.mdClient.Warmup(s.ctx, &md.WarmupOpts{
				BaseImage: img,
				Quiet:     true,
			})
			if err != nil {
				slog.Warn("warmup", "image", img, "err", err)
			} else if built {
				slog.Info("warmup", "image", img, "built", true)
			}
		}
		select {
		case <-ticker.C:
		case <-s.ctx.Done():
			return
		}
	}
}

// handleContainerDeath looks up a task by container name and archives it.
// The container is not destroyed — it transitions to StateStopped so it
// can be revived on the next server restart (e.g. after a Docker or
// machine restart).
func (s *Server) handleContainerDeath(containerName string) {
	s.mu.Lock()
	var found *taskEntry
	for _, e := range s.tasks {
		if e.task.Container != containerName {
			continue
		}
		found = e
		break
	}
	s.mu.Unlock()
	if found == nil {
		return
	}
	t := found.task
	state := t.GetState()
	// Only archive active tasks. Already-terminal tasks should not be touched.
	if state == task.StatePurged || state == task.StateFailed || state == task.StateStopped || state == task.StateStopping {
		return
	}
	deathBranch := ""
	if p := t.Primary(); p != nil {
		deathBranch = p.Branch
	}
	slog.Info("container", "msg", "died, archiving as stopped", "ctr", containerName, "task", t.ID, "br", deathBranch, "prev_state", state)
	// Detach any active session (SSH is dead).
	t.DetachSession()
	t.SetState(task.StateStopped)
	s.notifyTaskChange()
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

func (s *Server) toJSON(e *taskEntry) v1.Task {
	// Read all volatile fields in a single locked snapshot to avoid
	// data races with addMessage/RestoreMessages.
	snap := e.task.Snapshot()

	// Build Repos slice for API response.
	taskRepos := make([]v1.TaskRepo, len(e.task.Repos))
	for i, r := range e.task.Repos {
		taskRepos[i] = v1.TaskRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch, RemoteURL: s.repoURL(r.Name), Forge: s.repoForge(r.Name)}
	}
	if len(taskRepos) == 0 {
		taskRepos = nil
	}

	// Derive primary name for context window lookup.
	var primaryName string
	if p := e.task.Primary(); p != nil {
		primaryName = p.Name
	}

	j := v1.Task{
		ID:             e.task.ID,
		InitialPrompt:  e.task.InitialPrompt.Text,
		Title:          snap.Title,
		Repos:          taskRepos,
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
	if snap.ContextWindowLimit > 0 {
		j.ContextWindowLimit = snap.ContextWindowLimit
	} else if primaryName != "" {
		if r := s.runners[primaryName]; r != nil {
			if b := r.Backends[e.task.Harness]; b != nil {
				j.ContextWindowLimit = b.ContextWindowLimit(snap.Model)
			}
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
	j.ForgeIssue = snap.ForgeIssue
	j.CIStatus = v1.CIStatus(snap.CIStatus)
	if len(snap.CIChecks) > 0 {
		j.CIChecks = make([]v1.ForgeCheck, len(snap.CIChecks))
		for i := range snap.CIChecks {
			j.CIChecks[i] = checkToDTO(&snap.CIChecks[i])
		}
	}
	if s.authStore != nil && e.task.OwnerID != "" {
		if u, ok := s.authStore.FindByID(e.task.OwnerID); ok {
			j.Owner = u.Username
		}
	}
	return j
}
