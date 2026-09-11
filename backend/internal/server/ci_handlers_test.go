// Tests CI repair handler error contracts.

package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

func TestCIHandlers(t *testing.T) {
	t.Parallel()

	t.Run("fix CI", func(t *testing.T) {
		t.Parallel()

		t.Run("unknown repository", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/ci/fix-ci", strings.NewReader(`{"repo":"mistyped"}`))
			s.ciHandlers.routes().ServeHTTP(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
			err := decodeError(t, w)
			if err.Code != api.CodeUnknownRepository {
				t.Errorf("code = %q, want %q", err.Code, api.CodeUnknownRepository)
			}
		})

		t.Run("checkout without repository metadata", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			registerRouterCheckout(t, s.checkouts, "local", newRouterTestCheckout(t.TempDir()))
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/ci/fix-ci", strings.NewReader(`{"repo":"local"}`))
			s.ciHandlers.routes().ServeHTTP(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
			}
			err := decodeError(t, w)
			if err.Code != api.CodeConflict {
				t.Errorf("code = %q, want %q", err.Code, api.CodeConflict)
			}
			if err.Message != "no repository metadata configured for this path" {
				t.Errorf("message = %q, want repository metadata guidance", err.Message)
			}
		})

		t.Run("checkout without forge access", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			checkout := newRouterTestCheckout(t.TempDir())
			checkout.Repository = &repo.Repository{Remote: "https://example.com/acme/project.git"}
			registerRouterCheckout(t, s.checkouts, "project", checkout)
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodPost, "/ci/fix-ci", strings.NewReader(`{"repo":"project"}`))
			s.ciHandlers.routes().ServeHTTP(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
			}
			err := decodeError(t, w)
			if err.Code != api.CodeConflict {
				t.Errorf("code = %q, want %q", err.Code, api.CodeConflict)
			}
			if err.Message != "no forge token configured for this repo" {
				t.Errorf("message = %q, want forge access guidance", err.Message)
			}
		})
	})

	t.Run("fix PR", func(t *testing.T) {
		t.Parallel()

		t.Run("task without pull request", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			task := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
			insertTestTask(s, "t1", task)
			_, err := s.ciHandlers.fixPR(t.Context(), &v1.BotFixPRReq{TaskID: "t1"})
			apiErr, ok := errors.AsType[*api.Error](err)
			if !ok {
				t.Fatalf("error = %v, want API error", err)
			}
			if apiErr.Status != http.StatusConflict || apiErr.Code != api.CodeConflict {
				t.Errorf("error = (%d, %q), want (%d, %q)", apiErr.Status, apiErr.Code, http.StatusConflict, api.CodeConflict)
			}
		})
	})

	t.Run("get CI log", func(t *testing.T) {
		t.Parallel()

		t.Run("without repository metadata", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			task := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
			task.Repos = []taskslog.RepoMount{{Name: "local"}}
			insertTestTask(s, "t1", task)
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/ci/log/t1", nil)
			r.SetPathValue("id", "t1")
			s.ciHandlers.handleGetCILog(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
			}
			err := decodeError(t, w)
			if err.Code != api.CodeConflict {
				t.Errorf("code = %q, want %q", err.Code, api.CodeConflict)
			}
			if err.Message != "no repo info found" {
				t.Errorf("message = %q, want repository metadata guidance", err.Message)
			}
		})

		t.Run("without forge access", func(t *testing.T) {
			t.Parallel()

			s := newTestRouter(t, nil)
			checkout := newRouterTestCheckout(t.TempDir())
			checkout.Repository = &repo.Repository{Remote: "https://example.com/acme/project.git"}
			registerRouterCheckout(t, s.checkouts, "project", checkout)
			task := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, "")
			task.Repos = []taskslog.RepoMount{{Name: "project"}}
			insertTestTask(s, "t1", task)
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/ci/log/t1", nil)
			r.SetPathValue("id", "t1")
			s.ciHandlers.handleGetCILog(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
			}
			err := decodeError(t, w)
			if err.Code != api.CodeConflict {
				t.Errorf("code = %q, want %q", err.Code, api.CodeConflict)
			}
			if err.Message != "no forge token configured for this repo" {
				t.Errorf("message = %q, want forge access guidance", err.Message)
			}
		})
	})
}
