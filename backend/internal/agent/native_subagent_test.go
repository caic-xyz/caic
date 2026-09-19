// Tests replay of the native-subagent lifecycle contract.

package agent

import "testing"

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
