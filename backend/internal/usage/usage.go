// Package usage provides cached fetchers for LLM provider usage quotas.
// Each provider (Anthropic, DeepSeek, Gemini, Codex, …) has its own
// fetcher implementing the ProviderFetcher interface. The server
// auto-detects available providers from configuration and environment.
package usage

import "time"

const (
	// CacheTTL is the duration before cached usage data is considered stale.
	CacheTTL = 5 * time.Minute

	// Exponential backoff parameters for fetch errors.
	backoffMin = 5 * time.Minute
	backoffMax = 1 * time.Hour
)
