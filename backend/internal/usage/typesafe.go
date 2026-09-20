// TypeSafe API provider stub. No programmatic balance endpoint exists; this
// fetcher registers the provider so the UI can display its logo and link to
// the console for manual balance checks.

package usage

import (
	"context"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// TypeSafeFetcher registers the TypeSafe provider without balance data.
// Credit balances are only shown in the console.typesafe.ai console.
type TypeSafeFetcher struct {
	baseFetcher
}

// NewTypeSafeFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewTypeSafeFetcher(apiKey string) *TypeSafeFetcher {
	if apiKey == "" {
		return nil
	}
	return &TypeSafeFetcher{baseFetcher: newBaseFetcher(agent.QuotaProviderTypeSafe, "TypeSafe", AuthKindAPIKey, "https://console.typesafe.ai/usage")}
}

// Get returns provider metadata with no balance data.
func (f *TypeSafeFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
