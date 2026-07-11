// Package usage provides cached fetchers and domain types for LLM provider
// usage quotas.
package usage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// CacheTTL is the duration before cached usage data is considered stale.
	CacheTTL = 5 * time.Minute

	// Exponential backoff parameters for fetch errors.
	backoffMin = 5 * time.Minute
	backoffMax = 1 * time.Hour
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
	Get(ctx context.Context) *ProviderQuota
}

// QuotaRateLimit is a rate-limit window reported by a provider.
type QuotaRateLimit struct {
	Window   string
	UsedPct  float64
	ResetsAt time.Time
}

// QuotaBalance is a balance or credit snapshot reported by a provider.
type QuotaBalance struct {
	Currency string
	Total    float64
	Granted  float64
	ToppedUp float64
}

// QuotaExtraUsage is pay-as-you-go usage information reported by a provider.
type QuotaExtraUsage struct {
	Currency     string
	IsEnabled    bool
	UsedCredits  float64
	MonthlyLimit float64
	UsedPct      float64
}

// ProviderQuota is quota data for one provider.
type ProviderQuota struct {
	Provider string
	Label    string
	AuthKind string

	RateLimits []QuotaRateLimit
	Balance    QuotaBalance
	ExtraUsage QuotaExtraUsage
}

func newBaseFetcher(provider, label, authKind, usageURL string) baseFetcher {
	return baseFetcher{
		provider: provider,
		label:    label,
		authKind: authKind,
		usageURL: usageURL,
	}
}

// baseFetcher holds the shared provider metadata, caching, backoff, and
// locking logic used by all ProviderFetcher implementations.
type baseFetcher struct {
	provider string
	label    string
	authKind string
	usageURL string

	mu      sync.Mutex
	cached  *ProviderQuota
	fetchAt time.Time
	backoff time.Duration
	errorAt time.Time
}

// Provider returns the provider identifier.
func (b *baseFetcher) Provider() string { return b.provider }

// Label returns the human-readable provider name.
func (b *baseFetcher) Label() string { return b.label }

// AuthKind returns the authentication method.
func (b *baseFetcher) AuthKind() string { return b.authKind }

// UsageURL returns the link to the provider's usage/billing page.
func (b *baseFetcher) UsageURL() string { return b.usageURL }

func (b *baseFetcher) quota() *ProviderQuota {
	return &ProviderQuota{
		Provider: b.provider,
		Label:    b.label,
		AuthKind: b.authKind,
	}
}

func (b *baseFetcher) get(ctx context.Context, fetch func(context.Context) (*ProviderQuota, error)) *ProviderQuota {
	return b.getIf(ctx, nil, fetch)
}

func (b *baseFetcher) getIf(ctx context.Context, ok func() bool, fetch func(context.Context) (*ProviderQuota, error)) *ProviderQuota {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok != nil && !ok() {
		return nil
	}
	if b.cached != nil && time.Since(b.fetchAt) < CacheTTL {
		return b.cached
	}
	if b.backoff > 0 && time.Since(b.errorAt) < b.backoff {
		return b.cached
	}
	resp, err := fetch(ctx)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch "+b.label+" usage", "err", err)
		b.errorAt = time.Now()
		if b.backoff == 0 {
			b.backoff = backoffMin
		} else {
			b.backoff *= 2
			if b.backoff > backoffMax {
				b.backoff = backoffMax
			}
		}
		return b.cached
	}
	b.backoff = 0
	b.cached = resp
	b.fetchAt = time.Now()
	return resp
}
