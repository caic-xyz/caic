// Xiaomi MiMo API provider stub. No programmatic balance endpoint exists;
// this fetcher registers the provider so the UI can display its logo and
// link to the console for manual balance checks.

package usage

import (
	"context"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
)

// XiaomiFetcher registers the Xiaomi MiMo provider without balance data.
// The platform (platform.xiaomimimo.com) requires Xiaomi account OAuth for
// balance queries, which cannot be done with a plain API key.
type XiaomiFetcher struct{}

// NewXiaomiFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewXiaomiFetcher(apiKey string) *XiaomiFetcher {
	if apiKey == "" {
		return nil
	}
	return &XiaomiFetcher{}
}

// Provider returns the provider identifier.
func (f *XiaomiFetcher) Provider() string { return "xiaomi" }

// Label returns the human-readable provider name.
func (f *XiaomiFetcher) Label() string { return "Xiaomi MiMo" }

// AuthKind returns the authentication method.
func (f *XiaomiFetcher) AuthKind() string { return "apikey" }

// UsageURL returns the link to the provider's usage/billing page.
func (f *XiaomiFetcher) UsageURL() string { return "https://platform.xiaomimimo.com/console/balance" }

// Get returns provider metadata with no balance data.
func (f *XiaomiFetcher) Get(ctx context.Context) *v1.ProviderQuota {
	return &v1.ProviderQuota{
		Provider: f.Provider(),
		Label:    f.Label(),
		AuthKind: f.AuthKind(),
	}
}
