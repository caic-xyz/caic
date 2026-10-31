// Tests ordered turn barriers, transport replacement, and pre-ready recording ownership.

package taskrun

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
)

type turnBarrierRuntime struct {
	*runtimetest.FakeBackend

	started chan struct{}
	release chan struct{}
}

func (r *turnBarrierRuntime) TurnSnapshot(ctx context.Context, id runtime.ID, previous []runtime.FetchedBranch) ([]runtime.TurnRepository, error) {
	close(r.started)
	select {
	case <-r.release:
		return r.FakeBackend.TurnSnapshot(ctx, id, previous)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type setupBarrierRuntime struct {
	*runtimetest.FakeBackend

	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	abort    chan struct{}
}

func (r *setupBarrierRuntime) Launch(ctx context.Context, _ []runtime.Repo, opts *runtime.StartOptions) (runtime.ID, error) {
	if _, err := io.WriteString(opts.LogWriter, "starting instance\n"); err != nil {
		return "", err
	}
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	select {
	case <-r.release:
	case <-r.abort:
	}
	return "", ctx.Err()
}

type drainingTaskInput struct {
	io.WriteCloser

	ctx     context.Context
	records *agent.Records
	closing chan struct{}
	release chan struct{}
	abort   chan struct{}
}

func (w *drainingTaskInput) Close() error {
	close(w.closing)
	select {
	case <-w.release:
	case <-w.abort:
	}
	err := w.records.AppendMessage(w.ctx, &agent.LogMessage{Line: "old producer finished"})
	return errors.Join(err, w.WriteCloser.Close())
}

type replacingTaskBackend struct {
	*agenttest.FakeBackend

	closing chan struct{}
	release chan struct{}
	abort   chan struct{}
	started int
}

func (b *replacingTaskBackend) Start(ctx context.Context, opts *agent.Options) (*agent.Session, error) {
	line := "new transport"
	if b.started == 0 {
		line = "old transport"
	}
	if err := opts.Records.AppendMessage(ctx, &agent.LogMessage{Line: line}); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "python3", "-c", "input()")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, stdin.Close(), stdout.Close())
	}
	input := stdin
	if b.started == 0 {
		input = &drainingTaskInput{WriteCloser: stdin, ctx: context.WithoutCancel(ctx), records: opts.Records, closing: b.closing, release: b.release, abort: b.abort}
	}
	b.started++
	return agent.NewSession(ctx, cmd, agent.NewConn(ctx, opts.Logger, input, opts.Records, nil), stdout, opts.Logger), nil
}

func TestRun(t *testing.T) {
	t.Parallel()
	t.Run("PrepareRecords", func(t *testing.T) {
		t.Parallel()
		t.Run("turn_snapshot_precedes_later_input", func(t *testing.T) {
			t.Parallel()
			id := ksid.NewID()
			synctest.Test(t, func(t *testing.T) {
				backend := &turnBarrierRuntime{FakeBackend: testContainer(), started: make(chan struct{}), release: make(chan struct{})}
				backend.FetchedBranches = []runtime.FetchedBranch{{RepositoryPath: "/repo", BranchName: "caic-0", CommitHash: "2222222222222222222222222222222222222222"}}
				r := newTestAgentRuntime(t, newTestCheckout(t, "", "/repo", backend), "", nil)
				tk := mustNewTask(t, id, agent.Prompt{Text: "test"}, harness.Claude, "", "")
				tk.Repos = []repo.Mount{{Branch: "caic-0", ContainerPath: "/repo"}}
				tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
				tk.SetState(task.StateRunning)
				run := beginTestExecution(t, r, tk)
				result := make(chan error, 1)
				input := make(chan error, 1)
				written := make(chan struct{}, 1)
				var workers sync.WaitGroup
				workers.Go(func() {
					result <- taskNative(t.Context(), run.records, `{"type":"result","result":"done","num_turns":1}`)
				})
				<-backend.started
				workers.Go(func() {
					data := []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"next turn\"}}\n")
					input <- run.records.WriteInput(t.Context(), taskInputWriter(func(p []byte) (int, error) { written <- struct{}{}; return len(p), nil }), data)
				})
				synctest.Wait()
				if tk.MessageCount() != 0 {
					t.Fatal("turn published before its host snapshot")
				}
				select {
				case <-written:
					t.Fatal("later command delivered before turn snapshot")
				default:
				}
				close(backend.release)
				workers.Wait()
				if err := <-result; err != nil {
					t.Fatal(err)
				}
				if err := <-input; err != nil {
					t.Fatal(err)
				}
				messages := tk.Messages()
				types := make([]string, 0, len(messages))
				for _, message := range messages {
					types = append(types, message.Type())
				}
				want := []string{"result", "caic_diff_stat", "caic_turn_commit_snapshot", "user_input"}
				if !slices.Equal(types, want) {
					t.Fatalf("messages = %v, want %v", types, want)
				}
				rows, err := r.LogStore.LoadAllReadOnly()
				if err != nil || len(rows) != 1 {
					t.Fatalf("load = %v, %v", rows, err)
				}
				if err := rows[0].LoadMessages(); err != nil {
					t.Fatal(err)
				}
				replay := make([]string, 0, len(rows[0].Timeline))
				for _, message := range rows[0].Timeline {
					replay = append(replay, message.Message.Type())
				}
				if !slices.Equal(replay, want) {
					t.Fatalf("replay = %v, want %v", replay, want)
				}
			})
		})
	})
	t.Run("context_replacement_joins_old_producer", func(t *testing.T) {
		t.Parallel()
		backend := &replacingTaskBackend{FakeBackend: &agenttest.FakeBackend{WireFactory: claudecode.New().NewWire}, closing: make(chan struct{}), release: make(chan struct{}), abort: make(chan struct{})}
		r := newTestAgentRuntime(t, nil, "", agent.Backends{harness.Claude: backend})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
		tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
		tk.SetState(task.StateWaiting)
		var workers sync.WaitGroup
		t.Cleanup(func() {
			close(backend.abort)
			if err := r.Shutdown(context.WithoutCancel(t.Context()), tk); err != nil {
				t.Error(err)
			}
			workers.Wait()
		})
		if _, err := r.StartSession(t.Context(), tk, agent.Prompt{}); err != nil {
			t.Fatal(err)
		}
		replaced := make(chan error, 1)
		workers.Go(func() { _, err := r.ClearContextSession(t.Context(), tk); replaced <- err })
		<-backend.closing
		select {
		case err := <-replaced:
			t.Fatalf("replacement finished before producer drain: %v", err)
		default:
		}
		for _, message := range tk.Messages() {
			if system, ok := message.(*agent.SystemMessage); ok && system.Subtype == "context_cleared" {
				t.Fatal("context cleared while old producer still active")
			}
		}
		close(backend.release)
		if err := <-replaced; err != nil {
			t.Fatal(err)
		}
		var order []string
		for _, message := range tk.Messages() {
			switch message := message.(type) {
			case *agent.LogMessage:
				order = append(order, message.Line)
			case *agent.SystemMessage:
				order = append(order, message.Subtype)
			}
		}
		want := []string{"old transport", "old producer finished", "context_cleared", "new transport"}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("record order = %v, want %v", order, want)
		}
	})
	t.Run("shutdown_joins_pre_ready_setup", func(t *testing.T) {
		t.Parallel()
		backend := &setupBarrierRuntime{FakeBackend: &runtimetest.FakeBackend{}, started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), abort: make(chan struct{})}
		r := newTestAgentRuntimeWithRuntime(t, backend, nil, "")
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude, "", "")
		started := make(chan error, 1)
		stopped := make(chan error, 1)
		var workers sync.WaitGroup
		t.Cleanup(func() {
			close(backend.abort)
			if err := r.Shutdown(context.WithoutCancel(t.Context()), tk); err != nil {
				t.Error(err)
			}
			workers.Wait()
		})
		workers.Go(func() { _, err := r.Start(t.Context(), tk, ""); started <- err })
		<-backend.started
		if tk.HasSession() || tk.run.Load() == nil {
			t.Fatal("setup has no recording owner before transport attachment")
		}
		workers.Go(func() { stopped <- r.Shutdown(t.Context(), tk) })
		<-backend.canceled
		select {
		case err := <-stopped:
			t.Fatalf("shutdown returned before setup joined: %v", err)
		default:
		}
		close(backend.release)
		if err := <-started; !errors.Is(err, context.Canceled) {
			t.Fatalf("startup = %v, want cancellation", err)
		}
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		if tk.HasSession() || tk.run.Load() != nil {
			t.Fatal("shutdown retained a session or recording owner")
		}
		rows, err := r.LogStore.LoadAllReadOnly()
		if err != nil || len(rows) != 1 {
			t.Fatalf("load = %v, %v", rows, err)
		}
		if err := rows[0].LoadMessages(); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, message := range rows[0].Timeline {
			if log, ok := message.Message.(*agent.LogMessage); ok {
				lines = append(lines, log.Line)
			}
		}
		if len(lines) != 2 || lines[0] != "starting instance" || rows[0].LastTrailer.StartupFailure == nil {
			t.Fatalf("retained setup = %v, trailer = %+v", lines, rows[0].LastTrailer)
		}
	})
}
