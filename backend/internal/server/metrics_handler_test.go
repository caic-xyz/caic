// Tests for the server operation-latency endpoint.

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/metrics"
)

func TestGetMetrics(t *testing.T) {
	t.Parallel()

	t.Run("valid_handler_returns_snapshot", func(t *testing.T) {
		t.Parallel()
		store := metrics.NewStore(metrics.Resource{ServiceName: "caic", ServiceVersion: "1.2.3", Host: "host-1"})
		store.Record(t.Context(), "repo.diff", metrics.OutcomeOK, metrics.Duration(5*time.Millisecond),
			metrics.Attr{Key: "forge.name", Value: "github"})
		h := &serverHandlers{metrics: store}

		resp, err := h.getMetrics(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatalf("getMetrics: %v", err)
		}
		if len(resp.Series) != 1 || resp.Series[0].Name != "repo.diff" {
			t.Fatalf("resp = %+v, want one repo.diff series", resp)
		}
		if got := resp.Series[0].Attrs["forge.name"]; got != "github" {
			t.Fatalf("attributes = %+v, want forge.name github", resp.Series[0].Attrs)
		}
		resource := resp.Resource
		if resource.ServiceName != "caic" || resource.ServiceVersion != "1.2.3" || resource.Host != "host-1" {
			t.Fatalf("resource = %+v, want the store's resource", resource)
		}
		if resp.Since.IsZero() {
			t.Fatal("Since is zero")
		}

		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/server/metrics", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
	})
}
