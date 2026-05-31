// Detection of usage-quota fetchers and the title-generation LLM provider.

package server

import (
	"context"
	"log/slog"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
)

// detectProviders scans harness environment variables for known API keys and
// OAuth credential files, creating the appropriate ProviderFetcher for each
// provider found. OAuth-based providers (Anthropic, Codex) are always
// attempted since their credentials come from files, not env vars.
func detectProviders(ctx context.Context, harnessEnv map[string][]string) []usage.ProviderFetcher {
	// Collect all env vars across all harnesses.
	envKeys := make(map[string]struct{})
	for _, envs := range harnessEnv {
		for _, e := range envs {
			if k, _, ok := strings.Cut(e, "="); ok {
				envKeys[k] = struct{}{}
			}
		}
	}

	var fetchers []usage.ProviderFetcher

	// OAuth-based: always try these (they watch credential files).
	if f := usage.NewAnthropicFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}
	if f := usage.NewCodexFetcher(ctx); f != nil {
		fetchers = append(fetchers, f)
	}

	// API-key-based: detect from env vars.
	if _, ok := envKeys["DEEPSEEK_API_KEY"]; ok {
		key := firstEnvValue(harnessEnv, "DEEPSEEK_API_KEY")
		if f := usage.NewDeepSeekFetcher(key); f != nil {
			fetchers = append(fetchers, f)
		}
	}
	if _, ok := envKeys["OPENROUTER_API_KEY"]; ok {
		key := firstEnvValue(harnessEnv, "OPENROUTER_API_KEY")
		if f := usage.NewOpenRouterFetcher(key); f != nil {
			fetchers = append(fetchers, f)
		}
	}
	if _, ok := envKeys["XIAOMI_API_KEY"]; ok {
		key := firstEnvValue(harnessEnv, "XIAOMI_API_KEY")
		if f := usage.NewXiaomiFetcher(key); f != nil {
			fetchers = append(fetchers, f)
		}
	}

	slog.InfoContext(ctx, "provider usage fetchers", "count", len(fetchers))
	return fetchers
}

// firstEnvValue returns the value for the given key from the first harness
// that defines it.
func firstEnvValue(harnessEnv map[string][]string, key string) string {
	for _, envs := range harnessEnv {
		for _, e := range envs {
			k, v, ok := strings.Cut(e, "=")
			if ok && k == key {
				return v
			}
		}
	}
	return ""
}

// autoDetectLLMProvider detects the best available LLM provider from the
// genai providers registry by attempting to instantiate and ping each one.
// It prefers locally-available providers (codex, opencode, claudecode) over
// remote APIs (gemini). Returns "" if no suitable provider is found.
func autoDetectLLMProvider(ctx context.Context, geminiAPIKey string) string {
	// Preferred order: container-local providers first, then others.
	preferred := []string{
		"codex",
		"opencode",
		"claudecode",
		"gemini",
	}
	for _, name := range preferred {
		if pingProvider(ctx, name, geminiAPIKey) {
			return name
		}
	}
	// Fallback: iterate over all providers and pick the first one that responds to ping.
	for name := range providers.All {
		if pingProvider(ctx, name, geminiAPIKey) {
			return name
		}
	}
	return ""
}

// pingProvider attempts to instantiate and ping a provider, returning true if successful.
func pingProvider(ctx context.Context, name, geminiAPIKey string) bool {
	c, ok := providers.All[name]
	if !ok || c.Factory == nil {
		return false
	}
	var opts []genai.ProviderOption
	opts = append(opts, genai.ModelCheap)
	// Pass API key if configured for the provider.
	if name == "gemini" && geminiAPIKey != "" {
		opts = append(opts, genai.ProviderOptionAPIKey(geminiAPIKey))
	}
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
