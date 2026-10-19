// Tests for Entry.

package taskmgr

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func mustNewTask(t testing.TB, id ksid.ID, prompt agent.Prompt, h harness.Name, model string) *task.Task {
	tk, err := task.NewTask(id, prompt, h, model, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func newTestEntry(t *testing.T, tk *task.Task) *Entry {
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	return m.NewEntry(tk, nil)
}

func newTestPurgedEntry(t *testing.T, tk *task.Task, r *taskslog.Result, lt *taskslog.LoadedTask) *Entry {
	m := newTestManager(t, Config{ServerCtx: t.Context()})
	e := m.NewEntry(tk, lt)
	e.Finish(r)
	return e
}

func TestEntry(t *testing.T) {
	t.Parallel()
	t.Run("PendingInput", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
		tk.SetState(taskslog.StateRunning)
		e := m.NewEntry(tk, nil)
		m.Insert(tk.ID, e)
		if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "next"}); err != nil {
			t.Fatal(err)
		}
		pending := e.PendingInput()
		m.inputMu.Lock()
		read := make(chan bool, 1)
		var workers sync.WaitGroup
		workers.Go(func() {
			q := e.PendingInput()
			_, found := e.PendingInputImage(pending.ID, 0)
			read <- q == pending && !found
		})
		select {
		case ok := <-read:
			if !ok {
				t.Error("pending input changed while persistence was locked")
			}
		case <-time.After(time.Second):
			t.Error("pending input read blocked on manager persistence lock")
		}
		m.inputMu.Unlock()
		workers.Wait()
	})
	t.Run("clearResult", func(t *testing.T) {
		t.Parallel()
		for _, replaced := range []bool{false, true} {
			t.Run(fmt.Sprintf("new_result_%t", replaced), func(t *testing.T) {
				t.Parallel()
				e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
				old := &taskslog.Result{State: taskslog.StateFailed, Err: errors.New("old failure")}
				e.Finish(old)
				newResult := &taskslog.Result{State: taskslog.StateCrashed, Err: errors.New("new failure")}
				if replaced {
					e.Finish(newResult)
				}
				e.clearResult(old)
				if replaced {
					if e.Result() != newResult {
						t.Fatal("reconnect erased a newer failure")
					}
					select {
					case <-e.Done():
					default:
						t.Fatal("new failure lost terminal state")
					}
				} else {
					if e.Result() != nil {
						t.Fatal("stale failure was not cleared")
					}
					select {
					case <-e.Done():
						t.Fatal("reconnected task is terminal")
					default:
					}
				}
			})
		}
	})
	t.Run("Result", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_initially_nil", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			if e.Result() != nil {
				t.Error("Result() should be nil initially")
			}
		})
	})

	t.Run("LogPath", func(t *testing.T) {
		t.Parallel()
		e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
		if e.LogPath.Get() != "" {
			t.Errorf("LogPath() = %q, want empty", e.LogPath.Get())
		}
		e.LogPath.Set("/tmp/task.jsonl")
		if e.LogPath.Get() != "/tmp/task.jsonl" {
			t.Errorf("LogPath() = %q, want /tmp/task.jsonl", e.LogPath.Get())
		}
	})

	t.Run("Done", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			e.Finish(&taskslog.Result{State: taskslog.StateFailed})
			select {
			case <-e.Done():
			default:
				t.Error("Done() not closed after Finish")
			}
		})
		t.Run("reset_reopens", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			e.Finish(&taskslog.Result{State: taskslog.StateFailed})
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
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			r := &taskslog.Result{State: taskslog.StateFailed}
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
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			first := &taskslog.Result{State: taskslog.StateCrashed}
			second := &taskslog.Result{State: taskslog.StatePurged}
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
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			var n int
			e.Cleanup(func() { n++ })
			e.Cleanup(func() { n++ })
			if n != 1 {
				t.Errorf("Cleanup called %d times, want 1", n)
			}
		})
		t.Run("valid_after_reset", func(t *testing.T) {
			t.Parallel()
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
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
			e := newTestEntry(t, mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "", ""))
			if e.MonitorBranch() != "" {
				t.Error("MonitorBranch() should be empty initially")
			}
			e.SetMonitorBranch("caic-main")
			if e.MonitorBranch() != "caic-main" {
				t.Errorf("MonitorBranch() = %q, want %q", e.MonitorBranch(), "caic-main")
			}
		})
	})
}
