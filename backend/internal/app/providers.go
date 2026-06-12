// Provider detection for usage and title-generation services.

package app

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"

	"github.com/caic-xyz/caic/backend/internal/usage"
)

func detectProviders(ctx context.Context, coreEnv map[string]string, harnessEnv map[string][]string) []usage.ProviderFetcher {
	var fetchers []usage.ProviderFetcher

	if f := usage.NewAnthropicFetcher(ctx); f != nil {
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
		slog.Debug("provider factory failed", "prov", name, "err", err)
		return false
	}
	if pinger, ok := p.(genai.ProviderPing); ok {
		if err := pinger.Ping(ctx); err != nil {
			slog.Debug("provider ping failed", "prov", name, "err", err)
			return false
		}
	}
	slog.Info("provider detected", "prov", name)
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
