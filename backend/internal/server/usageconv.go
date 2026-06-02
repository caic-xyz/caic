// Usage quota conversions from internal domain types to API DTOs.

package server

import (
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

func providerQuotaToResp(q *usage.ProviderQuota) v1.ProviderQuota {
	if q == nil {
		return v1.ProviderQuota{}
	}
	out := v1.ProviderQuota{
		Provider:   q.Provider,
		Label:      q.Label,
		AuthKind:   q.AuthKind,
		RateLimits: make([]v1.QuotaRateLimit, len(q.RateLimits)),
		Balance: v1.QuotaBalance{
			Currency: q.Balance.Currency,
			Total:    q.Balance.Total,
			Granted:  q.Balance.Granted,
			ToppedUp: q.Balance.ToppedUp,
		},
		ExtraUsage: v1.QuotaExtraUsage{
			Currency:     q.ExtraUsage.Currency,
			IsEnabled:    q.ExtraUsage.IsEnabled,
			UsedCredits:  q.ExtraUsage.UsedCredits,
			MonthlyLimit: q.ExtraUsage.MonthlyLimit,
			UsedPct:      q.ExtraUsage.UsedPct,
		},
	}
	for i := range q.RateLimits {
		out.RateLimits[i] = v1.QuotaRateLimit{
			Window:   q.RateLimits[i].Window,
			UsedPct:  q.RateLimits[i].UsedPct,
			ResetsAt: q.RateLimits[i].ResetsAt,
		}
	}
	return out
}
