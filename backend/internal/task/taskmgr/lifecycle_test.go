// Tests queued input, reconnect shutdown, restored session state, and waiting-input delivery latency.

package taskmgr

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/task"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"
)

func TestLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("QueueInput", func(t *testing.T) {
		t.Run("startup_prompts_wait_for_a_ready_turn", func(t *testing.T) {
			t.Parallel()
			for _, state := range []taskslog.State{taskslog.StateBranching, taskslog.StateProvisioning, taskslog.StateStarting} {
				t.Run(string(state), func(t *testing.T) {
					t.Parallel()
					m := newTestManager(t, Config{ServerCtx: t.Context()})
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
					tk.SetState(state)
					e := m.NewEntry(tk, nil)
					m.Insert(tk.ID, e)
					if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "after startup"}); err != nil {
						t.Fatal(err)
					}
					m.drainInputQueue(t.Context())
					q := e.PendingInput()
					if q == nil || q.Delivering || q.Error != "" {
						t.Fatalf("startup queue=%+v", q)
					}
				})
			}
		})
		t.Run("write_failure_preserves_queue", func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskslog.NewStore(testLogger(), dir)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
			tk.SetState(taskslog.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "saved"}); err != nil {
				t.Fatal(err)
			}
			q := e.PendingInput()
			path := filepath.Join(dir, "input-queue.json")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := e.Lifecycle.CancelQueuedInput(q.ID); err == nil {
				t.Fatal("cancel reported false success")
			}
			if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "unsaved"}); err == nil {
				t.Fatal("enqueue reported false success")
			}
			if got := e.PendingInput(); got == nil || got.ID != q.ID {
				t.Fatalf("failed writes changed queue: %+v", got)
			}
			m.Insert(tk.ID, e)
			tk.SetState(taskslog.StatePurged)
			m.drainInputQueue(t.Context())
			if got := e.PendingInput(); got == nil || got.ID != q.ID {
				t.Fatalf("failed pruning write changed queue: %+v", got)
			}
		})
	})
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

// BenchmarkLifecycleInputDelivery measures Enter-to-delivery overhead for a
// waiting agent, with ordinary text and four other tasks holding 1 MiB images.
// It includes durable queue writes and worker completion, but excludes harness
// inference/network latency through an already attached in-memory transport.
func BenchmarkLifecycleInputDelivery(b *testing.B) {
	for _, attached := range []bool{false, true} {
		b.Run(fmt.Sprintf("other_images=%t", attached), func(b *testing.B) {
			for _, deferred := range []bool{false, true} {
				b.Run(fmt.Sprintf("deferred=%t", deferred), func(b *testing.B) {
					m := newTestManager(b, Config{ServerCtx: b.Context()})
					backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Codex}}
					messages := make(chan agent.TimedMessage, 8)
					var receiver sync.WaitGroup
					receiver.Go(func() {
						for range messages {
						}
					})
					b.Cleanup(func() { backend.stop(); close(messages); receiver.Wait() })
					tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
					e := m.NewEntry(tk, nil)
					m.Insert(tk.ID, e)
					session, err := backend.AttachRelay(b.Context(), &agent.Options{Logger: testLogger(), MsgCh: messages})
					if err != nil {
						b.Fatal(err)
					}
					tk.AttachSession(&task.SessionHandle{Session: session})
					if attached {
						for range 4 {
							other := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
							other.SetState(taskslog.StateRunning)
							oe := m.NewEntry(other, nil)
							m.Insert(other.ID, oe)
							if _, err := oe.Lifecycle.QueueInput(b.Context(), agent.Prompt{Text: "pending", Images: []v3.ImageData{{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 1<<20))}}}); err != nil {
								b.Fatal(err)
							}
						}
					}
					prompt := agent.Prompt{Text: "Continue implementing the requested change."}
					b.ResetTimer()
					for b.Loop() {
						tk.SetState(taskslog.StateWaiting)
						if deferred {
							if _, err := e.Lifecycle.QueueInput(b.Context(), prompt); err != nil {
								b.Fatal(err)
							}
							m.drainInputQueue(b.Context())
							m.background.Wait()
						} else if err := e.Lifecycle.SendInput(b.Context(), prompt); err != nil {
							b.Fatal(err)
						}
						if tk.GetState() != taskslog.StateRunning {
							b.Fatal("input was not delivered")
						}
						backend.mu.Lock()
						backend.prompts = backend.prompts[:0]
						backend.mu.Unlock()
					}
				})
			}
		})
	}
}
