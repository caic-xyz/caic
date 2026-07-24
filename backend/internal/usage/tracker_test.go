// Tests for canonical task quota updates merged into provider usage snapshots.

package usage

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestTrackerMerge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 23, 13, 48, 0, 0, time.UTC)
	providerSnapshot := []ProviderQuota{{
		Provider: agent.QuotaProviderAnthropic,
		Label:    "Anthropic",
		RateLimits: []QuotaRateLimit{{
			Window: "5h", UsedPct: 97, ResetsAt: now.Add(time.Hour),
		}},
	}}

	t.Run("InformativeUpdate", func(t *testing.T) {
		t.Parallel()
		tracker := NewTracker()
		resetAt := now.Add(90 * time.Minute)
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderAnthropic, ProviderLabel: "Anthropic", Window: "5h",
			Status: agent.RateLimitStatusAllowedWarning, UsedPct: 91, ResetsAt: resetAt, ObservedAt: now,
		}) {
			t.Fatal("Apply() = false, want true")
		}

		limit := tracker.Merge(providerSnapshot, now)[0].RateLimits[0]
		if limit.UsedPct != 91 {
			t.Errorf("UsedPct = %v, want 91", limit.UsedPct)
		}
		if !limit.ResetsAt.Equal(resetAt) {
			t.Errorf("ResetsAt = %v, want %v", limit.ResetsAt, resetAt)
		}
	})

	t.Run("NewerUpdateWins", func(t *testing.T) {
		t.Parallel()
		tracker := NewTracker()
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderAnthropic, ProviderLabel: "Anthropic", Window: "5h",
			Status: agent.RateLimitStatusRejected, ObservedAt: now,
		}) {
			t.Fatal("Apply(rejected) = false, want true")
		}
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderAnthropic, ProviderLabel: "Anthropic", Window: "5h",
			Status: agent.RateLimitStatusAllowed, UsedPct: 12, ObservedAt: now.Add(time.Minute),
		}) {
			t.Fatal("Apply(allowed) = false, want true")
		}

		limit := tracker.Merge(providerSnapshot, now)[0].RateLimits[0]
		if limit.UsedPct != 12 {
			t.Errorf("UsedPct = %v, want 12 from the newer update", limit.UsedPct)
		}
		if !limit.ResetsAt.Equal(now.Add(time.Hour)) {
			t.Errorf("ResetsAt = %v, want provider reset %v", limit.ResetsAt, now.Add(time.Hour))
		}
	})

	t.Run("ClaudeCodeSubscriptionUpdatesMergeWithFetcher", func(t *testing.T) {
		t.Parallel()
		tracker := NewTracker()
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderClaudeCode, ProviderLabel: "Claude Code", Window: "5h",
			Status: agent.RateLimitStatusRejected, ResetsAt: now.Add(90 * time.Minute), ObservedAt: now,
		}) {
			t.Fatal("Apply() = false, want true")
		}
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderClaudeCode, ProviderLabel: "Claude Code", Window: "7d",
			Status: agent.RateLimitStatusAllowedWarning, UsedPct: 91, ResetsAt: now.Add(7 * 24 * time.Hour), ObservedAt: now,
		}) {
			t.Fatal("Apply() = false, want true")
		}
		providerSnapshot := []ProviderQuota{{
			Provider: agent.QuotaProviderClaudeCode,
			Label:    "Claude Code",
			RateLimits: []QuotaRateLimit{
				{Window: "5h", UsedPct: 97, ResetsAt: now.Add(time.Hour)},
				{Window: "7d", UsedPct: 85, ResetsAt: now.Add(6 * 24 * time.Hour)},
			},
		}}

		merged := tracker.Merge(providerSnapshot, now)
		if len(merged) != 1 || len(merged[0].RateLimits) != 2 {
			t.Fatalf("Merge() = %#v, want two Claude Code quota windows", merged)
		}
		if got := merged[0].RateLimits[0]; got.UsedPct != 100 || !got.ResetsAt.Equal(now.Add(90*time.Minute)) {
			t.Errorf("5h merged rate limit = %#v, want rejected update", got)
		}
		if got := merged[0].RateLimits[1]; got.UsedPct != 91 || !got.ResetsAt.Equal(now.Add(7*24*time.Hour)) {
			t.Errorf("7d merged rate limit = %#v, want warning update", got)
		}
	})

	t.Run("ExpiredUpdateIgnored", func(t *testing.T) {
		t.Parallel()
		tracker := NewTracker()
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderAnthropic, ProviderLabel: "Anthropic", Window: "5h",
			Status: agent.RateLimitStatusRejected, ResetsAt: now.Add(-time.Minute), ObservedAt: now,
		}) {
			t.Fatal("Apply() = false, want true")
		}

		limit := tracker.Merge(providerSnapshot, now)[0].RateLimits[0]
		if limit.UsedPct != 97 {
			t.Errorf("UsedPct = %v, want original provider value 97", limit.UsedPct)
		}
	})

	t.Run("ResetlessUpdateExpires", func(t *testing.T) {
		t.Parallel()
		tracker := NewTracker()
		if !tracker.Apply(&TaskQuotaUpdate{
			Provider: agent.QuotaProviderAnthropic, ProviderLabel: "Anthropic", Window: "5h",
			Status: agent.RateLimitStatusRejected, ObservedAt: now,
		}) {
			t.Fatal("Apply() = false, want true")
		}

		current := tracker.Merge(providerSnapshot, now)[0].RateLimits[0]
		if current.UsedPct != 100 {
			t.Errorf("current UsedPct = %v, want 100", current.UsedPct)
		}
		expired := tracker.Merge(providerSnapshot, now.Add(taskQuotaUnknownResetTTL))[0].RateLimits[0]
		if expired.UsedPct != 97 {
			t.Errorf("expired UsedPct = %v, want provider value 97", expired.UsedPct)
		}
	})
}
