// Tests task HTTP handler state-precondition errors.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/server/api"
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
