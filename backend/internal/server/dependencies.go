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
		ctx:                    ctx,
		absRoot:                d.AbsRoot,
		logDir:                 d.LogDir,
		cacheDir:               d.CacheDir,
		harnessEnv:             d.HarnessEnv,
		tailscaleAvailable:     d.Tailscale,
		prefs:                  d.Preferences,
		authStore:              d.AuthStore,
		sessionSecret:          d.SessionSecret,
		githubOAuth:            d.GitHubOAuth,
		gitlabOAuth:            d.GitLabOAuth,
		githubAllowedUsers:     d.GitHubAllowedUsers,
		gitlabAllowedUsers:     d.GitLabAllowedUsers,
		githubWebhookSecret:    d.GitHubWebhookSecret,
		gitlabWebhookSecret:    d.GitLabWebhookSecret,
		githubAppAllowedOwners: d.GitHubAppAllowedOwners,
		hostState:              d.HostState,
		usageFetchers:          d.UsageFetchers,
		pprof:                  d.Pprof,
		geminiAPIKey:           d.GeminiAPIKey,
		voiceBridge:            d.VoiceBridge,
		forge:                  d.Forge,
		ciCache:                d.CICache,
		runtimeBackend:         d.Runtime,
		agentBackends:          d.AgentBackends,
		taskMgr:                d.TaskManager,
		provider:               d.Provider,
		repoReg:                newRepoRegistry(nil),
		ipgeoChecker:           d.IPGeoChecker,
	}
	return s, nil
}

// SetBot sets the forge automation bot owned by the server.
func (s *Server) SetBot(b *bot.Bot) {
	s.Bot = b
}

// SetCIService sets the CI service used by server handlers and adoption hooks.
func (s *Server) SetCIService(c *ci.Service) {
	s.ciService = c
}
