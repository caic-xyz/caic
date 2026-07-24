// Tests for conversions between backend domain values and API v1 DTOs.

package v1conv

import (
	"reflect"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

func TestTask(t *testing.T) {
	t.Parallel()
	t.Run("OverageDoesNotBlock", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{}
		tk.RestoreMessages([]agent.Message{&agent.RateLimitMessage{
			Status:         "rejected",
			ResetsAt:       time.Now().Add(time.Hour),
			RateLimitType:  "five_hour",
			IsUsingOverage: true,
		}})

		got := Task(t.Context(), taskmgr.NewEntry(tk, nil), TaskResolvers{})
		if got.RateLimit.Blocked {
			t.Error("RateLimit.Blocked = true while overage is active")
		}
	})

	t.Run("RetainsBlockAcrossQuotaWindows", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		tk := &task.Task{}
		tk.RestoreMessages([]agent.Message{
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusRejected,
				ResetsAt:      now.Add(time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "5h",
			},
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusAllowedWarning,
				ResetsAt:      now.Add(7 * 24 * time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "7d",
			},
		})

		got := Task(t.Context(), taskmgr.NewEntry(tk, nil), TaskResolvers{}).RateLimit
		if !got.Blocked || got.Window != "5h" {
			t.Errorf("RateLimit = %#v, want active 5h block", got)
		}
	})
}

func TestProviderQuota(t *testing.T) {
	t.Parallel()

	resetsAt := time.Date(2026, time.June, 1, 10, 30, 0, 0, time.UTC)
	got := ProviderQuota(&usage.ProviderQuota{
		Provider: agent.QuotaProviderAnthropic,
		Label:    "Anthropic",
		AuthKind: usage.AuthKindOAuth,
		RateLimits: []usage.QuotaRateLimit{
			{Window: "5h", UsedPct: 42.5, ResetsAt: resetsAt},
		},
		Balance: usage.QuotaBalance{
			Currency: "USD",
			Total:    12.75,
			Granted:  2.25,
			ToppedUp: 10.5,
		},
		ExtraUsage: usage.QuotaExtraUsage{
			Currency:     "USD",
			IsEnabled:    true,
			UsedCredits:  3.5,
			MonthlyLimit: 25,
			UsedPct:      14,
		},
	})

	want := v1.ProviderQuota{
		Provider: v1.QuotaProviderAnthropic,
		Label:    "Anthropic",
		AuthKind: v1.ProviderAuthKindOAuth,
		RateLimits: []v1.QuotaRateLimit{
			{Window: "5h", UsedPct: 42.5, ResetsAt: resetsAt},
		},
		Balance: v1.QuotaBalance{
			Currency: "USD",
			Total:    12.75,
			Granted:  2.25,
			ToppedUp: 10.5,
		},
		ExtraUsage: v1.QuotaExtraUsage{
			Currency:     "USD",
			IsEnabled:    true,
			UsedCredits:  3.5,
			MonthlyLimit: 25,
			UsedPct:      14,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderQuota() = %#v, want %#v", got, want)
	}
}
