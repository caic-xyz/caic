// Tests for Codex OAuth usage quota parsing.

package usage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestCodexFetcher(t *testing.T) {
	t.Parallel()

	t.Run("labels primary seven-day window from its duration", func(t *testing.T) {
		t.Parallel()

		got := codexWindowLabel(codexWindowSnapshot{LimitWindowSeconds: 7 * 24 * 60 * 60}, "primary")
		if got != "7d" {
			t.Errorf("codexWindowLabel() = %q, want %q", got, "7d")
		}
	})
}

func TestCodexFetcherGet(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type": "pro",
			"rate_limit": {
				"primary_window": {"used_percent": 68, "limit_window_seconds": 18000, "reset_at": 1780000000},
				"secondary_window": {"used_percent": 23, "limit_window_seconds": 604800, "reset_at": 1781000000}
			}
		}`))
	}))
	t.Cleanup(server.Close)

	f := &CodexFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderCodex, AuthKindOAuth, "https://chatgpt.com/settings/usage?tab=overview"),
		client:      &http.Client{Transport: redirectTransport{server.URL}},
		token:       "token",
	}
	quota := f.Get(t.Context())
	if quota == nil {
		t.Fatal("Get() = nil")
	}
	if len(quota.RateLimits) != 2 {
		t.Fatalf("RateLimits = %#v, want two windows", quota.RateLimits)
	}
	if got := quota.RateLimits[0]; got.Window != "5h" || got.Utilization != 0.68 {
		t.Errorf("5h window = %#v, want utilization 68%% / 100", got)
	}
	if got := quota.RateLimits[1]; got.Window != "7d" || got.Utilization != 0.23 {
		t.Errorf("7d window = %#v, want utilization 23%% / 100", got)
	}
}
