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
	s := &Server{
		ctx:                ctx,
		repos:              d.Repos,
		cacheSizes:         newCacheSizeStore(),
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
		voiceHandlers:      &voiceHandlers{bridge: d.VoiceBridge, gateway: d.VoiceGateway},
		forge:              d.Forge,
		ciCache:            d.CICache,
		runtimeProcesses:   &RuntimeProcesses{taskMgr: d.TaskManager, backend: d.Runtime},
		taskMgr:            d.TaskManager,
		provider:           d.Provider,
		ipgeoChecker:       d.IPGeoChecker,
	}
	s.initConcernAdapters()
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
	s.initConcernAdapters()
	return s.botClient
}

// CIAdapter returns the CI service backend adapter.
func (s *Server) CIAdapter() ci.Backend {
	s.initConcernAdapters()
	return s.ciAdapter
}

func (s *Server) initConcernAdapters() {
	if s.authHandlers == nil {
		s.authHandlers = &authHandlers{}
	}
	s.authHandlers.store = s.authStore
	s.authHandlers.sessionSecret = s.sessionSecret
	s.authHandlers.hostState = s.hostState
	s.authHandlers.githubOAuth = s.githubOAuth
	s.authHandlers.gitlabOAuth = s.gitlabOAuth
	s.authHandlers.githubAllowedUsers = s.githubAllowedUsers
	s.authHandlers.gitlabAllowedUsers = s.gitlabAllowedUsers
	if s.warnings == nil {
		s.warnings = newWarningStore(s.taskMgr)
	}
	s.runtimeProcesses.taskMgr = s.taskMgr
	s.runtimeProcesses.authEnabled = s.authEnabled
	if s.taskMgr != nil {
		s.runtimeProcesses.notifyChange = s.taskMgr.NotifyTaskChange
	}
	if s.taskHTTPHandlers == nil {
		s.taskHTTPHandlers = &taskHTTPHandlers{}
	}
	s.taskHTTPHandlers.taskMgr = s.taskMgr
	s.taskHTTPHandlers.repos = s.repos
	s.taskHTTPHandlers.forge = s.forge
	s.taskHTTPHandlers.ciService = s.ciService
	s.taskHTTPHandlers.authStore = s.authStore
	s.taskHTTPHandlers.warnings = s.warnings
	if s.taskHTTPHandlers.service == nil {
		s.taskHTTPHandlers.service = &taskAPIService{}
	}
	s.taskHTTPHandlers.service.ctx = s.ctx
	s.taskHTTPHandlers.service.taskMgr = s.taskMgr
	s.taskHTTPHandlers.service.prefs = s.prefs
	s.taskHTTPHandlers.service.repos = s.repos
	s.taskHTTPHandlers.service.forge = s.forge
	s.taskHTTPHandlers.service.ciService = s.ciService
	s.taskHTTPHandlers.service.authStore = s.authStore
	s.taskHTTPHandlers.service.fakeCI = s.fakeCI
	if s.botClient == nil {
		s.botClient = &BotClient{
			repos:     s.repos,
			taskMgr:   s.taskMgr,
			forge:     s.forge,
			tokenFunc: s.taskHTTPHandlers.service.resolveGitHubContainerToken,
		}
	}
	if s.voiceHandlers == nil {
		s.voiceHandlers = &voiceHandlers{}
	}
	if s.webFetchHandlers == nil {
		s.webFetchHandlers = &webFetchHandlers{}
	}
	if s.serverConfigHandlers == nil {
		s.serverConfigHandlers = &serverConfigHandlers{}
	}
	s.serverConfigHandlers.serverCtx = s.ctx
	s.serverConfigHandlers.tailscaleAvailable = s.tailscaleAvailable
	s.serverConfigHandlers.forge = s.forge
	s.serverConfigHandlers.prefs = s.prefs
	s.serverConfigHandlers.repos = s.repos
	s.serverConfigHandlers.taskMgr = s.taskMgr
	s.serverConfigHandlers.cacheSizes = s.cacheSizes
	s.serverConfigHandlers.authStore = s.authStore
	s.serverConfigHandlers.githubOAuth = s.githubOAuth
	s.serverConfigHandlers.gitlabOAuth = s.gitlabOAuth
	s.serverConfigHandlers.voiceGateway = s.voiceHandlers.metadata()
	s.ciHandlers = &ciHandlers{
		taskMgr:    s.taskMgr,
		repos:      s.repos,
		forge:      s.forge,
		provider:   s.provider,
		taskClient: s.botClient,
		authStore:  s.authStore,
	}
	if s.usageHandlers == nil {
		s.usageHandlers = &usageHandlers{
			taskMgr:  s.taskMgr,
			fetchers: s.usageFetchers,
		}
	}
	if s.ciAdapter == nil {
		var notifyChange func()
		if s.taskMgr != nil {
			notifyChange = s.taskMgr.NotifyTaskChange
		}
		s.ciAdapter = newCIAdapter(ciAdapterDeps{
			repos:        s.repos,
			taskMgr:      s.taskMgr,
			forge:        s.forge,
			prefs:        s.prefs,
			warnings:     s.warnings,
			taskCreator:  s.botClient,
			notifyChange: notifyChange,
		})
	}
	s.taskHTTPHandlers.service.ciAdapter = s.ciAdapter
}
