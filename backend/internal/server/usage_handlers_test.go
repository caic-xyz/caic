// Tests for immediate usage SSE updates from harness-reported quota changes.

package server

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

type quotaUpdateWriter struct {
	*httptest.ResponseRecorder

	onFirstEvent func()
	secondEvent  chan struct{}
	events       int
}

func (w *quotaUpdateWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if !bytes.Contains(data, []byte("event: message")) {
		return n, err
	}
	w.events++
	if w.events == 1 {
		w.onFirstEvent()
	}
	if w.events == 2 {
		close(w.secondEvent)
	}
	return n, err
}

type cancelAfterEventWriter struct {
	*httptest.ResponseRecorder

	cancel context.CancelFunc
}

func (w *cancelAfterEventWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(data)
	if bytes.Contains(data, []byte("event: message")) {
		w.cancel()
	}
	return n, err
}

func (w *cancelAfterEventWriter) eventsWritten() int {
	return bytes.Count(w.Body.Bytes(), []byte("event: message"))
}

func TestUsageHandlersHandleEvents(t *testing.T) {
	t.Parallel()

	t.Run("HarnessQuotaUpdateDuringWriteEmitsImmediately", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderClaudeCode,
			Label:    "Claude Code",
			AuthKind: usage.AuthKindOAuth,
			RateLimits: []usage.QuotaRateLimit{{
				Window: "5h", UsedPct: 25,
			}},
		}}}

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		writer := &quotaUpdateWriter{
			ResponseRecorder: httptest.NewRecorder(),
			secondEvent:      make(chan struct{}),
			onFirstEvent: func() {
				s.taskMgr.QuotaTracker.Apply(&usage.TaskQuotaUpdate{
					Provider: agent.QuotaProviderClaudeCode, ProviderLabel: "Claude Code", Window: "5h",
					Status: agent.RateLimitStatusRejected, ObservedAt: time.Now().UTC(),
				})
				s.taskMgr.NotifyTaskChange()
			},
		}
		done := make(chan struct{})
		go func() {
			s.usageHandlers.handleEvents(writer, httptest.NewRequestWithContext(ctx, "GET", "/usage/events", nil))
			close(done)
		}()

		select {
		case <-writer.secondEvent:
			cancel()
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("usage stream did not emit the harness quota update: %q", writer.Body.String())
		}
		<-done
		if !bytes.Contains(writer.Body.Bytes(), []byte(`"usedPct":100`)) {
			t.Fatalf("usage stream = %q, want rejected quota update", writer.Body.String())
		}
	})
}

func BenchmarkUsageHandlersHandleEvents(b *testing.B) {
	s := newTestRouter(b, nil)
	s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
		Provider: agent.QuotaProviderClaudeCode,
		Label:    "Claude Code",
		AuthKind: usage.AuthKindOAuth,
		RateLimits: []usage.QuotaRateLimit{{
			Window: "5h", UsedPct: 25,
		}},
	}}}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ctx, cancel := context.WithCancel(b.Context())
		writer := &cancelAfterEventWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
		s.usageHandlers.handleEvents(writer, httptest.NewRequestWithContext(ctx, "GET", "/usage/events", nil))
		if writer.eventsWritten() != 1 {
			b.Fatalf("usage events = %d, want 1", writer.eventsWritten())
		}
	}
}
