// Tests for usage quota conversions from domain types to API DTOs.

package server

import (
	"reflect"
	"testing"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

func TestProviderQuotaToResp(t *testing.T) {
	t.Parallel()

	resetsAt := time.Date(2026, time.June, 1, 10, 30, 0, 0, time.UTC)
	got := providerQuotaToResp(&usage.ProviderQuota{
		Provider: "anthropic",
		Label:    "Anthropic",
		AuthKind: "oauth",
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
		Provider: "anthropic",
		Label:    "Anthropic",
		AuthKind: "oauth",
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
		t.Fatalf("providerQuotaToResp() = %#v, want %#v", got, want)
	}
}
