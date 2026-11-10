// Tests accepted lifecycle intents, durable prompts, producer shutdown, and waiting-input delivery latency.

package taskmgr

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"

	"github.com/caic-xyz/caic/backend/internal/task"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/caic-xyz/caic/backend/internal/taskslog/data/inputqueue"
	"github.com/maruel/genai"
	"github.com/maruel/ksid"
)

func TestLifecycle(t *testing.T) {
	t.Parallel()
	t.Run("DelayedTitlePublication", func(t *testing.T) {
		t.Parallel()
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
		provider := &lifecycleTitleProvider{started: make(chan struct{}), release: make(chan struct{}, 1)}
		tk.Provider = provider
		entry := m.NewEntry(tk, nil)
		m.Insert(tk.ID, entry)
		t.Cleanup(func() { close(provider.release); entry.Lifecycle.wg.Wait() })
		entry.Lifecycle.generateTitle()
		<-provider.started
		version, _ := m.ChangeSnapshot()
		provider.release <- struct{}{}
		entry.Lifecycle.wg.Wait()
		_, _, changes, gap := m.TaskChanges(version)
		if gap || len(changes) != 1 || !changes[0].Catalog || !changes[0].Summary || !changes[0].Notifications {
			t.Fatalf("delayed title=%q events=%+v", tk.Title(), changes)
		}
	})
	t.Run("Close", func(t *testing.T) {
		t.Parallel()
		for _, completed := range []bool{false, true} {
			t.Run(fmt.Sprintf("initial_title_after_session_complete=%t", completed), func(t *testing.T) {
				t.Parallel()
				provider := &lifecycleTitleProvider{started: make(chan struct{}), release: make(chan struct{}, 1)}
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
				tk.Provider = provider
				tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "title-drain"), runtime.ConnectionTarget{SSHHost: "title-drain"}, "", "", 0)
				tk.SetState(task.StateRunning)
				entry := m.NewEntry(tk, nil)
				m.Insert(tk.ID, entry)
				command := "read line"
				if completed {
					command = "exit 17"
				}
				h := startManagedSession(t, entry, &localProcessBackend{
					FakeBackend: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire},
					command:     []string{"sh", "-c", command},
				})
				var workers sync.WaitGroup
				t.Cleanup(func() {
					close(provider.release)
					workers.Wait()
				})
				entry.Lifecycle.generateTitle()
				select {
				case <-provider.started:
				case <-time.After(time.Second):
					t.Fatal("initial title generation did not start")
				}
				if completed {
					entry.Lifecycle.watchSession(h)
					select {
					case <-entry.Done():
					case <-time.After(time.Second):
						t.Fatal("session watcher did not finalize while title generation was blocked")
					}
					loaded, err := m.logStore.LoadSource(entry.LogPath.Get())
					if err != nil {
						t.Fatal(err)
					}
					if err := loaded.LoadMessages(); err != nil {
						t.Fatal(err)
					}
					if loaded.LastTrailer == nil || loaded.LastTrailer.State != task.StateCrashed || loaded.LastTrailer.Err == nil {
						t.Fatalf("persisted finalization = %+v, want crashed result", loaded.LastTrailer)
					}
				}
				closed := make(chan error, 1)
				workers.Go(func() { closed <- entry.Lifecycle.Close() })
				if !completed {
					select {
					case <-h.Done():
					case <-time.After(time.Second):
						t.Fatal("Close did not cancel the local transport")
					}
					// Admission takes the same recording lock as physical shutdown.
					// It must reject cancellation while the pending title still owns its producer lease.
					err := entry.Lifecycle.agentRuntime.RecordHost(context.WithoutCancel(t.Context()), tk, &agent.LogMessage{Line: "after cancellation"})
					if !errors.Is(err, context.Canceled) {
						t.Errorf("shutdown admission = %v, want cancellation", err)
					}
					if tk.SessionDone() == nil {
						t.Error("Close detached the execution before the initial title producer joined")
					}
				}
				provider.release <- struct{}{}
				select {
				case err := <-closed:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Close did not join title completion and session finalization")
				}
				if got := tk.Title(); got != "Joined title" {
					t.Errorf("title = %q, want completed initial generation", got)
				}
			})
		}
	})
	for _, method := range []string{"Stop", "Purge"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			t.Run("error", func(t *testing.T) {
				t.Parallel()
				t.Run("missing journal preserves published state", func(t *testing.T) {
					t.Parallel()
					backend := &runtimetest.FakeBackend{}
					m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, backend, nil), Backends: backends.Default("", nil)})
					t.Cleanup(func() {
						if err := m.Close(); err != nil {
							t.Error(err)
						}
					})
					id, err := backend.Launch(t.Context(), nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "accepted"}, harness.Claude, "")
					tk.SetRuntimeConnectionInfo(id, runtime.ConnectionTarget{SSHHost: "fixture"}, "", "", 0)
					tk.SetState(task.StateWaiting)
					entry := m.NewEntry(tk, nil)
					m.Insert(tk.ID, entry)
					_, before, _ := tk.SnapshotWithStateHistory(0)
					if method == "Stop" {
						err = entry.Lifecycle.Stop(t.Context())
					} else {
						err = entry.Lifecycle.Purge(t.Context(), 0)
					}
					if err == nil {
						t.Fatal("lifecycle operation accepted missing journal")
					}
					_, after, transitions := tk.SnapshotWithStateHistory(before)
					if tk.GetState() != task.StateWaiting || after != before || len(transitions) != 0 {
						t.Fatalf("failed intent published state=%s transitions=%+v", tk.GetState(), transitions)
					}
					if entry.Result() != nil || len(tk.Messages()) != 0 || backend.Status(id) != runtimetest.StatusRunning {
						t.Fatalf("failed intent changed result, transcript, or runtime: result=%+v messages=%d runtime=%v", entry.Result(), len(tk.Messages()), backend.Status(id))
					}
				})
			})
		})
	}
	t.Run("QueueInput", func(t *testing.T) {
		t.Run("append_is_immutable_durable_and_rejects_stale_edits", func(t *testing.T) {
			t.Parallel()
			store := taskslog.NewStore(t.TempDir())
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), store, backends.Default("", nil))})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			first := agent.Prompt{Text: "first", Images: []v3.ImageData{{MediaType: "image/png", Data: "Zmlyc3Q="}}}
			if _, err := e.Lifecycle.QueueInput(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			old := e.PendingInput()
			tk.SetState(task.StateWaiting)
			second := agent.Prompt{Text: "second", Images: []v3.ImageData{{MediaType: "image/jpeg", Data: "c2Vjb25k"}}}
			if queued, err := e.Lifecycle.QueueInput(t.Context(), second); err != nil || !queued {
				t.Fatalf("append = %v, %v", queued, err)
			}
			q := e.PendingInput()
			want := agent.Prompt{Text: "first\nsecond", Images: []v3.ImageData{first.Images[0], second.Images[0]}}
			if q.ID == old.ID || !reflect.DeepEqual(q.Prompt, want) {
				t.Fatalf("appended input = %+v", q)
			}
			second.Images[0].Data = "caller mutation"
			if !reflect.DeepEqual(old.Prompt, first) || !reflect.DeepEqual(q.Prompt, want) {
				t.Fatal("append mutated an immutable prompt")
			}
			if err := e.Lifecycle.CancelQueuedInput(old.ID); err == nil {
				t.Fatal("stale edit removed appended content")
			}
			loaded, err := store.LoadInputQueue()
			if err != nil || len(loaded) != 1 || loaded[0].ID != q.ID.String() || loaded[0].Text != want.Text || !reflect.DeepEqual(loaded[0].Images, []inputqueue.Image{
				{MediaType: "image/png", Data: "Zmlyc3Q="},
				{MediaType: "image/jpeg", Data: "c2Vjb25k"},
			}) {
				t.Fatalf("persisted queue = %+v, %v", loaded, err)
			}
		})
		for _, tc := range []struct{ name, first, second, want string }{
			{"image then text", "", "second", "second"},
			{"text then image", "first", "", "first"},
			{"images only", "", "", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
				tk.SetState(task.StateRunning)
				e := m.NewEntry(tk, nil)
				m.Insert(tk.ID, e)
				for _, text := range []string{tc.first, tc.second} {
					if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: text, Images: []v3.ImageData{{MediaType: "image/png", Data: "aW1hZ2U="}}}); err != nil {
						t.Fatal(err)
					}
				}
				q := e.PendingInput()
				if q.Prompt.Text != tc.want || len(q.Prompt.Images) != 2 {
					t.Fatalf("appended prompt = %+v", q.Prompt)
				}
			})
		}
		t.Run("concurrent_appends_keep_every_message", func(t *testing.T) {
			t.Parallel()
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
			tk.SetState(task.StateRunning)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			var wg sync.WaitGroup
			for _, text := range []string{"one", "two", "three", "four"} {
				wg.Go(func() {
					if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: text}); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			lines := strings.Split(e.PendingInput().Prompt.Text, "\n")
			slices.Sort(lines)
			if !reflect.DeepEqual(lines, []string{"four", "one", "three", "two"}) {
				t.Fatalf("appended lines = %q", lines)
			}
		})
		for _, state := range []string{"delivering", "delivered"} {
			t.Run("append_rejects_"+state, func(t *testing.T) {
				t.Parallel()
				m := newTestManager(t, Config{ServerCtx: t.Context()})
				tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
				tk.SetState(task.StateRunning)
				e := m.NewEntry(tk, nil)
				m.Insert(tk.ID, e)
				if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "first"}); err != nil {
					t.Fatal(err)
				}
				q := *e.PendingInput()
				q.Delivering, q.Delivered = state == "delivering", state == "delivered"
				e.setPendingInput(&q)
				if _, err := e.Lifecycle.QueueInput(t.Context(), agent.Prompt{Text: "second"}); err == nil {
					t.Fatal("append changed a reserved prompt")
				}
				if e.PendingInput() != &q {
					t.Fatal("failed append changed pending input")
				}
			})
		}
		t.Run("startup_prompts_wait_for_a_ready_turn", func(t *testing.T) {
			t.Parallel()
			for _, state := range []task.State{task.StateBranching, task.StateProvisioning, task.StateStarting} {
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
			m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: taskhistory.NewStore(testLogger(), taskslog.NewStore(dir), backends.Default("", nil))})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
			tk.SetState(task.StateRunning)
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
			tk.SetState(task.StatePurged)
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
					tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "saved-session"})
					tk.SetState(task.StateWaiting)
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
	t.Run("finishPurge", func(t *testing.T) {
		t.Run("waits_for_the_operation_that_owns_the_incarnation", func(t *testing.T) {
			t.Parallel()
			fake := &runtimetest.FakeBackend{}
			m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, fake, nil)})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, harness.Claude, "")
			tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
			tk.SetState(task.StateStopped)
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			recordTestTaskLog(t, m, e)
			// A Stop that still owns the incarnation holds the lock while it
			// publishes its terminal result and trailer. The purge must not
			// transition the task or remove the runtime in the meantime.
			e.Lifecycle.operationMu.Lock()
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				e.Lifecycle.finishPurge(t.Context(), tk, false)
			}()
			select {
			case <-returned:
				e.Lifecycle.operationMu.Unlock()
				t.Fatal("finishPurge returned while a lifecycle operation held the incarnation")
			case <-time.After(20 * time.Millisecond):
			}
			e.Lifecycle.operationMu.Unlock()
			<-returned
			if got := tk.GetState(); got != task.StatePurged {
				t.Errorf("state = %s, want purged", got)
			}
			if got := fake.Status("ctr-1"); got != runtimetest.StatusPurged {
				t.Errorf("instance status = %s, want purged", got)
			}
		})
	})
	t.Run("reconnectImportedSession", func(t *testing.T) {
		t.Parallel()
		for _, state := range []task.State{task.StateAsking, task.StateHasPlan} {
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
					tk.SetSessionMetadata(agent.SessionMetadata{SessionID: "saved-session"})
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

// lifecycleTitleProvider gates the actual initial title request across transport settlement.
type lifecycleTitleProvider struct {
	genai.Provider

	started chan struct{}
	release chan struct{}
}

func (p *lifecycleTitleProvider) GenSync(ctx context.Context, _ genai.Messages, _ ...genai.GenOption) (genai.Result, error) {
	close(p.started)
	select {
	case <-p.release:
		return genai.Result{Replies: []genai.Reply{{Text: "Joined title"}}}, nil
	case <-ctx.Done():
		return genai.Result{}, ctx.Err()
	}
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
					backend := &reconnectInputBackend{FakeBackend: &agenttest.FakeBackend{HarnessName: harness.Claude}}
					b.Cleanup(backend.stop)
					tk := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Claude, "")
					e := m.NewEntry(tk, nil)
					m.Insert(tk.ID, e)
					startQueueSession(b, e, backend)
					if attached {
						for range 4 {
							other := mustNewTask(b, ksid.NewID(), agent.Prompt{Text: "initial"}, harness.Codex, "")
							other.SetState(task.StateRunning)
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
						tk.SetState(task.StateWaiting)
						if deferred {
							if _, err := e.Lifecycle.QueueInput(b.Context(), prompt); err != nil {
								b.Fatal(err)
							}
							m.drainInputQueue(b.Context())
							m.background.Wait()
						} else if err := e.Lifecycle.SendInput(b.Context(), prompt); err != nil {
							b.Fatal(err)
						}
						if tk.GetState() != task.StateRunning {
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
