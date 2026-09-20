// Groq API provider stub. No programmatic balance endpoint exists; this
// fetcher registers the provider so the UI can display its logo and link to
// the console for manual balance checks.

package usage

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// GroqFetcher registers the Groq provider without balance data. Usage and
// charges are only shown in the console.groq.com dashboard.
type GroqFetcher struct {
	baseFetcher
}

// NewGroqFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewGroqFetcher(apiKey string) *GroqFetcher {
	if apiKey == "" {
		return nil
	}
	return &GroqFetcher{baseFetcher: newBaseFetcher(agent.QuotaProviderGroq, "Groq", AuthKindAPIKey, "https://console.groq.com/settings/usage")}
}

// Get returns provider metadata with no balance data.
func (f *GroqFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
