// Tests the RunInfra credits fetcher over an HTTP stub.

package usage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestRunInfraFetcherGet(t *testing.T) {
	t.Parallel()
	t.Run("balance and spend cap", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"object": "credits",
				"balance_cents": 5476,
				"held_cents": 34,
				"available_cents": 5442,
				"currency": "usd",
				"period": {"start": "2026-08-10T00:00:00.000Z", "spent_cents": 1500},
				"spend_cap": {"limit_cents": 20000, "hard": false, "used_cents": 1620, "remaining_cents": 18380, "gates_inference": false},
				"plan_tier": "pro"
			}`))
		}))
		defer server.Close()
		orig := runInfraCreditsURL
		runInfraCreditsURL = server.URL
		t.Cleanup(func() { runInfraCreditsURL = orig })

		f := NewRunInfraFetcher("runinfra-key")
		if f == nil {
			t.Fatal("NewRunInfraFetcher(key) = nil")
		}
		quota := f.Get(t.Context())
		if quota == nil || quota.Provider != agent.QuotaProviderRunInfra {
			t.Fatalf("Get() = %#v, want runinfra provider quota", quota)
		}
		if quota.Balance.Currency != "USD" || quota.Balance.Total != 54.42 {
			t.Fatalf("balance = %#v, want USD 54.42", quota.Balance)
		}
		if quota.ExtraUsage.MonthlyLimit != 200 || quota.ExtraUsage.UsedCredits != 16.20 {
			t.Fatalf("extra usage = %#v, want limit 200 used 16.20", quota.ExtraUsage)
		}
	})

	t.Run("no spend cap", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"balance_cents":100,"available_cents":100,"currency":"usd","spend_cap":null}`))
		}))
		defer server.Close()
		orig := runInfraCreditsURL
		runInfraCreditsURL = server.URL
		t.Cleanup(func() { runInfraCreditsURL = orig })

		quota := NewRunInfraFetcher("key").Get(t.Context())
		if quota == nil {
			t.Fatal("Get() = nil")
		}
		if quota.Balance.Total != 1 || quota.ExtraUsage.MonthlyLimit != 0 {
			t.Fatalf("quota = %#v, want balance 1 without extra usage", quota)
		}
	})

	t.Run("empty key returns no fetcher", func(t *testing.T) {
		t.Parallel()
		if f := NewRunInfraFetcher(""); f != nil {
			t.Fatalf("NewRunInfraFetcher(\"\") = %v, want nil", f)
		}
	})
}
