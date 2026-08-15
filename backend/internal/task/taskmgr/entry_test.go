// Tests for Entry.

package taskmgr

import (
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
)

func newTestEntry(t *testing.T, tk *task.Task) *Entry {
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	return m.NewEntry(tk, nil)
}

func newTestPurgedEntry(t *testing.T, tk *task.Task, r *task.Result, lt *task.LoadedTask) *Entry {
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	e := m.NewEntry(tk, lt)
	e.Finish(r)
	return e
}

func TestEntry(t *testing.T) {
	t.Parallel()
	t.Run("Result", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			if e.Result() != nil {
				t.Error("Result() should be nil initially")
			}
			r := &task.Result{State: task.StateFailed}
			e.SetResult(r)
			if e.Result() != r {
				t.Error("Result() returned wrong pointer after SetResult")
			}
		})
	})

	t.Run("Done", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			e.CloseDone()
			select {
			case <-e.Done():
			default:
				t.Error("Done() not closed after CloseDone")
			}
		})
		t.Run("reset_reopens", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			e.CloseDone()
			e.Reset()
			select {
			case <-e.Done():
				t.Error("Done() should not be closed after Reset")
			default:
			}
			if e.Result() != nil {
				t.Error("Result() should be nil after Reset")
			}
		})
	})

	t.Run("Finish", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_closes_and_sets_result", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			r := &task.Result{State: task.StateFailed}
			e.Finish(r)
			select {
			case <-e.Done():
			default:
				t.Error("Done() not closed after Finish")
			}
			if e.Result() != r {
				t.Error("Result() returned wrong pointer after Finish")
			}
		})
		t.Run("valid_after_finished", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			first := &task.Result{State: task.StateCrashed}
			second := &task.Result{State: task.StatePurged}
			e.Finish(first)
			e.Finish(second)
			if e.Result() != second {
				t.Error("Result() should contain the latest result after a second Finish")
			}
			select {
			case <-e.Done():
			default:
				t.Error("Done() should stay closed after second Finish")
			}
		})
	})

	t.Run("Cleanup", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_exactly_once", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			var n int
			e.Cleanup(func() { n++ })
			e.Cleanup(func() { n++ })
			if n != 1 {
				t.Errorf("Cleanup called %d times, want 1", n)
			}
		})
		t.Run("valid_after_reset", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			var n int
			e.Cleanup(func() { n++ })
			e.Reset()
			e.Cleanup(func() { n++ })
			if n != 2 {
				t.Errorf("Cleanup called %d times after Reset, want 2", n)
			}
		})
	})

	t.Run("MonitorBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			if e.MonitorBranch() != "" {
				t.Error("MonitorBranch() should be empty initially")
			}
			e.SetMonitorBranch("caic-main")
			if e.MonitorBranch() != "caic-main" {
				t.Errorf("MonitorBranch() = %q, want %q", e.MonitorBranch(), "caic-main")
			}
		})
	})

	t.Run("LoadMessagesOnce", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_no_loaded_task", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			e.LoadMessagesOnce(func() { t.Error("should not be called without LoadedTask") })
		})
		t.Run("valid_exactly_once", func(t *testing.T) {
			t.Parallel()
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
			lt := &task.LoadedTask{TaskID: tk.ID.String()}
			e := newTestPurgedEntry(t, tk, &task.Result{State: task.StatePurged}, lt)
			var n int
			e.LoadMessagesOnce(func() { n++ })
			e.LoadMessagesOnce(func() { n++ })
			if n != 1 {
				t.Errorf("LoadMessagesOnce called %d times, want 1", n)
			}
		})
	})
}
