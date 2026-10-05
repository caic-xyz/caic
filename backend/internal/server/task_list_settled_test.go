// Tests task-list restoration status, complete snapshots, and registered membership during DTO failures.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"
)

// readNextTaskListEvent blocks until one SSE event with a data payload is
// delivered and returns the decoded TaskListEvent.
func readNextTaskListEvent(t *testing.T, r *bufio.Reader) v1.TaskListEvent {
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read task-list SSE: %v", err)
		}
		line = strings.TrimSuffix(line, "\r\n")
		if line == "" {
			break // end of one event frame
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
		}
	}
	if len(data) == 0 {
		t.Fatal("task-list SSE event had no data payload")
	}
	var ev v1.TaskListEvent
	if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &ev); err != nil {
		t.Fatalf("parse task-list SSE event: %v", err)
	}
	return ev
}

// connectTaskListStream opens a raw TCP SSE connection to the task-list
// endpoint and returns a reader positioned after the HTTP response head.
func connectTaskListStream(t *testing.T, s *testRouter) *bufio.Reader {
	h, err := s.buildHandler()
	if err != nil {
		t.Fatalf("buildHandler: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	host, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("dial task-list stream: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/events", http.NoBody)
	if err != nil {
		t.Fatalf("new task-list request: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write task-list request: %v", err)
	}

	// Skip the response head (status line + headers up to the blank line) so
	// the returned reader is positioned at the start of the SSE body. Parsing
	// the head manually avoids a Response body that would block on close.
	head := bufio.NewReader(conn)
	statusLine, err := head.ReadString('\n')
	if err != nil {
		t.Fatalf("read task-list status line: %v", err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		t.Fatalf("task-list stream status line = %q, want 200", strings.TrimSpace(statusLine))
	}
	for {
		line, err := head.ReadString('\n')
		if err != nil {
			t.Fatalf("read task-list response head: %v", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break // end of headers
		}
	}
	return head
}

// TestTaskListEventsRestorationStatus verifies the settled pass state is emitted as
// a status event on connect (before the snapshot), that the snapshot does not
// carry status, and that a status event is emitted on the completion transition.
func TestTaskListEventsRestorationStatus(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, finish error, wantErr string) {
		s := newTestRouter(t, nil)
		r := connectTaskListStream(t, s)

		// The pass state is emitted as a status event before the snapshot so the
		// client's indicator and empty-list handling are correct on first paint.
		initEv := readNextTaskListEvent(t, r)
		if initEv.Kind != "status" || initEv.Status == nil || !initEv.Status.Loading || initEv.Status.Error != "" {
			t.Fatalf("initial event = %+v, want kind status with loading=true", initEv)
		}

		snap := readNextTaskListEvent(t, r)
		if snap.Kind != "snapshot" {
			t.Fatalf("second event = %+v, want kind snapshot", snap)
		}
		if snap.Status != nil {
			t.Fatalf("snapshot carries status = %+v, want nil", snap.Status)
		}

		repos := readNextTaskListEvent(t, r)
		if repos.Kind != "repos" {
			t.Fatalf("third event = %+v, want kind repos", repos)
		}

		s.taskMgr.CompleteRestoration(finish)
		ev := readNextTaskListEvent(t, r)
		if ev.Kind != "status" || ev.Status == nil || ev.Status.Loading || ev.Status.Error != wantErr {
			t.Fatalf("status = %+v, want settled status with error %q", ev, wantErr)
		}
	}

	t.Run("completed", func(t *testing.T) {
		t.Parallel()
		check(t, nil, "")
	})
	t.Run("failed", func(t *testing.T) {
		t.Parallel()
		check(t, errors.New("load purged tasks: boom"), "load purged tasks: boom")
	})
}

func TestTaskListCompleteSnapshotAfterRestoration(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	r := connectTaskListStream(t, s)
	readNextTaskListEvent(t, r) // restoration status
	initial := readNextTaskListEvent(t, r)
	if initial.Kind != "snapshot" || initial.Complete == nil || *initial.Complete {
		t.Fatalf("initial snapshot = %+v, want explicit incomplete", initial)
	}
	readNextTaskListEvent(t, r) // repositories
	s.taskMgr.CompleteRestoration(nil)
	status := readNextTaskListEvent(t, r)
	if status.Kind != "status" || status.Status == nil || status.Status.Loading {
		t.Fatalf("completion status = %+v", status)
	}
	complete := readNextTaskListEvent(t, r)
	if complete.Kind != "snapshot" || complete.Complete == nil || !*complete.Complete || complete.Snapshot == nil {
		t.Fatalf("completed snapshot = %+v", complete)
	}
}

func TestTaskListDTOFailurePreservesMembership(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "private prompt"}, harness.Claude)
	insertTestTask(s, tk.ID, tk)
	s.taskMgr.CompleteRestoration(nil)
	r := connectTaskListStream(t, s)
	readNextTaskListEvent(t, r)
	initial := readNextTaskListEvent(t, r)
	if initial.Kind != "snapshot" || len(initial.Snapshot) != 1 || initial.Complete == nil || !*initial.Complete {
		t.Fatalf("initial snapshot = %+v", initial)
	}
	readNextTaskListEvent(t, r)
	tk.SetState(taskslog.State("invalid-state"))
	out, _, membership := testTaskHandlers(s).taskSvc.taskListSnapshotWithReplay(testHTTPContext(t), map[string]uint64{})
	if len(out) != 0 {
		t.Fatalf("failed DTO count = %d", len(out))
	}
	if !slices.Contains(membership, tk.ID.String()) {
		t.Fatal("DTO failure erased authorized registered membership")
	}
	s.taskMgr.NotifyTaskChange()
	// A warning is a deterministic barrier after the task-diff pass. The stream
	// must not synthesize a delete before it despite the failed conversion.
	testTaskHandlers(s).warnings.UpdateRuntimeRestore(&taskmgr.ImportError{Failed: 1, Err: errors.New("barrier")})
	for {
		event := readNextTaskListEvent(t, r)
		if event.Kind == "delete" {
			t.Fatal("DTO failure produced a false deletion")
		}
		if event.Kind == "warning" {
			break
		}
	}
	tk.SetState(taskslog.StateWaiting)
	s.taskMgr.NotifyTaskChange()
	for {
		event := readNextTaskListEvent(t, r)
		if event.Kind == "snapshot" {
			if event.Complete == nil || !*event.Complete || len(event.Snapshot) != 1 {
				t.Fatalf("recovered snapshot = %+v", event)
			}
			break
		}
	}
}

func TestTaskListIncompleteDTOOnConnect(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "private prompt"}, harness.Claude)
	tk.SetState(taskslog.State("invalid-state"))
	insertTestTask(s, tk.ID, tk)
	s.taskMgr.CompleteRestoration(nil)
	ctx, cancel := context.WithCancel(testHTTPContext(t))
	cancel()
	w := httptest.NewRecorder()
	testTaskHandlers(s).handleTaskListEvents(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/events", nil))
	for line := range strings.SplitSeq(w.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event v1.TaskListEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "snapshot" {
			if event.Complete == nil || *event.Complete || len(event.Snapshot) != 0 {
				t.Fatalf("failed conversion snapshot = %+v", event)
			}
			return
		}
	}
	t.Fatal("missing snapshot")
}
