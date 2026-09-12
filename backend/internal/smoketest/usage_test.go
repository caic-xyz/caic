// Tests deterministic usage-provider fixtures for smoke and e2e tests.

package smoketest

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func TestUsageFetchers(t *testing.T) {
	t.Parallel()

	visual := UsageFetchers(true)
	if len(visual) != 2 {
		t.Fatalf("visual provider count = %d, want 2", len(visual))
	}
	if visual[0].Provider() != agent.QuotaProviderAnthropic || visual[1].Provider() != agent.QuotaProviderCodex {
		t.Errorf("visual providers = [%q, %q], want [anthropic, codex]", visual[0].Provider(), visual[1].Provider())
	}

	behavioral := UsageFetchers(false)
	if len(behavioral) != 3 {
		t.Fatalf("behavioral provider count = %d, want 3", len(behavioral))
	}
	if behavioral[0].Provider() != agent.QuotaProviderClaudeCode {
		t.Errorf("first behavioral provider = %q, want claudecode", behavioral[0].Provider())
	}
	claude := behavioral[0].Get(t.Context())
	anthropic := behavioral[1].Get(t.Context())
	if claude.RateLimits[0].UsedPct == anthropic.RateLimits[0].UsedPct ||
		claude.RateLimits[1].UsedPct == anthropic.RateLimits[1].UsedPct {
		t.Errorf("Claude Code percentages = %#v, want values distinct from Anthropic %#v", claude.RateLimits, anthropic.RateLimits)
	}
}
