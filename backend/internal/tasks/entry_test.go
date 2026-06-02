// Tests for Entry.

package tasks

import (
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
)

func TestNewEntry(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
		}
		e := NewEntry(tk)
		if e.Task() != tk {
			t.Fatal("Task() returned wrong pointer")
		}
		if e.LoadedTask() != nil {
			t.Error("LoadedTask() should be nil for NewEntry")
		}
		if e.Result() != nil {
			t.Error("Result() should be nil for new entry")
		}
		select {
		case <-e.Done():
			t.Error("Done() should not be closed for new entry")
		default:
		}
	})
}

func TestNewPurgedEntry(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: "test"},
		}
		lt := &task.LoadedTask{TaskID: tk.ID.String()}
		r := &task.Result{State: task.StatePurged}
		e := newPurgedEntry(tk, r, lt)

		if e.Task() != tk {
			t.Fatal("Task() returned wrong pointer")
		}
		if e.LoadedTask() != lt {
			t.Fatal("LoadedTask() returned wrong pointer")
		}
		if e.Result() != r {
			t.Fatal("Result() returned wrong pointer")
		}
		select {
		case <-e.Done():
			// Done is pre-closed for purged entries.
		default:
			t.Error("Done() should be pre-closed for purged entries")
		}
	})
}

func TestEntry(t *testing.T) {
	t.Parallel()
	t.Run("Result", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
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
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			e.CloseDone()
			select {
			case <-e.Done():
			default:
				t.Error("Done() not closed after CloseDone")
			}
		})
		t.Run("reset_reopens", func(t *testing.T) {
			t.Parallel()
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
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

	t.Run("Cleanup", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_exactly_once", func(t *testing.T) {
			t.Parallel()
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			var n int
			e.Cleanup(func() { n++ })
			e.Cleanup(func() { n++ })
			if n != 1 {
				t.Errorf("Cleanup called %d times, want 1", n)
			}
		})
		t.Run("valid_after_reset", func(t *testing.T) {
			t.Parallel()
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
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
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
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
			e := NewEntry(&task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}})
			e.LoadMessagesOnce(func() { t.Error("should not be called without LoadedTask") })
		})
		t.Run("valid_exactly_once", func(t *testing.T) {
			t.Parallel()
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
			lt := &task.LoadedTask{TaskID: tk.ID.String()}
			e := newPurgedEntry(tk, &task.Result{State: task.StatePurged}, lt)
			var n int
			e.LoadMessagesOnce(func() { n++ })
			e.LoadMessagesOnce(func() { n++ })
			if n != 1 {
				t.Errorf("LoadMessagesOnce called %d times, want 1", n)
			}
		})
	})
}
