// Tests for Manager and standalone helpers.

package tasks

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
)

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
			for _, st := range []task.State{task.StateWaiting, task.StateStopped, task.StateFailed, task.StatePurged} {
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

	t.Run("SetRunnerBackends", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			m := New(Config{ServerCtx: t.Context()})
			r := &task.Runner{}
			m.RegisterRunner("my/repo", r)
			m.SetRunnerBackends(nil, map[agent.Harness]agent.Backend{"claude": nil})
			rr, _ := m.Runner("my/repo")
			if _, ok := rr.Backends["claude"]; !ok {
				t.Error("Backend not set on runner")
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
				Backends: map[agent.Harness]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			return m
		}
		t.Run("valid_sets_mounted_path_and_max_cpus", func(t *testing.T) {
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
			if len(tk.Repos) != 1 {
				t.Fatalf("len(Repos) = %d, want 1", len(tk.Repos))
			}
			if got := tk.Repos[0].MountedPath; got != "~/src/my/repo" {
				t.Errorf("MountedPath = %q, want %q", got, "~/src/my/repo")
			}
		})
		t.Run("valid_sudo_resolution_no_container", func(t *testing.T) {
			t.Parallel()
			// With no container backend, Start fails in the background
			// goroutine and the task transitions to Failed. SudoPassword
			// short-circuits to "" because Container is empty; this exercises
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
		// container, plus a runner with a fake backend.
		newForkManager := func(t *testing.T) (*Manager, *Entry) {
			m := New(Config{ServerCtx: t.Context()})
			m.RegisterRunner("my/repo", &task.Runner{
				Dir:      "/tmp/my-repo",
				Backends: map[agent.Harness]agent.Backend{"fake": &fakeBackend{models: []string{"m1"}}},
			})
			src := &task.Task{
				ID:            ksid.NewID(),
				InitialPrompt: agent.Prompt{Text: "src"},
				Repos:         []task.RepoMount{{Name: "my/repo", Branch: "caic-1", GitRoot: "/tmp/my-repo"}},
				Harness:       "fake",
				Container:     "md-agent-src",
				MaxCPUs:       5,
				GitHubToken:   true,
			}
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
		t.Run("valid_labels_match_make_labels", func(t *testing.T) {
			t.Parallel()
			m, src := newForkManager(t)
			id, err := m.Fork(t.Context(), src, ForkParams{Prompt: agent.Prompt{Text: "fork"}})
			if err != nil {
				t.Fatalf("Fork: %v", err)
			}
			e, _ := m.GetEntry(id)
			labels := task.MakeLabels(e.Task())
			// Sanity: MakeLabels includes the canonical caic.id label.
			if !slices.Contains(labels, "caic.id="+e.Task().ID.String()) {
				t.Errorf("labels %v missing caic.id", labels)
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
				Container:     "md-agent-src",
			}
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
			// Empty prompt triggers the plan-file fallback; with no container,
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
	})
}

func TestManager_Concurrency(t *testing.T) {
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
