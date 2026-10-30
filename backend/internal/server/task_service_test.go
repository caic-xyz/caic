// Tests task ordering, accepted input lifetime, and task metadata diagnostics.

package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
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
		svc.focusTask(newMCPPrincipalContext(ctx, &mcpPrincipal{TaskID: id}), id)
		select {
		case got := <-ch:
			t.Fatalf("background task changed focus to %q", got)
		default:
		}
		select {
		case got := <-other:
			t.Fatalf("other user's focus changed to %q", got)
		default:
		}
	})
	t.Run("taskListSnapshot/activeTasksFirst", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		inactiveID := ksid.NewID()
		inactive := mustNewTask(t, inactiveID, agent.Prompt{Text: "inactive"}, harness.Claude)
		inactive.SetState(taskslog.StateStopped)
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
				tk.SetState(taskslog.StateWaiting)
				insertTestTask(s, tk.ID, tk)
				e, _ := s.taskMgr.GetEntry(tk.ID)
				_, live, unsubscribe := tk.Subscribe(t.Context())
				t.Cleanup(unsubscribe)
				session, conn := newGatedInputSession(t, tk)
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
					response, err := testTaskHandlers(s).taskSvc.sendInput(ctx, e, &v1.InputReq{Prompt: v1.Prompt{Text: "accepted input"}})
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
				if tk.GetState() != taskslog.StateRunning || e.Result() != nil {
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

func newGatedInputSession(t *testing.T, tk *task.Task) (*agent.Session, *gatedInputConn) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	backend := &gatedInputBackend{FakeBackend: &agenttest.FakeBackend{}}
	r := &task.AgentRuntime{
		Backends:         agent.Backends{harness.Claude: backend},
		LogStore:         taskslog.NewStore(testLogger(), t.TempDir()),
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
		_ = h.Drain()
		_ = h.Log.Close()
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
	b.conn = &gatedInputConn{Conn: agent.NewConn(ctx, opts.Logger, stdin, opts.Log, claudecode.New().NewWire()), started: make(chan struct{}), release: make(chan struct{})}
	return agent.NewSession(ctx, cmd, b.conn, stdout, opts.MsgCh, opts.Logger), nil
}
