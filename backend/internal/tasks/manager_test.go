// Tests for Manager and standalone helpers.

package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/mdruntime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/tasktest"
)

type fakeRelayReader struct {
	statusFn   func(context.Context, runtime.ConnectionTarget) (bool, string, error)
	readTailFn func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error)
	readLogFn  func(context.Context, runtime.ConnectionTarget, int) string
}

func (f fakeRelayReader) Status(ctx context.Context, target runtime.ConnectionTarget) (alive bool, diag string, err error) {
	return f.statusFn(ctx, target)
}

func (f fakeRelayReader) ReadTail(ctx context.Context, target runtime.ConnectionTarget, parseFn func([]byte) ([]agent.Message, error), maxBytes int64) (msgs []agent.Message, size int64, err error) {
	return f.readTailFn(ctx, target, parseFn, maxBytes)
}

func (f fakeRelayReader) ReadLog(ctx context.Context, target runtime.ConnectionTarget, maxBytes int) string {
	return f.readLogFn(ctx, target, maxBytes)
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		cfg := Config{ServerCtx: t.Context()}
		m := New(cfg)
		if m == nil {
			t.Fatal("New returned nil")
		}
		if m.Len() != 0 {
			t.Errorf("Len() = %d after New, want 0", m.Len())
		}
		r, ok := m.Runner("")
		if !ok {
			t.Fatal("no-repo runner not registered")
		}
		if r == nil {
			t.Fatal("no-repo runner is nil")
		}
		select {
		case <-m.Changed():
			t.Error("Changed() should not be closed initially")
		default:
		}
	})
	t.Run("no-repo runner is fully constructed", func(t *testing.T) {
		t.Parallel()
		cfg := Config{
			ServerCtx:  t.Context(),
			LogDir:     "/tmp/logs",
			CacheDir:   "/tmp/cache",
			Backend:    &mdruntime.Backend{},
			Backends:   map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			HarnessEnv: map[string][]string{string(harness.Codex): {"CODEX_HOME=/tmp/codex"}},
		}
		m := New(cfg)
		r, ok := m.Runner("")
		if !ok {
			t.Fatal("no-repo runner not registered")
		}
		if r.LogDir != cfg.LogDir || r.CacheDir != cfg.CacheDir {
			t.Fatalf("runner dirs = log %q cache %q, want log %q cache %q", r.LogDir, r.CacheDir, cfg.LogDir, cfg.CacheDir)
		}
		if r.Runtime != cfg.Backend {
			t.Fatal("runner instance backend was not wired")
		}
		if len(r.HarnessEnv[string(harness.Codex)]) != 1 || r.HarnessEnv[string(harness.Codex)][0] != "CODEX_HOME=/tmp/codex" {
			t.Fatalf("HarnessEnv = %#v, want configured codex env", r.HarnessEnv)
		}
		if len(r.Backends) == 0 {
			t.Fatal("runner backends were not initialized")
		}
	})
}

func TestManager(t *testing.T) {
	t.Parallel()

	t.Run("RegisterRunner", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			r := &task.Runner{Dir: "/tmp/test"}
			m.RegisterRunner("my/repo", r)
			got, ok := m.Runner("my/repo")
			if !ok || got != r {
				t.Fatal("Runner() did not return registered runner")
			}
		})
	})

	t.Run("UnregisterRunner", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			r := &task.Runner{Dir: "/tmp/test"}
			m.RegisterRunner("my/repo", r)
			m.UnregisterRunner("my/repo")
			if _, ok := m.Runner("my/repo"); ok {
				t.Error("Runner() still returned runner after UnregisterRunner")
			}
		})
		t.Run("valid_removes_only_matching", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			r1 := &task.Runner{Dir: "/tmp/a"}
			r2 := &task.Runner{Dir: "/tmp/b"}
			m.RegisterRunner("a", r1)
			m.RegisterRunner("b", r2)
			m.UnregisterRunner("a")
			if _, ok := m.Runner("a"); ok {
				t.Error("Runner(a) should be removed")
			}
			if got, ok := m.Runner("b"); !ok || got != r2 {
				t.Error("Runner(b) should still be registered")
			}
		})
	})

	t.Run("GetEntry", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			got, ok := m.GetEntry(tk.ID.String())
			if !ok || got != e {
				t.Fatal("GetEntry did not return inserted entry")
			}
			if m.Len() != 1 {
				t.Errorf("Len() = %d, want 1", m.Len())
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			_, ok := m.GetEntry("nonexistent")
			if ok {
				t.Error("GetEntry should return false for nonexistent task")
			}
		})
	})

	// Regression: Range/RangeRunners must invoke fn unlocked so callbacks may
	// call back into the Manager (e.g. server.toJSON → Runner). Holding m.mu
	// during fn self-deadlocks the whole server (caught only by e2e before).
	t.Run("RangeCallbackCanCallManager", func(t *testing.T) {
		t.Parallel()
		m := New(Config{ServerCtx: t.Context()})
		tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
		m.Insert(tk.ID.String(), NewEntry(tk))
		done := make(chan struct{})
		go func() {
			m.Range(func(_ string, _ *Entry) bool {
				_, _ = m.Runner("") // re-enters m.mu; deadlocks if Range holds it
				return true
			})
			m.RangeRunners(func(_ string, _ *task.Runner) bool {
				_, _ = m.GetEntry(tk.ID.String())
				return true
			})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Range/RangeRunners deadlocked when callback called back into Manager")
		}
	})

	t.Run("Range", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			for range 5 {
				m.Insert(ksid.NewID().String(), NewEntry(&task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}))
			}
			var count int
			m.Range(func(id string, e *Entry) bool {
				count++
				return true
			})
			if count != 5 {
				t.Errorf("Range iterated %d entries, want 5", count)
			}
		})
		t.Run("valid_stops_on_false", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			for range 5 {
				m.Insert(ksid.NewID().String(), NewEntry(&task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}))
			}
			var count int
			m.Range(func(id string, e *Entry) bool {
				count++
				return false
			})
			if count != 1 {
				t.Errorf("Range iterated %d entries after stop, want 1", count)
			}
		})
	})

	t.Run("Changed", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_on_insert", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			oldCh := m.Changed()
			m.Insert(ksid.NewID().String(), NewEntry(&task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}))
			select {
			case <-oldCh:
			case <-time.After(time.Second):
				t.Fatal("old Changed() channel not closed after Insert")
			}
		})
		t.Run("valid_new_channel_after_mutation", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			ch1 := m.Changed()
			m.NotifyTaskChange()
			ch2 := m.Changed()
			if ch1 == ch2 {
				t.Fatal("Changed() returned same channel after mutation")
			}
			select {
			case <-ch1:
			default:
				t.Error("old Changed() channel not closed after mutation")
			}
		})
	})

	t.Run("FindTasksByPR", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID.String(), NewEntry(tk))
			found := m.FindTasksByPR("acme", "magic", 42)
			if len(found) != 1 {
				t.Fatalf("FindTasksByPR returned %d entries, want 1", len(found))
			}
			if found[0].Task() != tk {
				t.Error("FindTasksByPR returned wrong entry")
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 42)
			m.Insert(tk.ID.String(), NewEntry(tk))
			found := m.FindTasksByPR("other", "repo", 42)
			if len(found) != 0 {
				t.Errorf("FindTasksByPR returned %d entries for wrong owner, want 0", len(found))
			}
		})
	})

	t.Run("FindTasksMatchingBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), NewEntry(tk))
			found := m.FindTasksMatchingBranch("acme", "magic", "caic-1")
			if len(found) != 1 {
				t.Fatalf("FindTasksMatchingBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), NewEntry(tk))
			found := m.FindTasksMatchingBranch("acme", "magic", "caic-2")
			if len(found) != 0 {
				t.Errorf("FindTasksMatchingBranch returned %d entries for wrong branch, want 0", len(found))
			}
		})
	})

	t.Run("FindTasksMonitoringBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			e := NewEntry(tk)
			e.SetMonitorBranch("caic-1")
			m.Insert(tk.ID.String(), e)
			found := m.FindTasksMonitoringBranch("acme", "magic")
			if len(found) != 1 {
				t.Fatalf("FindTasksMonitoringBranch returned %d entries, want 1", len(found))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1"}},
			}
			tk.SetPR("acme", "magic", 0)
			m.Insert(tk.ID.String(), NewEntry(tk))
			found := m.FindTasksMonitoringBranch("acme", "magic")
			if len(found) != 0 {
				t.Errorf("FindTasksMonitoringBranch returned %d entries without monitor branch, want 0", len(found))
			}
		})
	})

	t.Run("ListPendingBotTasks", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
				ForgeIssue:    5,
				Repos:         []task.RepoMount{{Name: "repo/a"}},
			}
			tk.SetPR("acme", "magic", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))
			pending := m.ListPendingBotTasks()
			if len(pending) != 1 {
				t.Fatalf("ListPendingBotTasks returned %d tasks, want 1", len(pending))
			}
			if pending[0].IssueNumber != 5 {
				t.Errorf("IssueNumber = %d, want 5", pending[0].IssueNumber)
			}
		})
		t.Run("valid_skips_no_forge_issue", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks without ForgeIssue, want 0", len(pending))
			}
		})
		t.Run("valid_skips_terminal_states", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			for _, st := range []task.State{task.StateWaiting, task.StateStopped, task.StateCrashed, task.StateFailed, task.StatePurged} {
				tk := &task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
					ForgeIssue:    1,
				}
				tk.SetState(st)
				m.Insert(tk.ID.String(), NewEntry(tk))
			}
			pending := m.ListPendingBotTasks()
			if len(pending) != 0 {
				t.Errorf("ListPendingBotTasks returned %d tasks for terminal states, want 0", len(pending))
			}
		})
	})

	t.Run("resolveRunner", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_repo", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			r := &task.Runner{Dir: "/tmp/test"}
			m.RegisterRunner("my/repo", r)
			tk := &task.Task{
				InitialPrompt: agent.Prompt{Text: "test"},
				Repos:         []task.RepoMount{{Name: "my/repo"}},
			}
			got := m.resolveRunner(tk)
			if got != r {
				t.Error("resolveRunner returned wrong runner")
			}
		})
		t.Run("valid_no_repo_fallback", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
			got := m.resolveRunner(tk)
			if got == nil {
				t.Fatal("resolveRunner returned nil for no-repo task")
			}
		})
	})

	t.Run("applyLoadedSessionMetadata", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_fills_missing_session", func(t *testing.T) {
			t.Parallel()
			tk := &task.Task{Model: "requested"}
			lt := &task.LoadedTask{SessionID: "thread-1", Model: "reported", AgentVersion: "1.2.3"}
			applyLoadedSessionMetadata(tk, lt)
			if got := tk.GetSessionID(); got != "thread-1" {
				t.Errorf("SessionID = %q, want thread-1", got)
			}
			snap := tk.Snapshot()
			if snap.Model != "reported" {
				t.Errorf("Model = %q, want reported", snap.Model)
			}
			if snap.AgentVersion != "1.2.3" {
				t.Errorf("AgentVersion = %q, want 1.2.3", snap.AgentVersion)
			}
		})
		t.Run("valid_preserves_live_session", func(t *testing.T) {
			t.Parallel()
			tk := &task.Task{}
			tk.RestoreMessages([]agent.Message{&agent.InitMessage{SessionID: "live"}})
			applyLoadedSessionMetadata(tk, &task.LoadedTask{SessionID: "persisted"})
			if got := tk.GetSessionID(); got != "live" {
				t.Errorf("SessionID = %q, want live", got)
			}
		})
	})

	t.Run("EffectiveBaseBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_explicit", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("my/repo", &task.Runner{BaseBranch: "develop"})
			tk := &task.Task{
				Repos: []task.RepoMount{{Name: "my/repo", BaseBranch: "main"}},
			}
			if got := m.EffectiveBaseBranch(tk); got != "main" {
				t.Errorf("EffectiveBaseBranch = %q, want %q", got, "main")
			}
		})
		t.Run("valid_runner_default", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("my/repo", &task.Runner{BaseBranch: "develop"})
			tk := &task.Task{
				Repos: []task.RepoMount{{Name: "my/repo"}},
			}
			if got := m.EffectiveBaseBranch(tk); got != "develop" {
				t.Errorf("EffectiveBaseBranch = %q, want %q", got, "develop")
			}
		})
		t.Run("valid_no_repo", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{}
			if got := m.EffectiveBaseBranch(tk); got != "" {
				t.Errorf("EffectiveBaseBranch = %q for no-repo, want empty", got)
			}
		})
	})

	t.Run("SetTaskMonitorBranch", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
			e := NewEntry(tk)
			m.SetTaskMonitorBranch(e, "caic-1")
			if e.MonitorBranch() != "caic-1" {
				t.Errorf("MonitorBranch = %q, want %q", e.MonitorBranch(), "caic-1")
			}
		})
	})

	t.Run("WatchTaskCompletion", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_already_terminal", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateStopped)
			m.Insert(tk.ID.String(), NewEntry(tk))
			state, result, err := m.WatchTaskCompletion(t.Context(), tk.ID.String())
			if err != nil {
				t.Fatalf("WatchTaskCompletion error: %v", err)
			}
			if state != "stopped" {
				t.Errorf("state = %q, want %q", state, "stopped")
			}
			if result != "" {
				t.Errorf("result = %q, want empty", result)
			}
		})
		t.Run("error_not_found", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			_, _, err := m.WatchTaskCompletion(t.Context(), "nonexistent")
			if err == nil {
				t.Fatal("expected error for nonexistent task")
			}
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindNotFound {
				t.Fatalf("err = %v, want KindNotFound", err)
			}
		})
		t.Run("valid_waits_for_terminal", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))
			go func() {
				time.Sleep(50 * time.Millisecond)
				tk.SetState(task.StateStopped)
				m.NotifyTaskChange()
			}()
			state, _, err := m.WatchTaskCompletion(t.Context(), tk.ID.String())
			if err != nil {
				t.Fatalf("WatchTaskCompletion error: %v", err)
			}
			if state != "stopped" {
				t.Errorf("state = %q, want %q", state, "stopped")
			}
		})
		t.Run("error_context_cancelled", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "test"},
			}
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, _, err := m.WatchTaskCompletion(ctx, tk.ID.String())
			if err == nil {
				t.Fatal("expected error for cancelled context")
			}
		})
	})

	t.Run("LifecycleErrors", func(t *testing.T) {
		t.Parallel()
		// call returns the lifecycle error for a freshly-inserted task in the
		// given state. The not-found path is no longer the Manager's concern:
		// these methods take an *Entry, and the HTTP layer rejects unknown ids
		// before they are reached (see server.getTask).
		type tc struct {
			name  string
			state task.State
			want  ErrorKind
			call  func(m *Manager, e *Entry) error
		}
		stop := func(m *Manager, e *Entry) error { return m.Stop(t.Context(), e) }
		revive := func(m *Manager, e *Entry) error { return m.Revive(t.Context(), e) }
		syncOrigin := func(m *Manager, e *Entry) error {
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			return err
		}
		syncForceDefault := func(m *Manager, e *Entry) error {
			_, err := m.Sync(t.Context(), e, SyncTargetDefault, true)
			return err
		}
		fork := func(m *Manager, e *Entry) error {
			_, err := m.Fork(t.Context(), e, ForkParams{})
			return err
		}
		cases := []tc{
			{"stop_wrong_state", task.StateStopped, KindConflict, stop},
			{"revive_not_stopped", task.StateRunning, KindConflict, revive},
			{"sync_terminal_state", task.StateStopped, KindConflict, syncOrigin},
			{"sync_force_default", task.StateWaiting, KindBadRequest, syncForceDefault},
			{"fork_wrong_state", task.StateStopped, KindConflict, fork},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				m := New(Config{ServerCtx: t.Context()})
				tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "test"}}
				tk.SetState(c.state)
				e := NewEntry(tk)
				m.Insert(tk.ID.String(), e)
				err := c.call(m, e)
				if err == nil {
					t.Fatalf("expected *Error with Kind %v, got nil", c.want)
				}
				var te *Error
				if !errors.As(err, &te) {
					t.Fatalf("error %v is not a *Error", err)
				}
				if te.Kind != c.want {
					t.Errorf("Kind = %v, want %v (err=%v)", te.Kind, c.want, err)
				}
			})
		}
	})

	t.Run("Create", func(t *testing.T) {
		t.Parallel()
		// newManagerWithRepo returns a Manager with one repo runner that has a
		// fake backend for harness "fake".
		newManagerWithRepo := func(t *testing.T) *Manager {
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("my/repo", &task.Runner{
				Dir:      "/tmp/my-repo",
				Backends: map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			return m
		}
		t.Run("valid_sets_basename_mounted_path_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo", BaseBranch: "main"}},
				Harness: "fake",
				MaxCPUs: 7,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("entry not found after Create")
			}
			tk := e.Task()
			if tk.MaxCPUs != 7 {
				t.Errorf("MaxCPUs = %d, want 7", tk.MaxCPUs)
			}
			repos := tk.ReposSnapshot()
			if len(repos) != 1 {
				t.Fatalf("len(Repos) = %d, want 1", len(repos))
			}
			if got := repos[0].MountedPath; got != "~/src/repo" {
				t.Errorf("MountedPath = %q, want %q", got, "~/src/repo")
			}
		})
		t.Run("valid_sets_relative_mounted_path_for_basename_collision", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			m.RegisterRunner("other/repo", &task.Runner{
				Dir:      "/tmp/other-repo",
				Backends: map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo", BaseBranch: "main"}},
				Harness: "fake",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("entry not found after Create")
			}
			tk := e.Task()
			repos := tk.ReposSnapshot()
			if len(repos) != 1 {
				t.Fatalf("len(Repos) = %d, want 1", len(repos))
			}
			if got := repos[0].MountedPath; got != "~/src/my/repo" {
				t.Errorf("MountedPath = %q, want %q", got, "~/src/my/repo")
			}
		})
		t.Run("valid_sudo_resolution_no_container", func(t *testing.T) {
			t.Parallel()
			// With no instance backend, Start fails in the background
			// goroutine and the task transitions to Failed. SudoPassword
			// short-circuits to "" because Runtime is empty; this exercises
			// the goroutine's sudo branch without an SSH round-trip.
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo"}},
				Harness: "fake",
				Sudo:    true,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, _ := m.GetEntry(id)
			if !e.Task().Sudo {
				t.Error("Sudo flag not propagated to task")
			}
		})
		t.Run("valid_forge_issue_and_pr", func(t *testing.T) {
			t.Parallel()
			// The bot path passes ForgeIssue/ForgeOwner/ForgeRepo; Create records
			// the issue number and calls SetPR so ListPendingBotTasks works.
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:     agent.Prompt{Text: "hi"},
				Repos:      []CreateRepo{{Name: "my/repo"}},
				Harness:    "fake",
				ForgeIssue: 17,
				ForgeOwner: "acme",
				ForgeRepo:  "magic",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, _ := m.GetEntry(id)
			snap := e.Task().Snapshot()
			if snap.ForgeIssue != 17 {
				t.Errorf("ForgeIssue = %d, want 17", snap.ForgeIssue)
			}
			if snap.ForgeOwner != "acme" || snap.ForgeRepo != "magic" {
				t.Errorf("ForgeOwner/Repo = %q/%q, want acme/magic", snap.ForgeOwner, snap.ForgeRepo)
			}
		})
		t.Run("valid_no_forge_owner_leaves_pr_zero", func(t *testing.T) {
			t.Parallel()
			// HTTP creates leave the forge fields zero: SetPR must not be called.
			m := newManagerWithRepo(t)
			id, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "my/repo"}},
				Harness: "fake",
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			e, _ := m.GetEntry(id)
			snap := e.Task().Snapshot()
			if snap.ForgeIssue != 0 || snap.ForgeOwner != "" || snap.ForgeRepo != "" {
				t.Errorf("forge fields not zero: issue=%d owner=%q repo=%q", snap.ForgeIssue, snap.ForgeOwner, snap.ForgeRepo)
			}
		})
		t.Run("error_unknown_repo", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "ghost"}},
				Harness: "fake",
			})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := newManagerWithRepo(t)
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi", Images: []agent.ImageData{{}}},
				Repos:   []CreateRepo{{Name: "my/repo"}},
				Harness: "fake",
			})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})

	t.Run("Fork", func(t *testing.T) {
		t.Parallel()
		// newForkManager returns a Manager with a source task that has a
		// instance, plus a runner with a fake backend.
		newForkManager := func(t *testing.T) (*Manager, *Entry) {
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("my/repo", &task.Runner{
				Dir:      "/tmp/my-repo",
				Backends: map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1", GitRoot: "/tmp/my-repo"}},
				Harness:       "fake",
				MaxCPUs:       5,
				GitHubToken:   true,
			}
			src.SetRuntimeConnectionInfo("md-agent-src", runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := NewEntry(src)
			m.Insert(src.ID.String(), e)
			return m, e
		}
		t.Run("valid_resolved_overrides_and_max_cpus", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := m.Fork(t.Context(), src, ForkParams{
				Prompt:    agent.Prompt{Text: "fork"},
				Tailscale: true,
				USB:       true,
				Display:   true,
				Sudo:      true,
			})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			e, ok := m.GetEntry(id)
			if !ok {
				t.Fatal("fork entry not found")
			}
			tk := e.Task()
			if !tk.Tailscale || !tk.USB || !tk.Display || !tk.Sudo {
				t.Errorf("resolved overrides not applied: tailscale=%v usb=%v display=%v sudo=%v",
					tk.Tailscale, tk.USB, tk.Display, tk.Sudo)
			}
			if tk.MaxCPUs != 5 {
				t.Errorf("MaxCPUs = %d, want 5 (from source)", tk.MaxCPUs)
			}
		})
		t.Run("valid_metadata_matches_task", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := m.Fork(t.Context(), src, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			e, _ := m.GetEntry(id)
			metadata := task.MakeMetadata(e.Task())
			if metadata[runtime.MetadataTaskID] != e.Task().ID.String() {
				t.Errorf("metadata[%s] = %q, want %q", runtime.MetadataTaskID, metadata[runtime.MetadataTaskID], e.Task().ID.String())
			}
		})
		t.Run("error_extra_repo_overlap", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			_, err := m.Fork(t.Context(), src, ForkParams{
				Prompt:     agent.Prompt{Text: "fork"},
				ExtraRepos: []ForkRepo{{Name: "my/repo"}},
			})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_no_repo", func(t *testing.T) {
			t.Parallel()
			// Forking a task with no repos is invalid input (KindBadRequest,
			// 400), not a state conflict. Guards against the parity drift where
			// this returned 409.
			m := New(Config{ServerCtx: t.Context()})
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Harness:       "fake",
			}
			src.SetRuntimeConnectionInfo("md-agent-src", runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := NewEntry(src)
			m.Insert(src.ID.String(), e)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})

	t.Run("Restart", func(t *testing.T) {
		t.Parallel()
		t.Run("error_empty_prompt_no_container", func(t *testing.T) {
			t.Parallel()
			// Empty prompt triggers the plan-file fallback; with no instance,
			// agent.ReadPlan fails and Restart returns KindBadRequest.
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.Restart(t.Context(), e, agent.Prompt{})
			var te *Error
			if !errors.As(err, &te) {
				t.Fatalf("err %v is not a *Error", err)
			}
			if te.Kind != KindBadRequest {
				t.Errorf("Kind = %v, want KindBadRequest (err=%v)", te.Kind, err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateStopped)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.Restart(t.Context(), e, agent.Prompt{Text: "go"})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
	})

	t.Run("SendInput", func(t *testing.T) {
		t.Parallel()
		t.Run("error_no_session_is_sentinel", func(t *testing.T) {
			t.Parallel()
			// A waiting task with no active session: SendInput fails and the
			// error must satisfy errors.Is(err, ErrNoSession) while still
			// carrying the underlying diagnostic message.
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.SendInput(t.Context(), e, agent.Prompt{Text: "go"})
			if err == nil {
				t.Fatal("expected error from SendInput with no session")
			}
			if !errors.Is(err, ErrNoSession) {
				t.Errorf("errors.Is(err, ErrNoSession) = false, err = %v", err)
			}
			if !strings.Contains(err.Error(), "no active session") {
				t.Errorf("err message %q lost underlying diagnostic", err.Error())
			}
		})
		t.Run("error_delivery_failure_is_not_no_session", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}, Harness: harness.Codex}
			tk.SetState(task.StateWaiting)

			cmdCtx, cmdCancel := context.WithCancel(t.Context())
			cmd := exec.CommandContext(cmdCtx, "sleep", "60")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, codex.New("", nil).NewWire()), stdout, make(chan agent.Message, 256), nil)
			t.Cleanup(func() {
				cmdCancel()
				_ = s.Wait()
			})
			tk.AttachSession(&task.SessionHandle{Session: s})

			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err = m.SendInput(t.Context(), e, agent.Prompt{Text: "go"})
			if err == nil {
				t.Fatal("expected delivery error")
			}
			if errors.Is(err, ErrNoSession) {
				t.Fatalf("errors.Is(err, ErrNoSession) = true, err = %v", err)
			}
			var taskErr *Error
			if !errors.As(err, &taskErr) {
				t.Fatalf("err type = %T, want *Error", err)
			}
			if taskErr.Kind != KindConflict {
				t.Errorf("Kind = %s, want %s", taskErr.Kind, KindConflict)
			}
			if got := tk.GetState(); got != task.StateWaiting {
				t.Errorf("state = %s, want %s", got, task.StateWaiting)
			}
			if msgs := tk.Messages(); len(msgs) != 0 {
				t.Fatalf("messages = %d, want 0", len(msgs))
			}
		})
	})

	t.Run("Concurrency", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_concurrent_insert_and_range", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			var wg sync.WaitGroup
			for range 10 {
				tk := &task.Task{
					ID:            ksid.NewID(),
					InitialPrompt: agent.Prompt{Text: "test"},
				}
				id := tk.ID.String()
				wg.Go(func() {
					m.Insert(id, NewEntry(tk))
				})
			}
			for range 5 {
				wg.Go(func() {
					m.Range(func(id string, e *Entry) bool { return true })
				})
			}
			wg.Wait()
			if m.Len() != 10 {
				t.Errorf("Len() = %d after concurrent inserts, want 10", m.Len())
			}
		})
		t.Run("valid_concurrent_notify_change", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			var wg sync.WaitGroup
			for range 50 {
				wg.Go(func() {
					m.NotifyTaskChange()
				})
			}
			wg.Wait()
		})
	})
	t.Run("ClearContext", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateStopped)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.ClearContext(t.Context(), e)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_no_runner_backend", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.ClearContext(t.Context(), e)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindInternal {
				t.Fatalf("err = %v, want KindInternal", err)
			}
		})
	})
	t.Run("Compact", func(t *testing.T) {
		t.Parallel()
		t.Run("error_no_session", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.Compact(t.Context(), e, "shorten")
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
	})
	t.Run("SudoPassword", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_no_sudo", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          false,
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty for !Sudo", got)
			}
		})
		t.Run("valid_no_container", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
			}
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty for empty Runtime", got)
			}
		})
		t.Run("valid_cached", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
				SudoPassword:  "cached-pw",
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "cached-pw" {
				t.Errorf("SudoPassword = %q, want cached-pw", got)
			}
		})
		t.Run("valid_fetches_then_caches", func(t *testing.T) {
			t.Parallel()
			fake := &fakeMD{
				sudoFn: func(_ context.Context, id runtime.InstanceID) (string, error) {
					name := string(id)
					if name != "ctr-1" {
						t.Errorf("SudoPassword called with name %q, want ctr-1", name)
					}
					return "fetched-pw", nil
				},
			}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "fetched-pw" {
				t.Errorf("SudoPassword = %q, want fetched-pw", got)
			}
			if snap := tk.Snapshot(); snap.SudoPassword != "fetched-pw" {
				t.Error("password not cached on task after fetch")
			}
			// Second call must hit the cache, not the backend.
			if got := m.SudoPassword(t.Context(), tk); got != "fetched-pw" {
				t.Errorf("cached SudoPassword = %q, want fetched-pw", got)
			}
			if fake.sudoCalls != 1 {
				t.Errorf("sudoCalls = %d, want 1 (second call should be cached)", fake.sudoCalls)
			}
		})
		t.Run("valid_fetch_error_returns_empty", func(t *testing.T) {
			t.Parallel()
			fake := &fakeMD{
				sudoFn: func(_ context.Context, _ runtime.InstanceID) (string, error) {
					return "", errors.New("ssh boom")
				},
			}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Sudo:          true,
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			if got := m.SudoPassword(t.Context(), tk); got != "" {
				t.Errorf("SudoPassword = %q, want empty on fetch error", got)
			}
			if snap := tk.Snapshot(); snap.SudoPassword != "" {
				t.Errorf("task SudoPassword cached %q on error, want empty", snap.SudoPassword)
			}
		})
	})
	t.Run("HandleRuntimeInstanceExit", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_transitions_to_stopped", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Repos:         []task.RepoMount{{Name: "repo/x", Branch: "caic-1"}},
			}
			tk.SetRuntimeConnectionInfo("ctr-dead", runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))
			m.handleRuntimeInstanceExit("ctr-dead")
			if got := tk.GetState(); got != task.StateStopped {
				t.Errorf("state = %v, want StateStopped", got)
			}
		})
		t.Run("valid_skips_purged", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-purged", runtime.ConnectionTarget{SSHHost: "ctr-purged"}, "", "", 0)
			tk.SetState(task.StatePurged)
			m.Insert(tk.ID.String(), NewEntry(tk))
			m.handleRuntimeInstanceExit("ctr-purged")
			if got := tk.GetState(); got != task.StatePurged {
				t.Errorf("state = %v (should stay Purged)", got)
			}
		})
		t.Run("valid_skips_purging", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-purging", runtime.ConnectionTarget{SSHHost: "ctr-purging"}, "", "", 0)
			// A purge in progress: removing the instance emits the very "die"
			// event handled here. Acting on it would flap the task to Stopped
			// mid-purge and race the cleanup goroutine.
			tk.SetState(task.StatePurging)
			m.Insert(tk.ID.String(), NewEntry(tk))
			m.handleRuntimeInstanceExit("ctr-purging")
			if got := tk.GetState(); got != task.StatePurging {
				t.Errorf("state = %v (should stay Purging)", got)
			}
		})
		t.Run("valid_skips_stopping", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-stopping", runtime.ConnectionTarget{SSHHost: "ctr-stopping"}, "", "", 0)
			tk.SetState(task.StateStopping)
			m.Insert(tk.ID.String(), NewEntry(tk))
			m.handleRuntimeInstanceExit("ctr-stopping")
			if got := tk.GetState(); got != task.StateStopping {
				t.Errorf("state = %v (should stay Stopping)", got)
			}
		})
		t.Run("valid_skips_stopped", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-stopped", runtime.ConnectionTarget{SSHHost: "ctr-stopped"}, "", "", 0)
			tk.SetState(task.StateStopped)
			m.Insert(tk.ID.String(), NewEntry(tk))
			m.handleRuntimeInstanceExit("ctr-stopped")
			if got := tk.GetState(); got != task.StateStopped {
				t.Errorf("state = %v (should stay Stopped)", got)
			}
		})
		t.Run("valid_skips_wrong_container", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-alive", runtime.ConnectionTarget{SSHHost: "ctr-alive"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))
			m.handleRuntimeInstanceExit("ctr-other")
			if got := tk.GetState(); got != task.StateRunning {
				t.Errorf("state = %v (should stay Running)", got)
			}
		})
	})
	t.Run("LoadMessagesOnDemand", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_no_loaded_task", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			m.LoadMessagesOnDemand(e)
		})
	})
	t.Run("LoadPurgedTasks", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_empty", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			err := m.LoadPurgedTasks(nil)
			if err != nil {
				t.Fatalf("LoadPurgedTasks(nil): %v", err)
			}
			if m.Len() != 0 {
				t.Errorf("Len() = %d after nil load, want 0", m.Len())
			}
		})
		t.Run("valid_filters_old", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			old := time.Now().Add(-30 * 24 * time.Hour).UTC()
			all := []*task.LoadedTask{
				{
					TaskID:            ksid.NewID().String(),
					Prompt:            "old",
					Harness:           "claude",
					LastStateUpdateAt: old,
					State:             task.StateStopped,
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if m.Len() != 0 {
				t.Errorf("Len() = %d after loading old task, want 0", m.Len())
			}
		})
		t.Run("valid_creates_entries", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("repo/a", &task.Runner{})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test task",
					Title:             "Test Title",
					Harness:           "claude",
					Repos:             []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
					State:             task.StateStopped,
					Result:            &task.Result{State: task.StateStopped},
					StartedAt:         now.Add(-1 * time.Hour),
					LastStateUpdateAt: now,
					Tailscale:         true,
					USB:               true,
					Display:           true,
					Sudo:              true,
					GitHubToken:       true,
					Model:             "model-1",
					Effort:            "high",
					AgentVersion:      "1.2.3",
					ForgePR:           42,
					ForgeOwner:        "acme",
					ForgeRepo:         "magic",
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if m.Len() != 1 {
				t.Fatalf("Len() = %d, want 1", m.Len())
			}
			e, ok := m.GetEntry(id.String())
			if !ok {
				t.Fatal("entry not found for expected ID")
			}
			tk := e.Task()
			if tk.Tailscale != true || tk.USB != true || tk.Display != true {
				t.Error("instance flags not restored")
			}
			snap := tk.Snapshot()
			if !snap.Sudo || !snap.GitHubToken {
				t.Errorf("privileged flags not restored: sudo=%v gitHubToken=%v", snap.Sudo, snap.GitHubToken)
			}
			if tk.Model != "model-1" || tk.Effort != "high" {
				t.Errorf("model/effort = %q/%q, want model-1/high", tk.Model, tk.Effort)
			}
			if tk.Title() != "Test Title" {
				t.Errorf("Title = %q, want \"Test Title\"", tk.Title())
			}
			if tk.GetPR() != 42 {
				t.Errorf("PR = %d, want 42", tk.GetPR())
			}
			if e.Result() == nil || e.Result().State != task.StateStopped {
				t.Errorf("Result = %v, want StateStopped", e.Result())
			}
		})
		t.Run("valid_running_becomes_failed", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "was running",
					Repos:             []task.RepoMount{{Name: "repo/a"}},
					State:             task.StateRunning,
					LastStateUpdateAt: now,
				},
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			e, _ := m.GetEntry(id.String())
			if got := e.Task().GetState(); got != task.StateFailed {
				t.Errorf("state = %v, want StateFailed (running→failed)", got)
			}
		})
		t.Run("valid_max_per_repo", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			all := make([]*task.LoadedTask, 0, 7)
			for i := range 7 {
				all = append(all, &task.LoadedTask{
					TaskID:            ksid.NewID().String(),
					Prompt:            "task",
					Repos:             []task.RepoMount{{Name: "repo/a"}},
					State:             task.StateStopped,
					LastStateUpdateAt: now.Add(time.Duration(i) * time.Second),
				})
			}
			err := m.LoadPurgedTasks(all)
			if err != nil {
				t.Fatalf("LoadPurgedTasks: %v", err)
			}
			if got := m.Len(); got != 5 {
				t.Errorf("Len() = %d, want 5 (maxPurgedPerRepo)", got)
			}
		})
		t.Run("valid_fallback_title", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "this is the prompt",
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id.String())
			if got := e.Task().Title(); got != "this is the prompt" {
				t.Errorf("Title = %q, want prompt fallback", got)
			}
		})
		t.Run("valid_no_result_fallback_to_failed", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			id := ksid.NewID()
			all := []*task.LoadedTask{
				{
					TaskID:            id.String(),
					Prompt:            "test",
					LastStateUpdateAt: now,
				},
			}
			_ = m.LoadPurgedTasks(all)
			e, _ := m.GetEntry(id.String())
			if got := e.Result().State; got != task.StateFailed {
				t.Errorf("Result.State = %v, want StateFailed (fallback)", got)
			}
		})
		t.Run("valid_invalid_ksid_fallback", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			now := time.Now().UTC()
			all := []*task.LoadedTask{
				{
					TaskID:            "invalid",
					Prompt:            "test",
					LastStateUpdateAt: now,
					State:             task.StateStopped,
				},
			}
			_ = m.LoadPurgedTasks(all)
			if m.Len() != 1 {
				t.Errorf("Len() = %d, want 1 (new ksid fallback)", m.Len())
			}
		})
	})
	t.Run("Sync", func(t *testing.T) {
		t.Parallel()
		t.Run("error_pending_no_container", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StatePending)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_purging", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StatePurging)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_provisioning_no_runner", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateProvisioning)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetOrigin, false)
			if err == nil {
				t.Fatal("expected error for provisioning task without instance")
			}
		})
		t.Run("error_force_not_supported", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateRunning)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			_, err := m.Sync(t.Context(), e, SyncTargetDefault, true)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("Purge", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StatePurged)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.Purge(t.Context(), e)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_wins_race_with_stop", func(t *testing.T) {
			t.Parallel()
			stopStarted := make(chan struct{})
			stopReturned := make(chan struct{})
			releaseStop := make(chan struct{})
			fake := &tasktest.FakeRuntimeBackend{
				StopFunc: func(ctx context.Context, _ runtime.InstanceID) error {
					close(stopStarted)
					defer close(stopReturned)
					select {
					case <-releaseStop:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			m := New(Config{ServerCtx: t.Context(), Backend: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			entry := NewEntry(tk)
			m.Insert(tk.ID.String(), entry)

			if err := m.Stop(t.Context(), entry); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			select {
			case <-stopStarted:
			case <-time.After(time.Second):
				t.Fatal("StopTask did not reach backend Stop")
			}

			if err := m.Purge(t.Context(), entry); err != nil {
				t.Fatalf("Purge: %v", err)
			}
			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("purge did not close done")
			}
			stopDoneChanged := m.Changed()
			close(releaseStop)
			select {
			case <-stopReturned:
			case <-time.After(time.Second):
				t.Fatal("backend Stop did not return")
			}
			select {
			case <-stopDoneChanged:
			case <-time.After(time.Second):
				t.Fatal("StopTask completion did not notify")
			}

			deadline := time.Now().Add(time.Second)
			for fake.Count("Stop") == 0 || fake.Count("Purge") == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("backend calls = %+v, want Stop and Purge", fake.Calls())
				}
				time.Sleep(time.Millisecond)
			}
			deadline = time.Now().Add(time.Second)
			for tk.GetState() != task.StatePurged {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StatePurged after StopTask finishes", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
			if result := entry.Result(); result == nil || result.State != task.StatePurged {
				t.Fatalf("Result = %v, want StatePurged", result)
			}
		})
	})
	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateStopped)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.Stop(t.Context(), e)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_stops_container_backend", func(t *testing.T) {
			t.Parallel()
			// Backend is the interface seam, so a fake stands in for Docker.
			fake := &tasktest.FakeRuntimeBackend{}
			m := New(Config{ServerCtx: t.Context(), Backend: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			entry := NewEntry(tk)
			m.Insert(tk.ID.String(), entry)

			if err := m.Stop(t.Context(), entry); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			// Stop runs StopTask on a background goroutine; wait for the task to
			// settle rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != task.StateStopped {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StateStopped", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
			calls := fake.Calls()
			if len(calls) != 1 || calls[0].Method != "Stop" || calls[0].Name != "ctr-1" {
				t.Errorf("backend calls = %+v, want one Stop of ctr-1", calls)
			}
		})
	})
	t.Run("Revive", func(t *testing.T) {
		t.Parallel()
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetState(task.StateRunning)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.Revive(t.Context(), e)
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("valid_accepts_crashed", func(t *testing.T) {
			t.Parallel()
			releaseRevive := make(chan struct{})
			fake := &tasktest.FakeRuntimeBackend{
				ReviveFunc: func(ctx context.Context, _ runtime.InstanceID) error {
					select {
					case <-releaseRevive:
						return errors.New("revive boom")
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			m := New(Config{ServerCtx: t.Context(), Backend: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateCrashed)
			entry := NewEntry(tk)
			m.Insert(tk.ID.String(), entry)

			if err := m.Revive(t.Context(), entry); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			if got := tk.GetState(); got != task.StateProvisioning {
				t.Fatalf("state = %v, want provisioning", got)
			}
			close(releaseRevive)
			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("failed revive did not close done")
			}
		})
		t.Run("error_failure_closes_done_and_publishes_result", func(t *testing.T) {
			t.Parallel()
			releaseRevive := make(chan struct{})
			fake := &tasktest.FakeRuntimeBackend{
				ReviveFunc: func(ctx context.Context, _ runtime.InstanceID) error {
					select {
					case <-releaseRevive:
						return errors.New("revive boom")
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			m := New(Config{ServerCtx: t.Context(), Backend: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)
			entry := NewEntry(tk)
			m.Insert(tk.ID.String(), entry)

			firstChanged := m.Changed()
			if err := m.Revive(t.Context(), entry); err != nil {
				t.Fatalf("Revive: %v", err)
			}
			select {
			case <-firstChanged:
			case <-time.After(time.Second):
				t.Fatal("initial revive transition did not notify")
			}
			failedChanged := m.Changed()
			close(releaseRevive)

			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("failed revive did not close done")
			}
			select {
			case <-failedChanged:
			case <-time.After(time.Second):
				t.Fatal("failed revive did not notify")
			}
			if got := tk.GetState(); got != task.StateFailed {
				t.Fatalf("state = %v, want StateFailed", got)
			}
			result := entry.Result()
			if result == nil || result.State != task.StateFailed || result.Err == nil {
				t.Fatalf("Result = %v, want failed result with error", result)
			}
		})
	})
	t.Run("SendInput_Images", func(t *testing.T) {
		t.Parallel()
		t.Run("error_images_unsupported", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("repo/a", &task.Runner{
				Dir:      "/tmp/repo",
				Backends: map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Repos:         []task.RepoMount{{Name: "repo/a"}},
				Harness:       "fake",
			}
			tk.SetState(task.StateWaiting)
			e := NewEntry(tk)
			m.Insert(tk.ID.String(), e)
			err := m.SendInput(t.Context(), e, agent.Prompt{Text: "go", Images: []agent.ImageData{{}}})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("watchSession", func(t *testing.T) {
		t.Run("valid_session_error_crashes_and_stops_instance", func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 255")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			msgCh := make(chan agent.Message, 1)
			dispatchDone := make(chan struct{})
			close(dispatchDone)
			s := agent.NewSession(cmd, agent.NewConn(stdin, io.Discard, codex.New("", nil).NewWire()), stdout, msgCh, nil)
			h := &task.SessionHandle{Session: s, MsgCh: msgCh, DispatchDone: dispatchDone}
			tk := &task.Task{ID: ksid.NewID(), InitialPrompt: agent.Prompt{Text: "x"}}
			tk.SetRuntimeConnectionInfo("ssh-failed", runtime.ConnectionTarget{SSHHost: "ssh-failed"}, "", "", 0)
			tk.SetState(task.StateRunning)
			tk.AttachSession(h)
			entry := NewEntry(tk)
			runtimeBackend := &tasktest.FakeRuntimeBackend{}
			m := New(Config{ServerCtx: t.Context()})

			m.watchSession(entry, &task.Runner{Runtime: runtimeBackend}, h)

			select {
			case <-entry.Done():
			case <-time.After(time.Second):
				t.Fatal("watchSession did not finish")
			}
			if got := tk.GetState(); got != task.StateCrashed {
				t.Fatalf("state = %v, want crashed", got)
			}
			if got := runtimeBackend.Count("Stop"); got != 1 {
				t.Fatalf("Stop count = %d, want 1", got)
			}
			if got := runtimeBackend.Calls()[0].Name; got != "ssh-failed" {
				t.Fatalf("Stop instance = %q, want ssh-failed", got)
			}
		})
	})
	t.Run("SetParser", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_backend", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("repo/a", &task.Runner{
				Backends: map[harness.Name]agent.Backend{"claude": &fakeBackend{models: []string{"m1"}}},
			})
			lt := &task.LoadedTask{Harness: "claude"}
			m.setParser(lt)
			// No panic — setParser succeeded.
		})
		t.Run("valid_no_backend", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			lt := &task.LoadedTask{Harness: "pi"}
			m.setParser(lt)
			// No panic — graceful no-op when no matching backend.
		})
	})
	t.Run("LoadMessagesOnDemand_Purged", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_with_loaded_task", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			lt := &task.LoadedTask{TaskID: tk.ID.String()}
			e := newPurgedEntry(tk, &task.Result{State: task.StatePurged}, lt)
			m.Insert(tk.ID.String(), e)
			// LoadMessagesOnce triggers the fn but LoadMessages will fail
			// (no log file); this exercises the LoadMessagesOnce path.
			m.LoadMessagesOnDemand(e)
		})
	})
	t.Run("Create_Errors", func(t *testing.T) {
		t.Parallel()
		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("repo/a", &task.Runner{
				Dir:      "/tmp/repo",
				Backends: map[harness.Name]agent.Backend{}, // no backends
			})
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "repo/a"}},
				Harness: "bogus",
			})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("repo/a", &task.Runner{
				Dir:      "/tmp/repo",
				Backends: map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "repo/a"}},
				Harness: "fake",
				Model:   "unsupported-model",
			})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("repo/a", &task.Runner{
				Dir:      "/tmp/repo",
				Backends: map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			_, err := m.Create(t.Context(), CreateParams{
				Prompt:  agent.Prompt{Text: "hi"},
				Repos:   []CreateRepo{{Name: "repo/a"}, {Name: "ghost"}},
				Harness: "fake",
			})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})
	t.Run("Fork_Errors", func(t *testing.T) {
		t.Parallel()

		// forkSetup creates a Manager with a runner for "repo/a" and a source
		// task in StateWaiting. Returns the Manager and the source Entry.
		forkSetup := func(t *testing.T, sourceHarness harness.Name, backends map[harness.Name]agent.Backend) (*Manager, *Entry) {
			m := New(Config{ServerCtx: t.Context()})
			r := &task.Runner{Dir: "/tmp/repo", Backends: backends}
			m.RegisterRunner("repo/a", r)
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Repos:         []task.RepoMount{{Name: "repo/a", Branch: "caic-1"}},
				Harness:       sourceHarness,
			}
			src.SetRuntimeConnectionInfo("md-agent-src", runtime.ConnectionTarget{SSHHost: "md-agent-src"}, "", "", 0)
			src.SetState(task.StateWaiting)
			e := NewEntry(src)
			m.Insert(src.ID.String(), e)
			return m, e
		}

		defaultBackends := map[harness.Name]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}}

		t.Run("error_unknown_harness", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "bogus"})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unsupported_model", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "unsupported"})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_model_with_new_harness", func(t *testing.T) {
			t.Parallel()
			backends := map[harness.Name]agent.Backend{
				"fake":  &fakeBackend{models: []string{"m1"}},
				"fake2": &fakeBackend{models: []string{"m2"}},
			}
			m, e := forkSetup(t, "fake", backends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Harness: "fake2", Model: "unsupported"})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_no_container", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			// Overwrite the instance to empty.
			e.Task().SetRuntimeConnectionInfo("", runtime.ConnectionTarget{SSHHost: ""}, "", "", 0)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_wrong_state", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			e.Task().SetState(task.StateProvisioning)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindConflict {
				t.Fatalf("err = %v, want KindConflict", err)
			}
		})
		t.Run("error_unknown_extra_repo", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "fake", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, ExtraRepos: []ForkRepo{{Name: "ghost"}}})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
		t.Run("error_unknown_harness_when_model_set", func(t *testing.T) {
			t.Parallel()
			m, e := forkSetup(t, "bogus", defaultBackends)
			_, err := m.Fork(t.Context(), e, ForkParams{Prompt: agent.Prompt{Text: "fork"}, Model: "m1"})
			var te *Error
			if !errors.As(err, &te) || te.Kind != KindBadRequest {
				t.Fatalf("err = %v, want KindBadRequest", err)
			}
		})
	})

	t.Run("AdoptInstances", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_matches_only_primary_repo", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &fakeMD{metadata: map[string]string{
				"md-caic-caic-5\x00caic.id":      taskID.String(),
				"md-caic-caic-5\x00caic.harness": string(harness.Claude),
			}}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			m.RegisterRunner("caic-xyz/caic", &task.Runner{
				Dir:      "/home/user/src/caic-xyz/caic",
				Backends: map[harness.Name]agent.Backend{harness.Claude: &fakeBackend{models: []string{"m1"}}},
			})
			m.RegisterRunner("caic-xyz/md", &task.Runner{
				Dir:      "/home/user/src/caic-xyz/md",
				Backends: map[harness.Name]agent.Backend{harness.Claude: &fakeBackend{models: []string{"m1"}}},
			})

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
				{RelPath: "caic-xyz/md", AbsPath: "/home/user/src/caic-xyz/md"},
			}, []runtime.Instance{
				{
					ID:    "md-caic-caic-5",
					State: "exited",
					Repos: []runtime.Repo{
						{HostPath: "/home/user/src/caic-xyz/caic", Branch: "caic-5", MountPath: "/home/user/src/caic-xyz/caic"},
						{HostPath: "/home/user/src/caic-xyz/md", Branch: "caic-0", MountPath: "/home/user/src/caic-xyz/md"},
					},
				},
			}, nil)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if adopted[0].RelPath != "caic-xyz/caic" {
				t.Errorf("adopted RelPath = %q, want caic-xyz/caic", adopted[0].RelPath)
			}
			if adopted[0].Branch != "caic-5" {
				t.Errorf("adopted Branch = %q, want caic-5", adopted[0].Branch)
			}
			if m.Len() != 1 {
				t.Errorf("manager Len = %d, want 1", m.Len())
			}
		})
		t.Run("valid_dead_relay_exit_error_crashes_adopted_task", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			taskID := ksid.NewID()
			fake := &fakeMD{metadata: map[string]string{
				"dead-relay\x00caic.id":      taskID.String(),
				"dead-relay\x00caic.harness": string(harness.Claude),
			}}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			m.RegisterRunner("caic-xyz/caic", &task.Runner{
				Dir:      "/home/user/src/caic-xyz/caic",
				Backends: map[harness.Name]agent.Backend{harness.Claude: &fakeBackend{models: []string{"m1"}}},
			})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     1,
				Prompt:      "dead relay task",
				Repos:       []agent.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-7"}},
				Harness:     harness.Claude,
			})
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(logDir, "dead-relay.jsonl")
			body := string(meta) + "\n" + `{"type":"caic_exit","exit_code":2,"error":"Unknown option: --approve"}` + "\n"
			if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(ctx, []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    "dead-relay",
					State: "running",
					Repos: []runtime.Repo{{
						HostPath:  "/home/user/src/caic-xyz/caic",
						Branch:    "caic-7",
						MountPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetState(); got != task.StateCrashed {
				t.Errorf("state = %v, want crashed", got)
			}
			if adopted[0].Entry.Result() == nil {
				t.Fatal("entry result is nil")
			}
			if err := adopted[0].Entry.Result().Err; err == nil || !strings.Contains(err.Error(), "Unknown option: --approve") {
				t.Fatalf("result err = %v, want relay stderr", err)
			}
			persisted, err := os.ReadFile(logPath) //nolint:gosec // test-controlled temp path
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(persisted), `"type":"caic_result"`) || !strings.Contains(string(persisted), `"state":"crashed"`) {
				t.Fatalf("log missing crashed caic_result trailer:\n%s", persisted)
			}
		})
		t.Run("valid_dead_relay_tail_exit_error_crashes_adopted_task", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &fakeMD{metadata: map[string]string{
				"dead-relay-tail\x00caic.id":      taskID.String(),
				"dead-relay-tail\x00caic.harness": string(harness.Claude),
			}}
			m := New(Config{ServerCtx: t.Context(), Backend: &tasktest.FakeRuntimeBackend{}, Monitor: fake, Inventory: fake, Privilege: fake})
			m.relay = fakeRelayReader{
				statusFn: func(context.Context, runtime.ConnectionTarget) (bool, string, error) {
					return false, "dead", nil
				},
				readTailFn: func(context.Context, runtime.ConnectionTarget, func([]byte) ([]agent.Message, error), int64) ([]agent.Message, int64, error) {
					return []agent.Message{&agent.ExitMessage{ExitCode: 2, Error: "Unknown option: --approve"}}, 128, nil
				},
				readLogFn: func(context.Context, runtime.ConnectionTarget, int) string { return "relay exited" },
			}
			m.RegisterRunner("caic-xyz/caic", &task.Runner{
				Dir:      "/home/user/src/caic-xyz/caic",
				Backends: map[harness.Name]agent.Backend{harness.Claude: &fakeBackend{models: []string{"m1"}}},
			})

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    "dead-relay-tail",
					State: "running",
					Repos: []runtime.Repo{{
						HostPath:  "/home/user/src/caic-xyz/caic",
						Branch:    "caic-8",
						MountPath: "/home/user/src/caic-xyz/caic",
					}},
				},
			}, nil)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetState(); got != task.StateCrashed {
				t.Fatalf("state = %v, want crashed", got)
			}
			if err := adopted[0].Entry.Result().Err; err == nil || !strings.Contains(err.Error(), "Unknown option: --approve") {
				t.Fatalf("result err = %v, want relay stderr", err)
			}
		})
		t.Run("valid_loads_legacy_codex_session_metadata", func(t *testing.T) {
			t.Parallel()
			taskID := ksid.NewID()
			fake := &fakeMD{metadata: map[string]string{
				"md-caic-caic-6\x00caic.id":      taskID.String(),
				"md-caic-caic-6\x00caic.harness": string(harness.Codex),
			}}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			m.RegisterRunner("caic-xyz/caic", &task.Runner{
				Dir:      "/home/user/src/caic-xyz/caic",
				Backends: map[harness.Name]agent.Backend{harness.Codex: codex.New("", nil)},
			})

			logDir := t.TempDir()
			meta, err := json.Marshal(agent.MetaMessage{
				MessageType: "caic_meta",
				Version:     1,
				Prompt:      "legacy codex task",
				Repos:       []agent.MetaRepo{{Name: "caic-xyz/caic", Branch: "caic-6"}},
				Harness:     harness.Codex,
			})
			if err != nil {
				t.Fatal(err)
			}
			init := `{"method":"thread/started","params":{"thread":{"id":"thread-from-started","cliVersion":"1.0","createdAt":1,"cwd":"/repo","modelProvider":"openai","path":"/repo","preview":"","source":"user","status":{"type":"idle"},"updatedAt":2}}}`
			if err := os.WriteFile(filepath.Join(logDir, "legacy-codex.jsonl"), []byte(string(meta)+"\n"+init+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs, err := task.LoadLogs(logDir)
			if err != nil {
				t.Fatal(err)
			}

			adopted, err := m.AdoptInstances(t.Context(), []AdoptRepo{
				{RelPath: "caic-xyz/caic", AbsPath: "/home/user/src/caic-xyz/caic"},
			}, []runtime.Instance{
				{
					ID:    "md-caic-caic-6",
					State: "exited",
					Repos: []runtime.Repo{
						{HostPath: "/home/user/src/caic-xyz/caic", Branch: "caic-6", MountPath: "/home/user/src/caic-xyz/caic"},
					},
				},
			}, logs)
			if err != nil {
				t.Fatalf("AdoptInstances: %v", err)
			}
			if len(adopted) != 1 {
				t.Fatalf("adopted len = %d, want 1", len(adopted))
			}
			if got := adopted[0].Task.GetSessionID(); got != "thread-from-started" {
				t.Errorf("SessionID = %q, want thread-from-started", got)
			}
		})
	})

	t.Run("pushStats", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_pushes_to_active_tasks", func(t *testing.T) {
			t.Parallel()
			fake := &fakeMD{
				statsAllFn: func(_ context.Context, ids []runtime.InstanceID) (map[runtime.InstanceID]*runtime.Stats, error) {
					out := make(map[runtime.InstanceID]*runtime.Stats, len(ids))
					for _, id := range ids {
						out[id] = &runtime.Stats{CPUPerc: 0.5, MemUsed: 100}
					}
					return out, nil
				},
			}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))

			m.pushStats(t.Context())

			// Assert the snapshot actually landed on the task's stats ring.
			history, _, unsub := tk.SubscribeStats(t.Context())
			unsub()
			if len(history) != 1 {
				t.Fatalf("stats history len = %d, want 1", len(history))
			}
			if history[0].CPUPerc != 0.5 || history[0].MemUsed != 100 {
				t.Errorf("pushed stats = %+v, want CPUPerc=0.5 MemUsed=100", history[0])
			}
			if fake.statsAllCalls != 1 {
				t.Errorf("statsAllCalls = %d, want 1", fake.statsAllCalls)
			}
		})
		t.Run("valid_skips_inactive_states", func(t *testing.T) {
			t.Parallel()
			fake := &fakeMD{}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
			}
			tk.SetRuntimeConnectionInfo("ctr-dead", runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StatePurged)
			m.Insert(tk.ID.String(), NewEntry(tk))

			m.pushStats(t.Context())

			if fake.statsAllCalls != 0 {
				t.Errorf("statsAllCalls = %d for purged task, want 0", fake.statsAllCalls)
			}
		})
	})

	t.Run("watchRuntimeEvents", func(t *testing.T) {
		t.Parallel()
		t.Run("valid_dispatches_death", func(t *testing.T) {
			t.Parallel()
			events := make(chan runtime.Event, 1)
			fake := &fakeMD{events: events}
			m := New(Config{ServerCtx: t.Context(), Monitor: fake, Inventory: fake, Privilege: fake})
			tk := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "x"},
				Repos:         []task.RepoMount{{Name: "repo/x", Branch: "caic-1"}},
			}
			tk.SetRuntimeConnectionInfo("ctr-dead", runtime.ConnectionTarget{SSHHost: "ctr-dead"}, "", "", 0)
			tk.SetState(task.StateRunning)
			m.Insert(tk.ID.String(), NewEntry(tk))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			m.watchRuntimeEvents(ctx)

			events <- runtime.Event{InstanceID: "ctr-dead"}

			// The watcher dispatches on its own goroutine; wait for the state
			// transition rather than sleeping a fixed duration.
			deadline := time.Now().Add(2 * time.Second)
			for tk.GetState() != task.StateStopped {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v, want StateStopped after instance death", tk.GetState())
				}
				time.Sleep(time.Millisecond)
			}
		})
	})
}

func TestCountResultMessages(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		msgs := []agent.Message{
			&agent.TextMessage{Text: "hello"},
			&agent.ResultMessage{MessageType: "result"},
			&agent.TextMessage{Text: "world"},
			&agent.ResultMessage{MessageType: "result"},
		}
		if n := countResultMessages(msgs); n != 2 {
			t.Errorf("countResultMessages = %d, want 2", n)
		}
	})
	t.Run("valid_empty", func(t *testing.T) {
		t.Parallel()
		if n := countResultMessages(nil); n != 0 {
			t.Errorf("countResultMessages(nil) = %d, want 0", n)
		}
	})
}

func TestLastResultText(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result", Result: "first"},
			&agent.ResultMessage{MessageType: "result", Result: "last"},
		})
		if got := lastResultText(tk); got != "last" {
			t.Errorf("lastResultText = %q, want %q", got, "last")
		}
	})
	t.Run("valid_no_result", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		if got := lastResultText(tk); got != "" {
			t.Errorf("lastResultText = %q, want empty", got)
		}
	})
}

func TestNeedsTitleRegen(t *testing.T) {
	t.Parallel()
	t.Run("valid_no_log", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		if !needsTitleRegen(tk, nil) {
			t.Error("needsTitleRegen should return true when lt is nil")
		}
	})
	t.Run("valid_no_title_in_log", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		lt := &task.LoadedTask{TaskID: "test", Prompt: "test"}
		if !needsTitleRegen(tk, lt) {
			t.Error("needsTitleRegen should return true when lt.Title is empty")
		}
	})
	t.Run("valid_more_results_in_memory", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result"},
			&agent.ResultMessage{MessageType: "result"},
		})
		lt := &task.LoadedTask{
			TaskID: "test",
			Title:  "existing title",
			Msgs: []agent.Message{
				&agent.ResultMessage{MessageType: "result"},
			},
		}
		if !needsTitleRegen(tk, lt) {
			t.Error("needsTitleRegen should return true when restoredResults > logResults")
		}
	})
	t.Run("valid_same_count", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		tk.RestoreMessages([]agent.Message{
			&agent.ResultMessage{MessageType: "result"},
		})
		lt := &task.LoadedTask{
			TaskID: "test",
			Title:  "existing title",
			Msgs: []agent.Message{
				&agent.ResultMessage{MessageType: "result"},
			},
		}
		if needsTitleRegen(tk, lt) {
			t.Error("needsTitleRegen should return false when counts match")
		}
	})
	t.Run("valid_large_log_skips", func(t *testing.T) {
		t.Parallel()
		tk := &task.Task{InitialPrompt: agent.Prompt{Text: "test"}}
		lt := &task.LoadedTask{
			TaskID:  "test",
			Title:   "existing title",
			LogSize: 200 << 20, // 200 MiB, above maxLogSize
		}
		if needsTitleRegen(tk, lt) {
			t.Error("needsTitleRegen should return false for large logs")
		}
	})
}

func TestRefreshAdoptedDiffStat(t *testing.T) {
	t.Parallel()
	t.Run("valid_waiting_fetches_branch_diff", func(t *testing.T) {
		t.Parallel()
		fake := &tasktest.FakeRuntimeBackend{
			DiffFunc: func(context.Context, runtime.InstanceID, int, ...string) (string, error) {
				return "5\t1\tmain.go\n", nil
			},
		}
		runner := &task.Runner{Runtime: fake, Dir: "/repo"}
		tk := &task.Task{Repos: []task.RepoMount{{GitRoot: "/repo", Branch: "caic-0"}}}
		tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateWaiting)

		refreshAdoptedDiffStat(t.Context(), runner, tk)

		if got := fake.Count("Fetch"); got != 1 {
			t.Errorf("Fetch count = %d, want 1", got)
		}
		if got := fake.Count("Diff"); got != 1 {
			t.Errorf("Diff count = %d, want 1", got)
		}
		ds := tk.Snapshot().DiffStat
		if len(ds) != 1 || ds[0].Path != "main.go" || ds[0].Added != 5 || ds[0].Deleted != 1 {
			t.Errorf("DiffStat = %+v, want [{main.go 5 1}]", ds)
		}
	})
	t.Run("valid_running_skips_branch_diff", func(t *testing.T) {
		t.Parallel()
		fake := &tasktest.FakeRuntimeBackend{
			DiffFunc: func(context.Context, runtime.InstanceID, int, ...string) (string, error) {
				return "5\t1\tmain.go\n", nil
			},
		}
		runner := &task.Runner{Runtime: fake, Dir: "/repo"}
		tk := &task.Task{Repos: []task.RepoMount{{GitRoot: "/repo", Branch: "caic-0"}}}
		tk.SetRuntimeConnectionInfo("ctr-1", runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
		tk.SetState(task.StateRunning)

		refreshAdoptedDiffStat(t.Context(), runner, tk)

		if got := fake.Count("Fetch"); got != 0 {
			t.Errorf("Fetch count = %d, want 0", got)
		}
		if got := fake.Count("Diff"); got != 0 {
			t.Errorf("Diff count = %d, want 0", got)
		}
		if ds := tk.Snapshot().DiffStat; len(ds) != 0 {
			t.Errorf("DiffStat = %+v, want empty", ds)
		}
	})
}

func TestErrTaskNotFound(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if errTaskNotFound.Error() != "task not found" {
			t.Errorf("errTaskNotFound = %q, want %q", errTaskNotFound, "task not found")
		}
	})
	t.Run("kind", func(t *testing.T) {
		t.Parallel()
		var te *Error
		if !errors.As(error(errTaskNotFound), &te) {
			t.Fatal("errTaskNotFound is not a *Error")
		}
		if te.Kind != KindNotFound {
			t.Errorf("Kind = %v, want KindNotFound", te.Kind)
		}
	})
}
