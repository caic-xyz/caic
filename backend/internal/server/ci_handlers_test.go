// Tests CI repair handler error contracts.

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/server/api"
)

func TestCIHandlersFixCI(t *testing.T) {
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

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
		}
		err := decodeError(t, w)
		if err.Code != api.CodeBadRequest {
			t.Errorf("code = %q, want %q", err.Code, api.CodeBadRequest)
		}
		if err.Message != "no repository metadata configured for this path" {
			t.Errorf("message = %q, want repository metadata guidance", err.Message)
		}
	})
}
