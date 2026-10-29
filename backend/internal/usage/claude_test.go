// Tests the Claude Code OAuth usage fetcher over an HTTP stub.

package usage

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestCredentialWatchersClose(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{".claude", ".codex"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	claude := NewClaudeCodeFetcher(t.Context())
	codex := NewCodexFetcher(t.Context())
	if claude.watcher == nil || codex.watcher == nil {
		t.Fatal("credential watchers failed to start")
	}
	if err := claude.Close(); err != nil {
		t.Fatal(err)
	}
	if err := codex.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-claude.watcher.Events; ok {
		t.Fatal("Claude watcher remained open after Close")
	}
	if _, ok := <-codex.watcher.Events; ok {
		t.Fatal("Codex watcher remained open after Close")
	}
}

func TestClaudeCodeFetcherGet(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"five_hour": {"utilization": 69.0, "resets_at": "2026-10-01T22:30:00Z"},
			"seven_day": {"utilization": 63.5, "resets_at": "2026-10-02T03:00:00Z"},
			"extra_usage": {"is_enabled": true, "monthly_limit": 20000, "used_credits": 500, "utilization": 2.5}
		}`))
	}))
	t.Cleanup(server.Close)

	f := &ClaudeCodeFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderClaudeCode, AuthKindOAuth, "https://claude.ai/settings/usage"),
		client:      &http.Client{Transport: redirectTransport{server.URL}},
		token:       "oat-test",
	}
	quota := f.Get(t.Context())
	if quota == nil {
		t.Fatal("Get() = nil")
	}
	if len(quota.RateLimits) != 2 {
		t.Fatalf("RateLimits = %#v, want two windows", quota.RateLimits)
	}
	if got := quota.RateLimits[0]; got.Window != "5h" || got.Utilization != 0.69 {
		t.Errorf("5h window = %#v, want utilization 69%% / 100", got)
	}
	if got := quota.RateLimits[1]; got.Window != "7d" || got.Utilization != 0.635 {
		t.Errorf("7d window = %#v, want utilization 63.5%% / 100", got)
	}
	if quota.Balance.Utilization != 0.025 {
		t.Errorf("balance utilization = %v, want 2.5%% / 100", quota.Balance.Utilization)
	}
}
