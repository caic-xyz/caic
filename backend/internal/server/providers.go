// Detection of usage-quota fetchers and the title-generation LLM provider.

package server

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"

	"github.com/caic-xyz/caic/backend/internal/usage"
)

// detectProviders creates usage fetchers for configured providers. OAuth-based
// providers watch credential files; API-key providers use genai provider
// metadata to resolve the expected environment variable name.
func detectProviders(ctx context.Context, coreEnv map[string]string, harnessEnv map[string][]string) []usage.ProviderFetcher {
	var fetchers []usage.ProviderFetcher

	// OAuth-based: always try these (they watch credential files).
	if f := usage.NewAnthropicFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}
	if f := usage.NewCodexFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}

	for _, entry := range apiKeyUsageFetchers {
		key := providerAPIKey(entry.provider, coreEnv, harnessEnv, "")
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

// autoDetectLLMProvider detects the best available LLM provider from the
// genai providers registry by attempting to instantiate and ping each one.
// It prefers locally-available providers (codex, opencode, claudecode) over
// remote APIs (gemini). Returns "" if no suitable provider is found.
func autoDetectLLMProvider(ctx context.Context, coreEnv map[string]string, geminiAPIKey string) string {
	// Preferred order: container-local providers first, then others.
	preferred := []string{
		"codex",
		"opencode",
		"claudecode",
		"gemini",
	}
	for _, name := range preferred {
		if pingProvider(ctx, name, coreEnv, geminiAPIKey) {
			return name
		}
	}
	// Fallback: iterate over all providers and pick the first one that responds to ping.
	for name := range providers.All {
		if pingProvider(ctx, name, coreEnv, geminiAPIKey) {
			return name
		}
	}
	return ""
}

// pingProvider attempts to instantiate and ping a provider, returning true if successful.
func pingProvider(ctx context.Context, name string, coreEnv map[string]string, geminiAPIKey string) bool {
	c, ok := providers.All[name]
	if !ok || c.Factory == nil {
		return false
	}
	var opts []genai.ProviderOption
	opts = append(opts, genai.ModelCheap)
	opts = appendProviderAPIKey(opts, name, coreEnv, geminiAPIKey)
	p, err := c.Factory(ctx, opts...)
	if err != nil {
		slog.Debug("provider factory failed", "prov", name, "err", err)
		return false
	}
	// If the provider supports pinging, verify it's accessible.
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
	geminiAPIKey string,
) []genai.ProviderOption {
	return appendProviderAPIKeyWithEnv(opts, providerName, coreEnv, geminiAPIKey, os.Getenv)
}

func appendProviderAPIKeyWithEnv(
	opts []genai.ProviderOption,
	providerName string,
	coreEnv map[string]string,
	geminiAPIKey string,
	getenv func(string) string,
) []genai.ProviderOption {
	key := providerAPIKeyWithEnv(providerName, coreEnv, nil, geminiAPIKey, getenv)
	if key == "" {
		return opts
	}
	return append(opts, genai.ProviderOptionAPIKey(key))
}

func providerAPIKey(providerName string, coreEnv map[string]string, harnessEnv map[string][]string, geminiAPIKey string) string {
	return providerAPIKeyWithEnv(providerName, coreEnv, harnessEnv, geminiAPIKey, os.Getenv)
}

func providerAPIKeyWithEnv(
	providerName string,
	coreEnv map[string]string,
	harnessEnv map[string][]string,
	geminiAPIKey string,
	getenv func(string) string,
) string {
	c, ok := providers.All[providerName]
	if !ok || c.APIKeyEnvVar == "" {
		return ""
	}
	if key := configuredAPIKey(coreEnv, harnessEnv, c.APIKeyEnvVar); key != "" {
		return key
	}
	if providerName == "gemini" {
		if geminiAPIKey != "" {
			return geminiAPIKey
		}
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
