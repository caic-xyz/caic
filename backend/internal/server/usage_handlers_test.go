// Tests for immediate usage SSE updates from harness-reported quota changes.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
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

func TestUsageHandlersBuildResp(t *testing.T) {
	t.Parallel()

	now := time.Now()
	s := newTestRouter(t, nil)
	s.usageHandlers.fetchers = []usage.ProviderFetcher{
		&staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderAnthropic, Label: "Anthropic", AuthKind: usage.AuthKindOAuth, FetchedAt: now,
		}},
		&staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderCodex, Label: "Codex", AuthKind: usage.AuthKindOAuth, FetchedAt: now.Add(-usage.CacheTTL),
		}},
		&staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderDeepSeek, Label: "DeepSeek", AuthKind: usage.AuthKindAPIKey, FetchedAt: now, FetchError: true,
		}},
	}

	got := s.usageHandlers.buildResp(t.Context())
	statuses := make(map[v1.QuotaProvider]v1.ProviderFetchStatus, len(got.Providers))
	for _, provider := range got.Providers {
		statuses[provider.Provider] = provider.FetchStatus
	}
	if statuses[v1.QuotaProviderAnthropic] != v1.ProviderFetchStatusFresh ||
		statuses[v1.QuotaProviderCodex] != v1.ProviderFetchStatusStale ||
		statuses[v1.QuotaProviderDeepSeek] != v1.ProviderFetchStatusError {
		t.Fatalf("provider fetch statuses = %#v, want fresh/stale/error", statuses)
	}
}

func TestUsageHandlersHandleGetDashboard(t *testing.T) {
	t.Parallel()

	s := newTestRouter(t, nil)
	at := time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC)
	s.usageHandlers.rollup.Observe(usagedb.TaskMeta{
		TaskID:  ksid.NewID(),
		Harness: "claude",
		Repos:   []string{"caic", "sdk"},
	}, &usagedb.Event{
		At:           at,
		Model:        "claude-opus",
		CostUSD:      0.12,
		TurnBoundary: true,
		Delta: usagedb.Delta{
			TokenBuckets: usagedb.TokenBuckets{Input: 10, CacheWrite1h: 20, CacheRead: 30, Output: 40, Reasoning: 5},
			Turns:        1,
			ToolCalls:    map[string]int{"Read": 2},
			ToolTimings:  map[string]usagedb.ToolTiming{"Read": {Count: 1, DurationMs: 1250}},
			SkillReads:   map[string]int{"review": 2},
		},
	})

	h := s.buildAPIHandler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/usage/dashboard", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var got v1.UsageDashboardResp
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.DataSince != "2026-02-05" || len(got.Days) != 1 {
		t.Fatalf("dashboard = %#v, want one day since 2026-02-05", got)
	}
	day := got.Days[0]
	if day.Tokens.Input != 10 || day.Tokens.CacheWrite1h != 20 || day.Tokens.CacheRead != 30 || day.Tokens.Output != 40 {
		t.Errorf("tokens = %#v", day.Tokens)
	}
	if day.CostUSD != 0.12 || day.Turns != 1 {
		t.Errorf("totals = cost %v, turns %d", day.CostUSD, day.Turns)
	}
	if len(day.Models) != 1 || day.Models[0].Model != "claude-opus" || len(day.Harnesses) != 1 || day.Harnesses[0].Harness != "claude" {
		t.Errorf("breakdowns = models %#v, harnesses %#v", day.Models, day.Harnesses)
	}
	// Model and harness drill-down carry the full delta fold with task-day
	// skill and repo semantics matching the day leaderboards.
	model := day.Models[0]
	if model.Tools[0] != (v1.UsageDashboardCount{Name: "Read", Count: 2}) || model.Skills[0].Name != "review" || model.Skills[0].Count != 1 {
		t.Errorf("model drill-down = tools %#v, skills %#v", model.Tools, model.Skills)
	}
	if model.Repos[0].Repo != "caic" || model.Repos[1].Repo != "sdk" || model.Repos[0].Tasks != 1 {
		t.Errorf("model repos = %#v", model.Repos)
	}
	harness := day.Harnesses[0]
	if harness.Tools[0] != (v1.UsageDashboardCount{Name: "Read", Count: 2}) || harness.ToolTimings[0].DurationMs != 1250 || harness.Skills[0].Count != 1 {
		t.Errorf("harness drill-down = %#v / %#v / %#v", harness.Tools, harness.ToolTimings, harness.Skills)
	}
	if len(harness.Models) != 1 || harness.Models[0].Model != "claude-opus" || harness.Models[0].Tokens.Output != 40 {
		t.Errorf("harness-by-model cross = %#v", harness.Models)
	}
	if len(day.Repos) != 2 || day.Repos[0].Repo != "caic" || day.Repos[1].Repo != "sdk" {
		t.Errorf("repos = %#v", day.Repos)
	}
	// The skill leaderboard counts tasks, so the task's two reads fold to one.
	if len(day.Skills) != 1 || day.Skills[0] != (v1.UsageDashboardCount{Name: "review", Count: 1}) {
		t.Errorf("skills = %#v", day.Skills)
	}
	if len(day.ToolTimings) != 1 || day.ToolTimings[0] != (v1.UsageDashboardToolTiming{Name: "Read", Count: 1, DurationMs: 1250}) {
		t.Errorf("tool timings = %#v", day.ToolTimings)
	}
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

func BenchmarkUsageHandlersHandleGetDashboard(b *testing.B) {
	s := newTestRouter(b, nil)
	s.usageHandlers.rollup.Observe(usagedb.TaskMeta{TaskID: ksid.NewID(), Harness: "claude", Repos: []string{"caic"}}, &usagedb.Event{
		At:           time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC),
		Model:        "claude-opus",
		TurnBoundary: true,
		Delta:        usagedb.Delta{TokenBuckets: usagedb.TokenBuckets{Input: 100, Output: 50}, Turns: 1},
	})
	h := s.buildAPIHandler()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/caic/v1/usage/dashboard", nil))
		if w.Code != http.StatusOK {
			b.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
	}
}
