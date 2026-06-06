// Constructed dependencies for the HTTP server.

package server

import (
	"context"

	"github.com/maruel/genai"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/ipgeo"
	"github.com/caic-xyz/caic/backend/internal/server/voicertc"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// Dependencies contains already-constructed server dependencies.
type Dependencies struct {
	AbsRoot       string
	LogDir        string
	CacheDir      string
	HarnessEnv    map[string][]string
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
	AgentBackends map[agent.Harness]agent.Backend
	TaskManager   *tasks.Manager
	Provider      genai.Provider
	IPGeoChecker  *ipgeo.Checker

	GeminiAPIKey           string
	GitHubAllowedUsers     map[string]struct{}
	GitLabAllowedUsers     map[string]struct{}
	GitHubWebhookSecret    []byte
	GitLabWebhookSecret    []byte
	GitHubAppAllowedOwners map[string]struct{}
	Pprof                  bool
}

// New creates a new HTTP server from already-assembled dependencies.
func New(ctx context.Context, d Dependencies) (*Server, error) { //nolint:gocritic // Dependencies is a startup value bag.
	s := &Server{
		ctx:                ctx,
		absRoot:            d.AbsRoot,
		logDir:             d.LogDir,
		cacheDir:           d.CacheDir,
		harnessEnv:         d.HarnessEnv,
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
		geminiAPIKey:       d.GeminiAPIKey,
		voiceBridge:        d.VoiceBridge,
		voiceGateway:       d.VoiceGateway,
		forge:              d.Forge,
		ciCache:            d.CICache,
		runtimeBackend:     d.Runtime,
		agentBackends:      d.AgentBackends,
		taskMgr:            d.TaskManager,
		provider:           d.Provider,
		repoReg:            newRepoRegistry(nil),
		ipgeoChecker:       d.IPGeoChecker,
	}
	s.initConcernAdapters()
	// The webhook concern owns the forge webhook secrets and the GitHub App
	// owner allowlist. Its bot and CI service are injected later, once the
	// server wires them up via SetBot/SetCIService.
	s.webhooks = s.newWebhookHandlers(d.GitHubWebhookSecret, d.GitLabWebhookSecret, d.GitHubAppAllowedOwners)
	return s, nil
}

// newWebhookHandlers builds the forge webhook concern from the supplied webhook
// configuration and the server's shared dependencies. The bot and CI service
// are captured as currently set; New injects them after construction.
func (s *Server) newWebhookHandlers(githubSecret, gitlabSecret []byte, appAllowedOwners map[string]struct{}) *WebhookHandlers {
	return &WebhookHandlers{
		serverCtx:        s.ctx,
		githubSecret:     githubSecret,
		gitlabSecret:     gitlabSecret,
		appAllowedOwners: appAllowedOwners,
		bot:              s.Bot,
		ciService:        s.ciService,
		ciCache:          s.ciCache,
		forge:            s.forge,
		taskMgr:          s.taskMgr,
		repoReg:          s.repoReg,
		prefs:            s.prefs,
	}
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
	if s.repoReg == nil {
		s.repoReg = newRepoRegistry(nil)
	}
	if s.botClient == nil {
		s.botClient = newBotClient(botClientDeps{
			repoReg:   s.repoReg,
			taskMgr:   s.taskMgr,
			forge:     s.forge,
			tokenFunc: s.resolveGitHubContainerToken,
		})
	}
	if s.warnings == nil {
		s.warnings = newWarningStore(s.taskMgr)
	}
	if s.ciAdapter == nil {
		var notifyChange func()
		if s.taskMgr != nil {
			notifyChange = s.taskMgr.NotifyTaskChange
		}
		s.ciAdapter = newCIAdapter(ciAdapterDeps{
			repoReg:      s.repoReg,
			taskMgr:      s.taskMgr,
			forge:        s.forge,
			prefs:        s.prefs,
			warnings:     s.warnings,
			taskCreator:  s.botClient,
			infoForRepo:  s.repoInfoFor,
			notifyChange: notifyChange,
		})
	}
}
