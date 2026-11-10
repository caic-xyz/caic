// Tests and benchmarks backend-generated Go Mode notification events.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/auth"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/server/mcpschema"
	taskstate "github.com/caic-xyz/caic/backend/internal/task"
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
	t.Run("CapturedTaskQuotaRecovery", func(t *testing.T) {
		t.Parallel()
		id := ksid.NewID()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			tk := mustNewTask(t, id, agent.Prompt{Text: "Resume task"}, "claude")
			tk.SeedTimelineParts(nil, agenttest.TimedMessages([]agent.Message{&agent.RateLimitMessage{Status: agent.RateLimitStatusRejected, ResetsAt: time.Now().Add(time.Hour), QuotaProvider: agent.QuotaProviderAnthropic, RateLimitType: "fivehour"}}))
			tk.SetState(taskstate.StateWaiting)
			insertTestTask(s, id, tk)
			feed := newNotificationFeed()
			svc := testTaskHandlers(s).taskSvc
			if got := feed.notifications(t.Context(), svc, v1.UsageResp{}, ""); len(got) != 0 {
				t.Fatalf("initial quota baseline=%+v", got)
			}
			time.Sleep(2 * time.Hour)
			got := feed.notifications(t.Context(), svc, v1.UsageResp{}, "")
			if len(got) != 1 || got[0].Title != "Quota available" {
				t.Fatalf("captured quota recovery=%+v", got)
			}
		})
	})
	t.Run("CapturePrecedesReconciliation", func(t *testing.T) {
		t.Parallel()
		id := ksid.NewID()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			svc := testTaskHandlers(s).taskSvc
			feed := newNotificationFeed()
			ctx := auth.NewContext(t.Context(), &auth.User{ID: "owner"})
			tk := mustNewTask(t, id, agent.Prompt{Text: "owned"}, "claude")
			tk.OwnerID = "owner"
			tk.SetState(taskstate.StateRunning)
			insertTestTask(s, id, tk)
			feed.notifications(ctx, svc, v1.UsageResp{}, "")
			results := make(chan []mcpschema.ServiceNotification, 1)
			var workers sync.WaitGroup
			t.Cleanup(workers.Wait)
			// The read is waiting for ownership before it can capture state.
			feed.mu.Lock()
			started := make(chan struct{})
			workers.Go(func() { close(started); results <- feed.notifications(ctx, svc, v1.UsageResp{}, "") })
			<-started
			tk.SetState(taskstate.StateWaiting)
			feed.mu.Unlock()
			synctest.Wait()
			first := <-results
			if len(first) != 1 {
				t.Fatalf("capture missed accepted transition: %+v", first)
			}
			// Replacement changes both incarnation and owner visibility while another
			// read is waiting. It must reconcile absence, not an already captured DTO.
			feed.mu.Lock()
			started = make(chan struct{})
			workers.Go(func() { close(started); results <- feed.notifications(ctx, svc, v1.UsageResp{}, "") })
			<-started
			replacement := mustNewTask(t, id, agent.Prompt{Text: "foreign"}, "claude")
			replacement.OwnerID = "foreign"
			replacement.SetState(taskstate.StateWaiting)
			insertTestTask(s, id, replacement)
			feed.mu.Unlock()
			synctest.Wait()
			if got := <-results; len(got) != 0 {
				t.Fatalf("withdrawn task retained alerts: %+v", got)
			}
			restored := mustNewTask(t, id, agent.Prompt{Text: "restored"}, "claude")
			restored.OwnerID = "owner"
			restored.SetState(taskstate.StateWaiting)
			insertTestTask(s, id, restored)
			if got := feed.notifications(ctx, svc, v1.UsageResp{}, ""); len(got) != 0 {
				t.Fatalf("replacement incarnation was not a quiet baseline: %+v", got)
			}
		})
	})
	t.Run("StaleConcurrentSnapshot", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "attention"}, "claude")
		tk.SetState(taskstate.StateRunning)
		feed := newNotificationFeed()
		capture := func() ([]v1.Task, map[string]taskStateReplay) {
			snap, seq, _ := tk.SnapshotWithStateHistory(math.MaxUint64)
			var metadata v1.Task
			if err := apiconv.PopulateTaskMetadata(&metadata, tk, &snap, time.Now()); err != nil {
				t.Error(err)
				return nil, nil
			}
			return []v1.Task{metadata}, map[string]taskStateReplay{tk.ID.String(): {seq: seq, timelineID: tk.TimelineID()}}
		}
		tasks, replays := capture()
		notificationSnapshot(feed, t.Context(), tasks, replays, v1.UsageResp{}, "")
		tk.SetState(taskstate.StateWaiting)
		captured, release := make(chan struct{}), make(chan struct{})
		result := make(chan []mcpschema.ServiceNotification, 1)
		var workers sync.WaitGroup
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(func() { cancel(); workers.Wait() })
		workers.Go(func() {
			old, oldReplay := capture()
			close(captured)
			select {
			case <-release:
				result <- notificationSnapshot(feed, ctx, old, oldReplay, v1.UsageResp{}, "")
			case <-ctx.Done():
			}
		})
		<-captured
		tk.SetState(taskstate.StateRunning)
		tk.SetState(taskstate.StateWaiting)
		tasks, replays = capture()
		current := notificationSnapshot(feed, t.Context(), tasks, replays, v1.UsageResp{}, "")
		if len(current) != 1 {
			t.Fatalf("latest=%+v", current)
		}
		close(release)
		stale := <-result
		if len(stale) != 1 || stale[0].ID != current[0].ID {
			t.Errorf("stale snapshot manufactured/removed alert: %+v", stale)
		}
		repeated := notificationSnapshot(feed, t.Context(), tasks, replays, v1.UsageResp{}, "")
		if len(repeated) != 1 || repeated[0].ID != current[0].ID {
			t.Errorf("newer repeat manufactured alert: %+v", repeated)
		}
		tk.SetTitle("Updated attention title")
		tasks, replays = capture()
		updated := notificationSnapshot(feed, t.Context(), tasks, replays, v1.UsageResp{}, "")
		if len(updated) != 1 || updated[0].ID != current[0].ID || updated[0].Body != "Updated attention title" {
			t.Errorf("equal-sequence metadata=%+v", updated)
		}
		oldTasks, oldReplays := tasks, replays
		tk.SetState(taskstate.StateRunning)
		tasks, replays = capture()
		if got := notificationSnapshot(feed, t.Context(), tasks, replays, v1.UsageResp{}, ""); len(got) != 0 {
			t.Errorf("withdrawal=%+v", got)
		}
		if got := notificationSnapshot(feed, t.Context(), oldTasks, oldReplays, v1.UsageResp{}, ""); len(got) != 0 {
			t.Errorf("stale snapshot resurrected withdrawn alert: %+v", got)
		}
	})
	t.Run("boundedOwners", func(t *testing.T) {
		t.Parallel()
		feed := newNotificationFeed()
		owner := func(id string) {
			notificationSnapshot(feed, auth.NewContext(t.Context(), &auth.User{ID: id}), nil, nil, v1.UsageResp{}, "")
		}
		owner("recent")
		owner("victim")
		for i := range maxNotificationOwners - 2 {
			owner(fmt.Sprintf("owner-%d", i))
		}
		owner("recent")
		owner("overflow")
		feed.mu.Lock()
		defer feed.mu.Unlock()
		for name, size := range map[string]int{
			"blocked": len(feed.blockedTaskIDs), "states": len(feed.states),
			"generations": len(feed.generations), "events": len(feed.events),
		} {
			if size > maxNotificationOwners {
				t.Fatalf("%s owners=%d, cap=%d", name, size, maxNotificationOwners)
			}
		}
		if _, ok := feed.states["recent"]; !ok {
			t.Fatal("recent owner evicted")
		}
		if _, ok := feed.states["victim"]; ok {
			t.Fatal("least recently used owner retained")
		}
	})
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
				name:     "DSHOfficialDeepSeek",
				task:     v1.Task{Harness: v1.HarnessDsh, ReportedModel: "deepseek-official/deepseek-v4-flash"},
				provider: agent.QuotaProviderDeepSeek,
				want:     true,
			},
			{
				name:     "DSHOtherProviderDoesNotUseDeepSeekQuota",
				task:     v1.Task{Harness: v1.HarnessDsh, ReportedModel: "openrouter/deepseek/deepseek-v4-pro"},
				provider: agent.QuotaProviderDeepSeek,
				want:     false,
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
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatal(got)
		}
		usage.Providers[0].RateLimits[0].Utilization = 0
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatalf("unrelated pool recovery notified Claude task: %#v", got)
		}
		usage.Providers[0].RateLimits[1].Utilization = 1
		notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, "")
		usage.Providers[0].FetchStatus = v1.ProviderFetchStatusStale
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatalf("stale quota claimed recovery: %#v", got)
		}
		usage.Providers[0].FetchStatus = v1.ProviderFetchStatusFresh
		usage.Providers[0].RateLimits[1].Utilization = 0
		task.RequestedModel = ""
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatalf("unknown model claimed recovery: %#v", got)
		}
		task.RequestedModel = "claude-sonnet"
		usage.Providers[0].UnassessedWindows = []v1.UnassessedQuotaWindow{{Group: "Claude and GPT models", Window: "3p-monthly"}}
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatalf("unassessed quota claimed recovery: %#v", got)
		}
		usage.Providers[0].UnassessedWindows = nil
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 1 || got[0].Title != "Quota available" {
			t.Fatalf("pool recovery = %#v", got)
		}
		usage.Providers[0].FetchStatus = v1.ProviderFetchStatusStale
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatalf("unknown quota retained recovery alert: %#v", got)
		}
		usage.Providers[0].FetchStatus = v1.ProviderFetchStatusFresh
		if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, usage, ""); len(got) != 0 {
			t.Fatalf("reobserving availability replayed recovery: %#v", got)
		}
	})

	t.Run("NumberedTask", func(t *testing.T) {
		t.Parallel()
		feed := newNotificationFeed()
		tasks := make([]v1.Task, 4)
		for i := range tasks {
			tasks[i] = v1.Task{ID: ksid.NewID(), Title: "Other task", State: v1.TaskStateRunning}
		}
		tasks[3].Title = "Foreground notification investigation"
		notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")
		tasks[3].State = v1.TaskStateWaiting
		got := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")
		if len(got) != 1 || got[0].Title != "Task #4 is waiting" || got[0].Body != "Foreground notification investigation" {
			t.Fatalf("numbered notification = %#v", got)
		}
	})

	t.Run("CarriesActiveTask", func(t *testing.T) {
		t.Parallel()
		feed := newNotificationFeed()
		tasks := []v1.Task{
			{ID: ksid.NewID(), Title: "Other task", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Focused task", State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Third task", State: v1.TaskStateRunning},
		}
		notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")
		tasks[1].State = v1.TaskStateWaiting
		got := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, tasks[1].ID.String())
		if len(got) != 1 {
			t.Fatalf("notifications = %#v, want one alert", got)
		}
		if got[0].ActiveTaskID != tasks[1].ID.String() || got[0].ActiveTaskNumber != 2 {
			t.Fatalf("active task = %q #%d, want %q #2", got[0].ActiveTaskID, got[0].ActiveTaskNumber, tasks[1].ID)
		}
		data, err := json.Marshal(got[0])
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["activeTaskId"] != tasks[1].ID.String() || decoded["activeTaskNumber"] != float64(2) {
			t.Fatalf("active task JSON = %s", data)
		}
		if got := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, ""); len(got) != 1 || got[0].ActiveTaskID != "" || got[0].ActiveTaskNumber != 0 {
			t.Fatalf("unfocused notifications = %#v, want no active task", got)
		}
	})

	t.Run("AttentionLifecycle", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			state v1.TaskState
			title string
		}{
			{v1.TaskStateWaiting, "Task #1 is waiting"},
			{v1.TaskStateAsking, "Task #1 has a question"},
			{v1.TaskStateCrashed, "Task #1 crashed"},
			{v1.TaskStateFailed, "Task #1 failed"},
			{v1.TaskStateHasPlan, "Task #1 has a plan ready"},
		} {
			t.Run(string(tc.state), func(t *testing.T) {
				t.Parallel()
				feed := newNotificationFeed()
				task := v1.Task{ID: ksid.NewID(), Title: "Review plan", State: tc.state}
				if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, ""); len(got) != 0 {
					t.Fatalf("initial attention state notified: %#v", got)
				}
				task.State = v1.TaskStateRunning
				notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, "")
				task.State = tc.state
				got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, "")
				if len(got) != 1 || got[0].Title != tc.title || got[0].Body != "Review plan" || got[0].ItemID != task.ID.String() || got[0].URL != "/task/@"+task.ID.String() {
					t.Fatalf("attention notification = %#v", got)
				}
				first := got[0]
				if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, ""); len(got) != 1 || got[0].ID != first.ID {
					t.Fatalf("repeated snapshot changed occurrence: %#v", got)
				}
				task.State = v1.TaskStateRunning
				if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, ""); len(got) != 0 {
					t.Fatalf("running task retained alert: %#v", got)
				}
				task.State = tc.state
				if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, ""); len(got) != 1 || got[0].ID == first.ID {
					t.Fatalf("second occurrence = %#v", got)
				}
				if got := notificationSnapshot(feed, t.Context(), nil, nil, v1.UsageResp{}, ""); len(got) != 0 {
					t.Fatalf("deleted task retained alert: %#v", got)
				}
				if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, ""); len(got) != 0 {
					t.Fatalf("reappearing task notified: %#v", got)
				}
			})
		}
	})

	t.Run("AttentionStateChanges", func(t *testing.T) {
		t.Parallel()
		task := v1.Task{ID: ksid.NewID(), Title: "Check changes", State: v1.TaskStateRunning}
		synctest.Test(t, func(t *testing.T) {
			feed := newNotificationFeed()
			ids := make(map[string]struct{})
			for range 2 {
				task.State = v1.TaskStateRunning
				notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, "")
				for _, state := range []v1.TaskState{v1.TaskStateWaiting, v1.TaskStateAsking, v1.TaskStateHasPlan} {
					task.State = state
					got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, "")
					if len(got) != 1 {
						t.Fatalf("changed attention state = %#v", got)
					}
					if _, exists := ids[got[0].ID]; exists {
						t.Fatalf("reused occurrence ID: %q", got[0].ID)
					}
					ids[got[0].ID] = struct{}{}
				}
			}
		})
	})

	t.Run("QuotaRecoveryOccurrences", func(t *testing.T) {
		t.Parallel()
		task := v1.Task{ID: ksid.NewID(), Title: "Resume work", State: v1.TaskStateWaiting}
		synctest.Test(t, func(t *testing.T) {
			feed := newNotificationFeed()
			var previous string
			for range 2 {
				task.RateLimit = v1.TaskRateLimit{Blocked: true, ResetsAt: time.Now().Add(time.Hour)}
				notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, "")
				task.RateLimit = v1.TaskRateLimit{}
				got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, v1.UsageResp{}, "")
				if len(got) != 1 || got[0].ID == previous {
					t.Fatalf("quota recovery occurrence = %#v", got)
				}
				previous = got[0].ID
			}
		})
	})

	t.Run("ReorderingKeepsDestination", func(t *testing.T) {
		t.Parallel()
		feed := newNotificationFeed()
		tasks := []v1.Task{
			{ID: ksid.NewID(), State: v1.TaskStateRunning},
			{ID: ksid.NewID(), Title: "Target task", State: v1.TaskStateRunning},
		}
		notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")
		tasks[1].State = v1.TaskStateWaiting
		first := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")[0]
		tasks[0], tasks[1] = tasks[1], tasks[0]
		tasks[0].Title = "Renamed task"
		got := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")
		if len(got) != 1 || got[0].Title != "Task #1 is waiting" || got[0].Body != "Renamed task" || got[0].ID != first.ID || got[0].URL != first.URL || got[0].ItemID != first.ItemID {
			t.Fatalf("reordered notification = %#v; originally %#v", got, first)
		}
	})

	t.Run("ExpiredAndBounded", func(t *testing.T) {
		t.Parallel()
		feed := newNotificationFeed()
		tasks := make([]v1.Task, 101)
		for i := range tasks {
			tasks[i] = v1.Task{ID: ksid.NewID(), State: v1.TaskStateRunning}
		}
		notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, "")
		for i := range tasks {
			tasks[i].State = v1.TaskStateWaiting
		}
		if got := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, ""); len(got) != 100 {
			t.Fatalf("feed bound = %d", len(got))
		}
		feed.events["local"][50].ExpiresAt = time.Now().Add(-time.Hour)
		if got := notificationSnapshot(feed, t.Context(), tasks, nil, v1.UsageResp{}, ""); len(got) != 99 {
			t.Fatalf("expired alert retained: %d", len(got))
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

	if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, blockedUsage, ""); got == nil || len(got) != 0 {
		t.Fatalf("initial notifications = %#v, want empty array", got)
	}
	got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, availableUsage, "")
	if len(got) != 1 {
		t.Fatalf("notifications = %#v, want one", got)
	}
	if got[0].Title != "Quota available" || got[0].Body != "Build feature can continue." || got[0].ItemID != task.ID.String() || got[0].URL != "/task/@"+task.ID.String() {
		t.Fatalf("notification = %#v", got[0])
	}
	if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, availableUsage, ""); len(got) != 1 {
		t.Fatalf("notifications after repeated availability = %#v, want one retained event", got)
	}
	if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, blockedUsage, ""); len(got) != 0 {
		t.Fatalf("quota blocked again retained recovery: %#v", got)
	}
	notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, availableUsage, "")
	task.State = v1.TaskStateRunning
	if got := notificationSnapshot(feed, t.Context(), []v1.Task{task}, nil, availableUsage, ""); len(got) != 0 {
		t.Fatalf("resumed task retained recovery: %#v", got)
	}
}

func BenchmarkNotificationFeed(b *testing.B) {
	tasks := make([]v1.Task, 100)
	for i := range tasks {
		tasks[i] = v1.Task{ID: ksid.NewID(), Title: "Build feature", State: v1.TaskStateRunning}
	}
	feed := newNotificationFeed()
	notificationSnapshot(feed, b.Context(), tasks, nil, v1.UsageResp{}, "")
	b.ResetTimer()
	for b.Loop() {
		notificationSnapshot(feed, b.Context(), tasks, nil, v1.UsageResp{}, "")
	}
}

func notificationSnapshot(f *notificationFeed, ctx context.Context, tasks []v1.Task, replays map[string]taskStateReplay, usage v1.UsageResp, activeTaskID string) []mcpschema.ServiceNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reconcileLocked(ctx, tasks, replays, usage, activeTaskID)
}
