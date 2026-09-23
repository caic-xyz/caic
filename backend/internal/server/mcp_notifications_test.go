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
				task:     v1.Task{Harness: v1.HarnessOpenCode, ReportedModel: "deepseek/deepseek-v3"},
				provider: agent.QuotaProviderDeepSeek,
				want:     true,
			},
			{
				name:     "PiXiaomi",
				task:     v1.Task{Harness: v1.HarnessPi, ReportedModel: "xiaomi/mimo-v2.5"},
				provider: agent.QuotaProviderXiaomi,
				want:     true,
			},
			{
				name:     "PiCodex",
				task:     v1.Task{Harness: v1.HarnessPi, ReportedModel: "codex/gpt-5"},
				provider: agent.QuotaProviderCodex,
				want:     true,
			},
			{
				name:     "PiOpenRouter",
				task:     v1.Task{Harness: v1.HarnessPi, ReportedModel: "openrouter/anthropic/claude-opus"},
				provider: agent.QuotaProviderOpenRouter,
				want:     true,
			},
			{
				name:     "AggregatorDoesNotUseUnderlyingModelProvider",
				task:     v1.Task{Harness: v1.HarnessOpenCode, ReportedModel: "openrouter/anthropic/claude-opus"},
				provider: agent.QuotaProviderAnthropic,
				want:     false,
			},
			{
				name:     "NoOpenCodeQuotaProvider",
				task:     v1.Task{Harness: v1.HarnessOpenCode, ReportedModel: "opencode/deepseek-v4-flash-free"},
				provider: agent.QuotaProvider("opencode"),
				want:     false,
			},
			{
				name:     "NoPiQuotaProvider",
				task:     v1.Task{Harness: v1.HarnessPi, ReportedModel: "pi/model"},
				provider: agent.QuotaProvider("pi"),
				want:     false,
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				if got := taskUsesProvider(&tt.task, tt.provider); got != tt.want {
					t.Errorf("taskUsesProvider(%q, %q) = %t, want %t", tt.task.ReportedModel, tt.provider, got, tt.want)
				}
			})
		}
	})

	t.Run("ReadyTransitions", func(t *testing.T) {
		t.Parallel()
		for _, state := range []v1.TaskState{v1.TaskStateWaiting, v1.TaskStateAsking, v1.TaskStateHasPlan} {
			feed := newNotificationFeed()
			task := v1.Task{ID: ksid.NewID(), Title: "Review plan", State: v1.TaskStateRunning}
			if got := feed.notifications(t.Context(), []v1.Task{task}, v1.UsageResp{}); len(got) != 0 {
				t.Fatalf("initial notifications = %#v", got)
			}
			task.State = state
			got := feed.notifications(t.Context(), []v1.Task{task}, v1.UsageResp{})
			if len(got) != 1 || got[0].Title != "Task ready" || got[0].Body != "Review plan needs attention." {
				t.Fatalf("ready notification for %s = %#v", state, got)
			}
			if got := feed.notifications(t.Context(), []v1.Task{task}, v1.UsageResp{}); len(got) != 1 {
				t.Fatalf("duplicate ready notification for %s = %#v", state, got)
			}
			task.State = v1.TaskStateRunning
			feed.notifications(t.Context(), []v1.Task{task}, v1.UsageResp{})
			task.State = state
			if got := feed.notifications(t.Context(), []v1.Task{task}, v1.UsageResp{}); len(got) != 2 {
				t.Fatalf("second ready transition for %s = %#v", state, got)
			}
			feed.notifications(t.Context(), nil, v1.UsageResp{})
			// A task absent from the previous snapshot has no transition to replay.
			if got := feed.notifications(t.Context(), []v1.Task{task}, v1.UsageResp{}); len(got) != 2 {
				t.Fatalf("reappearing task for %s = %#v", state, got)
			}
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
