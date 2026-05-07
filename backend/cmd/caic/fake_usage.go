//go:build e2e

// Fake usage providers that return canned quota data for e2e testing,
// simulating Anthropic and Codex OAuth usage responses.

package main

import (
	"context"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// fakeFetcher implements usage.ProviderFetcher with a static response.
type fakeFetcher struct {
	provider string
	label    string
	authKind string
	resp     v1.ProviderQuota
}

func (f *fakeFetcher) Provider() string                { return f.provider }
func (f *fakeFetcher) Label() string                   { return f.label }
func (f *fakeFetcher) AuthKind() string                { return f.authKind }
func (f *fakeFetcher) UsageURL() string                { return "https://example.com" }
func (f *fakeFetcher) Get(_ context.Context) *v1.ProviderQuota { return &f.resp }

// fakeUsageFetchers returns canned Anthropic and Codex usage for e2e tests.
func fakeUsageFetchers() []usage.ProviderFetcher {
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
			resp: v1.ProviderQuota{
				Provider: "anthropic",
				Label:    "Anthropic",
				AuthKind: "oauth",
				RateLimits: []v1.QuotaRateLimit{
					{Window: "5h", UsedPct: 42, ResetsAt: fiveHourReset},
					{Window: "7d", UsedPct: 15, ResetsAt: sevenDayReset},
				},
				ExtraUsage: v1.QuotaExtraUsage{
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
			resp: v1.ProviderQuota{
				Provider: "codex",
				Label:    "Codex",
				AuthKind: "oauth",
				RateLimits: []v1.QuotaRateLimit{
					{Window: "primary", UsedPct: 68, ResetsAt: primaryReset},
					{Window: "secondary", UsedPct: 23, ResetsAt: secondaryReset},
				},
				Balance: v1.QuotaBalance{
					Currency: "USD",
					Total:    5.00,
				},
			},
		},
	}
}
