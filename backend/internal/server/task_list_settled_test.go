// Tests for the settled-history status carried by the task-list SSE stream.

package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
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

// TestTaskListEventsSettledStatus verifies the settled pass state is emitted as
// a status event on connect (before the snapshot), that the snapshot does not
// carry status, and that a status event is emitted on the completion transition.
func TestTaskListEventsSettledStatus(t *testing.T) {
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

		s.taskMgr.CompleteSettledLoad(finish)
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
