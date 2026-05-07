// ProviderFetcher abstracts a single provider's quota API. Each concrete
// implementation knows how to authenticate (OAuth token file watching or
// static API key) and how to map the provider-specific response into the
// generic ProviderQuota shape.
package usage

import (
	"context"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// ProviderFetcher fetches quota for one provider. It owns its own caching
// and backoff strategy.
type ProviderFetcher interface {
	// Provider returns the provider identifier (e.g. "anthropic", "deepseek").
	Provider() string
	// Label returns a human-readable provider name (e.g. "Anthropic", "DeepSeek").
	Label() string
	// AuthKind returns "oauth" or "apikey".
	AuthKind() string
	// UsageURL returns the link to the provider's usage/billing page.
	UsageURL() string
	// Get returns the latest quota data, using cached values when fresh.
	Get(ctx context.Context) *v1.ProviderQuota
}
