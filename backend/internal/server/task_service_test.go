// Tests accepted input lifetime at the submission boundary and task metadata diagnostics.

package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"path/filepath"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
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
				session, conn := newGatedInputSession(t)
				tk.AttachSession(&task.SessionHandle{Session: session})
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				if when == "before_send" {
					cancel()
				}
				sent := make(chan error, 1)
				go func() {
					response, err := testTaskHandlers(s).taskSvc.sendInput(ctx, e, &v1.InputReq{Prompt: v1.Prompt{Text: "accepted input"}})
					if err == nil && (response == nil || response.Status != "sent") {
						err = fmt.Errorf("input response = %+v", response)
					}
					sent <- err
				}()
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
				messages := tk.Messages()
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

func (c *gatedInputConn) SendPrompt(p agent.Prompt) error {
	close(c.started)
	<-c.release
	return c.Conn.SendPrompt(p)
}

func (*gatedInputConn) ReadMessages(r io.Reader, _ chan<- agent.TimedMessage) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func newGatedInputSession(t *testing.T) (*agent.Session, *gatedInputConn) {
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "cat")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	log := testLogger()
	conn := &gatedInputConn{Conn: agent.NewConn(ctx, log, stdin, agent.DiscardLogSink{Version: agent.LogVersionV2}, claudecode.New().NewWire()), started: make(chan struct{}), release: make(chan struct{})}
	session := agent.NewSession(ctx, cmd, conn, stdout, make(chan agent.TimedMessage, 8), log)
	t.Cleanup(func() { cancel(); _ = session.Wait() })
	return session, conn
}
