// Constructed dependencies for the HTTP router.

package server

import (
	"context"
	"errors"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/autoupdate"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/gomode"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

// Dependencies contains already-constructed server dependencies. The caller
// (internal/app) owns the lifetime of the long-lived automation services
// (Bot, CIService, and their adapters); the router only routes requests to them.
type Dependencies struct {
	Repos          *repos.Service
	Tailscale      bool
	Preferences    *preferences.Store
	AuthStore      *auth.Store
	SessionSecret  []byte
	GitHubOAuth    *auth.ProviderConfig
	GitLabOAuth    *auth.ProviderConfig
	HostState      *auth.HostState
	UsageFetchers  []usage.ProviderFetcher
	VoiceBridge    *voicertc.Bridge
	VoiceGateway   VoiceGatewayConfig
	Forge          *forgemanager.Manager
	CICache        *forgecache.Cache
	ProcessBackend runtime.Backend
	TaskManager    *tasks.Manager
	Provider       genai.Provider
	IPGeoChecker   *ipgeo.Checker

	// App-owned automation services, routed to by HTTP handlers and webhooks.
	Bot        *bot.Bot
	CIService  *ci.Service
	TaskClient bot.Client // creates bot-driven tasks for manual CI repair
	Warnings   *WarningStore
	CacheSizes *CacheSizeStore

	GitHubAllowedUsers     map[string]struct{}
	GitLabAllowedUsers     map[string]struct{}
	GitHubWebhookSecret    []byte
	GitLabWebhookSecret    []byte
	GitHubAppAllowedOwners map[string]struct{}
	Pprof                  bool
}

// New creates a new HTTP router from already-assembled dependencies. It wires
// the injected services into HTTP handler concerns and the webhook endpoint; it
// does not construct or own application services.
func New(ctx context.Context, d Dependencies) (*Router, error) { //nolint:gocritic // Dependencies is a startup value bag.
	if d.ProcessBackend == nil {
		return nil, errors.New("process backend is required")
	}
	voice := &voiceHandlers{bridge: d.VoiceBridge, gateway: d.VoiceGateway}
	voiceMetadata := voice.metadata()
	goModeSettings := newGoModeSettings(voiceMetadata, d.AuthStore != nil)
	goModeHandler, err := gomode.NewHandler(&goModeSettings)
	if err != nil {
		return nil, err
	}
	webFetch := &webFetchHandlers{}
	taskService := &taskAPIService{
		ctx:       ctx,
		taskMgr:   d.TaskManager,
		prefs:     d.Preferences,
		repos:     d.Repos,
		forge:     d.Forge,
		ciService: d.CIService,
		authStore: d.AuthStore,
	}
	s := &Router{
		ctx:              ctx,
		authHandlers:     &authHandlers{store: d.AuthStore, sessionSecret: d.SessionSecret, hostState: d.HostState, githubOAuth: d.GitHubOAuth, gitlabOAuth: d.GitLabOAuth, githubAllowedUsers: d.GitHubAllowedUsers, gitlabAllowedUsers: d.GitLabAllowedUsers},
		ciHandlers:       &ciHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, provider: d.Provider, taskClient: d.TaskClient, authStore: d.AuthStore},
		goModeHandler:    goModeHandler,
		runtimeProcesses: &RuntimeProcesses{taskMgr: d.TaskManager, backend: d.ProcessBackend, notifyChange: notifyChangeFn(d.TaskManager)},
		serverConfigHandlers: &serverConfigHandlers{
			serverCtx:          ctx,
			tailscaleAvailable: d.Tailscale,
			forge:              d.Forge,
			prefs:              d.Preferences,
			repos:              d.Repos,
			taskMgr:            d.TaskManager,
			cacheSizes:         d.CacheSizes,
			authStore:          d.AuthStore,
			githubOAuth:        d.GitHubOAuth,
			gitlabOAuth:        d.GitLabOAuth,
			voiceGateway:       voiceMetadata,
		},
		taskHTTPHandlers: &taskHTTPHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, ciService: d.CIService, authStore: d.AuthStore, warnings: d.Warnings, service: taskService},
		usageHandlers:    &usageHandlers{taskMgr: d.TaskManager, fetchers: d.UsageFetchers},
		voiceHandlers:    voice,
		webFetchHandlers: webFetch,
		authStore:        d.AuthStore,
		sessionSecret:    d.SessionSecret,
		hostState:        d.HostState,
		pprof:            d.Pprof,
		ipgeoChecker:     d.IPGeoChecker,
	}
	mcpRegistry := &caicToolRegistry{serverConfig: s.serverConfigHandlers, tasks: taskService, ci: s.ciHandlers, usage: s.usageHandlers, webFetch: webFetch}
	s.mcpHandlers = &mcp.Handler{
		Registry:   mcpRegistry,
		ServerInfo: mcp.Implementation{Name: "caic", Title: "caic", Version: autoupdate.Version},
	}
	s.runtimeProcesses.authEnabled = s.authEnabled
	// The webhook concern owns the forge webhook secrets and the GitHub App
	// owner allowlist, and dispatches to the app-owned bot and CI service.
	s.webhooks = &WebhookHandlers{
		serverCtx:        ctx,
		githubSecret:     d.GitHubWebhookSecret,
		gitlabSecret:     d.GitLabWebhookSecret,
		appAllowedOwners: d.GitHubAppAllowedOwners,
		bot:              d.Bot,
		ciService:        d.CIService,
		ciCache:          d.CICache,
		forge:            d.Forge,
		taskMgr:          d.TaskManager,
		repos:            d.Repos,
		prefs:            d.Preferences,
	}
	return s, nil
}

// notifyChangeFn returns the task-change notifier for taskMgr, or nil.
func notifyChangeFn(taskMgr *tasks.Manager) func() {
	if taskMgr == nil {
		return nil
	}
	return taskMgr.NotifyTaskChange
}
