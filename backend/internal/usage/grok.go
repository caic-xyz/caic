// xAI Grok API provider stub. Prepaid credit balances are only exposed
// through the Management API, which requires a separate management key and
// team ID; this fetcher registers the provider so the UI can display its
// logo and link to the console for manual balance checks.

package usage

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// GrokFetcher registers the xAI Grok provider without balance data. The
// regular API key cannot query billing; the Management API
// (management-api.x.ai) needs a management key and team ID.
type GrokFetcher struct {
	baseFetcher
}

// NewGrokFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewGrokFetcher(apiKey string) *GrokFetcher {
	if apiKey == "" {
		return nil
	}
	return &GrokFetcher{baseFetcher: newBaseFetcher(agent.QuotaProviderGrok, "Grok", AuthKindAPIKey, "https://console.x.ai/team/default/billing")}
}

// Get returns provider metadata with no balance data.
func (f *GrokFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
