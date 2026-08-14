// Provider setup for usage, title generation, and forge OAuth tokens.

package app

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// usageFetchers returns cfg.UsageFetchers when non-nil (fake/e2e), otherwise
// auto-detects providers from the environment.
func usageFetchers(cfg *server.Config, ctx context.Context) []usage.ProviderFetcher {
	if cfg.UsageFetchers != nil {
		return cfg.UsageFetchers
	}
	return detectProviders(ctx, cfg.Agent.CoreEnv, cfg.Agent.HarnessEnv)
}

func detectProviders(ctx context.Context, coreEnv map[string]string, harnessEnv map[string][]string) []usage.ProviderFetcher {
	var fetchers []usage.ProviderFetcher

	if f := usage.NewClaudeCodeFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}
	if f := usage.NewCodexFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}

	for _, entry := range apiKeyUsageFetchers {
		key := providerAPIKey(entry.provider, coreEnv, harnessEnv)
		if key == "" {
			continue
		}
		if f := entry.factory(key); f != nil {
			fetchers = append(fetchers, f)
		}
	}

	slog.InfoContext(ctx, "provider usage fetchers", "count", len(fetchers))
	return fetchers
}

func autoDetectLLMProvider(ctx context.Context, coreEnv map[string]string) string {
	preferred := []string{
		"codex",
		"opencode",
		"claudecode",
		"pi",
	}
	for _, name := range preferred {
		if pingProvider(ctx, name, coreEnv) {
			return name
		}
	}
	for name := range providers.All {
		if pingProvider(ctx, name, coreEnv) {
			return name
		}
	}
	return ""
}

func pingProvider(ctx context.Context, name string, coreEnv map[string]string) bool {
	c, ok := providers.All[name]
	if !ok || c.Factory == nil {
		return false
	}
	opts := []genai.ProviderOption{genai.ModelCheap}
	opts = appendProviderAPIKey(opts, name, coreEnv)
	p, err := c.Factory(ctx, opts...)
	if err != nil {
		slog.DebugContext(ctx, "provider factory failed", "prov", name, "err", err)
		return false
	}
	if pinger, ok := p.(genai.ProviderPing); ok {
		if err := pinger.Ping(ctx); err != nil {
			slog.DebugContext(ctx, "provider ping failed", "prov", name, "err", err)
			return false
		}
	}
	slog.InfoContext(ctx, "provider detected", "prov", name)
	return true
}

func appendProviderAPIKey(
	opts []genai.ProviderOption,
	providerName string,
	coreEnv map[string]string,
) []genai.ProviderOption {
	return appendProviderAPIKeyWithEnv(opts, providerName, coreEnv, os.Getenv)
}

func appendProviderAPIKeyWithEnv(
	opts []genai.ProviderOption,
	providerName string,
	coreEnv map[string]string,
	getenv func(string) string,
) []genai.ProviderOption {
	key := providerAPIKeyWithEnv(providerName, coreEnv, nil, getenv)
	if key == "" {
		return opts
	}
	return append(opts, genai.ProviderOptionAPIKey(key))
}

func providerAPIKey(providerName string, coreEnv map[string]string, harnessEnv map[string][]string) string {
	return providerAPIKeyWithEnv(providerName, coreEnv, harnessEnv, os.Getenv)
}

func providerAPIKeyWithEnv(
	providerName string,
	coreEnv map[string]string,
	harnessEnv map[string][]string,
	getenv func(string) string,
) string {
	c, ok := providers.All[providerName]
	if !ok || c.APIKeyEnvVar == "" {
		return ""
	}
	if key := configuredAPIKey(coreEnv, harnessEnv, c.APIKeyEnvVar); key != "" {
		return key
	}
	return getenv(c.APIKeyEnvVar)
}

func configuredAPIKey(coreEnv map[string]string, harnessEnv map[string][]string, envVar string) string {
	if v, ok := coreEnv[envVar]; ok {
		return v
	}
	for _, envs := range harnessEnv {
		for _, e := range envs {
			k, v, ok := strings.Cut(e, "=")
			if ok && k == envVar {
				return v
			}
		}
	}
	return ""
}

var apiKeyUsageFetchers = []struct {
	provider string
	factory  func(string) usage.ProviderFetcher
}{
	{provider: "deepseek", factory: func(key string) usage.ProviderFetcher { return usage.NewDeepSeekFetcher(key) }},
	{provider: "openrouter", factory: func(key string) usage.ProviderFetcher { return usage.NewOpenRouterFetcher(key) }},
	{provider: "xiaomi", factory: func(key string) usage.ProviderFetcher { return usage.NewXiaomiFetcher(key) }},
}

// authForgeTokenSource adapts authenticated request users to forge OAuth tokens.
type authForgeTokenSource struct{}

func (authForgeTokenSource) TokenFor(ctx context.Context, kind forge.Kind) (forgemgr.OAuthToken, bool) {
	u, ok := auth.UserFromContext(ctx)
	if !ok || u.AccessToken == "" || !providerMatchesForge(u.Provider, kind) {
		return forgemgr.OAuthToken{}, false
	}
	return forgemgr.OAuthToken{AccessToken: u.AccessToken, UserID: u.ID}, true
}

func providerMatchesForge(provider auth.Provider, kind forge.Kind) bool {
	switch kind {
	case forge.KindGitHub:
		return provider == auth.ProviderGitHub
	case forge.KindGitLab:
		return provider == auth.ProviderGitLab
	default:
		return false
	}
}
