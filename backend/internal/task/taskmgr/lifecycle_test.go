// Tests reconnect shutdown and restored session state.

package taskmgr

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"
)

func TestLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("SendInput", func(t *testing.T) {
		t.Parallel()
		t.Run("shutdown_cancels_reconnect", func(t *testing.T) {
			t.Parallel()
			for _, stage := range []string{"attach", "readiness"} {
				t.Run(stage, func(t *testing.T) {
					t.Parallel()
					started := make(chan struct{}, 1)
					healthy := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Codex}, attached: started}
					t.Cleanup(healthy.stop)
					var backend agent.Backend = healthy
					if stage == "attach" {
						backend = &blockedReconnectBackend{reconnectInputBackend: healthy, started: started}
					}
					m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: agent.Backends{harness.Codex: backend}})
					t.Cleanup(m.cancelServerCtx)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "blocked-reconnect"), runtime.ConnectionTarget{SSHHost: "blocked-reconnect"}, "", "", 0)
					tk.SetSessionMetadata("saved-session", "", "", "", 0)
					tk.SetState(taskslog.StateWaiting)
					e := m.NewEntry(tk, nil)
					m.Insert(tk.ID, e)
					recordTestTaskLog(t, m, e)
					sent := make(chan error, 1)
					var workers sync.WaitGroup
					workers.Go(func() { sent <- e.Lifecycle.SendInput(t.Context(), agent.Prompt{Text: "reconnect input"}) })
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						t.Fatal("reconnect did not start")
					}
					closed := make(chan error, 1)
					workers.Go(func() { closed <- m.Close() })
					select {
					case err := <-closed:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("shutdown did not finish")
					}
					select {
					case err := <-sent:
						// Cancellation may race the transport's exit notification.
						// Either failure is valid; successful delivery is not.
						if err == nil {
							t.Fatal("reconnect input succeeded during shutdown")
						}
					case <-time.After(5 * time.Second):
						t.Fatalf("%s ignored shutdown cancellation", stage)
					}
					workers.Wait()
					if tk.HasSession() {
						t.Fatal("shutdown left an attached local session")
					}
				})
			}
		})
	})
	t.Run("reconnectImportedSession", func(t *testing.T) {
		t.Parallel()
		for _, state := range []taskslog.State{taskslog.StateAsking, taskslog.StateHasPlan} {
			for _, stage := range []string{"attach", "readiness"} {
				t.Run(string(state)+"/"+stage, func(t *testing.T) {
					t.Parallel()
					var backend agent.Backend = &agenttest.FakeBackend{HarnessName: harness.Codex}
					if stage == "readiness" {
						b := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Codex}, exitImmediately: true}
						t.Cleanup(b.stop)
						backend = b
					}
					m := newTestManager(t, Config{ServerCtx: t.Context(), Backends: agent.Backends{harness.Codex: backend}})
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
					tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "restore-gate"), runtime.ConnectionTarget{SSHHost: "restore-gate"}, "", "", 0)
					tk.SetSessionMetadata("saved-session", "", "", "", 0)
					tk.SetState(state)
					e := m.NewEntry(tk, nil)
					m.Insert(tk.ID, e)
					recordTestTaskLog(t, m, e)
					e.Lifecycle.reconnectImportedSession()
					e.Lifecycle.wg.Wait()
					if got := tk.GetState(); got != state {
						t.Fatalf("failed %s reconnect changed %s to %s", stage, state, got)
					}
				})
			}
		}
	})
}

// blockedReconnectBackend models an SSH setup call that ends only on cancellation.
type blockedReconnectBackend struct {
	*reconnectInputBackend

	started chan struct{}
}

func (b *blockedReconnectBackend) AttachRelay(ctx context.Context, _ *agent.Options) (*agent.Session, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
