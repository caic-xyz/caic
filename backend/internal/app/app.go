// Package app assembles the caic backend application.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/voicegateway"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

const repoDiscoveryDepth = 3

// New creates the caic backend server application.
func New(ctx context.Context, rootDir string, cfg *server.Config) (*server.Server, error) {
	if cfg.Dirs.CacheDir == "" {
		return nil, errors.New("CacheDir is required")
	}
	logDir := filepath.Join(cfg.Dirs.CacheDir, "tasks")
	migrateTaskLogs(cfg.Dirs.CacheDir, logDir)

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
	type logsResult struct {
		logs []*task.LoadedTask
		err  error
	}
	type instancesResult struct {
		instances []runtime.Instance
		err       error
	}

	repoCh := make(chan reposResult, 1)
	logCh := make(chan logsResult, 1)
	instanceCh := make(chan instancesResult, 1)

	go func() {
		defer trace.StartRegion(ctx, "discover-repos").End()
		paths, err := gitutil.DiscoverRepos(rootDir, repoDiscoveryDepth)
		repoCh <- reposResult{paths, err}
	}()
	go func() {
		defer trace.StartRegion(ctx, "load-logs").End()
		logs, err := task.LoadLogs(logDir)
		logCh <- logsResult{logs, err}
	}()
	go func() {
		defer trace.StartRegion(ctx, "list-runtime-instances").End()
		instances, err := runtimeInventory.List(ctx)
		instanceCh <- instancesResult{instances, err}
	}()

	repoRes := <-repoCh
	logRes := <-logCh
	instanceRes := <-instanceCh

	if repoRes.err != nil {
		return nil, fmt.Errorf("discover repos: %w", repoRes.err)
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
	cache, err := forgecache.Open(filepath.Join(cfg.Dirs.CacheDir, "ci_results.json"))
	if err != nil {
		slog.Warn("cannot open CI cache; falling back to in-memory", "err", err)
		cache, _ = forgecache.Open("")
	}

	var voiceBridge *voicertc.Bridge
	if cfg.Voice.Gateway.Mode == server.VoiceGatewayModeEmbedded {
		voiceCfg := cfg.Voice.Gateway.Config
		port := voiceCfg.Server.WebRTCUDPPort
		switch {
		case port < 0:
		case voiceCfg.Backend == voicegateway.BackendGeminiLive && cfg.Agent.GeminiAPIKey == "":
			slog.Info("voice bridge disabled: GEMINI_API_KEY not set")
		default:
			voiceBridge, err = voicertc.NewBridge(ctx, &voiceCfg, cfg.Agent.GeminiAPIKey, port)
			if err != nil {
				return nil, fmt.Errorf("voice bridge: %w", err)
			}
		}
	}

	forgeManager := server.NewForgeManager(cfg.GitHub.Token, cfg.GitLab.Token)
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

	ipgeoChecker, err := ipgeo.NewChecker(ctx, cfg.IPGeo.Allowlist, cfg.IPGeo.DB, "")
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeo.DB != "" {
		slog.Info("ipgeo", "path", cfg.IPGeo.DB, "list", cfg.IPGeo.Allowlist)
	}

	s, err := server.New(ctx, server.Dependencies{
		AbsRoot:                absRoot,
		LogDir:                 logDir,
		CacheDir:               cfg.Dirs.CacheDir,
		HarnessEnv:             cfg.Agent.HarnessEnv,
		Tailscale:              cfg.Runtime.TailscaleAPIKey != "",
		Preferences:            prefsStore,
		AuthStore:              authStore,
		SessionSecret:          sessionSecret,
		GitHubOAuth:            githubOAuth,
		GitLabOAuth:            gitlabOAuth,
		HostState:              hostState,
		UsageFetchers:          detectProviders(ctx, cfg.Agent.CoreEnv, cfg.Agent.HarnessEnv),
		VoiceBridge:            voiceBridge,
		VoiceGateway:           cfg.Voice.Gateway,
		Forge:                  forgeManager,
		CICache:                cache,
		Runtime:                runtimeBackend,
		AgentBackends:          agentBackends,
		TaskManager:            taskMgr,
		Provider:               provider,
		IPGeoChecker:           ipgeoChecker,
		GeminiAPIKey:           cfg.Agent.GeminiAPIKey,
		GitHubAllowedUsers:     parseAllowedUsers(cfg.GitHub.OAuthAllowedUsers),
		GitLabAllowedUsers:     parseAllowedUsers(cfg.GitLab.OAuthAllowedUsers),
		GitHubWebhookSecret:    cfg.GitHub.WebhookSecret,
		GitLabWebhookSecret:    cfg.GitLab.WebhookSecret,
		GitHubAppAllowedOwners: githubAppAllowedOwners,
		Pprof:                  cfg.Debug.Pprof,
	})
	if err != nil {
		return nil, err
	}

	results := make([]server.RepoInitResult, len(repoRes.paths))
	var wg sync.WaitGroup
	for i, abs := range repoRes.paths {
		wg.Go(func() {
			defer trace.StartRegion(ctx, "repo-runner-init").End()
			result, err := s.DiscoverRepoRunner(ctx, abs)
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
		s.RegisterRepoRunner(&results[i])
	}
	s.WarnRepoBasenameCollisions()

	s.SetBot(bot.New(ctx, s.BotClient()))
	s.SetCIService(ci.NewService(cache, provider, s.CIAdapter()))
	s.RegisterNoRepoRunner(ctx)
	taskMgr.Start()

	phase3 := trace.StartRegion(ctx, "load-purged-tasks")
	if logRes.err != nil {
		slog.Warn("load logs failed", "err", logRes.err)
	} else if err := taskMgr.LoadPurgedTasks(logRes.logs); err != nil {
		phase3.End()
		return nil, fmt.Errorf("load purged tasks: %w", err)
	}
	phase3.End()

	phase4 := trace.StartRegion(ctx, "adopt-runtime-instances")
	if instanceRes.err != nil {
		slog.Warn("list failed, skipping adoption", "rt", mdClient.Runtime, "err", instanceRes.err)
	} else {
		adopted, err := taskMgr.AdoptInstances(ctx, s.AdoptionRepos(), instanceRes.instances, logRes.logs)
		if err != nil {
			phase4.End()
			return nil, fmt.Errorf("adopt runtime instances: %w", err)
		}
		for i := range adopted {
			at := &adopted[i]
			if at.ForgeOwner != "" && at.Task.GetPR() > 0 && at.ForgeKind != "" {
				s.WireAdoptedCIMonitoring(ctx, at)
			}
			if at.Task.ForgeIssue == 0 && at.Task.GetPR() == 0 && at.ForgeOwner != "" && at.Branch != "" && at.ForgeKind != "" {
				go s.LookupExternalPRForTask(at) //nolint:contextcheck // Server-lifetime goroutine uses s.ctx internally.
			}
		}
	}
	phase4.End()

	region := trace.StartRegion(ctx, "bot-resume-comments")
	s.Bot.ResumePendingComments()
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
		s.RefreshHarnessModels() //nolint:contextcheck // Server-lifetime goroutine uses s.ctx internally.
	}()
	go s.WatchNewRepos()

	return s, nil
}

func initProvider(ctx context.Context, cfg *server.Config, backend *mdruntime.Backend) genai.Provider {
	llmProvider := cfg.LLM.Provider
	if !cfg.LLM.Disable && llmProvider == "" {
		llmProvider = autoDetectLLMProvider(ctx, cfg.Agent.CoreEnv, cfg.Agent.GeminiAPIKey)
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
	opts = appendProviderAPIKey(opts, llmProvider, cfg.Agent.CoreEnv, cfg.Agent.GeminiAPIKey)
	p, err := c.Factory(ctx, opts...)
	if err != nil {
		slog.Warn("LLM provider init failed", "prov", llmProvider, "err", err)
		return nil
	}
	slog.Info("title", "prov", p.Name(), "mdl", p.ModelID())
	backend.Provider = p
	return p
}
