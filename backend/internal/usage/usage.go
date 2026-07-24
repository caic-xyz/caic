// Package usage provides cached fetchers, task quota tracking, and domain types
// for LLM provider usage quotas.
package usage

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

const (
	// CacheTTL is the duration before cached usage data is considered stale.
	CacheTTL = 5 * time.Minute

	// Exponential backoff parameters for fetch errors.
	backoffMin = 5 * time.Minute
	backoffMax = 1 * time.Hour

	// Task updates without a provider reset are no longer authoritative after
	// the normal provider refresh interval.
	taskQuotaUnknownResetTTL = CacheTTL
)

// AuthKind identifies a provider authentication method.
type AuthKind string

// Provider authentication methods.
const (
	AuthKindOAuth  AuthKind = "oauth"
	AuthKindAPIKey AuthKind = "apikey"
)

// ProviderFetcher fetches quota for one provider. It owns its own caching
// and backoff strategy.
type ProviderFetcher interface {
	// Provider returns the provider identifier (e.g. "anthropic", "deepseek").
	Provider() agent.QuotaProvider
	// Label returns a human-readable provider name (e.g. "Anthropic", "DeepSeek").
	Label() string
	// AuthKind returns the provider authentication method.
	AuthKind() AuthKind
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
	Provider agent.QuotaProvider
	Label    string
	AuthKind AuthKind

	RateLimits []QuotaRateLimit
	Balance    QuotaBalance
	ExtraUsage QuotaExtraUsage
}

// TaskQuotaUpdate is a canonical, provider-neutral quota update emitted by a
// harness adapter. Provider and Window are stable identifiers; adapters own
// translating their native protocol values into them.
type TaskQuotaUpdate struct {
	Provider      agent.QuotaProvider
	ProviderLabel string
	Window        string
	Status        agent.RateLimitStatus
	UsedPct       float64
	ResetsAt      time.Time
	ObservedAt    time.Time
}

type quotaKey struct {
	provider agent.QuotaProvider
	window   string
}

// Tracker retains the newest quota update for each provider window reported by
// tasks. It is safe for concurrent use by task event dispatch and HTTP reads.
type Tracker struct {
	mu      sync.Mutex
	updates map[quotaKey]TaskQuotaUpdate
}

// NewTracker creates an empty task quota tracker.
func NewTracker() *Tracker {
	return &Tracker{updates: make(map[quotaKey]TaskQuotaUpdate)}
}

// Apply records update when it is newer than the previously recorded value for
// that provider window. It returns true when the tracked state changed.
func (t *Tracker) Apply(update *TaskQuotaUpdate) bool {
	if !validTaskQuotaUpdate(update) {
		return false
	}
	normalized := *update
	normalized.UsedPct = min(max(normalized.UsedPct, 0), 100)
	if normalized.Status == agent.RateLimitStatusRejected {
		normalized.UsedPct = 100
	}

	key := quotaKey{provider: normalized.Provider, window: normalized.Window}
	t.mu.Lock()
	defer t.mu.Unlock()
	previous, ok := t.updates[key]
	if ok && !normalized.ObservedAt.After(previous.ObservedAt) {
		return false
	}
	t.updates[key] = normalized
	return true
}

func validTaskQuotaUpdate(update *TaskQuotaUpdate) bool {
	if update == nil || !update.Provider.Valid() || update.Window == "" || update.ObservedAt.IsZero() {
		return false
	}
	return update.Status.Valid()
}

// Merge applies tracked task updates to provider snapshots. A task update is
// authoritative until its reset time. An update without a reset expires after
// the normal provider refresh interval and otherwise retains the provider
// snapshot's reset time when one is available.
func (t *Tracker) Merge(providers []ProviderQuota, now time.Time) []ProviderQuota {
	merged := cloneProviderQuotas(providers)
	updates := t.activeUpdates(now)
	byProvider := make(map[agent.QuotaProvider]int, len(merged))
	for i := range merged {
		byProvider[merged[i].Provider] = i
	}
	for i := range updates {
		update := &updates[i]
		index, ok := byProvider[update.Provider]
		if !ok {
			merged = append(merged, ProviderQuota{
				Provider: update.Provider,
				Label:    update.ProviderLabel,
			})
			index = len(merged) - 1
			byProvider[update.Provider] = index
		}
		quota := &merged[index]
		if quota.Label == "" {
			quota.Label = update.ProviderLabel
		}
		if quota.Label == "" {
			quota.Label = string(update.Provider)
		}
		mergeTaskQuotaUpdate(quota, update)
	}
	return merged
}

func (t *Tracker) activeUpdates(now time.Time) []TaskQuotaUpdate {
	t.mu.Lock()
	defer t.mu.Unlock()
	updates := make([]TaskQuotaUpdate, 0, len(t.updates))
	for _, update := range t.updates {
		if !update.ResetsAt.IsZero() && !update.ResetsAt.After(now) {
			continue
		}
		if update.ResetsAt.IsZero() && !update.ObservedAt.Add(taskQuotaUnknownResetTTL).After(now) {
			continue
		}
		updates = append(updates, update)
	}
	slices.SortFunc(updates, func(a, b TaskQuotaUpdate) int {
		if diff := cmp.Compare(a.Provider, b.Provider); diff != 0 {
			return diff
		}
		return cmp.Compare(a.Window, b.Window)
	})
	return updates
}

func cloneProviderQuotas(providers []ProviderQuota) []ProviderQuota {
	cloned := make([]ProviderQuota, len(providers))
	copy(cloned, providers)
	for i := range cloned {
		cloned[i].RateLimits = slices.Clone(cloned[i].RateLimits)
	}
	return cloned
}

func mergeTaskQuotaUpdate(quota *ProviderQuota, update *TaskQuotaUpdate) {
	for i := range quota.RateLimits {
		if quota.RateLimits[i].Window != update.Window {
			continue
		}
		quota.RateLimits[i].UsedPct = update.UsedPct
		if !update.ResetsAt.IsZero() {
			quota.RateLimits[i].ResetsAt = update.ResetsAt
		}
		return
	}
	quota.RateLimits = append(quota.RateLimits, QuotaRateLimit{
		Window:   update.Window,
		UsedPct:  update.UsedPct,
		ResetsAt: update.ResetsAt,
	})
}

func newBaseFetcher(provider agent.QuotaProvider, label string, authKind AuthKind, usageURL string) baseFetcher {
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
	provider agent.QuotaProvider
	label    string
	authKind AuthKind
	usageURL string

	mu      sync.Mutex
	cached  *ProviderQuota
	fetchAt time.Time
	backoff time.Duration
	errorAt time.Time
}

// Provider returns the provider identifier.
func (b *baseFetcher) Provider() agent.QuotaProvider { return b.provider }

// Label returns the human-readable provider name.
func (b *baseFetcher) Label() string { return b.label }

// AuthKind returns the authentication method.
func (b *baseFetcher) AuthKind() AuthKind { return b.authKind }

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
