// Fake usage providers that return canned quota data for smoke and e2e tests.

package smoketest

import (
	"context"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// fakeFetcher implements usage.ProviderFetcher with a static response.
type fakeFetcher struct {
	provider agent.QuotaProvider
	label    string
	authKind usage.AuthKind
	resp     usage.ProviderQuota
}

// Provider implements usage.ProviderFetcher.
func (f *fakeFetcher) Provider() agent.QuotaProvider { return f.provider }

// Label implements usage.ProviderFetcher.
func (f *fakeFetcher) Label() string { return f.label }

// AuthKind implements usage.ProviderFetcher.
func (f *fakeFetcher) AuthKind() usage.AuthKind { return f.authKind }

// UsageURL implements usage.ProviderFetcher.
func (f *fakeFetcher) UsageURL() string { return "https://example.com" }

// Get implements usage.ProviderFetcher.
func (f *fakeFetcher) Get(_ context.Context) *usage.ProviderQuota { return &f.resp }

// UsageFetchers returns canned Anthropic and Codex usage for smoke and e2e tests.
func UsageFetchers() []usage.ProviderFetcher {
	now := time.Now().UTC()
	fiveHourReset := now.Add(2 * time.Hour)
	sevenDayReset := now.Add(3 * 24 * time.Hour)
	primaryReset := now.Add(1 * time.Hour)
	secondaryReset := now.Add(30 * time.Minute)

	return []usage.ProviderFetcher{
		&fakeFetcher{
			provider: agent.QuotaProviderAnthropic,
			label:    "Anthropic",
			authKind: usage.AuthKindOAuth,
			resp: usage.ProviderQuota{
				Provider: agent.QuotaProviderAnthropic,
				Label:    "Anthropic",
				AuthKind: usage.AuthKindOAuth,
				RateLimits: []usage.QuotaRateLimit{
					{Window: "5h", UsedPct: 42, ResetsAt: fiveHourReset},
					{Window: "7d", UsedPct: 15, ResetsAt: sevenDayReset},
				},
				ExtraUsage: usage.QuotaExtraUsage{
					Currency:     "USD",
					IsEnabled:    true,
					UsedCredits:  3.50,
					MonthlyLimit: 25.00,
					UsedPct:      14,
				},
			},
		},
		&fakeFetcher{
			provider: agent.QuotaProviderCodex,
			label:    "Codex",
			authKind: usage.AuthKindOAuth,
			resp: usage.ProviderQuota{
				Provider: agent.QuotaProviderCodex,
				Label:    "Codex",
				AuthKind: usage.AuthKindOAuth,
				RateLimits: []usage.QuotaRateLimit{
					{Window: "primary", UsedPct: 68, ResetsAt: primaryReset},
					{Window: "secondary", UsedPct: 23, ResetsAt: secondaryReset},
				},
				Balance: usage.QuotaBalance{
					Currency: "USD",
					Total:    5.00,
				},
			},
		},
	}
}
