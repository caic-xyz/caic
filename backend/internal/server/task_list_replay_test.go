// Tests for replaying short-lived task state transitions on the task-list SSE stream.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// TestTaskListEventsReplayTransitions verifies that a task whose state advances
// twice between two snapshots still delivers the interrupted state. The
// handler reads a snapshot in which the state has already advanced to stopped,
// so only the per-task transition journal can recover stopping.
func TestTaskListEventsReplayTransitions(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude)
	tk.SetState(taskslog.StateWaiting)
	insertTestTask(s, tk.ID.String(), tk)

	r := connectTaskListStream(t, s)
	for range 3 {
		// Initial connect emits status, snapshot, then repos.
		readNextTaskListEvent(t, r)
	}

	// Both transitions land before the stream's next snapshot, reproducing a
	// purge whose stop finishes faster than the snapshot loop.
	tk.SetState(taskslog.StateStopping)
	tk.SetState(taskslog.StateStopped)
	s.taskMgr.NotifyTaskChange()

	var states []string
	for len(states) < 2 {
		ev := readNextTaskListEvent(t, r)
		if ev.Kind != "patch" || ev.Patch == nil {
			continue
		}
		raw, ok := ev.Patch["state"]
		if !ok {
			continue
		}
		states = append(states, string(raw))
	}
	if states[0] != `"stopping"` || states[1] != `"stopped"` {
		t.Fatalf("states = %v, want [\"stopping\" \"stopped\"]", states)
	}
}

// TestTaskListEventsReplayPurgeTransitions exercises the real purge flow and
// verifies the stopping state reaches the stream even when StopTask completes
// before the next snapshot.
func TestTaskListEventsReplayPurgeTransitions(t *testing.T) {
	t.Parallel()
	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, harness.Claude)
	tk.Repos = []taskslog.RepoMount{{Name: "r"}}
	tk.SetRuntimeConnectionInfo(runtime.NewID("test-runtime", "ctr-1"), runtime.ConnectionTarget{SSHHost: "ctr-1"}, "", "", 0)
	tk.SetState(taskslog.StateWaiting)
	registerRouterCheckout(t, s.taskMgr.Checkouts, "r", newRouterTestCheckout(t.TempDir()))
	insertTestTask(s, tk.ID.String(), tk)

	r := connectTaskListStream(t, s)
	for range 3 {
		readNextTaskListEvent(t, r)
	}

	req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/api/caic/v1/tasks/"+tk.ID.String()+"/purge", http.NoBody)
	req.SetPathValue("id", tk.ID.String())
	w := httptest.NewRecorder()
	handleWithTask(testTaskHandlers(s), testTaskHandlers(s).taskSvc.purgeTask)(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("purge status = %d, want 200: %s", w.Code, w.Body.String())
	}

	var states []string
	for len(states) < 2 {
		ev := readNextTaskListEvent(t, r)
		if ev.Kind != "patch" || ev.Patch == nil {
			continue
		}
		raw, ok := ev.Patch["state"]
		if !ok {
			continue
		}
		states = append(states, string(raw))
	}
	if states[0] != `"stopping"` || states[1] != `"stopped"` {
		t.Fatalf("states = %v, want [\"stopping\" \"stopped\"]", states)
	}
}
