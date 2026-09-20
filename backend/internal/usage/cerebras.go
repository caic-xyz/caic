// Cerebras API provider stub. No programmatic balance endpoint exists; this
// fetcher registers the provider so the UI can display its logo and link to
// the console for manual balance checks.

package usage

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// CerebrasFetcher registers the Cerebras Inference provider without balance
// data. Credit balances are only shown in the cloud.cerebras.ai console and
// no billing endpoint is exposed for API keys.
type CerebrasFetcher struct {
	baseFetcher
}

// NewCerebrasFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewCerebrasFetcher(apiKey string) *CerebrasFetcher {
	if apiKey == "" {
		return nil
	}
	return &CerebrasFetcher{baseFetcher: newBaseFetcher(agent.QuotaProviderCerebras, "Cerebras", AuthKindAPIKey, "https://cloud.cerebras.ai/platform/")}
}

// Get returns provider metadata with no balance data.
func (f *CerebrasFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
