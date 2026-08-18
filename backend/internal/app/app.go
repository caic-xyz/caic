// Package app assembles the caic backend application.
package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/git"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"golang.org/x/sync/errgroup"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/gomode/voicegateway"
	"github.com/caic-xyz/caic/gomode/voicegateway/voicertc"
	"github.com/caic-xyz/caic/oauth/oauthclient"
)

const repoDiscoveryDepth = 3

// App owns the caic backend application lifetime.
type App struct {
	Server *server.Router

	voiceBridge     *voicertc.Bridge
	backgroundTasks []backgroundTask
	taskMgr         *taskmgr.Manager
}

type backgroundTask func(context.Context) error

type mdRuntime struct {
	client  *md.Client
	backend *mdruntime.Backend
}

type repoDiscoveryResult struct {
	paths []string
	err   error
}

type instanceDiscoveryResult struct {
	instances []runtime.Instance
	err       error
}

// Serve starts the HTTP server and closes app-owned resources when serving ends.
func (a *App) Serve(ctx context.Context, ln net.Listener) (err error) {
	defer func() { err = errors.Join(err, a.taskMgr.Close()) }()

	group, groupCtx := errgroup.WithContext(ctx)
	for _, task := range a.backgroundTasks {
		group.Go(func() error { return task(groupCtx) })
	}
	group.Go(func() error { return a.Server.Serve(groupCtx, ln) })
	err = group.Wait()
	if a.voiceBridge != nil {
		a.voiceBridge.CloseAll(context.WithoutCancel(ctx))
	}
	return err
}

// New creates the caic backend server application.
func New(ctx context.Context, log *slog.Logger, rootDir string, cfg *server.Config) (*App, error) {
	if log == nil {
		return nil, errors.New("logger is required")
	}
	appLog := log.With("cmp", "app")
	if cfg.Dirs.ConfigDir == "" {
		return nil, errors.New("ConfigDir is required")
	}
	if cfg.Dirs.CacheDir == "" {
		return nil, errors.New("CacheDir is required")
	}
	if err := os.MkdirAll(cfg.Dirs.ConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	if err := os.MkdirAll(cfg.Dirs.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	logDir := filepath.Join(cfg.Dirs.CacheDir, "tasks")
	if err := cleanupLegacyReplayArtifacts(logDir); err != nil {
		return nil, fmt.Errorf("remove legacy replay artifacts: %w", err)
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	ctx, startTask := trace.NewTask(ctx, "server.startup")
	defer startTask.End()

	runtimes, mdRuntimes, err := initRuntimeSystem(ctx, appLog, cfg)
	if err != nil {
		return nil, err
	}

	repoCh := make(chan repoDiscoveryResult, 1)

	go func() {
		defer trace.StartRegion(ctx, "discover-repos").End()
		paths, err := git.DiscoverCheckouts(rootDir, repoDiscoveryDepth)
		repoCh <- repoDiscoveryResult{paths, err}
	}()
	settings, err := loadSettings(appLog, filepath.Join(cfg.Dirs.ConfigDir, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	var hostState *auth.HostState
	isAuto := strings.EqualFold(cfg.Auth.ExternalURL, "auto")
	if isAuto {
		hostState = &auth.HostState{}
	} else if cfg.Auth.ExternalURL != "" {
		hostState = auth.NewHostState(cfg.Auth.ExternalURL)
	}

	appLog.InfoContext(ctx, "github", "pat", auth.MaskedToken(cfg.GitHub.Token), "oauth", auth.MaskedToken(cfg.GitHub.OAuthClientID))
	appLog.InfoContext(ctx, "gitlab", "pat", auth.MaskedToken(cfg.GitLab.Token), "oauth", auth.MaskedToken(cfg.GitLab.OAuthClientID))
	appLog.InfoContext(ctx, "google", "oauth", auth.MaskedToken(cfg.Google.OAuthClientID))

	var authStore *auth.Store
	var sessionSecret []byte
	var githubOAuth *oauthclient.ProviderConfig
	var gitlabOAuth *oauthclient.ProviderConfig
	var googleOAuth *oauthclient.ProviderConfig
	oauthConfigured := cfg.GitHub.OAuthClientID != "" || cfg.GitLab.OAuthClientID != "" || cfg.Google.OAuthClientID != ""
	if oauthConfigured {
		secret, err := hex.DecodeString(settings.SessionSecret)
		if err != nil {
			return nil, fmt.Errorf("decode session secret: %w", err)
		}
		sessionSecret = secret
		store, err := auth.Open(filepath.Join(cfg.Dirs.ConfigDir, "users.json"))
		if err != nil {
			return nil, fmt.Errorf("open users store: %w", err)
		}
		authStore = store
		if cfg.GitHub.OAuthClientID != "" && cfg.GitHub.OAuthClientSecret != "" {
			githubOAuth = oauthclient.NewGitHubConfig(
				cfg.GitHub.OAuthClientID, cfg.GitHub.OAuthClientSecret,
				func(r *http.Request) string {
					u := hostState.ExternalURL(r)
					if u == "" {
						return ""
					}
					return u + "/auth/github/callback"
				},
			)
		}
		if cfg.GitLab.OAuthClientID != "" && cfg.GitLab.OAuthClientSecret != "" {
			gitlabOAuth = oauthclient.NewGitLabConfig(
				cfg.GitLab.OAuthClientID, cfg.GitLab.OAuthClientSecret, cfg.GitLab.URL,
				func(r *http.Request) string {
					u := hostState.ExternalURL(r)
					if u == "" {
						return ""
					}
					return u + "/auth/gitlab/callback"
				},
			)
		}
		if cfg.Google.OAuthClientID != "" && cfg.Google.OAuthClientSecret != "" {
			googleOAuth = oauthclient.NewGoogleConfig(
				cfg.Google.OAuthClientID, cfg.Google.OAuthClientSecret,
				func(r *http.Request) string {
					u := hostState.ExternalURL(r)
					if u == "" {
						return ""
					}
					return u + "/auth/google/callback"
				},
			)
		}
	}

	prefsStore, err := preferences.Open(filepath.Join(cfg.Dirs.ConfigDir, "preferences.json"))
	if err != nil {
		return nil, fmt.Errorf("open preferences: %w", err)
	}

	agentBackends := backends.Default(cfg.Dirs.CacheDir, cfg.Agent.HarnessEnv)
	if cfg.Agent.Backends != nil {
		agentBackends = cfg.Agent.Backends
	}
	cache, err := forgecache.Open(filepath.Join(cfg.Dirs.CacheDir, "ci_results.json"))
	if err != nil {
		appLog.WarnContext(ctx, "cannot open CI cache; falling back to in-memory", "err", err)
		cache, _ = forgecache.Open("")
	}

	var voiceBridge *voicertc.Bridge
	if cfg.Voice.Gateway.Mode == server.VoiceGatewayModeEmbedded {
		voiceCfg := cfg.Voice.Gateway.Config
		port := voiceCfg.Server.WebRTCUDPPort
		key := providerAPIKey("gemini", cfg.Agent.CoreEnv, nil)
		switch {
		case port < 0:
		case voiceCfg.Backend == voicegateway.BackendGeminiLive && key == "":
			appLog.InfoContext(ctx, "voice bridge disabled: GEMINI_API_KEY not set")
		default:
			voiceBridge, err = voicertc.NewBridge(ctx, &voiceCfg, key, port)
			if err != nil {
				return nil, fmt.Errorf("voice bridge: %w", err)
			}
		}
	}

	forgeManager := forgemgr.New(log, cfg.GitHub.Token, cfg.GitLab.Token, nil, authForgeTokenSource{})
	if cfg.GitHub.AppID != 0 && len(cfg.GitHub.AppPrivateKeyPEM) > 0 {
		app, err := github.NewAppClient(cfg.GitHub.AppID, cfg.GitHub.AppPrivateKeyPEM, forgeManager.GitHubAppThrottle())
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		forgeManager.SetGitHubApp(app)
	}

	provider := initProvider(ctx, appLog, cfg)
	for i := range mdRuntimes {
		mdRuntimes[i].backend.Provider = provider
	}

	checkoutRegistry := repo.NewRegistry()
	taskMgr, err := taskmgr.New(taskmgr.Config{
		ServerCtx:           ctx,
		Log:                 log,
		CacheDir:            cfg.Dirs.CacheDir,
		Runtimes:            runtimes,
		Checkouts:           checkoutRegistry,
		Backends:            agentBackends,
		HarnessEnv:          cfg.Agent.HarnessEnv,
		RuntimeMetadata:     cfg.Runtime.Metadata,
		RuntimeStartTimeout: time.Hour,
		Provider:            provider,
	})
	if err != nil {
		return nil, fmt.Errorf("task manager: %w", err)
	}
	keepTaskMgr := false
	defer func() {
		if !keepTaskMgr {
			_ = taskMgr.Close()
		}
	}()
	if err := taskMgr.BeginImport(); err != nil {
		appLog.WarnContext(ctx, "runtime event watch unavailable during startup", "err", err)
	}
	instanceCh := make(chan instanceDiscoveryResult, 1)
	go func() {
		defer trace.StartRegion(ctx, "list-runtime-instances").End()
		instances, err := runtimes.List(ctx)
		instanceCh <- instanceDiscoveryResult{instances, err}
	}()
	// Compression replaces log paths, so finish maintenance before task import can
	// expose any task to a replay request.
	err = func() error {
		startupCtx, tk := trace.NewTask(ctx, "prepare-purged-task-logs")
		defer tk.End()
		start := time.Now()
		logs, err := task.LoadLogs(logDir)
		if err != nil {
			return fmt.Errorf("load logs: %w", err)
		}
		appLog.InfoContext(startupCtx, "loaded task log headers", "n", len(logs), "dur", time.Since(start))
		start = time.Now()
		if err := taskMgr.Logs.CompressTerminalLogs(logs); err != nil {
			appLog.WarnContext(startupCtx, "compress terminal task logs failed", "err", err)
		} else {
			appLog.InfoContext(startupCtx, "compressed terminal task logs", "dur", time.Since(start))
		}
		start = time.Now()
		if err := taskMgr.LoadPurgedTasks(logs); err != nil {
			appLog.ErrorContext(startupCtx, "load purged tasks failed", "err", err)
		} else {
			appLog.InfoContext(startupCtx, "loaded purged task entries", "dur", time.Since(start))
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}

	repoRes := <-repoCh
	if repoRes.err != nil {
		return nil, fmt.Errorf("discover repos: %w", repoRes.err)
	}
	instanceRes := <-instanceCh
	if instanceRes.err != nil {
		return nil, fmt.Errorf("list runtime instances: %w", instanceRes.err)
	}

	repoStatus := ci.NewRepoStatusStore()

	// Long-lived forge automation, owned by app and routed to by the HTTP layer.
	warnings := server.NewWarningStore(taskMgr)
	cacheSizes := server.NewCacheSizeStore(log.With("cmp", "cache-sizes"))
	botClient := &botClient{log: log.With("cmp", "bot"), checkouts: checkoutRegistry, taskMgr: taskMgr, forgeMgr: forgeManager}
	ciAdapter := &ciAdapter{
		checkouts:   checkoutRegistry,
		repoStatus:  repoStatus,
		taskMgr:     taskMgr,
		forgeMgr:    forgeManager,
		prefs:       prefsStore,
		warnings:    warnings,
		taskCreator: botClient,
	}
	ciService := ci.NewService(log, cache, provider, ciAdapter)
	botService := bot.New(ctx, log, botClient)

	ipgeoChecker, err := ipgeo.NewChecker(ctx, log.With("cmp", "ipgeo"), cfg.IPGeo.Allowlist, cfg.IPGeo.DB, cfg.Dirs.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeo.DB != "" {
		appLog.InfoContext(ctx, "ipgeo", "path", cfg.IPGeo.DB, "list", cfg.IPGeo.Allowlist)
	}

	s, err := server.New(ctx, log, server.Dependencies{
		Checkouts:                  checkoutRegistry,
		CheckoutRoot:               absRoot,
		RepoStatus:                 repoStatus,
		Tailscale:                  cfg.Runtime.TailscaleAPIKey != "",
		Preferences:                prefsStore,
		AuthStore:                  authStore,
		SessionSecret:              sessionSecret,
		OAuthPrivateKeyPEM:         []byte(settings.OAuthPrivateKeyPEM),
		OAuthKeyID:                 settings.OAuthKeyID,
		OAuthRefreshTokenStorePath: filepath.Join(cfg.Dirs.ConfigDir, "oauth_refresh_tokens.json"),
		AuditLogPath:               filepath.Join(cfg.Dirs.CacheDir, "oauth_audit.jsonl"),
		GitHubOAuth:                githubOAuth,
		GitLabOAuth:                gitlabOAuth,
		GoogleOAuth:                googleOAuth,
		HostState:                  hostState,
		UsageFetchers:              usageFetchers(ctx, appLog, cfg),
		VoiceBridge:                voiceBridge,
		VoiceGateway:               cfg.Voice.Gateway,
		ForgeMgr:                   forgeManager,
		CICache:                    cache,
		Runtimes:                   runtimes,
		TaskMgr:                    taskMgr,
		Provider:                   provider,
		IPGeoChecker:               ipgeoChecker,
		Bot:                        botService,
		CIService:                  ciService,
		TaskClient:                 botClient,
		Warnings:                   warnings,
		CacheSizes:                 cacheSizes,
		GitHubAllowedUsers:         cfg.GitHub.OAuthAllowedUsers,
		GitLabAllowedUsers:         cfg.GitLab.OAuthAllowedUsers,
		GoogleAllowedUsers:         cfg.Google.OAuthAllowedUsers,
		GitHubWebhookSecret:        cfg.GitHub.WebhookSecret,
		GitLabWebhookSecret:        cfg.GitLab.WebhookSecret,
		GitHubAppAllowedOwners:     cfg.GitHub.AppAllowedOwners,
		Pprof:                      cfg.Debug.Pprof,
		FakeCI:                     cfg.FakeCI,
	})
	if err != nil {
		return nil, err
	}

	liveBranches := repo.LiveBranchesByRoot(instanceRes.instances)
	checkouts := make(chan *repo.Checkout, len(repoRes.paths))
	var wg sync.WaitGroup
	for _, abs := range repoRes.paths {
		wg.Go(func() {
			defer trace.StartRegion(ctx, "repo-checkout-init").End()
			repoLog := appLog.With("phase", "repo-discovery", "path", abs)
			result, err := repo.DiscoverCheckout(ctx, repoLog, abs, liveBranches[abs])
			if err != nil {
				repoLog.WarnContext(ctx, "skipping repo", "err", err)
				return
			}
			result.RelPath = checkoutRelPath(absRoot, abs)
			checkouts <- result
			repoLog.DebugContext(ctx, "discovered repo", "checkout", result.RelPath, "br", result.BaseBranch)
		})
	}
	wg.Wait()
	close(checkouts)
	for checkout := range checkouts {
		if err := checkoutRegistry.RegisterCheckout(checkout); err != nil {
			appLog.WarnContext(ctx, "skipping duplicate checkout", "checkout", checkout.RelPath, "err", err)
		}
	}

	phase3 := trace.StartRegion(ctx, "load-live-task-logs")
	liveLogs, err := loadRuntimeTaskLogs(ctx, logDir, runtimes, instanceRes.instances)
	if err != nil {
		appLog.WarnContext(ctx, "load live task logs failed; affected instances will not be imported", "err", err)
	}
	phase3.End()

	phase4 := trace.StartRegion(ctx, "import-runtime-instances")
	imported, err := taskMgr.ImportInstances(ctx, instanceRes.instances, liveLogs)
	if err != nil {
		appLog.ErrorContext(ctx, "import runtime instances failed; affected instances will remain unmanaged", "err", err)
	}
	taskMgr.Start()
	backgroundTasks := []backgroundTask{}
	importWiring := &importedTaskWiring{
		log:       log.With("cmp", "import-ci"),
		authStore: authStore,
		ciService: ciService,
		forgeMgr:  forgeManager,
		taskMgr:   taskMgr,
	}
	for _, entry := range imported {
		t := entry.Task()
		primary := t.Primary()
		if primary == nil {
			continue
		}
		checkout, ok := taskMgr.Checkouts.Checkout(primary.Name)
		if !ok || checkout.Repository == nil {
			continue
		}
		if t.GetPR() > 0 {
			backgroundTasks = append(backgroundTasks, func(ctx context.Context) error {
				importWiring.WireCIMonitoring(ctx, entry, checkout)
				return nil
			})
		}
		if t.ForgeIssue == 0 && t.GetPR() == 0 && primary.Branch != "" {
			backgroundTasks = append(backgroundTasks, func(ctx context.Context) error {
				importWiring.LookupExternalPRForTask(ctx, entry, checkout)
				return nil
			})
		}
	}
	phase4.End()

	region := trace.StartRegion(ctx, "bot-resume-comments")
	botService.ResumePendingComments()
	region.End()

	if !cfg.Runtime.SkipWarmup {
		backgroundTasks = append(backgroundTasks, func(ctx context.Context) error {
			_, tk := trace.NewTask(ctx, "warmup-images")
			defer tk.End()
			trace.Log(ctx, "startup", "warmup-images: begin")
			var errs []error
			for i := range mdRuntimes {
				if err := warmupImages(ctx, log.With("cmp", "warmup"), mdRuntimes[i].client, prefsStore); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		})
	}
	backgroundTasks = append(backgroundTasks,
		func(ctx context.Context) error {
			_, tk := trace.NewTask(ctx, "watch-harness-model-cache")
			defer tk.End()
			trace.Log(ctx, "startup", "watch-harness-model-cache: begin")
			return watchHarnessModelCache(ctx, log.With("cmp", "model-refresh"), cfg.Dirs.CacheDir, runtimes, taskMgr, cfg.Agent.HarnessEnv)
		},
		func(ctx context.Context) error {
			_, tk := trace.NewTask(ctx, "refresh-cache-sizes")
			defer tk.End()
			trace.Log(ctx, "startup", "refresh-cache-sizes: begin")
			cacheSizes.RefreshLoop(ctx)
			return nil
		},
		func(ctx context.Context) error {
			newRepoWatcher(ctx, log.With("cmp", "repo-watcher"), absRoot, checkoutRegistry, repoStatus, runtimes).watch()
			return nil
		},
	)
	keepTaskMgr = true
	return &App{Server: s, voiceBridge: voiceBridge, backgroundTasks: backgroundTasks, taskMgr: taskMgr}, nil
}

func checkoutRelPath(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return filepath.Base(dir)
	}
	if rel == "." {
		return ""
	}
	return rel
}

// cleanupLegacyReplayArtifacts removes obsolete derived files left by older
// releases. It is a one-time startup upgrade and removes only the known top-level
// temporary directory.
func cleanupLegacyReplayArtifacts(logDir string) error {
	entries, err := os.ReadDir(logDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(logDir, name)
		if name == ".replay-tmp" {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			continue
		}
		if entry.IsDir() || (!strings.HasSuffix(name, ".events.zst") && !strings.HasSuffix(name, ".taskmeta.json")) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func initRuntimeSystem(ctx context.Context, log *slog.Logger, cfg *server.Config) (*runtime.Router, []mdRuntime, error) {
	if cfg.Runtime.System != nil {
		runtimeRouter, err := runtime.NewRouter(log, []runtime.System{cfg.Runtime.System})
		if err != nil {
			return nil, nil, fmt.Errorf("init fake runtime router: %w", err)
		}
		return runtimeRouter, nil, nil
	}

	var mdRuntimes []mdRuntime
	var runtimes []runtime.System
	for _, name := range mdruntime.AvailableRuntimeNames() {
		mdClient, err := mdruntime.New(ctx, log, cfg.Runtime.TailscaleAPIKey, cfg.GitHub.Token, name)
		if err != nil {
			return nil, nil, fmt.Errorf("init %s md runtime adapter: %w", name, err)
		}
		mdClient.DigestCacheTTL = warmupInterval
		backend := mdruntime.NewBackend(log, mdClient)
		backend.HarnessEnv = cfg.Agent.HarnessEnv
		if _, err := backend.List(ctx); err != nil {
			log.WarnContext(ctx, "container runtime unavailable", "runtime", name, "err", err)
			continue
		}
		mdRuntimes = append(mdRuntimes, mdRuntime{client: mdClient, backend: backend})
		runtimes = append(runtimes, backend)
	}
	if len(runtimes) == 0 {
		return nil, nil, errors.New("no container runtime available: install docker or podman")
	}

	runtimeRouter, err := runtime.NewRouter(log, runtimes)
	if err != nil {
		return nil, nil, fmt.Errorf("init runtime router: %w", err)
	}
	return runtimeRouter, mdRuntimes, nil
}

func loadRuntimeTaskLogs(ctx context.Context, logDir string, inventory *runtime.Router, instances []runtime.Instance) ([]*task.LoadedTask, error) {
	ids := make([]string, 0, len(instances))
	var errs []error
	for i := range instances {
		id, err := runtimeTaskID(ctx, inventory, instances[i].ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	logs, err := task.LoadLogsForTaskIDs(logDir, ids)
	if err != nil {
		errs = append(errs, err)
	}
	return logs, errors.Join(errs...)
}

func runtimeTaskID(ctx context.Context, inventory *runtime.Router, id runtime.ID) (string, error) {
	value, err := inventory.Metadata(ctx, id, runtime.MetadataTaskID)
	if err != nil {
		return "", fmt.Errorf("metadata %s on %s: %w", runtime.MetadataTaskID, id, err)
	}
	if value != "" {
		return value, nil
	}
	value, err = inventory.Metadata(ctx, id, runtime.MetadataLegacyTaskID)
	if err != nil {
		return "", fmt.Errorf("metadata %s on %s: %w", runtime.MetadataLegacyTaskID, id, err)
	}
	return value, nil
}

func initProvider(ctx context.Context, log *slog.Logger, cfg *server.Config) genai.Provider {
	llmProvider := cfg.LLM.Provider
	if !cfg.LLM.Disable && llmProvider == "" {
		llmProvider = autoDetectLLMProvider(ctx, log, cfg.Agent.CoreEnv)
		if llmProvider != "" {
			log.InfoContext(ctx, "auto-detected LLM provider", "prov", llmProvider)
		}
	}

	if cfg.LLM.Disable || llmProvider == "" {
		return nil
	}
	c, ok := providers.All[llmProvider]
	if !ok || c.Factory == nil {
		log.WarnContext(ctx, "unknown LLM provider for title generation", "prov", llmProvider)
		return nil
	}
	var opts []genai.ProviderOption
	if cfg.LLM.Model != "" {
		opts = append(opts, genai.ProviderOptionModel(cfg.LLM.Model))
	} else {
		opts = append(opts, genai.ModelCheap)
	}
	opts = appendProviderAPIKey(opts, llmProvider, cfg.Agent.CoreEnv)
	p, err := c.Factory(ctx, opts...)
	if err != nil {
		log.WarnContext(ctx, "LLM provider init failed", "prov", llmProvider, "err", err)
		return nil
	}
	log.InfoContext(ctx, "title provider initialized", "prov", p.Name(), "mdl", p.ModelID())
	return p
}
