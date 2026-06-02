// Fake usage providers that return canned quota data for smoke and e2e tests.

package smoketest

import (
	"context"
	"time"

	"github.com/caic-xyz/caic/backend/internal/usage"
)

// fakeFetcher implements usage.ProviderFetcher with a static response.
type fakeFetcher struct {
	provider string
	label    string
	authKind string
	resp     usage.ProviderQuota
}

// Provider implements usage.ProviderFetcher.
func (f *fakeFetcher) Provider() string { return f.provider }

// Label implements usage.ProviderFetcher.
func (f *fakeFetcher) Label() string { return f.label }

// AuthKind implements usage.ProviderFetcher.
func (f *fakeFetcher) AuthKind() string { return f.authKind }

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
			provider: "anthropic",
			label:    "Anthropic",
			authKind: "oauth",
			resp: usage.ProviderQuota{
				Provider: "anthropic",
				Label:    "Anthropic",
				AuthKind: "oauth",
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
			provider: "codex",
			label:    "Codex",
			authKind: "oauth",
			resp: usage.ProviderQuota{
				Provider: "codex",
				Label:    "Codex",
				AuthKind: "oauth",
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
