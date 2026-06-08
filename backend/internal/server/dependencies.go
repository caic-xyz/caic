// Constructed dependencies for the HTTP server.

package server

import (
	"context"
	"errors"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/voicegateway/voicertc"
)

// Dependencies contains already-constructed server dependencies.
type Dependencies struct {
	Repos         *repos.Service
	Tailscale     bool
	Preferences   *preferences.Store
	AuthStore     *auth.Store
	SessionSecret []byte
	GitHubOAuth   *auth.ProviderConfig
	GitLabOAuth   *auth.ProviderConfig
	HostState     *auth.HostState
	UsageFetchers []usage.ProviderFetcher
	VoiceBridge   *voicertc.Bridge
	VoiceGateway  VoiceGatewayConfig
	Forge         *ForgeManager
	CICache       *forgecache.Cache
	Runtime       runtime.Backend
	TaskManager   *tasks.Manager
	Provider      genai.Provider
	IPGeoChecker  *ipgeo.Checker

	GitHubAllowedUsers     map[string]struct{}
	GitLabAllowedUsers     map[string]struct{}
	GitHubWebhookSecret    []byte
	GitLabWebhookSecret    []byte
	GitHubAppAllowedOwners map[string]struct{}
	Pprof                  bool
}

// New creates a new HTTP server from already-assembled dependencies.
func New(ctx context.Context, d Dependencies) (*Server, error) { //nolint:gocritic // Dependencies is a startup value bag.
	if d.Runtime == nil {
		return nil, errors.New("runtime backend is required")
	}
	voice := &voiceHandlers{bridge: d.VoiceBridge, gateway: d.VoiceGateway}
	cacheSizes := newCacheSizeStore()
	warnings := newWarningStore(d.TaskManager)
	taskService := &taskAPIService{
		ctx:       ctx,
		taskMgr:   d.TaskManager,
		prefs:     d.Preferences,
		repos:     d.Repos,
		forge:     d.Forge,
		authStore: d.AuthStore,
	}
	botClient := &BotClient{
		repos:     d.Repos,
		taskMgr:   d.TaskManager,
		forge:     d.Forge,
		tokenFunc: taskService.resolveGitHubContainerToken,
	}
	var notifyChange func()
	if d.TaskManager != nil {
		notifyChange = d.TaskManager.NotifyTaskChange
	}
	ciAdapter := newCIAdapter(ciAdapterDeps{
		repos:        d.Repos,
		taskMgr:      d.TaskManager,
		forge:        d.Forge,
		prefs:        d.Preferences,
		warnings:     warnings,
		taskCreator:  botClient,
		notifyChange: notifyChange,
	})
	taskService.ciAdapter = ciAdapter
	s := &Server{
		ctx:              ctx,
		repos:            d.Repos,
		taskMgr:          d.TaskManager,
		cacheSizes:       cacheSizes,
		ciCache:          d.CICache,
		provider:         d.Provider,
		botClient:        botClient,
		ciAdapter:        ciAdapter,
		authHandlers:     &authHandlers{store: d.AuthStore, sessionSecret: d.SessionSecret, hostState: d.HostState, githubOAuth: d.GitHubOAuth, gitlabOAuth: d.GitLabOAuth, githubAllowedUsers: d.GitHubAllowedUsers, gitlabAllowedUsers: d.GitLabAllowedUsers},
		ciHandlers:       &ciHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, provider: d.Provider, taskClient: botClient, authStore: d.AuthStore},
		runtimeProcesses: &RuntimeProcesses{taskMgr: d.TaskManager, backend: d.Runtime, notifyChange: notifyChange},
		serverConfigHandlers: &serverConfigHandlers{
			serverCtx:          ctx,
			tailscaleAvailable: d.Tailscale,
			forge:              d.Forge,
			prefs:              d.Preferences,
			repos:              d.Repos,
			taskMgr:            d.TaskManager,
			cacheSizes:         cacheSizes,
			authStore:          d.AuthStore,
			githubOAuth:        d.GitHubOAuth,
			gitlabOAuth:        d.GitLabOAuth,
			voiceGateway:       voice.metadata(),
		},
		taskHTTPHandlers:   &taskHTTPHandlers{taskMgr: d.TaskManager, repos: d.Repos, forge: d.Forge, authStore: d.AuthStore, warnings: warnings, service: taskService},
		usageHandlers:      &usageHandlers{taskMgr: d.TaskManager, fetchers: d.UsageFetchers},
		voiceHandlers:      voice,
		webFetchHandlers:   &webFetchHandlers{},
		tailscaleAvailable: d.Tailscale,
		prefs:              d.Preferences,
		authStore:          d.AuthStore,
		sessionSecret:      d.SessionSecret,
		githubOAuth:        d.GitHubOAuth,
		gitlabOAuth:        d.GitLabOAuth,
		githubAllowedUsers: d.GitHubAllowedUsers,
		gitlabAllowedUsers: d.GitLabAllowedUsers,
		hostState:          d.HostState,
		usageFetchers:      d.UsageFetchers,
		pprof:              d.Pprof,
		forge:              d.Forge,
		ipgeoChecker:       d.IPGeoChecker,
		warnings:           warnings,
	}
	s.runtimeProcesses.authEnabled = s.authEnabled
	// The webhook concern owns the forge webhook secrets and the GitHub App
	// owner allowlist. Its bot and CI service are injected later, once the
	// server wires them up via SetBot/SetCIService.
	s.webhooks = &WebhookHandlers{
		serverCtx:        s.ctx,
		githubSecret:     d.GitHubWebhookSecret,
		gitlabSecret:     d.GitLabWebhookSecret,
		appAllowedOwners: d.GitHubAppAllowedOwners,
		bot:              s.Bot,
		ciService:        s.ciService,
		ciCache:          s.ciCache,
		forge:            s.forge,
		taskMgr:          s.taskMgr,
		repos:            s.repos,
		prefs:            s.prefs,
	}
	return s, nil
}

// SetBot sets the forge automation bot owned by the server.
// Must be called after New, which establishes the webhook concern.
func (s *Server) SetBot(b *bot.Bot) {
	s.Bot = b
	s.webhooks.bot = b
}

// SetCIService sets the CI service used by server handlers and adoption hooks.
// Must be called after New, which establishes the webhook concern.
func (s *Server) SetCIService(c *ci.Service) {
	s.ciService = c
	s.webhooks.ciService = c
	if s.taskHTTPHandlers != nil {
		s.taskHTTPHandlers.ciService = c
		if s.taskHTTPHandlers.service != nil {
			s.taskHTTPHandlers.service.ciService = c
		}
	}
}

// BotClient returns the bot-facing task client.
func (s *Server) BotClient() bot.Client {
	return s.botClient
}

// CIAdapter returns the CI service backend adapter.
func (s *Server) CIAdapter() ci.Backend {
	return s.ciAdapter
}
