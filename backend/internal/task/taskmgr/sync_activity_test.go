// Tests the sync recorder's bounds, ordering, and non-blocking sink, and the sync records Lifecycle.Sync writes.

package taskmgr

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun/taskruntest"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/caic-xyz/caic/internal/testlog"
)

// syncSink collects the records of a recorder under test.
type syncSink struct {
	mu      sync.Mutex
	records []agent.SyncActivityMessage
	gate    chan struct{} // When non-nil, output records wait for it to close.
	failOut error         // When non-nil, output records fail with it.
}

func (s *syncSink) record(_ context.Context, m agent.Message) error {
	sa, ok := m.(*agent.SyncActivityMessage)
	if !ok {
		return errors.New("not a sync activity record")
	}
	if sa.Phase == v3.SyncPhaseOutput {
		if s.gate != nil {
			<-s.gate
		}
		if s.failOut != nil {
			return s.failOut
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, *sa)
	return nil
}

func (s *syncSink) snapshot() []agent.SyncActivityMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.SyncActivityMessage(nil), s.records...)
}

func joinedOutput(records []agent.SyncActivityMessage) string {
	var b strings.Builder
	for i := range records {
		if records[i].Phase == v3.SyncPhaseOutput && !records[i].Truncated {
			b.WriteString(records[i].Output)
		}
	}
	return b.String()
}

func TestSyncRecorder(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("orders_start_output_end", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetOrigin, "caic-3")
			if _, err := io.WriteString(s, "one\ntwo\n"); err != nil {
				t.Fatal(err)
			}
			s.finish(v3.SyncStatusPushed, nil)
			got := sink.snapshot()
			if len(got) < 3 || got[0].Phase != v3.SyncPhaseStart || got[len(got)-1].Phase != v3.SyncPhaseEnd {
				t.Fatalf("records = %+v", got)
			}
			if got[0].Target != "origin" || got[0].Branch != "caic-3" {
				t.Errorf("start = %+v", got[0])
			}
			if end := got[len(got)-1]; end.Status != v3.SyncStatusPushed || end.Error != "" {
				t.Errorf("end = %+v", end)
			}
			for i := range got {
				if got[i].SyncID == "" || got[i].SyncID != got[0].SyncID || got[i].Ts == 0 {
					t.Errorf("record %d = %+v", i, got[i])
				}
			}
			if out := joinedOutput(got); out != "one\ntwo\n" {
				t.Errorf("output = %q", out)
			}
		})
		t.Run("bounds_output_with_marker", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetOrigin, "b")
			line := strings.Repeat("x", 999) + "\n"
			total := 0
			for total < 3*maxSyncOutputBytes {
				n, err := io.WriteString(s, line)
				if err != nil || n != len(line) {
					t.Fatalf("Write = %d, %v; a bounded sink still accepts output", n, err)
				}
				total += n
			}
			s.finish(v3.SyncStatusPushed, nil)
			got := sink.snapshot()
			if out := joinedOutput(got); len(out) != maxSyncOutputBytes {
				t.Errorf("persisted %d bytes, want exactly the bound %d", len(out), maxSyncOutputBytes)
			}
			var markers int
			for i := range got {
				if len(got[i].Output) > maxSyncChunkBytes && !got[i].Truncated {
					t.Errorf("record %d holds %d bytes, over the chunk bound", i, len(got[i].Output))
				}
				if got[i].Truncated {
					markers++
					if i != len(got)-2 || !strings.Contains(got[i].Output, "truncated") {
						t.Errorf("marker record %d = %+v, want the record before the end", i, got[i])
					}
				}
			}
			if markers != 1 || got[len(got)-1].Phase != v3.SyncPhaseEnd {
				t.Errorf("markers = %d, last = %+v", markers, got[len(got)-1])
			}
		})
		t.Run("coalesces_many_small_writes", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetOrigin, "b")
			for range 10000 {
				if _, err := io.WriteString(s, "progress line\n"); err != nil {
					t.Fatal(err)
				}
			}
			s.finish(v3.SyncStatusPushed, nil)
			got := sink.snapshot()
			if want := 10000 * len("progress line\n"); len(joinedOutput(got)) != want {
				t.Fatalf("persisted %d bytes, want %d", len(joinedOutput(got)), want)
			}
			// Start and end, the output in a few chunks, not one record per write.
			if len(got) > 14 {
				t.Errorf("%d records for 10000 writes", len(got))
			}
		})
		t.Run("keeps_runes_whole_across_flushes", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetOrigin, "b")
			if _, err := s.Write([]byte{'a', 0xC3}); err != nil {
				t.Fatal(err)
			}
			s.flush(false)
			if _, err := s.Write([]byte{0xA9}); err != nil {
				t.Fatal(err)
			}
			s.finish(v3.SyncStatusPushed, nil)
			for _, r := range sink.snapshot() {
				if !utf8.ValidString(r.Output) || strings.ContainsRune(r.Output, utf8.RuneError) {
					t.Errorf("record output %q is not whole UTF-8", r.Output)
				}
			}
			if out := joinedOutput(sink.snapshot()); out != "aé" {
				t.Errorf("output = %q", out)
			}
		})
		t.Run("sink_does_not_wait_for_the_log", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{gate: make(chan struct{})}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetOrigin, "b")
			written := make(chan error, 1)
			go func() {
				for range 200 {
					if _, err := io.WriteString(s, strings.Repeat("y", 1024)); err != nil {
						written <- err
						return
					}
				}
				written <- nil
			}()
			// Every output append blocks on the gate, yet the hook's writes finish.
			select {
			case err := <-written:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Write blocked on a stalled log append")
			}
			close(sink.gate)
			s.finish(v3.SyncStatusPushed, nil)
			if out := joinedOutput(sink.snapshot()); len(out) != 200*1024 {
				t.Errorf("persisted %d bytes, want %d", len(out), 200*1024)
			}
		})
		t.Run("end_error_is_bounded", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetDefault, "main")
			s.finish(v3.SyncStatusFailed, errors.New(strings.Repeat("é", maxSyncErrorBytes)))
			got := sink.snapshot()
			end := got[len(got)-1]
			if end.Status != v3.SyncStatusFailed || len(end.Error) > maxSyncErrorBytes+len("…[truncated]") || !utf8.ValidString(end.Error) || !strings.HasSuffix(end.Error, "[truncated]") {
				t.Errorf("end = %d byte error %q", len(end.Error), end.Error[max(0, len(end.Error)-20):])
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("failing_log_stops_the_sink_but_not_the_end_record", func(t *testing.T) {
			t.Parallel()
			sink := &syncSink{failOut: errors.New("log closed")}
			s := startSyncRecorder(t.Context(), testlog.New(t), sink.record, SyncTargetOrigin, "b")
			if _, err := io.WriteString(s, "x"); err != nil {
				t.Fatal(err)
			}
			s.finish(v3.SyncStatusFailed, errors.New("push failed"))
			if _, err := io.WriteString(s, "more"); err == nil {
				t.Error("Write after a log failure succeeded; the runtime must stop sending")
			}
			got := sink.snapshot()
			if end := got[len(got)-1]; end.Phase != v3.SyncPhaseEnd || end.Error != "push failed" {
				t.Errorf("records = %+v, want an end record", got)
			}
		})
	})
}

// hookBackend runs a scripted pre-push hook.
type hookBackend struct {
	*runtimetest.FakeBackend

	hook func(out io.Writer) error
}

func (b *hookBackend) RunPrePush(_ context.Context, _ runtime.ID, _ int, _ string, _ runtime.PushRefs, out io.Writer) error {
	return b.hook(out)
}

func newSyncTestEntry(t *testing.T, backend func(dir string, fetched []runtime.FetchedBranch) testRuntimeBackend) *Entry {
	dir := t.TempDir()
	initTaskRepo(t, dir, []string{"branch", "caic-0"})
	checkout, err := repo.NewCheckout(t.Context(), testlog.New(t), dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "caic-0")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	fetched := []runtime.FetchedBranch{{RepositoryPath: "/home/user/src/repo", BranchName: "caic-0", CommitHash: strings.TrimSpace(string(out))}}
	m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, backend(dir, fetched), nil)})
	registerCheckout(t, m.Checkouts, "repo", checkout)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
	tk.Repos = []repo.Mount{{Name: "repo", GitRoot: dir, ContainerPath: "~/src/repo", Branch: "caic-0"}}
	tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
	taskruntest.SetState(tk, task.StateWaiting)
	e := m.NewEntry(tk, nil)
	m.Insert(tk.ID, e)
	recordTestTaskLog(t, m, e)
	return e
}

// loggedSyncRecords returns the sync records persisted in the task log, in order.
func loggedSyncRecords(t *testing.T, e *Entry) []v3.SyncActivityMessage {
	var out []v3.SyncActivityMessage
	err := taskslog.ScanFile(e.LogPath.Get(), true, func(_ os.FileInfo, scanner *taskslog.Scanner, _ v3.MetaMessage) error {
		for scanner.Scan() {
			frame, err := taskslog.DecodeFrame(scanner.Frame().Encoded)
			if err != nil {
				return err
			}
			if sa, ok := frame.Control.(*v3.SyncActivityMessage); ok {
				out = append(out, *sa)
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLifecycleSyncRecords(t *testing.T) {
	t.Parallel()
	hooked := func(hook func(io.Writer) error) func(string, []runtime.FetchedBranch) testRuntimeBackend {
		return func(_ string, fetched []runtime.FetchedBranch) testRuntimeBackend {
			return &hookBackend{FakeBackend: &runtimetest.FakeBackend{FetchedBranches: fetched}, hook: hook}
		}
	}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("pushed_sync_records_start_output_end", func(t *testing.T) {
			t.Parallel()
			e := newSyncTestEntry(t, hooked(func(out io.Writer) error {
				_, err := io.WriteString(out, "lint ok\ntests ok\n")
				return err
			}))
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err != nil {
				t.Fatal(err)
			}
			got := loggedSyncRecords(t, e)
			if len(got) < 3 || got[0].Phase != v3.SyncPhaseStart || got[len(got)-1].Phase != v3.SyncPhaseEnd {
				t.Fatalf("records = %+v", got)
			}
			if got[0].Target != "origin" || got[0].Branch != "caic-0" {
				t.Errorf("start = %+v", got[0])
			}
			var out strings.Builder
			for _, r := range got[1 : len(got)-1] {
				if r.Phase != v3.SyncPhaseOutput || r.SyncID != got[0].SyncID {
					t.Errorf("middle record = %+v", r)
				}
				out.WriteString(r.Output)
			}
			if out.String() != "lint ok\ntests ok\n" {
				t.Errorf("output = %q", out.String())
			}
			if end := got[len(got)-1]; end.Status != v3.SyncStatusPushed || end.Error != "" || end.SyncID != got[0].SyncID {
				t.Errorf("end = %+v", end)
			}
		})
		t.Run("second_sync_gets_its_own_id", func(t *testing.T) {
			t.Parallel()
			e := newSyncTestEntry(t, hooked(func(io.Writer) error { return nil }))
			for range 2 {
				if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err != nil {
					t.Fatal(err)
				}
			}
			got := loggedSyncRecords(t, e)
			if len(got) != 4 || got[0].SyncID == got[2].SyncID || got[0].SyncID != got[1].SyncID || got[2].SyncID != got[3].SyncID {
				t.Fatalf("records = %+v, want two start/end pairs with distinct ids", got)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("rejected_hook_still_writes_end_with_error", func(t *testing.T) {
			t.Parallel()
			e := newSyncTestEntry(t, hooked(func(out io.Writer) error {
				if _, err := io.WriteString(out, "lint failed\n"); err != nil {
					return err
				}
				return errors.New("hook exited 1")
			}))
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err == nil {
				t.Fatal("Sync succeeded despite the rejected hook")
			}
			got := loggedSyncRecords(t, e)
			if len(got) != 3 || got[0].Phase != v3.SyncPhaseStart || got[1].Output != "lint failed\n" {
				t.Fatalf("records = %+v", got)
			}
			if end := got[2]; end.Phase != v3.SyncPhaseEnd || end.Status != v3.SyncStatusFailed || !strings.Contains(end.Error, "hook exited 1") {
				t.Errorf("end = %+v", end)
			}
		})
		t.Run("canceled_caller_still_records_the_end", func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			e := newSyncTestEntry(t, hooked(func(io.Writer) error {
				cancel()
				return context.Canceled
			}))
			if _, err := e.Lifecycle.Sync(ctx, SyncTargetOrigin, false); err == nil {
				t.Fatal("Sync succeeded despite cancellation")
			}
			got := loggedSyncRecords(t, e)
			if len(got) != 2 || got[1].Phase != v3.SyncPhaseEnd || got[1].Status != v3.SyncStatusFailed {
				t.Fatalf("records = %+v, want start and a failed end", got)
			}
		})
		t.Run("rejected_up_front_writes_nothing", func(t *testing.T) {
			t.Parallel()
			e := newSyncTestEntry(t, hooked(func(io.Writer) error { return nil }))
			taskruntest.SetState(e.Task(), task.StatePurging)
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err == nil {
				t.Fatal("Sync of a purging task succeeded")
			}
			taskruntest.SetState(e.Task(), task.StateWaiting)
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetDefault, true); err == nil {
				t.Fatal("forced default sync succeeded")
			}
			if got := loggedSyncRecords(t, e); len(got) != 0 {
				t.Fatalf("records = %+v, want none", got)
			}
		})
	})
}

// ctxSyncBackend blocks a sync step until its context ends or release closes.
type ctxSyncBackend struct {
	*runtimetest.FakeBackend

	started     chan struct{} // Closed when the blocking step starts.
	release     chan struct{} // Closed to let the step succeed.
	once        sync.Once
	ctxErr      chan error // Receives the context error of a cancelled step.
	rootOnly    bool       // Let submodule hooks finish before blocking on the root.
	refreshOnly bool       // Let all hooks finish before blocking on refresh.
}

func (b *ctxSyncBackend) RunPrePush(ctx context.Context, _ runtime.ID, _ int, _ string, dest runtime.PushRefs, _ io.Writer) error {
	if b.refreshOnly || (b.rootOnly && dest.Submodule != nil) {
		return nil
	}
	return b.wait(ctx)
}

func (b *ctxSyncBackend) RefreshRefs(ctx context.Context, _ runtime.ID) error {
	if b.refreshOnly {
		return b.wait(ctx)
	}
	return nil
}

func (b *ctxSyncBackend) wait(ctx context.Context) error {
	first := false
	b.once.Do(func() { first = true })
	if !first {
		return nil
	}
	close(b.started)
	select {
	case <-ctx.Done():
		b.ctxErr <- ctx.Err()
		return ctx.Err()
	case <-b.release:
		return nil
	}
}

func TestLifecycleCancelSync(t *testing.T) {
	t.Parallel()
	newBlocked := func(t *testing.T) (*Entry, *ctxSyncBackend) {
		backend := &ctxSyncBackend{started: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
		e := newSyncTestEntry(t, func(_ string, fetched []runtime.FetchedBranch) testRuntimeBackend {
			backend.FakeBackend = &runtimetest.FakeBackend{FetchedBranches: fetched}
			return backend
		})
		return e, backend
	}
	runSync := func(ctx context.Context, e *Entry) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := e.Lifecycle.Sync(ctx, SyncTargetOrigin, false)
			done <- err
		}()
		return done
	}
	waitStarted := func(t *testing.T, b *ctxSyncBackend) {
		select {
		case <-b.started:
		case <-time.After(30 * time.Second):
			t.Fatal("sync step did not start")
		}
	}
	waitDone := func(t *testing.T, done <-chan error) error {
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("Sync did not return")
			return nil
		}
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("cancels_the_hook_and_records_cancelled", func(t *testing.T) {
			t.Parallel()
			e, b := newBlocked(t)
			done := runSync(t.Context(), e)
			waitStarted(t, b)
			if err := e.Lifecycle.CancelSync(); err != nil {
				t.Fatal(err)
			}
			err := waitDone(t, done)
			if got, ok := errors.AsType[*Error](err); !ok || got.Kind != KindConflict || !strings.Contains(got.Msg, "cancelled") {
				t.Fatalf("Sync error = %v, want a cancelled conflict", err)
			}
			got := loggedSyncRecords(t, e)
			if len(got) != 2 || got[1].Phase != v3.SyncPhaseEnd || got[1].Status != v3.SyncStatusCancelled || got[1].Error != "" {
				t.Fatalf("records = %+v, want start and a cancelled end without details", got)
			}
			// The cancelled sync released its lock: a following sync succeeds.
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err != nil {
				t.Fatalf("sync after cancel: %v", err)
			}
			got = loggedSyncRecords(t, e)
			if len(got) != 4 || got[3].Status != v3.SyncStatusPushed {
				t.Fatalf("records = %+v, want a pushed end after the cancelled one", got)
			}
		})
		for _, fixture := range []struct {
			name    string
			refresh bool
			failed  string
		}{
			{"reports the child update when cancelled during the parent hook", false, `pre-push hook for "~/src/repo" on remote "origin" branch "caic-0" failed`},
			{"reports both updates when cancelled during refresh", true, "push completed but refreshing container upstream refs failed"},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				t.Parallel()
				child := t.TempDir()
				initTaskRepo(t, child)
				childTip := preparationGit(t, child, "rev-parse", "HEAD")
				parentRemote := filepath.Join(t.TempDir(), "remote.git")
				var parentTip string
				b := &ctxSyncBackend{
					started: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1),
					rootOnly: true, refreshOnly: fixture.refresh,
				}
				e := newSyncTestEntry(t, func(root string, fetched []runtime.FetchedBranch) testRuntimeBackend {
					preparationGit(t, root, "init", "--bare", parentRemote)
					preparationGit(t, root, "remote", "set-url", "origin", parentRemote)
					preparationGit(t, root, "push", "origin", "main")
					preparationGit(t, root, "checkout", "caic-0")
					preparationGit(t, root, "update-index", "--add", "--cacheinfo", "160000,"+childTip+",lib")
					preparationGit(t, root, "commit", "-m", "Pin child")
					parentTip = preparationGit(t, root, "rev-parse", "HEAD")
					fetched[0].CommitHash = parentTip
					preparationGit(t, root, "checkout", "main")
					b.FakeBackend = &runtimetest.FakeBackend{FetchedBranches: append(fetched, runtime.FetchedBranch{
						RepositoryPath: fetched[0].RepositoryPath + "/lib", Submodule: "lib",
						GitDir: filepath.Join(child, ".git"), CommitHash: childTip,
					})}
					return b
				})
				done := runSync(t.Context(), e)
				waitStarted(t, b)
				if err := e.Lifecycle.CancelSync(); err != nil {
					t.Fatal(err)
				}
				err := waitDone(t, done)
				want := fixture.failed + ` after Git confirmed updates to "/home/user/src/repo/lib" on remote "origin" branch "caic-0" to ` + childTip
				if fixture.refresh {
					want += `, "~/src/repo" on remote "origin" branch "caic-0" to ` + parentTip
				}
				if got, ok := errors.AsType[*Error](err); !ok || got.Kind != KindConflict || !strings.HasPrefix(err.Error(), "sync cancelled: "+want+": ") {
					t.Fatalf("Sync error = %v, want cancelled conflict containing %q", err, want)
				}
				if _, ok := errors.AsType[*repo.PushRefreshError](err); ok != fixture.refresh || !errors.Is(err, context.Canceled) {
					t.Fatalf("Sync error = %v, want cancellation with refresh error = %t", err, fixture.refresh)
				}
				records := loggedSyncRecords(t, e)
				if len(records) == 0 {
					t.Fatal("no sync records")
				}
				if end := records[len(records)-1]; end.Status != v3.SyncStatusCancelled || !strings.HasPrefix(end.Error, want+": ") {
					t.Fatalf("end record = %+v, want cancellation containing %q", end, want)
				}
				if got := preparationGit(t, child, "rev-parse", "refs/heads/caic-0"); got != childTip {
					t.Fatalf("child branch = %s, want %s", got, childTip)
				}
				got := preparationGit(t, parentRemote, "for-each-ref", "--format=%(objectname)", "refs/heads/caic-0")
				if (fixture.refresh && got != parentTip) || (!fixture.refresh && got != "") {
					t.Fatalf("parent branch = %q, want update to %q only on refresh failure", got, parentTip)
				}
			})
		}
		t.Run("request_cancel_alone_does_not_cancel", func(t *testing.T) {
			t.Parallel()
			e, b := newBlocked(t)
			ctx, cancel := context.WithCancel(t.Context())
			done := runSync(ctx, e)
			waitStarted(t, b)
			cancel()
			select {
			case err := <-b.ctxErr:
				t.Fatalf("hook context ended with the request: %v", err)
			case err := <-done:
				t.Fatalf("Sync ended with the request: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(b.release)
			// The summary refresh still follows the gone request; the push outcome is what counts.
			_ = waitDone(t, done)
			got := loggedSyncRecords(t, e)
			if end := got[len(got)-1]; end.Status != v3.SyncStatusPushed {
				t.Fatalf("records = %+v, want a pushed end", got)
			}
		})
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		t.Run("idle_cancel_is_a_conflict", func(t *testing.T) {
			t.Parallel()
			e, b := newBlocked(t)
			close(b.release)
			err := e.Lifecycle.CancelSync()
			if got, ok := errors.AsType[*Error](err); !ok || got.Kind != KindConflict {
				t.Fatalf("CancelSync = %v, want a conflict", err)
			}
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err != nil {
				t.Fatal(err)
			}
			if err := e.Lifecycle.CancelSync(); err == nil {
				t.Fatal("CancelSync after the sync ended succeeded")
			}
		})
	})
}

func TestEndInterruptedSync(t *testing.T) {
	t.Parallel()
	// seed writes the given sync records to the log of a task owned by a first
	// manager, then returns that manager's log store and the task ID.
	seed := func(t *testing.T, records ...*agent.SyncActivityMessage) (*taskhistory.Store, ksid.ID) {
		m := newTestManager(t, Config{ServerCtx: t.Context()})
		tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
		e := m.NewEntry(tk, nil)
		recordTestTaskLog(t, m, e)
		for _, r := range records {
			if err := e.RecordHost(t.Context(), r); err != nil {
				t.Fatal(err)
			}
		}
		return m.logStore, tk.ID
	}
	// restore starts a new manager over the store, as after a server restart.
	restore := func(t *testing.T, store *taskhistory.Store, id ksid.ID) *Entry {
		m := newTestManager(t, Config{ServerCtx: t.Context(), LogStore: store})
		plain, err := store.LoadUnsettled()
		if err != nil {
			t.Fatal(err)
		}
		if err := m.LoadUnsettledTasks(plain); err != nil {
			t.Fatal(err)
		}
		e, ok := m.GetEntry(id)
		if !ok {
			t.Fatalf("task %s not restored", id)
		}
		return e
	}
	start := &agent.SyncActivityMessage{SyncID: "s1", Phase: v3.SyncPhaseStart, Target: "origin", Branch: "caic-0"}
	out := &agent.SyncActivityMessage{SyncID: "s1", Phase: v3.SyncPhaseOutput, Output: "lint\n"}
	end := &agent.SyncActivityMessage{SyncID: "s1", Phase: v3.SyncPhaseEnd, Status: v3.SyncStatusPushed}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		t.Run("unended_sync_fails_once", func(t *testing.T) {
			t.Parallel()
			store, id := seed(t, start, out)
			e := restore(t, store, id)
			got := loggedSyncRecords(t, e)
			if len(got) != 3 {
				t.Fatalf("records = %+v, want start, output, end", got)
			}
			last := got[2]
			if last.Phase != v3.SyncPhaseEnd || last.SyncID != "s1" || last.Status != v3.SyncStatusFailed || last.Error != syncInterruptedError || last.Ts == 0 {
				t.Errorf("end = %+v", last)
			}
			// A second restart finds the sync ended and writes nothing.
			e = restore(t, store, id)
			if got := loggedSyncRecords(t, e); len(got) != 3 {
				t.Errorf("after second restore records = %+v, want 3", got)
			}
		})
		t.Run("ended_sync_is_untouched", func(t *testing.T) {
			t.Parallel()
			store, id := seed(t, start, out, end)
			e := restore(t, store, id)
			if got := loggedSyncRecords(t, e); len(got) != 3 {
				t.Errorf("records = %+v, want 3", got)
			}
		})
		t.Run("live_task_is_not_closed", func(t *testing.T) {
			t.Parallel()
			// The task is already registered in this process: restore skips it.
			m := newTestManager(t, Config{ServerCtx: t.Context()})
			tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
			e := m.NewEntry(tk, nil)
			m.Insert(tk.ID, e)
			recordTestTaskLog(t, m, e)
			if err := e.RecordHost(t.Context(), start); err != nil {
				t.Fatal(err)
			}
			plain, err := m.logStore.LoadUnsettled()
			if err != nil {
				t.Fatal(err)
			}
			if err := m.LoadUnsettledTasks(plain); err != nil {
				t.Fatal(err)
			}
			if got := loggedSyncRecords(t, e); len(got) != 1 {
				t.Errorf("records = %+v, want only the running start", got)
			}
		})
	})
}
