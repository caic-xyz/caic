// Package app assembles the caic backend application.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"runtime/trace"
	"strings"
	"sync"

	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"

	"github.com/caic-xyz/caic/backend/internal/agent/registry"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

const repoDiscoveryDepth = 3

// App owns the caic backend application lifetime.
type App struct {
	Server *server.Router

	voiceBridge        *voicertc.Bridge
	backgroundStarters []func()
}

// Serve starts the HTTP server and closes app-owned resources when serving ends.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	for _, start := range a.backgroundStarters {
		go start()
	}
	err := a.Server.Serve(ctx, ln)
	if a.voiceBridge != nil {
		a.voiceBridge.CloseAll()
	}
	return err
}

// SetUsageFetchers replaces provider usage fetchers for smoke and e2e tests.
func (a *App) SetUsageFetchers(fetchers []usage.ProviderFetcher) {
	a.Server.SetUsageFetchers(fetchers)
}

// SetFakeCI injects a fake CI simulation hook for smoke and e2e tests.
func (a *App) SetFakeCI(f func(context.Context, *task.Task)) {
	a.Server.SetFakeCI(f)
}

// New creates the caic backend server application.
func New(ctx context.Context, rootDir string, cfg *server.Config) (*App, error) {
	if cfg.Dirs.CacheDir == "" {
		return nil, errors.New("CacheDir is required")
	}
	logDir := filepath.Join(cfg.Dirs.CacheDir, "tasks")

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	ctx, startTask := trace.NewTask(ctx, "server.startup")
	defer startTask.End()

	mdClient, err := mdruntime.New(cfg.Runtime.TailscaleAPIKey, cfg.GitHub.Token, cfg.Runtime.Name)
	if err != nil {
		return nil, fmt.Errorf("init md runtime adapter: %w", err)
	}
	mdClient.DigestCacheTTL = warmupInterval
	runtimeInfo := mdruntime.NewRuntimeInfoBackend(mdClient)
	backend := mdruntime.NewBackend(mdClient)
	backend.HarnessEnv = cfg.Agent.HarnessEnv
	runtimeBackend := runtime.Backend(backend)
	if cfg.Runtime.Backend != nil {
		runtimeBackend = cfg.Runtime.Backend
	}
	runtimeMonitor := runtime.Monitor(runtimeInfo)
	if cfg.Runtime.Monitor != nil {
		runtimeMonitor = cfg.Runtime.Monitor
	}
	runtimeInventory := runtime.Inventory(runtimeInfo)
	if cfg.Runtime.Inventory != nil {
		runtimeInventory = cfg.Runtime.Inventory
	}
	runtimePrivilege := runtime.PrivilegeInfo(runtimeInfo)
	if cfg.Runtime.Privilege != nil {
		runtimePrivilege = cfg.Runtime.Privilege
	}

	type reposResult struct {
		paths []string
		err   error
	}
	type instancesResult struct {
		instances []runtime.Instance
		err       error
	}

	repoCh := make(chan reposResult, 1)
	instanceCh := make(chan instancesResult, 1)

	go func() {
		defer trace.StartRegion(ctx, "discover-repos").End()
		paths, err := gitutil.DiscoverRepos(rootDir, repoDiscoveryDepth)
		repoCh <- reposResult{paths, err}
	}()
	go func() {
		defer trace.StartRegion(ctx, "list-runtime-instances").End()
		instances, err := runtimeInventory.List(ctx)
		instanceCh <- instancesResult{instances, err}
	}()

	repoRes := <-repoCh
	instanceRes := <-instanceCh

	if repoRes.err != nil {
		return nil, fmt.Errorf("discover repos: %w", repoRes.err)
	}
	if instanceRes.err != nil {
		return nil, fmt.Errorf("list runtime instances: %w", instanceRes.err)
	}

	settings, err := loadSettings(filepath.Join(cfg.Dirs.ConfigDir, "settings.json"))
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

	slog.Info("github", "pat", auth.MaskedToken(cfg.GitHub.Token), "oauth", auth.MaskedToken(cfg.GitHub.OAuthClientID))
	slog.Info("gitlab", "pat", auth.MaskedToken(cfg.GitLab.Token), "oauth", auth.MaskedToken(cfg.GitLab.OAuthClientID))

	var authStore *auth.Store
	var sessionSecret []byte
	var githubOAuth *auth.ProviderConfig
	var gitlabOAuth *auth.ProviderConfig
	oauthConfigured := cfg.GitHub.OAuthClientID != "" || cfg.GitLab.OAuthClientID != ""
	if cfg.Auth.ExternalURL != "" && (oauthConfigured || !isAuto) {
		secret, err := hexDecode(settings.SessionSecret)
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
			c := auth.GitHubConfig(cfg.GitHub.OAuthClientID, cfg.GitHub.OAuthClientSecret, hostState)
			githubOAuth = &c
		}
		if cfg.GitLab.OAuthClientID != "" && cfg.GitLab.OAuthClientSecret != "" {
			c := auth.GitLabConfig(cfg.GitLab.OAuthClientID, cfg.GitLab.OAuthClientSecret, cfg.GitLab.URL, hostState)
			gitlabOAuth = &c
		}
	}

	prefsStore, err := preferences.Open(filepath.Join(cfg.Dirs.ConfigDir, "preferences.json"))
	if err != nil {
		return nil, fmt.Errorf("open preferences: %w", err)
	}

	agentBackends := registry.DefaultBackends(cfg.Dirs.CacheDir, cfg.Agent.HarnessEnv)
	if cfg.Agent.Backends != nil {
		agentBackends = cfg.Agent.Backends
	}
	cache, err := forgecache.Open(filepath.Join(cfg.Dirs.CacheDir, "ci_results.json"))
	if err != nil {
		slog.Warn("cannot open CI cache; falling back to in-memory", "err", err)
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
			slog.Info("voice bridge disabled: GEMINI_API_KEY not set")
		default:
			voiceBridge, err = voicertc.NewBridge(ctx, &voiceCfg, key, port)
			if err != nil {
				return nil, fmt.Errorf("voice bridge: %w", err)
			}
		}
	}

	forgeManager := forgemanager.New(cfg.GitHub.Token, cfg.GitLab.Token, nil)
	githubAppAllowedOwners := map[string]struct{}(nil)
	if cfg.GitHub.AppID != 0 && len(cfg.GitHub.AppPrivateKeyPEM) > 0 {
		app, err := github.NewAppClient(cfg.GitHub.AppID, cfg.GitHub.AppPrivateKeyPEM, forgeManager.GitHubAppThrottle())
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		forgeManager.SetGitHubApp(app)
		if cfg.GitHub.AppAllowedOwners != "" {
			githubAppAllowedOwners = parseAllowedUsers(cfg.GitHub.AppAllowedOwners)
		}
	}

	provider := initProvider(ctx, cfg, backend)

	taskMgr := tasks.New(tasks.Config{
		ServerCtx:  ctx,
		LogDir:     logDir,
		CacheDir:   cfg.Dirs.CacheDir,
		Backend:    runtimeBackend,
		Monitor:    runtimeMonitor,
		Inventory:  runtimeInventory,
		Privilege:  runtimePrivilege,
		Backends:   agentBackends,
		HarnessEnv: cfg.Agent.HarnessEnv,
		Prefs:      prefsStore,
		Provider:   provider,
	})
	repoService := repos.NewService(
		absRoot,
		logDir,
		cfg.Dirs.CacheDir,
		cfg.Agent.HarnessEnv,
		repos.NewRegistry(nil),
		taskMgr,
		runtimeBackend,
		agentBackends,
	)

	// Long-lived forge automation, owned by app and routed to by the HTTP layer.
	warnings := server.NewWarningStore(taskMgr)
	cacheSizes := server.NewCacheSizeStore()
	botClient := &botClient{repos: repoService, taskMgr: taskMgr, forge: forgeManager}
	ciAdapter := &ciAdapter{
		repos:        repoService,
		taskMgr:      taskMgr,
		forge:        forgeManager,
		prefs:        prefsStore,
		warnings:     warnings,
		taskCreator:  botClient,
		notifyChange: taskMgr.NotifyTaskChange,
	}
	ciService := ci.NewService(cache, provider, ciAdapter)
	botService := bot.New(ctx, botClient)

	ipgeoChecker, err := ipgeo.NewChecker(ctx, cfg.IPGeo.Allowlist, cfg.IPGeo.DB, cfg.Dirs.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeo.DB != "" {
		slog.Info("ipgeo", "path", cfg.IPGeo.DB, "list", cfg.IPGeo.Allowlist)
	}

	s, err := server.New(ctx, server.Dependencies{
		Repos:                         repoService,
		Tailscale:                     cfg.Runtime.TailscaleAPIKey != "",
		Preferences:                   prefsStore,
		AuthStore:                     authStore,
		SessionSecret:                 sessionSecret,
		MCPOAuthPrivateKeyPEM:         []byte(settings.MCPOAuthPrivateKeyPEM),
		MCPOAuthKeyID:                 settings.MCPOAuthKeyID,
		MCPOAuthRefreshTokenStorePath: filepath.Join(cfg.Dirs.ConfigDir, "mcp_oauth_refresh_tokens.json"),
		MCPAuditLogPath:               filepath.Join(cfg.Dirs.CacheDir, "mcp_audit.jsonl"),
		GitHubOAuth:                   githubOAuth,
		GitLabOAuth:                   gitlabOAuth,
		HostState:                     hostState,
		UsageFetchers:                 detectProviders(ctx, cfg.Agent.CoreEnv, cfg.Agent.HarnessEnv),
		VoiceBridge:                   voiceBridge,
		VoiceGateway:                  cfg.Voice.Gateway,
		Forge:                         forgeManager,
		CICache:                       cache,
		ProcessBackend:                runtimeBackend,
		TaskManager:                   taskMgr,
		Provider:                      provider,
		IPGeoChecker:                  ipgeoChecker,
		Bot:                           botService,
		CIService:                     ciService,
		TaskClient:                    botClient,
		Warnings:                      warnings,
		CacheSizes:                    cacheSizes,
		GitHubAllowedUsers:            parseAllowedUsers(cfg.GitHub.OAuthAllowedUsers),
		GitLabAllowedUsers:            parseAllowedUsers(cfg.GitLab.OAuthAllowedUsers),
		GitHubWebhookSecret:           cfg.GitHub.WebhookSecret,
		GitLabWebhookSecret:           cfg.GitLab.WebhookSecret,
		GitHubAppAllowedOwners:        githubAppAllowedOwners,
		Pprof:                         cfg.Debug.Pprof,
	})
	if err != nil {
		return nil, err
	}

	results := make([]repos.InitResult, len(repoRes.paths))
	var wg sync.WaitGroup
	for i, abs := range repoRes.paths {
		wg.Go(func() {
			defer trace.StartRegion(ctx, "repo-runner-init").End()
			result, err := repoService.DiscoverRunner(ctx, abs)
			if err != nil {
				slog.Warn("skipping repo", "path", abs, "err", err)
				return
			}
			if result.InitErr != nil {
				slog.Warn("runner init failed", "path", abs, "err", result.InitErr)
			}
			results[i] = result
			slog.Debug("discovered repo", "path", result.Info.RelPath, "br", result.Info.BaseBranch)
		})
	}
	wg.Wait()
	for i := range results {
		repoService.RegisterRunner(&results[i])
	}

	repoService.RegisterNoRepoRunner(ctx)
	taskMgr.Start()

	phase3 := trace.StartRegion(ctx, "load-live-task-logs")
	liveLogs, err := loadRuntimeTaskLogs(ctx, logDir, runtimeInventory, instanceRes.instances)
	if err != nil {
		slog.WarnContext(ctx, "load live task logs failed", "err", err)
	}
	phase3.End()

	phase4 := trace.StartRegion(ctx, "adopt-runtime-instances")
	adopted, err := taskMgr.AdoptInstances(ctx, repoService.AdoptionRepos(), instanceRes.instances, liveLogs)
	if err != nil {
		phase4.End()
		return nil, fmt.Errorf("adopt runtime instances: %w", err)
	}
	adoption := &adoptedTaskWiring{
		ctx:       ctx,
		authStore: authStore,
		ciService: ciService,
		forge:     forgeManager,
		taskMgr:   taskMgr,
		repos:     repoService,
	}
	for i := range adopted {
		at := &adopted[i]
		if at.ForgeOwner != "" && at.Task.GetPR() > 0 && at.ForgeKind != "" {
			adoption.WireCIMonitoring(ctx, at)
		}
		if at.Task.ForgeIssue == 0 && at.Task.GetPR() == 0 && at.ForgeOwner != "" && at.Branch != "" && at.ForgeKind != "" {
			go adoption.LookupExternalPRForTask(at) //nolint:contextcheck // App-lifetime goroutine uses adoption ctx.
		}
	}
	phase4.End()

	region := trace.StartRegion(ctx, "bot-resume-comments")
	botService.ResumePendingComments()
	region.End()

	if !cfg.Runtime.SkipWarmup {
		go func() {
			_, tk := trace.NewTask(ctx, "warmup-images")
			defer tk.End()
			trace.Log(ctx, "startup", "warmup-images: begin")
			warmupImages(ctx, mdClient, prefsStore)
		}()
	}
	go func() {
		_, tk := trace.NewTask(ctx, "refresh-harness-models")
		defer tk.End()
		trace.Log(ctx, "startup", "refresh-harness-models: begin")
		refreshHarnessModels(ctx, cfg.Dirs.CacheDir, runtimeBackend, taskMgr, cfg.Agent.HarnessEnv)
	}()
	go func() {
		_, tk := trace.NewTask(ctx, "refresh-cache-sizes")
		defer tk.End()
		trace.Log(ctx, "startup", "refresh-cache-sizes: begin")
		cacheSizes.RefreshLoop(ctx)
	}()
	go newRepoWatcher(ctx, absRoot, repoService).Watch()

	backgroundStarters := []func(){
		func() {
			startupCtx, tk := trace.NewTask(ctx, "load-purged-tasks")
			defer tk.End()
			trace.Log(startupCtx, "startup", "load-purged-tasks: begin")
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				slog.WarnContext(startupCtx, "load logs failed", "err", err)
				return
			}
			if err := task.CompressTerminalLogs(logs); err != nil {
				slog.WarnContext(startupCtx, "compress terminal task logs failed", "err", err)
			}
			if removed, err := eventreplay.PruneStaleCaches(logDir); err != nil {
				slog.WarnContext(startupCtx, "prune stale replay caches failed", "err", err)
			} else if removed > 0 {
				slog.InfoContext(startupCtx, "pruned stale replay caches", "n", removed)
			}
			if err := taskMgr.LoadPurgedTasks(logs); err != nil {
				slog.ErrorContext(startupCtx, "load purged tasks failed", "err", err)
			}
		},
	}

	return &App{Server: s, voiceBridge: voiceBridge, backgroundStarters: backgroundStarters}, nil
}

func loadRuntimeTaskLogs(ctx context.Context, logDir string, inventory runtime.Inventory, instances []runtime.Instance) ([]*task.LoadedTask, error) {
	seen := make(map[string]struct{}, len(instances))
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
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	logs, err := task.LoadLogsForTaskIDs(logDir, ids)
	if err != nil {
		errs = append(errs, err)
	}
	return logs, errors.Join(errs...)
}

func runtimeTaskID(ctx context.Context, inventory runtime.Inventory, id runtime.InstanceID) (string, error) {
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

func initProvider(ctx context.Context, cfg *server.Config, backend *mdruntime.Backend) genai.Provider {
	llmProvider := cfg.LLM.Provider
	if !cfg.LLM.Disable && llmProvider == "" {
		llmProvider = autoDetectLLMProvider(ctx, cfg.Agent.CoreEnv)
		if llmProvider != "" {
			slog.Info("auto-detected LLM provider", "prov", llmProvider)
		}
	}

	if cfg.LLM.Disable || llmProvider == "" {
		return nil
	}
	c, ok := providers.All[llmProvider]
	if !ok || c.Factory == nil {
		slog.Warn("unknown LLM provider for title generation", "prov", llmProvider)
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
		slog.Warn("LLM provider init failed", "prov", llmProvider, "err", err)
		return nil
	}
	slog.Info("title", "prov", p.Name(), "mdl", p.ModelID())
	backend.Provider = p
	return p
}
