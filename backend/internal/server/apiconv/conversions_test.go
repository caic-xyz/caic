// Tests conversions between backend domain values and API v1 DTOs.

package apiconv

import (
	"reflect"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/maruel/ksid"
)

func mustNewTask(t testing.TB, id ksid.ID, prompt agent.Prompt) *task.Task {
	tk, err := task.NewTask(id, prompt, harness.Claude, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestAgentHarness(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		harness v1.Harness
		want    harness.Name
	}{
		{v1.HarnessClaude, harness.Claude},
		{v1.HarnessCodex, harness.Codex},
		{v1.HarnessOpenCode, harness.OpenCode},
		{v1.HarnessPi, harness.Pi},
	} {
		got, err := AgentHarness(test.harness)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("AgentHarness(%q) = %q, want %q", test.harness, got, test.want)
		}
	}
	if _, err := AgentHarness(v1.Harness("other")); err == nil {
		t.Error("AgentHarness(other) error = nil, want error")
	}
}

func TestForgeCheck(t *testing.T) {
	t.Parallel()

	queuedAt := time.Date(2026, time.June, 1, 10, 30, 0, 0, time.UTC)
	check := &forge.Check{
		Name:        "test",
		Owner:       "caic-xyz",
		Repo:        "caic",
		RunID:       12,
		JobID:       34,
		Status:      forge.CheckRunStatusCompleted,
		Conclusion:  forge.CheckRunConclusionSuccess,
		QueuedAt:    queuedAt,
		StartedAt:   queuedAt.Add(time.Minute),
		CompletedAt: queuedAt.Add(3 * time.Minute),
	}
	want := v1.ForgeCheck{
		Name:        "test",
		Owner:       "caic-xyz",
		Repo:        "caic",
		RunID:       12,
		JobID:       34,
		Status:      v1.CheckStatusCompleted,
		Conclusion:  v1.CheckConclusionSuccess,
		QueuedAt:    queuedAt,
		StartedAt:   queuedAt.Add(time.Minute),
		CompletedAt: queuedAt.Add(3 * time.Minute),
	}
	got, err := ForgeCheck(check)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ForgeCheck() = %#v, want %#v", got, want)
	}
	check.Status = forge.CheckRunStatus("other")
	if _, err := ForgeCheck(check); err == nil {
		t.Error("ForgeCheck(other status) error = nil, want error")
	}
}

func TestCIStatus(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		status forge.CIStatus
		want   v1.CIStatus
	}{
		{forge.CIStatusNone, ""},
		{forge.CIStatusPending, v1.CIStatusPending},
		{forge.CIStatusSuccess, v1.CIStatusSuccess},
		{forge.CIStatusFailure, v1.CIStatusFailure},
	} {
		got, err := CIStatus(test.status)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("CIStatus(%q) = %q, want %q", test.status, got, test.want)
		}
	}
	if _, err := CIStatus(forge.CIStatus("other")); err == nil {
		t.Error("CIStatus(other) error = nil, want error")
	}
}

func TestDiffStat(t *testing.T) {
	t.Parallel()

	if got := DiffStat(nil); got != nil {
		t.Errorf("DiffStat(nil) = %#v, want nil", got)
	}
	diff := agent.DiffStat{{Path: "main.go", Added: 3, Deleted: 1, Binary: true}}
	want := v1.DiffStat{{Path: "main.go", Added: 3, Deleted: 1, Binary: true}}
	if got := DiffStat(diff); !reflect.DeepEqual(got, want) {
		t.Errorf("DiffStat() = %#v, want %#v", got, want)
	}
}

func TestForgePRState(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		state forge.PRState
		want  v1.ForgePRState
	}{
		{forge.PRStateOpen, v1.ForgePRStateOpen},
		{forge.PRStateClosed, v1.ForgePRStateClosed},
		{forge.PRStateMerged, v1.ForgePRStateMerged},
		{"", ""},
	} {
		got, err := ForgePRState(test.state)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("ForgePRState(%q) = %q, want %q", test.state, got, test.want)
		}
	}
	if _, err := ForgePRState(forge.PRState("other")); err == nil {
		t.Error("ForgePRState(other) error = nil, want error")
	}
}

func TestHarness(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		harness harness.Name
		want    v1.Harness
	}{
		{harness.Claude, v1.HarnessClaude},
		{harness.Codex, v1.HarnessCodex},
		{harness.OpenCode, v1.HarnessOpenCode},
		{harness.Pi, v1.HarnessPi},
	} {
		got, err := Harness(test.harness)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("Harness(%q) = %q, want %q", test.harness, got, test.want)
		}
	}
	if _, err := Harness(harness.Name("other")); err == nil {
		t.Error("Harness(other) error = nil, want error")
	}
}

func TestParseHarness(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		want v1.Harness
	}{
		{string(harness.Claude), v1.HarnessClaude},
		{string(harness.Codex), v1.HarnessCodex},
		{string(harness.OpenCode), v1.HarnessOpenCode},
		{string(harness.Pi), v1.HarnessPi},
	} {
		got, err := ParseHarness(test.name)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("ParseHarness(%q) = %q, want %q", test.name, got, test.want)
		}
	}
	if _, err := ParseHarness("other"); err == nil {
		t.Error("ParseHarness(other) error = nil, want error")
	}
}

func TestRepoForge(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		forge forge.Kind
		want  v1.Forge
	}{
		{"", ""},
		{forge.KindGitHub, v1.ForgeGitHub},
		{forge.KindGitLab, v1.ForgeGitLab},
	} {
		got, err := RepoForge(test.forge)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("RepoForge(%q) = %q, want %q", test.forge, got, test.want)
		}
	}
	if _, err := RepoForge(forge.Kind("other")); err == nil {
		t.Error("RepoForge(other) error = nil, want error")
	}
}

func TestPromptToAgent(t *testing.T) {
	t.Parallel()

	prompt := v1.Prompt{
		Text: "Explain this screenshot",
		Images: []v1.ImageData{
			{MediaType: "image/png", Data: "iVBORw0KGgo="},
		},
	}
	want := agent.Prompt{
		Text: "Explain this screenshot",
		Images: []agent.ImageData{
			{MediaType: "image/png", Data: "iVBORw0KGgo="},
		},
	}
	if got := PromptToAgent(prompt); !reflect.DeepEqual(got, want) {
		t.Errorf("PromptToAgent() = %#v, want %#v", got, want)
	}
}

func TestSafetyIssues(t *testing.T) {
	t.Parallel()

	if got := SafetyIssues(nil); got != nil {
		t.Errorf("SafetyIssues(nil) = %#v, want nil", got)
	}
	issues := []repo.SafetyIssue{{File: "secret.env", Kind: "secret", Detail: "possible credential"}}
	want := []v1.SafetyIssue{{File: "secret.env", Kind: "secret", Detail: "possible credential"}}
	if got := SafetyIssues(issues); !reflect.DeepEqual(got, want) {
		t.Errorf("SafetyIssues() = %#v, want %#v", got, want)
	}
}

func TestStatsEvent(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, time.March, 20, 10, 30, 0, 0, time.UTC)
	stats := &runtime.Stats{Ts: ts, CPUPerc: 12.5, MemUsed: 100, MemLimit: 200, MemPerc: 50, NetRx: 300, NetTx: 400, BlockRead: 500, BlockWrite: 600, DiskUsed: 700}
	want := v1.EventMessage{
		Kind: v1.EventKindStats,
		Ts:   ts.UnixMilli(),
		Stats: &v1.EventStats{
			Ts: ts.UnixMilli(), CPUPerc: 12.5, MemUsed: 100, MemLimit: 200,
			MemPerc: 50, NetRx: 300, NetTx: 400, BlockRead: 500, BlockWrite: 600, DiskUsed: 700,
		},
	}
	if got := StatsEvent(stats); !reflect.DeepEqual(got, want) {
		t.Errorf("StatsEvent() = %#v, want %#v", got, want)
	}
}

func TestTaskState(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		state taskslog.State
		want  v1.TaskState
	}{
		{taskslog.StatePending, v1.TaskStatePending},
		{taskslog.StateBranching, v1.TaskStateBranching},
		{taskslog.StateProvisioning, v1.TaskStateProvisioning},
		{taskslog.StateStarting, v1.TaskStateStarting},
		{taskslog.StateRunning, v1.TaskStateRunning},
		{taskslog.StateWaiting, v1.TaskStateWaiting},
		{taskslog.StateAsking, v1.TaskStateAsking},
		{taskslog.StateHasPlan, v1.TaskStateHasPlan},
		{taskslog.StatePulling, v1.TaskStatePulling},
		{taskslog.StatePushing, v1.TaskStatePushing},
		{taskslog.StateStopping, v1.TaskStateStopping},
		{taskslog.StateStopped, v1.TaskStateStopped},
		{taskslog.StatePurging, v1.TaskStatePurging},
		{taskslog.StateCrashed, v1.TaskStateCrashed},
		{taskslog.StateFailed, v1.TaskStateFailed},
		{taskslog.StatePurged, v1.TaskStatePurged},
	} {
		got, err := TaskState(test.state)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("TaskState(%q) = %q, want %q", test.state, got, test.want)
		}
	}
	if _, err := TaskState(taskslog.State("bogus")); err == nil {
		t.Error("TaskState(bogus) error = nil, want error")
	}
}

func TestTask(t *testing.T) {
	t.Parallel()
	t.Run("OverageDoesNotBlock", func(t *testing.T) {
		t.Parallel()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"})
		tk.SetState(taskslog.StatePending)
		tk.RestoreMessages([]agent.Message{&agent.RateLimitMessage{
			Status:         "rejected",
			ResetsAt:       time.Now().Add(time.Hour),
			RateLimitType:  "five_hour",
			IsUsingOverage: true,
		}})

		got, err := Task(&TaskInput{Task: tk, Snapshot: tk.Snapshot()})
		if err != nil {
			t.Fatal(err)
		}
		if got.RateLimit.Blocked {
			t.Error("RateLimit.Blocked = true while overage is active")
		}
	})

	t.Run("RetainsBlockAcrossQuotaWindows", func(t *testing.T) {
		t.Parallel()
		now := time.Now()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"})
		tk.SetState(taskslog.StatePending)
		tk.RestoreMessages([]agent.Message{
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusRejected,
				ResetsAt:      now.Add(time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "5h",
			},
			&agent.RateLimitMessage{
				Status:        agent.RateLimitStatusAllowedWarning,
				ResetsAt:      now.Add(7 * 24 * time.Hour),
				QuotaProvider: agent.QuotaProviderClaudeCode,
				QuotaWindow:   "7d",
			},
		})

		gotTask, err := Task(&TaskInput{Task: tk, Snapshot: tk.Snapshot()})
		if err != nil {
			t.Fatal(err)
		}
		got := gotTask.RateLimit
		if !got.Blocked || got.Window != "5h" {
			t.Errorf("RateLimit = %#v, want active 5h block", got)
		}
	})
	t.Run("IncludesForkOrigin", func(t *testing.T) {
		t.Parallel()
		parentID := ksid.NewID()
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"})
		tk.ForkedFromTaskID = parentID
		tk.SetState(taskslog.StatePending)

		got, err := Task(&TaskInput{Task: tk, Snapshot: tk.Snapshot()})
		if err != nil {
			t.Fatal(err)
		}
		if got.ForkedFromTaskID != parentID {
			t.Errorf("ForkedFromTaskID = %s, want %s", got.ForkedFromTaskID, parentID)
		}
	})
}

func TestProcessInfos(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, time.March, 20, 10, 30, 0, 0, time.UTC)
	got := ProcessInfos([]runtime.ProcessInfo{{
		PID:       42,
		PPID:      1,
		PGRP:      42,
		User:      "user",
		State:     "S",
		Priority:  20,
		Nice:      5,
		Threads:   3,
		CPU:       1.5,
		Mem:       2.5,
		RSSBytes:  2_097_152,
		CPUTime:   3 * time.Second,
		StartedAt: startedAt,
		Command:   "agent run",
	}})
	want := []v1.ProcessInfo{{
		PID:       42,
		PPID:      1,
		PGRP:      42,
		User:      "user",
		State:     "S",
		Priority:  20,
		Nice:      5,
		Threads:   3,
		CPU:       1.5,
		Mem:       2.5,
		RSSBytes:  2_097_152,
		CPUTime:   3 * time.Second,
		StartedAt: startedAt,
		Command:   "agent run",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProcessInfos() = %#v, want %#v", got, want)
	}
}

func TestProviderQuota(t *testing.T) {
	t.Parallel()

	resetsAt := time.Date(2026, time.June, 1, 10, 30, 0, 0, time.UTC)
	got, err := ProviderQuota(&usage.ProviderQuota{
		Provider: agent.QuotaProviderAnthropic,
		Label:    "Anthropic",
		AuthKind: usage.AuthKindOAuth,
		RateLimits: []usage.QuotaRateLimit{
			{Window: "5h", UsedPct: 42.5, ResetsAt: resetsAt},
		},
		Balance: usage.QuotaBalance{
			Currency: "USD",
			Total:    12.75,
			Granted:  2.25,
			ToppedUp: 10.5,
		},
		ExtraUsage: usage.QuotaExtraUsage{
			Currency:     "USD",
			IsEnabled:    true,
			UsedCredits:  3.5,
			MonthlyLimit: 25,
			UsedPct:      14,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := v1.ProviderQuota{
		Provider: v1.QuotaProviderAnthropic,
		Label:    "Anthropic",
		AuthKind: v1.ProviderAuthKindOAuth,
		RateLimits: []v1.QuotaRateLimit{
			{Window: "5h", UsedPct: 42.5, ResetsAt: resetsAt},
		},
		Balance: v1.QuotaBalance{
			Currency: "USD",
			Total:    12.75,
			Granted:  2.25,
			ToppedUp: 10.5,
		},
		ExtraUsage: v1.QuotaExtraUsage{
			Currency:     "USD",
			IsEnabled:    true,
			UsedCredits:  3.5,
			MonthlyLimit: 25,
			UsedPct:      14,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProviderQuota() = %#v, want %#v", got, want)
	}
	if _, err := ProviderQuota(&usage.ProviderQuota{Provider: agent.QuotaProvider("other"), AuthKind: usage.AuthKindOAuth}); err == nil {
		t.Error("ProviderQuota(other provider) error = nil, want error")
	}
}
