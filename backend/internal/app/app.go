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
	"path/filepath"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md/git"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"golang.org/x/sync/errgroup"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/repo/repomgr"
	"github.com/caic-xyz/caic/backend/internal/repo/repowork"
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
}

type backgroundTask func(context.Context) error

// Serve starts the HTTP server and closes app-owned resources when serving ends.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	runCtx, cancel := context.WithCancel(ctx)
	group, bgCtx := errgroup.WithContext(runCtx)
	for _, task := range a.backgroundTasks {
		group.Go(func() error { return task(bgCtx) })
	}
	serverErr := a.Server.Serve(runCtx, ln)
	cancel()
	if a.voiceBridge != nil {
		a.voiceBridge.CloseAll()
	}
	bgErrCh := make(chan error, 1)
	go func() { bgErrCh <- group.Wait() }()
	select {
	case bgErr := <-bgErrCh:
		if serverErr != nil {
			return serverErr
		}
		return bgErr
	case <-time.After(10 * time.Second):
		slog.WarnContext(context.WithoutCancel(ctx), "background shutdown timed out")
		return serverErr
	}
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
	backend := mdruntime.NewBackend(mdClient)
	backend.HarnessEnv = cfg.Agent.HarnessEnv
	runtimeInfo := mdruntime.NewRuntimeInfoBackend(mdClient, backend)
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
		paths, err := git.DiscoverCheckouts(rootDir, repoDiscoveryDepth)
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

	slog.InfoContext(ctx, "github", "pat", auth.MaskedToken(cfg.GitHub.Token), "oauth", auth.MaskedToken(cfg.GitHub.OAuthClientID))
	slog.InfoContext(ctx, "gitlab", "pat", auth.MaskedToken(cfg.GitLab.Token), "oauth", auth.MaskedToken(cfg.GitLab.OAuthClientID))
	slog.InfoContext(ctx, "google", "oauth", auth.MaskedToken(cfg.Google.OAuthClientID))

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
		slog.WarnContext(ctx, "cannot open CI cache; falling back to in-memory", "err", err)
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
			slog.InfoContext(ctx, "voice bridge disabled: GEMINI_API_KEY not set")
		default:
			voiceBridge, err = voicertc.NewBridge(ctx, &voiceCfg, key, port)
			if err != nil {
				return nil, fmt.Errorf("voice bridge: %w", err)
			}
		}
	}

	forgeManager := forgemgr.New(cfg.GitHub.Token, cfg.GitLab.Token, nil)
	if cfg.GitHub.AppID != 0 && len(cfg.GitHub.AppPrivateKeyPEM) > 0 {
		app, err := github.NewAppClient(cfg.GitHub.AppID, cfg.GitHub.AppPrivateKeyPEM, forgeManager.GitHubAppThrottle())
		if err != nil {
			return nil, fmt.Errorf("github app: %w", err)
		}
		forgeManager.SetGitHubApp(app)
	}

	provider := initProvider(ctx, cfg, backend)

	workspaceRegistry := repowork.NewRegistry(ctx, runtimeBackend)
	taskMgr := taskmgr.New(taskmgr.Config{
		ServerCtx:         ctx,
		LogDir:            logDir,
		CacheDir:          cfg.Dirs.CacheDir,
		Backend:           runtimeBackend,
		WorkspaceRegistry: workspaceRegistry,
		Monitor:           runtimeMonitor,
		Inventory:         runtimeInventory,
		Privilege:         runtimePrivilege,
		Backends:          agentBackends,
		EventReplayFactory: func(path string, h harness.Name) task.EventReplayWriter {
			return eventreplay.NewMessageWriter(path, h)
		},
		HarnessEnv: cfg.Agent.HarnessEnv,
		Prefs:      prefsStore,
		Provider:   provider,
	})
	repoService := repomgr.NewService(ctx, absRoot, repo.New(nil), workspaceRegistry)
	repoStatus := ci.NewRepoStatusStore()

	// Long-lived forge automation, owned by app and routed to by the HTTP layer.
	warnings := server.NewWarningStore(taskMgr)
	cacheSizes := server.NewCacheSizeStore()
	botClient := &botClient{repoSvc: repoService, taskMgr: taskMgr, forgeMgr: forgeManager}
	ciAdapter := &ciAdapter{
		repoMgr:     repoService,
		repoStatus:  repoStatus,
		taskMgr:     taskMgr,
		forgeMgr:    forgeManager,
		prefs:       prefsStore,
		warnings:    warnings,
		taskCreator: botClient,
	}
	ciService := ci.NewService(cache, provider, ciAdapter)
	botService := bot.New(ctx, botClient)

	ipgeoChecker, err := ipgeo.NewChecker(ctx, cfg.IPGeo.Allowlist, cfg.IPGeo.DB, cfg.Dirs.CacheDir)
	if err != nil {
		return nil, fmt.Errorf("ipgeo: %w", err)
	}
	if cfg.IPGeo.DB != "" {
		slog.InfoContext(ctx, "ipgeo", "path", cfg.IPGeo.DB, "list", cfg.IPGeo.Allowlist)
	}

	s, err := server.New(ctx, server.Dependencies{
		RepoSvc:                    repoService,
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
		UsageFetchers:              usageFetchers(cfg, ctx),
		VoiceBridge:                voiceBridge,
		VoiceGateway:               cfg.Voice.Gateway,
		ForgeMgr:                   forgeManager,
		CICache:                    cache,
		ProcessBackend:             runtimeBackend,
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

	results := make([]repomgr.InitResult, len(repoRes.paths))
	var wg sync.WaitGroup
	for i, abs := range repoRes.paths {
		wg.Go(func() {
			defer trace.StartRegion(ctx, "repo-workspace-init").End()
			result, err := repoService.DiscoverWorkspace(ctx, abs)
			if err != nil {
				slog.WarnContext(ctx, "skipping repo", "path", abs, "err", err)
				return
			}
			results[i] = result
			slog.DebugContext(ctx, "discovered repo", "path", result.Info.RelPath, "br", result.Info.BaseBranch)
		})
	}
	wg.Wait()
	for i := range results {
		repoService.RegisterWorkspace(&results[i], moveRepoStatus(repoStatus))
	}

	taskMgr.Start()

	phase3 := trace.StartRegion(ctx, "load-live-task-logs")
	liveLogs, err := loadRuntimeTaskLogs(ctx, logDir, runtimeInventory, instanceRes.instances)
	if err != nil {
		slog.WarnContext(ctx, "load live task logs failed", "err", err)
	}
	phase3.End()

	phase4 := trace.StartRegion(ctx, "adopt-runtime-instances")
	adopted, err := taskMgr.AdoptInstances(ctx, adoptionRepos(repoService.Snapshot()), instanceRes.instances, liveLogs)
	if err != nil {
		phase4.End()
		return nil, fmt.Errorf("adopt runtime instances: %w", err)
	}
	backgroundTasks := []backgroundTask{}
	adoption := &adoptedTaskWiring{
		authStore: authStore,
		ciService: ciService,
		forgeMgr:  forgeManager,
		taskMgr:   taskMgr,
		repoSvc:   repoService,
	}
	for i := range adopted {
		at := &adopted[i]
		if at.ForgeOwner != "" && at.Task.GetPR() > 0 && at.ForgeKind != "" {
			backgroundTasks = append(backgroundTasks, func(ctx context.Context) error {
				adoption.WireCIMonitoring(ctx, at)
				return nil
			})
		}
		if at.Task.ForgeIssue == 0 && at.Task.GetPR() == 0 && at.ForgeOwner != "" && at.Branch != "" && at.ForgeKind != "" {
			backgroundTasks = append(backgroundTasks, func(ctx context.Context) error {
				adoption.LookupExternalPRForTask(ctx, at)
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
			return warmupImages(ctx, mdClient, prefsStore)
		})
	}
	backgroundTasks = append(backgroundTasks,
		func(ctx context.Context) error {
			_, tk := trace.NewTask(ctx, "refresh-harness-models")
			defer tk.End()
			trace.Log(ctx, "startup", "refresh-harness-models: begin")
			refreshHarnessModels(ctx, cfg.Dirs.CacheDir, runtimeBackend, runtimeInventory, taskMgr, cfg.Agent.HarnessEnv)
			return nil
		},
		func(ctx context.Context) error {
			_, tk := trace.NewTask(ctx, "refresh-cache-sizes")
			defer tk.End()
			trace.Log(ctx, "startup", "refresh-cache-sizes: begin")
			cacheSizes.RefreshLoop(ctx)
			return nil
		},
		func(ctx context.Context) error {
			newRepoWatcher(ctx, absRoot, repoService, repoStatus).Watch()
			return nil
		},
		func(ctx context.Context) error {
			startupCtx, tk := trace.NewTask(ctx, "load-purged-tasks")
			defer tk.End()
			trace.Log(startupCtx, "startup", "load-purged-tasks: begin")
			start := time.Now()
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				return fmt.Errorf("load logs: %w", err)
			}
			slog.InfoContext(startupCtx, "loaded task log headers", "n", len(logs), "dur", time.Since(start))
			start = time.Now()
			if err := taskMgr.LoadPurgedTasks(logs); err != nil {
				slog.ErrorContext(startupCtx, "load purged tasks failed", "err", err)
			} else {
				slog.InfoContext(startupCtx, "loaded purged task entries", "dur", time.Since(start))
			}
			start = time.Now()
			if err := task.CompressTerminalLogs(logs); err != nil {
				slog.WarnContext(startupCtx, "compress terminal task logs failed", "err", err)
			} else {
				slog.InfoContext(startupCtx, "compressed terminal task logs", "dur", time.Since(start))
			}
			if removed, err := eventreplay.PruneStaleCaches(logDir); err != nil {
				slog.WarnContext(startupCtx, "prune stale replay caches failed", "err", err)
			} else if removed > 0 {
				slog.InfoContext(startupCtx, "pruned stale replay caches", "n", removed)
			}
			return nil
		},
	)

	return &App{Server: s, voiceBridge: voiceBridge, backgroundTasks: backgroundTasks}, nil
}

func adoptionRepos(in []repo.Info) []taskmgr.AdoptRepo {
	out := make([]taskmgr.AdoptRepo, len(in))
	for i := range in {
		r := &in[i]
		out[i] = taskmgr.AdoptRepo{
			RelPath:    r.RelPath,
			AbsPath:    r.AbsPath,
			ForgeKind:  string(r.ForgeKind),
			ForgeOwner: r.ForgeOwner,
			ForgeRepo:  r.ForgeRepo,
		}
	}
	return out
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
			slog.InfoContext(ctx, "auto-detected LLM provider", "prov", llmProvider)
		}
	}

	if cfg.LLM.Disable || llmProvider == "" {
		return nil
	}
	c, ok := providers.All[llmProvider]
	if !ok || c.Factory == nil {
		slog.WarnContext(ctx, "unknown LLM provider for title generation", "prov", llmProvider)
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
		slog.WarnContext(ctx, "LLM provider init failed", "prov", llmProvider, "err", err)
		return nil
	}
	slog.InfoContext(ctx, "title", "prov", p.Name(), "mdl", p.ModelID())
	backend.Provider = p
	return p
}
