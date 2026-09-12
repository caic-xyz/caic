// Tests fake/e2e fixture selection for behavioral and visual modes.

//go:build e2e

package main

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
)

func TestFakeAgentBackends(t *testing.T) {
	t.Parallel()

	visual := fakeAgentBackends(true)
	if len(visual) != 1 || visual[harness.Claude] == nil {
		t.Fatalf("visual backends = %#v, want only Claude", visual)
	}

	behavioral := fakeAgentBackends(false)
	if len(behavioral) != 3 {
		t.Fatalf("behavioral backend count = %d, want 3", len(behavioral))
	}
	for _, test := range []struct {
		harness  harness.Name
		provider agent.QuotaProvider
	}{
		{harness: harness.Claude, provider: agent.QuotaProviderClaudeCode},
		{harness: harness.Codex, provider: agent.QuotaProviderCodex},
		{harness: harness.Pi, provider: ""},
	} {
		backend := behavioral[test.harness]
		if backend == nil {
			t.Errorf("behavioral backend %q is missing", test.harness)
			continue
		}
		if got := backend.QuotaProvider(); got != test.provider {
			t.Errorf("backend %q provider = %q, want %q", test.harness, got, test.provider)
		}
	}
}
