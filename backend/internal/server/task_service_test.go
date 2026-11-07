// Tests and benchmarks task ordering, bounded conversation focus, recoverable compaction errors, input lifetime, and metadata diagnostics.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
)

func TestTaskInfoCompareWarnings(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		recorded := v1.TaskInfoRecorded{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      "~/.config/caic",
				ContainerPath: "~/.config/caic",
			}},
		}
		observed := v1.TaskInfoObservedRuntime{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      filepath.ToSlash(filepath.Join(home, ".config", "caic")),
				ContainerPath: "/home/user/.config/caic",
			}},
		}

		warnings := taskInfoCompareWarnings(&recorded, &observed)
		if len(warnings) != 0 {
			t.Fatalf("warnings = %+v, want none", warnings)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		recorded := v1.TaskInfoRecorded{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      "~/.config/caic",
				ContainerPath: "~/.config/caic",
			}},
		}
		observed := v1.TaskInfoObservedRuntime{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      filepath.ToSlash(filepath.Join(home, ".config", "other")),
				ContainerPath: "/home/user/.config/caic",
			}},
		}

		warnings := taskInfoCompareWarnings(&recorded, &observed)
		if len(warnings) != 1 {
			t.Fatalf("warnings = %+v, want one warning", warnings)
		}
	})
}

func TestTaskService(t *testing.T) {
	t.Parallel()
	t.Run("focusTask/boundedSessions", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		svc := testTaskHandlers(s).taskSvc
		recent := withMCPClientSession(t.Context(), "recent")
		victim := withMCPClientSession(t.Context(), "victim")
		id := ksid.NewID()
		svc.focusTask(recent, id)
		svc.focusTask(victim, id)
		for i := range maxRetainedTaskFocus - 2 {
			svc.focusTask(withMCPClientSession(t.Context(), fmt.Sprintf("flood-%d", i)), id)
		}
		if _, ok := svc.focusedTaskID(recent); !ok {
			t.Fatal("recent focus disappeared before reaching the bound")
		}
		evicted, stop := svc.subscribeTaskFocus(victim)
		t.Cleanup(stop)
		svc.focusTask(withMCPClientSession(t.Context(), "overflow"), id)
		if _, ok := svc.focusedTaskID(victim); ok {
			t.Fatal("least recently used session retained its focus")
		}
		if got, ok := svc.focusedTaskID(recent); !ok || got != id {
			t.Fatalf("recent focus=%s,%t, want %s", got, ok, id)
		}
		select {
		case got := <-evicted:
			if got != "" {
				t.Fatalf("eviction event=%q, want cleared focus", got)
			}
		default:
			t.Fatal("evicted session subscriber was not invalidated")
		}
		for i := range maxRetainedTaskFocus {
			svc.focusTask(withMCPClientSession(t.Context(), fmt.Sprintf("more-%d", i)), id)
		}
		svc.focusMu.Lock()
		defer svc.focusMu.Unlock()
		if len(svc.focusTasks) > maxRetainedTaskFocus {
			t.Fatalf("retained focus sessions=%d, cap=%d", len(svc.focusTasks), maxRetainedTaskFocus)
		}
	})
	t.Run("focusTask/concurrentSessions", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		svc := testTaskHandlers(s).taskSvc
		var workers sync.WaitGroup
		for i := range 32 {
			workers.Go(func() {
				ctx := withMCPClientSession(t.Context(), fmt.Sprintf("concurrent-%d", i))
				id := ksid.NewID()
				for range 20 {
					_, stop := svc.subscribeTaskFocus(ctx)
					svc.focusTask(ctx, id)
					if got, ok := svc.focusedTaskID(ctx); !ok || got != id {
						t.Errorf("concurrent session focus=%s,%t, want %s", got, ok, id)
					}
					_ = svc.focusedTaskIDString(t.Context())
					_ = svc.taskListSnapshot(ctx)
					stop()
				}
			})
		}
		workers.Wait()
	})
	t.Run("compactContext", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			harness     harness.Name
			wireFactory func() agent.WireFormat
		}{
			{harness.OpenCode, opencode.New("", nil).NewWire},
			{harness.Codex, codex.New("", nil).NewWire},
		} {
			t.Run(string(tc.harness), func(t *testing.T) {
				t.Parallel()

				s := newTestRouter(t, nil)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, tc.harness)
				tk.SetState(task.StateWaiting)
				insertTestTask(s, tk.ID, tk)
				entry, _ := s.taskMgr.GetEntry(tk.ID)
				session, _ := newGatedInputSession(t, tk, tc.wireFactory)
				_, err := testTaskHandlers(s).taskSvc.compactContext(t.Context(), entry, &v1.CompactReq{Instructions: "preserve the failing test"})
				apiError, ok := errors.AsType[*api.Error](err)
				if !ok || apiError.Status != http.StatusBadRequest || apiError.Message != "compact context: custom compaction instructions are not supported" {
					t.Fatalf("compact error=%v, want unsupported-instructions bad request", err)
				}
				select {
				case <-session.Done():
					t.Fatal("rejected instructions disconnected the session")
				default:
				}
			})
		}
	})
	t.Run("focusTask/userIsolation", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		svc := testTaskHandlers(s).taskSvc
		ctx := auth.NewContext(t.Context(), &auth.User{ID: "owner"})
		ch, cancel := svc.subscribeTaskFocus(ctx)
		t.Cleanup(cancel)
		other, cancelOther := svc.subscribeTaskFocus(t.Context())
		t.Cleanup(cancelOther)
		id := ksid.NewID()
		svc.focusTask(ctx, ksid.NewID())
		svc.focusTask(ctx, id)
		if got := <-ch; got != id.String() {
			t.Fatalf("focus = %q, want latest %q", got, id)
		}
		if got, ok := svc.focusedTaskID(ctx); !ok || got != id {
			t.Fatalf("focused task = %q, %t; want %q", got, ok, id)
		}
		svc.focusTask(newMCPPrincipalContext(ctx, &mcpPrincipal{TaskID: id}), id)
		select {
		case got := <-ch:
			t.Fatalf("background task changed focus to %q", got)
		default:
		}
		if got, ok := svc.focusedTaskID(ctx); !ok || got != id {
			t.Fatalf("background task changed focused task = %q, %t; want %q", got, ok, id)
		}
		select {
		case got := <-other:
			t.Fatalf("other user's focus changed to %q", got)
		default:
		}
		if got, ok := svc.focusedTaskID(t.Context()); ok {
			t.Fatalf("other user's focused task = %q, want none", got)
		}
	})
	t.Run("focusTask/sessionIsolation", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		svc := testTaskHandlers(s).taskSvc
		owner := auth.NewContext(t.Context(), &auth.User{ID: "owner"})
		first := ksid.NewID()
		second := ksid.NewID()
		firstSession := withMCPClientSession(owner, "tab-a")
		secondSession := withMCPClientSession(owner, "tab-b")

		svc.focusTask(firstSession, first)
		if got, ok := svc.focusedTaskID(firstSession); !ok || got != first {
			t.Fatalf("first session focus = %q, %t; want %q", got, ok, first)
		}
		if got, ok := svc.focusedTaskID(secondSession); ok {
			t.Fatalf("second session inherited focus %q", got)
		}
		if got, ok := svc.focusedTaskID(owner); !ok || got != first {
			t.Fatalf("sole session focus = %q, %t; want %q", got, ok, first)
		}

		svc.focusTask(secondSession, second)
		if got, ok := svc.focusedTaskID(firstSession); !ok || got != first {
			t.Fatalf("first session focus changed to %q, %t", got, ok)
		}
		if got, ok := svc.focusedTaskID(secondSession); !ok || got != second {
			t.Fatalf("second session focus = %q, %t; want %q", got, ok, second)
		}
		if got, ok := svc.focusedTaskID(owner); ok {
			t.Fatalf("ambiguous session focus = %q, want none", got)
		}

		svc.focusTask(secondSession, first)
		if got, ok := svc.focusedTaskID(owner); !ok || got != first {
			t.Fatalf("agreed session focus = %q, %t; want %q", got, ok, first)
		}
	})

	t.Run("taskListSnapshot/activeTasksFirst", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		inactiveID := ksid.NewID()
		inactive := mustNewTask(t, inactiveID, agent.Prompt{Text: "inactive"}, harness.Claude)
		inactive.SetState(task.StateStopped)
		insertTestTask(s, inactiveID, inactive)

		activeID := ksid.NewID()
		active := mustNewTask(t, activeID, agent.Prompt{Text: "active"}, harness.Claude)
		insertTestTask(s, activeID, active)

		tasks := testTaskHandlers(s).taskSvc.taskListSnapshot(t.Context())
		if len(tasks) != 2 {
			t.Fatalf("task count = %d, want 2", len(tasks))
		}
		if got := tasks[0].InitialPrompt; got != "active" {
			t.Errorf("first task = %q, want active", got)
		}
		if got := tasks[1].InitialPrompt; got != "inactive" {
			t.Errorf("second task = %q, want inactive", got)
		}
	})
	t.Run("sendInput", func(t *testing.T) {
		t.Parallel()
		for _, when := range []string{"before_send", "during_send"} {
			t.Run("request_cancellation/"+when, func(t *testing.T) {
				t.Parallel()
				s := newTestRouter(t, nil)
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude)
				tk.SetState(task.StateWaiting)
				insertTestTask(s, tk.ID, tk)
				e, _ := s.taskMgr.GetEntry(tk.ID)
				_, live, unsubscribe := tk.Subscribe(t.Context())
				t.Cleanup(unsubscribe)
				session, conn := newGatedInputSession(t, tk, claudecode.New().NewWire)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				if when == "before_send" {
					cancel()
				}
				sent := make(chan error, 1)
				var workers sync.WaitGroup
				t.Cleanup(func() {
					cancel()
					select {
					case <-conn.release:
					default:
						close(conn.release)
					}
					workers.Wait()
				})
				workers.Go(func() {
					response, err := testTaskHandlers(s).taskSvc.sendInput(ctx, e, &v1.InputReq{Prompt: v1.Prompt{Text: "accepted input"}}, nil)
					if err == nil && (response == nil || response.Status != "sent") {
						err = fmt.Errorf("input response = %+v", response)
					}
					sent <- err
				})
				if when == "during_send" {
					select {
					case <-conn.started:
					case <-time.After(5 * time.Second):
						t.Fatal("send did not start")
					}
					cancel()
				}
				close(conn.release)
				select {
				case err := <-sent:
					if err != nil {
						t.Fatalf("accepted input did not survive request cancellation: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("accepted send did not finish")
				}
				if tk.GetState() != task.StateRunning || e.Result() != nil {
					t.Fatalf("accepted input state=%s result=%+v", tk.GetState(), e.Result())
				}
				select {
				case <-live:
				case <-time.After(5 * time.Second):
					t.Fatal("accepted input was not dispatched")
				}
				var messages []agent.Message
				for _, m := range tk.Messages() {
					if !agent.IsInputDeliveryControl(m) {
						messages = append(messages, m)
					}
				}
				if len(messages) != 1 {
					t.Fatalf("recorded %d messages", len(messages))
				}
				input, ok := messages[0].(*agent.UserInputMessage)
				if !ok || input.Text != "accepted input" {
					t.Fatalf("recorded message = %#v", messages[0])
				}
				select {
				case <-session.Done():
					t.Fatal("request cancellation disconnected the session")
				default:
				}
			})
		}
	})
}

// gatedInputConn pauses a real prompt write while the request is canceled.
type gatedInputConn struct {
	agent.Conn

	started chan struct{}
	release chan struct{}
}

func (c *gatedInputConn) SendPrompt(p agent.Prompt, data []byte) error {
	close(c.started)
	<-c.release
	return c.Conn.SendPrompt(p, data)
}

func newGatedInputSession(t *testing.T, tk *taskrun.Task, wireFactory func() agent.WireFormat) (*agent.Session, *gatedInputConn) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	backend := &gatedInputBackend{FakeBackend: &agenttest.FakeBackend{WireFactory: wireFactory}}
	r := &taskrun.AgentRuntime{
		Backends:         agent.Backends{tk.Harness: backend},
		LogStore:         taskhistory.NewStore(testLogger(), taskslog.NewStore(t.TempDir()), backends.Default("", nil)),
		LogPath:          &taskslog.Path{},
		Log:              testLogger(),
		NotifyTaskChange: func() {},
	}
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "fixture"), runtime.ConnectionTarget{SSHHost: "fixture"}, "", "", 0)
	h, err := r.StartSession(ctx, tk, agent.Prompt{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := r.Shutdown(context.WithoutCancel(ctx), tk); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return h.Session, backend.conn
}

// gatedInputBackend uses the task runtime's real dispatch and log ownership.
type gatedInputBackend struct {
	*agenttest.FakeBackend

	conn *gatedInputConn
}

func (b *gatedInputBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", "while IFS= read -r input; do :; done")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b.conn = &gatedInputConn{Conn: agent.NewConn(ctx, opts.Logger, stdin, opts.Records, nil), started: make(chan struct{}), release: make(chan struct{})}
	return agent.NewSession(ctx, cmd, b.conn, stdout, opts.Logger), nil
}

func BenchmarkTaskListSnapshot(b *testing.B) {
	s := newTestRouter(b, nil)
	for i := range 10 {
		id := ksid.NewID()
		tk := mustNewTask(b, id, agent.Prompt{Text: "benchmark task"}, harness.Claude)
		if i%2 == 0 {
			tk.SetState(task.StateStopped)
		}
		insertTestTask(s, id, tk)
	}

	taskSvc := testTaskHandlers(s).taskSvc
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if got := len(taskSvc.taskListSnapshot(b.Context())); got != 10 {
			b.Fatalf("task count = %d, want 10", got)
		}
	}
}

func BenchmarkTaskListSnapshotWithReplay(b *testing.B) {
	s := newTestRouter(b, nil)
	for i := range 10 {
		id := ksid.NewID()
		tk := mustNewTask(b, id, agent.Prompt{Text: "benchmark task"}, harness.Claude)
		if i%2 == 0 {
			tk.SetState(task.StateStopped)
		}
		insertTestTask(s, id, tk)
	}

	taskSvc := testTaskHandlers(s).taskSvc
	// Seed every cursor to the current sequence so the benchmark measures the
	// steady state where no task has pending transitions to replay.
	cursors := map[string]uint64{}
	s.taskMgr.Range(func(id ksid.ID, e *taskmgr.Entry) bool {
		_, seq, _ := e.Task().SnapshotWithStateHistory(0)
		cursors[id.String()] = seq
		return true
	})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		out, replays, _ := taskSvc.taskListSnapshotWithReplay(b.Context(), cursors)
		if len(out) != 10 || len(replays) != 10 {
			b.Fatalf("task count = %d, replay count = %d, want 10", len(out), len(replays))
		}
	}
}
