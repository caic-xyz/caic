// Tests for backend-generated Go Mode notification events.

package server

import (
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestNotificationFeed(t *testing.T) {
	t.Parallel()
	t.Run("MatchesTaskModelProvider", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			name     string
			task     v1.Task
			provider agent.QuotaProvider
			want     bool
		}{
			{
				name:     "OpenCodeDeepSeek",
				task:     v1.Task{Harness: v1.HarnessOpenCode, Model: "deepseek/deepseek-v3"},
				provider: agent.QuotaProviderDeepSeek,
				want:     true,
			},
			{
				name:     "PiXiaomi",
				task:     v1.Task{Harness: v1.HarnessPi, Model: "xiaomi/mimo-v2.5"},
				provider: agent.QuotaProviderXiaomi,
				want:     true,
			},
			{
				name:     "PiCodex",
				task:     v1.Task{Harness: v1.HarnessPi, Model: "codex/gpt-5"},
				provider: agent.QuotaProviderCodex,
				want:     true,
			},
			{
				name:     "PiOpenRouter",
				task:     v1.Task{Harness: v1.HarnessPi, Model: "openrouter/anthropic/claude-opus"},
				provider: agent.QuotaProviderOpenRouter,
				want:     true,
			},
			{
				name:     "AggregatorDoesNotUseUnderlyingModelProvider",
				task:     v1.Task{Harness: v1.HarnessOpenCode, Model: "openrouter/anthropic/claude-opus"},
				provider: agent.QuotaProviderAnthropic,
				want:     false,
			},
			{
				name:     "NoOpenCodeQuotaProvider",
				task:     v1.Task{Harness: v1.HarnessOpenCode, Model: "opencode/deepseek-v4-flash-free"},
				provider: agent.QuotaProvider("opencode"),
				want:     false,
			},
			{
				name:     "NoPiQuotaProvider",
				task:     v1.Task{Harness: v1.HarnessPi, Model: "pi/model"},
				provider: agent.QuotaProvider("pi"),
				want:     false,
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if got := taskUsesProvider(&tt.task, tt.provider); got != tt.want {
					t.Errorf("taskUsesProvider(%q, %q) = %t, want %t", tt.task.Model, tt.provider, got, tt.want)
				}
			})
		}
	})

	feed := newNotificationFeed()
	task := v1.Task{
		ID:      ksid.NewID(),
		Title:   "Build feature",
		State:   v1.TaskStateWaiting,
		Harness: v1.HarnessClaude,
	}
	now := time.Now().UTC()
	blockedUsage := v1.UsageResp{Providers: []v1.ProviderQuota{{
		Provider: v1.QuotaProviderClaudeCode,
		RateLimits: []v1.QuotaRateLimit{{
			UsedPct:  100,
			ResetsAt: now.Add(time.Hour),
		}},
	}}}
	availableUsage := v1.UsageResp{Providers: []v1.ProviderQuota{{
		Provider: v1.QuotaProviderClaudeCode,
		RateLimits: []v1.QuotaRateLimit{{
			UsedPct:  42,
			ResetsAt: now.Add(time.Hour),
		}},
	}}}

	if got := feed.notifications(t.Context(), []v1.Task{task}, blockedUsage); got == nil || len(got) != 0 {
		t.Fatalf("initial notifications = %#v, want empty array", got)
	}
	got := feed.notifications(t.Context(), []v1.Task{task}, availableUsage)
	if len(got) != 1 {
		t.Fatalf("notifications = %#v, want one", got)
	}
	if got[0].Title != "Quota available" || got[0].Body != "Build feature can continue." {
		t.Fatalf("notification = %#v", got[0])
	}
	if got := feed.notifications(t.Context(), []v1.Task{task}, availableUsage); len(got) != 1 {
		t.Fatalf("notifications after repeated availability = %#v, want one retained event", got)
	}
}
