// Tests the sync recorder's bounds, ordering, and non-blocking sink, and the sync records Lifecycle.Sync writes.

package taskmgr

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetOrigin, "caic-3")
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetOrigin, "b")
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetOrigin, "b")
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetOrigin, "b")
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetOrigin, "b")
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetDefault, "main")
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
			s := startSyncRecorder(t.Context(), testLogger(), sink.record, SyncTargetOrigin, "b")
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

func newSyncTestEntry(t *testing.T, backend func(fetched []runtime.FetchedBranch) testRuntimeBackend) *Entry {
	dir := t.TempDir()
	initTaskRepo(t, dir, []string{"branch", "caic-0"})
	checkout, err := repo.NewCheckout(t.Context(), testLogger(), dir, "", nil)
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
	m := newTestManager(t, Config{ServerCtx: t.Context(), Runtimes: newTestRuntime(t, backend(fetched), nil)})
	registerCheckout(t, m.Checkouts, "repo", checkout)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "x"}, "", "")
	tk.Repos = []repo.Mount{{Name: "repo", GitRoot: dir, ContainerPath: "~/src/repo", Branch: "caic-0"}}
	tk.SetRuntimeConnectionInfo("test-runtime:ctr", runtime.ConnectionTarget{}, "", "", 0)
	tk.SetState(task.StateWaiting)
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
	hooked := func(hook func(io.Writer) error) func([]runtime.FetchedBranch) testRuntimeBackend {
		return func(fetched []runtime.FetchedBranch) testRuntimeBackend {
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
			e.Task().SetState(task.StatePurging)
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetOrigin, false); err == nil {
				t.Fatal("Sync of a purging task succeeded")
			}
			e.Task().SetState(task.StateWaiting)
			if _, err := e.Lifecycle.Sync(t.Context(), SyncTargetDefault, true); err == nil {
				t.Fatal("forced default sync succeeded")
			}
			if got := loggedSyncRecords(t, e); len(got) != 0 {
				t.Fatalf("records = %+v, want none", got)
			}
		})
	})
}
