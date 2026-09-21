// Tests replay of the native-subagent and background-command lifecycle contracts.

package agent

import (
	"slices"
	"testing"
)

func TestNativeSubagentTimeline(t *testing.T) {
	t.Parallel()

	t.Run("single lifecycle preserves reported fields", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		timeline.Apply(&NativeSubagent{
			ID:      "child-1",
			GroupID: "spawn-1",
			Label:   "reviewer",
			Prompt:  "Review the change",
			Status:  NativeSubagentStatusRunning,
		})
		timeline.Apply(&NativeSubagent{
			ID:     "child-1",
			Status: NativeSubagentStatusCompleted,
			Result: "No findings.",
		})

		got := timeline.Subagents()
		if len(got) != 1 || got[0].ID != "child-1" || got[0].GroupID != "spawn-1" || got[0].Label != "reviewer" || got[0].Prompt != "Review the change" || got[0].Status != NativeSubagentStatusCompleted || got[0].Result != "No findings." {
			t.Fatalf("subagents = %#v", got)
		}
	})

	t.Run("concurrent agents retain grouping and independent lifecycles", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		for _, subagent := range []NativeSubagent{
			{ID: "child-a", GroupID: "parallel-1", Status: NativeSubagentStatusRunning},
			{ID: "child-b", GroupID: "parallel-1", Status: NativeSubagentStatusRunning},
			{ID: "child-c", GroupID: "parallel-1", Status: NativeSubagentStatusRunning},
		} {
			timeline.Apply(&subagent)
		}
		timeline.Apply(&NativeSubagent{ID: "child-b", Status: NativeSubagentStatusFailed, Result: "permission denied"})

		got := timeline.Subagents()
		if len(got) != 3 || got[0].GroupID != "parallel-1" || got[1].Status != NativeSubagentStatusFailed || got[2].GroupID != "parallel-1" {
			t.Fatalf("subagents = %#v", got)
		}
		if got[0].Status != NativeSubagentStatusRunning || got[2].Status != NativeSubagentStatusRunning {
			t.Fatalf("subagents = %#v, want child-a and child-c still running", got)
		}
	})

	t.Run("a terminal result replaces an interim one", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		// An asynchronous run reports an artifact reference while it is paused, and
		// its real output when it completes.
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusRunning})
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusPaused, Result: "Output file: /tmp/child.md"})
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusCompleted, Result: "the actual joke"})

		got := timeline.Subagents()
		if len(got) != 1 || got[0].Status != NativeSubagentStatusCompleted || got[0].Result != "the actual joke" {
			t.Fatalf("subagents = %#v, want the completed result", got)
		}
		// Neither an interim observation nor a second terminal report replaces the
		// first terminal outcome.
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusPaused, Result: "Output file: /tmp/child.md"})
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusCompleted, Result: "a noisier repeat"})
		if got := timeline.Subagents(); got[0].Result != "the actual joke" {
			t.Fatalf("result = %q, want the first terminal result to stay", got[0].Result)
		}
	})

	t.Run("Active counts only running cards", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		if got := timeline.Active(); got != 0 {
			t.Fatalf("empty Active() = %d, want 0", got)
		}
		timeline.Apply(&NativeSubagent{ID: "running", Status: NativeSubagentStatusRunning})
		timeline.Apply(&NativeSubagent{ID: "paused", Status: NativeSubagentStatusPaused})
		timeline.Apply(&NativeSubagent{ID: "unknown", Status: NativeSubagentStatusUnknown})
		timeline.Apply(&NativeSubagent{ID: "queued"})
		if got := timeline.Active(); got != 1 {
			t.Fatalf("Active() = %d, want 1: unknown, paused, and absent statuses are not active", got)
		}
		timeline.Apply(&NativeSubagent{ID: "running", Status: NativeSubagentStatusCompleted})
		if got := timeline.Active(); got != 0 {
			t.Fatalf("Active() = %d, want 0 after the running card settled", got)
		}
	})

	t.Run("ActiveBackground counts only running detached cards", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		if got := timeline.ActiveBackground(); got != 0 {
			t.Fatalf("empty ActiveBackground() = %d, want 0", got)
		}
		timeline.Apply(&NativeSubagent{ID: "background", Status: NativeSubagentStatusRunning, Background: true})
		timeline.Apply(&NativeSubagent{ID: "foreground", Status: NativeSubagentStatusRunning})
		timeline.Apply(&NativeSubagent{ID: "idle-background", Status: NativeSubagentStatusPaused, Background: true})
		if got := timeline.Active(); got != 2 {
			t.Fatalf("Active() = %d, want 2 running cards", got)
		}
		if got := timeline.ActiveBackground(); got != 1 {
			t.Fatalf("ActiveBackground() = %d, want 1 detached running card", got)
		}
		// Detachment is monotonic: a later foreground observation cannot clear it,
		// and an early one is upgraded by a later background observation.
		timeline.Apply(&NativeSubagent{ID: "background", Status: NativeSubagentStatusRunning})
		timeline.Apply(&NativeSubagent{ID: "foreground", Status: NativeSubagentStatusRunning, Background: true})
		got := timeline.Subagents()
		if !got[0].Background || !got[1].Background {
			t.Fatalf("cards = %#v, want both detached", got)
		}
		if active := timeline.ActiveBackground(); active != 2 {
			t.Fatalf("ActiveBackground() = %d, want 2 after the upgrade", active)
		}
	})

	t.Run("partial observability remains unknown instead of active", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		timeline.Apply(&NativeSubagent{ID: "child-1", Label: "delegate"})
		timeline.Apply(&NativeSubagent{Label: "uncorrelatable", Status: NativeSubagentStatusRunning})

		got := timeline.Subagents()
		if len(got) != 1 || got[0].Status != NativeSubagentStatusUnknown || got[0].Prompt != "" || got[0].Result != "" {
			t.Fatalf("subagents = %#v", got)
		}
	})

	t.Run("terminal observations are idempotent and cannot reopen", func(t *testing.T) {
		t.Parallel()
		var timeline NativeSubagentTimeline
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusRunning})
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusInterrupted})
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusInterrupted})
		timeline.Apply(&NativeSubagent{ID: "child-1", Status: NativeSubagentStatusRunning})

		got := timeline.Subagents()
		if len(got) != 1 || got[0].Status != NativeSubagentStatusInterrupted {
			t.Fatalf("subagents = %#v, want one interrupted card", got)
		}
	})

	t.Run("compaction and replay do not duplicate cards or stick active", func(t *testing.T) {
		t.Parallel()
		observations := []NativeSubagent{
			{ID: "child-1", GroupID: "spawn-1", Status: NativeSubagentStatusRunning},
			{ID: "child-1", Status: NativeSubagentStatusCompleted, Result: "done"},
			// A compaction boundary replays the same settled lifecycle before
			// newer records. The repeated terminal observation is harmless.
			{ID: "child-1", Status: NativeSubagentStatusCompleted, Result: "done"},
			{ID: "child-2", GroupID: "spawn-2", Status: NativeSubagentStatusRunning},
			{ID: "child-2", Status: NativeSubagentStatusFailed, Result: "failed"},
		}
		var timeline NativeSubagentTimeline
		for _, observation := range observations {
			timeline.Apply(&observation)
		}

		got := timeline.Subagents()
		if len(got) != 2 || got[0].Status != NativeSubagentStatusCompleted || got[1].Status != NativeSubagentStatusFailed {
			t.Fatalf("subagents = %#v, want two terminal cards", got)
		}
	})
}

func TestBackgroundCommandTimeline(t *testing.T) {
	t.Parallel()
	t.Run("EmptyIdentityIgnored", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{Status: BackgroundCommandStatusRunning})
		if got := timeline.Commands(); len(got) != 0 {
			t.Fatalf("Commands() = %#v, want none", got)
		}
	})
	t.Run("FirstObservationOrderPreserved", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{ID: "b", Status: BackgroundCommandStatusRunning})
		timeline.Apply(&BackgroundCommand{ID: "a", Status: BackgroundCommandStatusRunning})
		got := timeline.Commands()
		if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
			t.Fatalf("Commands() = %#v, want b then a", got)
		}
	})
	t.Run("MissingFieldsEnriched", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusRunning})
		code := 3
		timeline.Apply(&BackgroundCommand{ID: "x", Label: "lint", ToolUseID: "toolu_1", OutputRef: "/tmp/out", ExitCode: &code})
		got := timeline.Commands()[0]
		if got.Label != "lint" || got.ToolUseID != "toolu_1" || got.OutputRef != "/tmp/out" || got.ExitCode == nil || *got.ExitCode != 3 {
			t.Fatalf("enriched card = %#v", got)
		}
	})
	t.Run("TerminalIsFrozenAndResultUpgrades", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusRunning, Result: "started"})
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusCompleted, Result: "done (exit code 0)"})
		// A replayed later observation cannot reopen the lifecycle or replace
		// the first terminal report.
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusRunning, Result: "stale interim"})
		got := timeline.Commands()[0]
		if got.Status != BackgroundCommandStatusCompleted {
			t.Errorf("Status = %q, want completed", got.Status)
		}
		if got.Result != "done (exit code 0)" {
			t.Errorf("Result = %q, want the first terminal report", got.Result)
		}
	})
	t.Run("InterimResultUpgradedByTerminal", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusCompleted, Result: "first terminal"})
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusFailed, Result: "second terminal"})
		got := timeline.Commands()[0]
		if got.Result != "first terminal" {
			t.Errorf("Result = %q, want the first terminal report", got.Result)
		}
	})
	t.Run("UnknownStatusNeverInventsTerminal", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatus("weird")})
		got := timeline.Commands()[0]
		if got.Status != BackgroundCommandStatusRunning {
			t.Errorf("Status = %q, want running", got.Status)
		}
	})
	t.Run("ObserveEmitsOnlyChanges", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		if got := timeline.Observe(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusRunning}); len(got) != 1 {
			t.Fatalf("first observe emitted %d messages, want 1", len(got))
		}
		if got := timeline.Observe(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusRunning}); len(got) != 0 {
			t.Fatalf("unchanged observe emitted %d messages, want 0", len(got))
		}
		code := 0
		if got := timeline.Observe(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusCompleted, ExitCode: &code}); len(got) != 1 {
			t.Fatalf("terminal observe emitted %d messages, want 1", len(got))
		}
		if got := timeline.Observe(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusCompleted, ExitCode: &code}); len(got) != 0 {
			t.Fatalf("replayed terminal observe emitted %d messages, want 0", len(got))
		}
	})
	t.Run("CommandsReturnsIndependentSlice", func(t *testing.T) {
		t.Parallel()
		var timeline BackgroundCommandTimeline
		timeline.Apply(&BackgroundCommand{ID: "x", Status: BackgroundCommandStatusRunning})
		got := timeline.Commands()
		got[0].Label = "mutated"
		if timeline.Commands()[0].Label != "" {
			t.Fatal("Commands() exposed the timeline's internal card")
		}
	})
	t.Run("StatusesMatchHouseVocabulary", func(t *testing.T) {
		t.Parallel()
		if !BackgroundCommandStatusCompleted.Terminal() || !BackgroundCommandStatusFailed.Terminal() || !BackgroundCommandStatusInterrupted.Terminal() {
			t.Fatal("terminal statuses must report Terminal()")
		}
		if BackgroundCommandStatusRunning.Terminal() {
			t.Fatal("running must not be terminal")
		}
		for _, status := range []BackgroundCommandStatus{BackgroundCommandStatusRunning, BackgroundCommandStatusCompleted, BackgroundCommandStatusFailed, BackgroundCommandStatusInterrupted} {
			if !slices.Contains([]string{"running", "completed", "failed", "interrupted"}, string(status)) {
				t.Errorf("unexpected status spelling %q", status)
			}
		}
	})
}
