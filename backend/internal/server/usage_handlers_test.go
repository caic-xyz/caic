// Tests for immediate usage SSE updates from harness-reported quota changes.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent/backends"
	taskhistory "github.com/caic-xyz/caic/backend/internal/task/history"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
	"github.com/maruel/gomode/mcp"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/backend/internal/usagedb"
	"github.com/caic-xyz/caic/backend/internal/usagedb/data"
	"github.com/maruel/ksid"
)

func TestLocalUsage(t *testing.T) {
	t.Parallel()
	t.Run("within-task coverage survives terminal and cached reload", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name     string
			reported float64
			mixed    bool
		}{{"complete zero", 0, false}, {"partial zero", 0, true}, {"partial positive", .25, true}} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				live := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "coverage"}, harness.Pi)
				messages := []agent.TimedMessage{{Message: &agent.UsageMessage{Additive: true, PerCall: true, ReportedCostUSD: &tc.reported, Usage: agent.Usage{InputTokens: 10}}}}
				if tc.mixed {
					messages = append(messages, agent.TimedMessage{Message: &agent.UsageMessage{Additive: true, PerCall: true, Usage: agent.Usage{InputTokens: 20}}})
				}
				messages = append(messages, agent.TimedMessage{Message: &agent.ResultMessage{}})
				live.SeedTimelineParts(nil, messages)
				snapshot := live.Snapshot()
				result := &task.Result{State: task.StatePurged, CostUSD: snapshot.CostUSD, CostKnown: snapshot.CostKnown, CostComplete: snapshot.CostComplete, Usage: snapshot.Usage}
				dir := t.TempDir()
				trailer := taskhistory.ResultMessage("", result)
				trailer.MessageType = "result"
				encoded, err := json.Marshal(trailer)
				if err != nil {
					t.Fatal(err)
				}
				header, err := taskslog.EncodeHeader(&v3.MetaMessage{MessageType: "caic_meta", Version: 3, Harness: "pi", Prompt: "coverage", StartedAt: live.StartedAt})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, live.ID.String()+".jsonl"), append(append(header, '\n'), append(encoded, '\n')...), 0o600); err != nil {
					t.Fatal(err)
				}
				logs := taskhistory.NewStore(slog.New(slog.DiscardHandler), taskslog.NewStore(dir), backends.Default("", nil))
				for _, phase := range []string{"live", "terminal", "cold", "cached"} {
					t.Run(phase, func(t *testing.T) { //nolint:paralleltest // Ordered cold/cache reloads exercise one retained task.
						s := newTestRouter(t, nil)
						tk := live
						if phase == "cold" || phase == "cached" {
							loaded, err := logs.LoadForTaskIDs([]string{live.ID.String()})
							if err != nil {
								t.Fatal(err)
							}
							if len(loaded) != 1 || loaded[0].LastTrailer == nil {
								t.Fatalf("restored=%+v", loaded)
							}
							result = loaded[0].LastTrailer
							tk = mustNewTask(t, live.ID, agent.Prompt{Text: "restored"}, harness.Pi)
							tk.StartedAt = loaded[0].StartedAt
						}
						insertTestTask(s, tk.ID, tk)
						if phase != "live" {
							entry, _ := s.taskMgr.GetEntry(tk.ID)
							entry.Finish(result)
						}
						w := httptest.NewRecorder()
						s.buildAPIHandler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/tasks/"+tk.ID.String(), nil))
						var dto v1.Task
						if err := json.Unmarshal(w.Body.Bytes(), &dto); err != nil {
							t.Fatal(err)
						}
						if w.Code != 200 || !dto.CostKnown || dto.CostComplete == tc.mixed || dto.CostUSD != tc.reported {
							t.Fatalf("task subtotal/completeness lost: %d %+v", w.Code, dto)
						}
						for _, window := range localUsage(s.taskMgr, time.Now()).Windows {
							if window.CostKnown == tc.mixed || window.CostUSD != tc.reported {
								t.Fatalf("local coverage=%+v", window)
							}
						}
						summary := taskMCPSummary(1, &dto)
						detail, err := taskMCPDetail(1, &dto)
						if err != nil || !summary.CostKnown || !detail.CostKnown || summary.CostComplete == tc.mixed || detail.CostComplete == tc.mixed {
							t.Fatalf("MCP coverage lost: %+v %+v %v", summary, detail, err)
						}
						tools := &mcpTools{usage: s.usageHandlers}
						text := toolTextOutput(t, tools.handleGetUsage(t.Context(), &mcp.ToolRequest[struct{}]{}).Structured)
						if tc.mixed && tc.reported == 0 && !strings.HasPrefix(text, "1h cost: unknown (") {
							t.Fatalf("partial zero looks free: %s", text)
						}
						if tc.mixed && tc.reported > 0 && !strings.HasPrefix(text, "1h cost: $0.25 known subtotal; total unknown (") {
							t.Fatalf("subtotal hidden: %s", text)
						}
						if !tc.mixed && !strings.HasPrefix(text, "1h cost: $0.00 (") {
							t.Fatalf("known zero lost: %s", text)
						}
					})
				}
			})
		}
	})

	for _, terminal := range []bool{false, true} {
		name := "live"
		if terminal {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, known := range []bool{false, true} {
				name := "unpriced DSH"
				h := harness.Dsh
				if known {
					name, h = "reported zero", harness.Claude
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					s := newTestRouter(t, nil)
					tk := mustNewTask(t, ksid.NewID(), agent.Prompt{Text: "test"}, h)
					tk.SeedTimelineParts(nil, []agent.TimedMessage{{Message: &agent.ResultMessage{CostReported: known}}})
					insertTestTask(s, tk.ID, tk)
					if terminal {
						entry, _ := s.taskMgr.GetEntry(tk.ID)
						entry.Finish(&task.Result{State: task.StatePurged, CostKnown: known, CostComplete: known})
					}
					w := httptest.NewRecorder()
					s.usageHandlers.handleGetUsage(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/caic/v1/usage", nil))
					var got v1.UsageResp
					if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if len(got.Local.Windows) != 3 {
						t.Fatalf("windows = %+v", got.Local.Windows)
					}
					for _, window := range got.Local.Windows {
						if window.CostKnown != known || window.CostUSD != 0 {
							t.Fatalf("window = %+v; want costKnown=%v and zero USD", window, known)
						}
					}
				})
			}
		})
	}
}

type quotaUpdateWriter struct {
	*httptest.ResponseRecorder

	onFirstEvent func()
	secondEvent  chan struct{}
	events       int
}

func (w *quotaUpdateWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(raw)
	if !bytes.Contains(raw, []byte("event: message")) {
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

func (w *cancelAfterEventWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(raw)
	if bytes.Contains(raw, []byte("event: message")) {
		w.cancel()
	}
	return n, err
}

func (w *cancelAfterEventWriter) eventsWritten() int {
	return bytes.Count(w.Body.Bytes(), []byte("event: message"))
}

type gatedUsageFetcher struct {
	staticUsageFetcher

	entered chan struct{}
	release chan struct{}
}

func (f *gatedUsageFetcher) Get(ctx context.Context) *usage.ProviderQuota {
	close(f.entered)
	<-f.release
	return f.staticUsageFetcher.Get(ctx)
}

func TestUsageHandlersHandleEvents(t *testing.T) {
	t.Parallel()

	t.Run("heartbeat during provider refresh", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			fetcher := &gatedUsageFetcher{entered: make(chan struct{}), release: make(chan struct{})}
			s.usageHandlers.fetchers = []usage.ProviderFetcher{fetcher}
			ctx, cancel := context.WithCancel(t.Context())
			writer := &deadlineResponseWriter{ResponseRecorder: httptest.NewRecorder()}
			var workers sync.WaitGroup
			workers.Go(func() {
				s.usageHandlers.handleEvents(writer, httptest.NewRequestWithContext(ctx, "GET", "/usage/events", nil))
			})
			<-fetcher.entered
			time.Sleep(15 * time.Second)
			synctest.Wait()
			body := writer.Body.String()
			deadlines := append([]time.Time(nil), writer.deadlines...)
			cancel()
			close(fetcher.release)
			workers.Wait()
			before := writer.Body.String()
			time.Sleep(45 * time.Second)
			synctest.Wait()
			want := "event: heartbeat\ndata: {\"time\":\"2000-01-01T00:00:15Z\"}\n\n"
			if body != want {
				t.Fatalf("frames = %q, want %q", body, want)
			}
			if len(deadlines) < 2 || deadlines[len(deadlines)-2].IsZero() || !deadlines[len(deadlines)-1].IsZero() {
				t.Fatalf("heartbeat deadlines = %v", deadlines)
			}
			if writer.Body.String() != before {
				t.Fatal("retired heartbeat worker wrote after shutdown")
			}
		})
	})

	t.Run("HarnessQuotaUpdateDuringWriteEmitsImmediately", func(t *testing.T) {
		t.Parallel()

		s := newTestRouter(t, nil)
		s.usageHandlers.fetchers = []usage.ProviderFetcher{&staticUsageFetcher{quota: usage.ProviderQuota{
			Provider: agent.QuotaProviderClaudeCode,
			Label:    "Claude Code",
			AuthKind: usage.AuthKindOAuth,
			RateLimits: []usage.QuotaRateLimit{{
				Window: "5h", Utilization: 0.25,
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
		var workers sync.WaitGroup
		t.Cleanup(func() { cancel(); workers.Wait() })
		workers.Go(func() {
			s.usageHandlers.handleEvents(writer, httptest.NewRequestWithContext(ctx, "GET", "/usage/events", nil))
			close(done)
		})

		select {
		case <-writer.secondEvent:
			cancel()
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("usage stream did not emit the harness quota update: %q", writer.Body.String())
		}
		<-done
		if !bytes.Contains(writer.Body.Bytes(), []byte(`"utilization":1`)) {
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
		At:             at,
		Model:          "claude-opus",
		CostUSD:        0.12,
		CostProvenance: data.CostProvenance{ReportedUSD: .12, ReportedParts: 1},
		TurnBoundary:   true,
		Delta: data.Delta{
			TokenBuckets: data.TokenBuckets{Input: 10, CacheWrite1h: 20, CacheWrites: []data.CacheWrite{{TTLSeconds: 7200, Tokens: 50}}, CacheRead: 30, Output: 40, Reasoning: 5},
			Turns:        1,
			ToolCalls:    map[string]int{"Read": 2},
			ToolTimings:  map[string]data.ToolTiming{"Read": data.ToolCallTiming(1250)},
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
	if len(day.Tokens.CacheWrites) != 2 || day.Tokens.CacheWrites[1].TTLSeconds != 7200 || day.Tokens.CacheWrites[1].Tokens != 50 {
		t.Fatalf("duration buckets = %+v", day.Tokens.CacheWrites)
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
	wantCost := v1.UsageDashboardCost{ReportedUSD: .12, ReportedParts: 1}
	if day.Cost != wantCost || model.Cost != wantCost || day.Harnesses[0].Cost != wantCost || day.Harnesses[0].Models[0].Cost != wantCost {
		t.Errorf("API source components lost: %+v", day)
	}
	hr := day.Harnesses[0]
	if hr.Tools[0] != (v1.UsageDashboardCount{Name: "Read", Count: 2}) || hr.ToolTimings[0].DurationMs != 1250 || hr.Skills[0].Count != 1 {
		t.Errorf("harness drill-down = %#v / %#v / %#v", hr.Tools, hr.ToolTimings, hr.Skills)
	}
	if len(hr.Models) != 1 || hr.Models[0].Model != "claude-opus" || hr.Models[0].Tokens.Output != 40 {
		t.Errorf("harness-by-model cross = %#v", hr.Models)
	}
	if len(day.Repos) != 2 || day.Repos[0].Repo != "caic" || day.Repos[1].Repo != "sdk" {
		t.Errorf("repos = %#v", day.Repos)
	}
	// The skill leaderboard counts tasks, so the task's two reads fold to one.
	if len(day.Skills) != 1 || day.Skills[0] != (v1.UsageDashboardCount{Name: "review", Count: 1}) {
		t.Errorf("skills = %#v", day.Skills)
	}
	// The duration distribution travels with the totals so a client can derive
	// a median over any selected range.
	wantTiming := v1.UsageDashboardToolTiming{Name: "Read", Count: 1, DurationMs: 1250, Buckets: []v1.UsageDashboardToolDuration{{UpperMs: 2000, Count: 1}}}
	if len(day.ToolTimings) != 1 || !reflect.DeepEqual(day.ToolTimings[0], wantTiming) {
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
			Window: "5h", Utilization: 0.25,
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
		Delta:        data.Delta{TokenBuckets: data.TokenBuckets{Input: 100, Output: 50}, Turns: 1},
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
