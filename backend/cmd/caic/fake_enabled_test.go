// Tests fake/e2e harness identities, quota groups, and model-refresh capabilities.

//go:build e2e

package main

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestFakeAgentBackends(t *testing.T) {
	t.Parallel()

	backends := fakeAgentBackends()
	if len(backends) != 4 {
		t.Fatalf("fake backend count = %d, want 4", len(backends))
	}
	for _, test := range []struct {
		harness  harness.Name
		provider agent.QuotaProvider
	}{
		{harness: harness.Antigravity, provider: ""},
		{harness: harness.Claude, provider: agent.QuotaProviderClaudeCode},
		{harness: harness.Codex, provider: agent.QuotaProviderCodex},
		{harness: harness.Pi, provider: ""},
	} {
		backend := backends[test.harness]
		if backend == nil {
			t.Errorf("fake backend %q is missing", test.harness)
			continue
		}
		if got := backend.QuotaProvider(); got != test.provider {
			t.Errorf("backend %q provider = %q, want %q", test.harness, got, test.provider)
		}
		_, refreshable := backend.(agent.ModelFetcher)
		if want := test.harness != harness.Claude; refreshable != want {
			t.Errorf("backend %q supports refresh = %t, want %t", test.harness, refreshable, want)
		}
	}
}
