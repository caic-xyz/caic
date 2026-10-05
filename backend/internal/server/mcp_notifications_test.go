// Tests for backend-generated Go Mode notification events.

package server

import (
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestTaskQuotaBlocked(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, tc := range []struct {
		name      string
		requested string
		reported  string
		status    v1.ProviderFetchStatus
		windows   []v1.QuotaRateLimit
		blocked   bool
		known     bool
	}{
		{"Claude ignores Gemini exhaustion", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "gemini-weekly", Utilization: 1, ResetsAt: now.Add(time.Hour)}, {Window: "3p-weekly"}, {Window: "3p-5h"}}, false, true},
		{"Gemini blocked", "gemini-flash", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "gemini-5h", Utilization: 1, ResetsAt: now.Add(time.Hour)}}, true, true},
		{"reported model wins", "claude-sonnet", "gemini-flash", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "gemini-weekly", Utilization: 1, ResetsAt: now.Add(time.Hour)}}, true, true},
		{"GPT OSS uses third-party pool", "gpt-oss-120b", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-weekly"}, {Window: "3p-5h"}}, false, true},
		{"missing model", "", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "gemini-weekly", Utilization: 1, ResetsAt: now.Add(time.Hour)}}, false, false},
		{"unknown model", "future-model", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "gemini-weekly", Utilization: 1, ResetsAt: now.Add(time.Hour)}}, false, false},
		{"missing pool", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "gemini-weekly"}}, false, false},
		{"missing window", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-weekly"}}, false, false},
		{"new bucket", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-future"}}, false, false},
		{"additional exhausted window", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-weekly"}, {Window: "3p-5h"}, {Window: "3p-monthly", Utilization: 1, ResetsAt: now.Add(time.Hour)}}, false, false},
		{"unmapped pool", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-weekly"}, {Window: "3p-5h"}, {Window: "future-pool"}}, false, false},
		{"stale quota", "claude-sonnet", "", v1.ProviderFetchStatusStale, []v1.QuotaRateLimit{{Window: "3p-weekly"}, {Window: "3p-5h"}}, false, false},
		{"expired exhaustion", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-weekly", Utilization: 1, ResetsAt: now.Add(-time.Hour)}, {Window: "3p-5h"}}, false, false},
		{"exhausted without reset", "claude-sonnet", "", v1.ProviderFetchStatusFresh, []v1.QuotaRateLimit{{Window: "3p-weekly", Utilization: 1}}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := v1.Task{Harness: v1.HarnessAntigravity, RequestedModel: tc.requested, ReportedModel: tc.reported}
			usage := v1.UsageResp{Providers: []v1.ProviderQuota{{Provider: v1.QuotaProviderAntigravity, FetchStatus: tc.status, RateLimits: tc.windows}}}
			blocked, known := taskQuotaBlocked(&task, &usage, now)
			if blocked != tc.blocked || known != tc.known {
				t.Fatalf("quota = blocked %t known %t; want %t %t", blocked, known, tc.blocked, tc.known)
			}
		})
	}
}

func TestAntigravityTaskQuotaBlocked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		window v1.UnassessedQuotaWindow
		known  bool
	}{
		{"amount-only new window", v1.UnassessedQuotaWindow{Group: "Claude and GPT models", Window: "3p-monthly"}, false},
		{"unmapped window", v1.UnassessedQuotaWindow{Group: "Future pool", Window: "future-monthly"}, false},
		{"missing ID", v1.UnassessedQuotaWindow{Group: "Claude and GPT models"}, false},
		{"known matching window", v1.UnassessedQuotaWindow{Group: "Claude and GPT models", Window: "3p-weekly"}, false},
		{"known other pool", v1.UnassessedQuotaWindow{Group: "Gemini Models", Window: "gemini-weekly"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := v1.Task{Harness: v1.HarnessAntigravity, RequestedModel: "claude-sonnet"}
			usage := v1.UsageResp{Providers: []v1.ProviderQuota{{
				Provider:          v1.QuotaProviderAntigravity,
				FetchStatus:       v1.ProviderFetchStatusFresh,
				RateLimits:        []v1.QuotaRateLimit{{Window: "3p-weekly"}, {Window: "3p-5h"}},
				UnassessedWindows: []v1.UnassessedQuotaWindow{tc.window},
			}}}
			blocked, known := antigravityTaskQuotaBlocked(&task, &usage, time.Now())
			if blocked || known != tc.known {
				t.Fatalf("quota = blocked %t known %t; want false %t", blocked, known, tc.known)
			}
		})
	}
}

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
				name:     "AntigravitySubscription",
				task:     v1.Task{Harness: v1.HarnessAntigravity, ReportedModel: "claude-sonnet"},
				provider: agent.QuotaProviderAntigravity,
				want:     true,
			},
			{
				name:     "AntigravityDoesNotUseAnthropicAPIQuota",
				task:     v1.Task{Harness: v1.HarnessAntigravity, ReportedModel: "anthropic/claude-sonnet"},
				provider: agent.QuotaProviderAnthropic,
				want:     false,
			},
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

	t.Run("AntigravityPoolRecovery", func(t *testing.T) {
		t.Parallel()
		feed := newNotificationFeed()
		task := v1.Task{ID: ksid.NewID(), Title: "Claude task", State: v1.TaskStateWaiting, Harness: v1.HarnessAntigravity, RequestedModel: "claude-sonnet"}
		usage := v1.UsageResp{Providers: []v1.ProviderQuota{{Provider: v1.QuotaProviderAntigravity, FetchStatus: v1.ProviderFetchStatusFresh, RateLimits: []v1.QuotaRateLimit{{Window: "gemini-weekly", Utilization: 1, ResetsAt: time.Now().Add(time.Hour)}, {Window: "3p-weekly", ResetsAt: time.Now().Add(time.Hour)}, {Window: "3p-5h", ResetsAt: time.Now().Add(time.Hour)}}}}}
		if got := feed.notifications(t.Context(), []v1.Task{task}, usage); len(got) != 0 {
			t.Fatal(got)
		}
		usage.Providers[0].RateLimits[0].Utilization = 0
		if got := feed.notifications(t.Context(), []v1.Task{task}, usage); len(got) != 0 {
			t.Fatalf("unrelated pool recovery notified Claude task: %#v", got)
		}
		usage.Providers[0].RateLimits[1].Utilization = 1
		feed.notifications(t.Context(), []v1.Task{task}, usage)
		usage.Providers[0].FetchStatus = v1.ProviderFetchStatusStale
		if got := feed.notifications(t.Context(), []v1.Task{task}, usage); len(got) != 0 {
			t.Fatalf("stale quota claimed recovery: %#v", got)
		}
		usage.Providers[0].FetchStatus = v1.ProviderFetchStatusFresh
		usage.Providers[0].RateLimits[1].Utilization = 0
		task.RequestedModel = ""
		if got := feed.notifications(t.Context(), []v1.Task{task}, usage); len(got) != 0 {
			t.Fatalf("unknown model claimed recovery: %#v", got)
		}
		task.RequestedModel = "claude-sonnet"
		usage.Providers[0].UnassessedWindows = []v1.UnassessedQuotaWindow{{Group: "Claude and GPT models", Window: "3p-monthly"}}
		if got := feed.notifications(t.Context(), []v1.Task{task}, usage); len(got) != 0 {
			t.Fatalf("unassessed quota claimed recovery: %#v", got)
		}
		usage.Providers[0].UnassessedWindows = nil
		if got := feed.notifications(t.Context(), []v1.Task{task}, usage); len(got) != 1 || got[0].Title != "Quota available" {
			t.Fatalf("pool recovery = %#v", got)
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
			Utilization: 1,
			ResetsAt:    now.Add(time.Hour),
		}},
	}}}
	availableUsage := v1.UsageResp{Providers: []v1.ProviderQuota{{
		Provider: v1.QuotaProviderClaudeCode,
		RateLimits: []v1.QuotaRateLimit{{
			Utilization: 0.42,
			ResetsAt:    now.Add(time.Hour),
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
