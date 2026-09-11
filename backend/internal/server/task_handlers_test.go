// Tests task HTTP handler state-precondition errors.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestTaskHandlersVNCWithoutDisplay(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	insertTestTask(s, "t1", mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, ""))
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/vnc/ws", nil)
	r.SetPathValue("id", "t1")
	s.taskHandlers.handleVNCWebSocket(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
	err := decodeError(t, w)
	if err.Code != api.CodeConflict {
		t.Errorf("code = %q, want %q", err.Code, api.CodeConflict)
	}
}

func TestTaskHandlersHandoff(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "finish the feature"}, "")
	tk.SeedTimeline([]agent.Message{
		&agent.UserInputMessage{Text: "finish the feature"},
		&agent.TextMessage{Text: "The API remains to be connected."},
	})
	insertTestTask(s, "t1", tk)
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/t1/handoff", nil)
	s.taskHandlers.routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp v1.TaskHandoffResp
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Continue this task in a new agent", "finish the feature", "The API remains to be connected."} {
		if !strings.Contains(resp.Prompt, want) {
			t.Errorf("prompt does not contain %q:\n%s", want, resp.Prompt)
		}
	}
}
