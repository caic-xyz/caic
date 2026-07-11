// Xiaomi MiMo API provider stub. No programmatic balance endpoint exists;
// this fetcher registers the provider so the UI can display its logo and
// link to the console for manual balance checks.

package usage

import "context"

// XiaomiFetcher registers the Xiaomi MiMo provider without balance data.
// The platform (platform.xiaomimimo.com) requires Xiaomi account OAuth for
// balance queries, which cannot be done with a plain API key.
type XiaomiFetcher struct {
	baseFetcher
}

// NewXiaomiFetcher creates a fetcher. Returns nil when apiKey is empty.
func NewXiaomiFetcher(apiKey string) *XiaomiFetcher {
	if apiKey == "" {
		return nil
	}
	return &XiaomiFetcher{baseFetcher: newBaseFetcher("xiaomi", "Xiaomi MiMo", "apikey", "https://platform.xiaomimimo.com/console/balance")}
}

// Get returns provider metadata with no balance data.
func (f *XiaomiFetcher) Get(_ context.Context) *ProviderQuota {
	return f.quota()
}
