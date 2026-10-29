// Tests whole-machine history, immediate pressure, retained warnings, shared health updates, and scoped access.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/maruel/ksid"
	"github.com/shirou/gopsutil/v4/cpu"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestReadSystemSample(t *testing.T) {
	t.Parallel()
	previous, err := cpu.TimesWithContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := readSystemSample(t.Context(), previous)
	if err != nil {
		t.Fatal(err)
	}
	if s.Time.IsZero() || len(s.CPUs) == 0 || s.MemTotal == 0 || s.MemUsed > s.MemTotal || s.MemPerc < 0 || s.MemPerc > 100 || s.DiskTotal == 0 || s.DiskUsed > s.DiskTotal {
		t.Fatalf("invalid machine measurements: %+v", s)
	}
}

func TestGetSystemStats(t *testing.T) {
	t.Parallel()
	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		h := &serverHandlers{systemStats: &systemStats{}}
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/server/system-stats", http.NoBody))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp v1.SystemStatsResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Samples == nil {
			t.Fatal("samples must be an array")
		}
	})
	t.Run("retained_on_error", func(t *testing.T) {
		t.Parallel()
		h := &serverHandlers{systemStats: &systemStats{samples: []v1.SystemSample{{Time: time.Now().UTC(), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 25}}}}, err: "memory unavailable"}}
		resp, err := h.getSystemStats(t.Context(), &api.EmptyReq{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Error != "memory unavailable" || len(resp.Samples) != 1 {
			t.Fatalf("response: %+v", resp)
		}
		resp.Samples[0].CPUs[0].UsagePerc = 90
		if h.systemStats.samples[0].CPUs[0].UsagePerc != 25 {
			t.Fatal("snapshot aliases store")
		}
	})
}

func BenchmarkReadSystemSample(b *testing.B) {
	previous, err := cpu.TimesWithContext(b.Context(), true)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		_, next, err := readSystemSample(b.Context(), previous)
		if err != nil {
			b.Fatal(err)
		}
		previous = next
	}
}

func TestSystemCPUUsage(t *testing.T) {
	t.Parallel()
	t.Run("concentrated_and_distributed", func(t *testing.T) {
		t.Parallel()
		previous := []cpu.TimesStat{{CPU: "cpu0"}, {CPU: "cpu3"}}
		concentrated, err := systemCPUUsage(previous, []cpu.TimesStat{{CPU: "cpu3", Idle: 10}, {CPU: "cpu0", User: 10, Guest: 10}})
		if err != nil {
			t.Fatal(err)
		}
		if concentrated[0].ID != "cpu3" || concentrated[0].UsagePerc != 0 || concentrated[1].UsagePerc != 100 {
			t.Fatalf("concentrated: %+v", concentrated)
		}
		distributed, err := systemCPUUsage(previous, []cpu.TimesStat{{CPU: "cpu0", User: 5, Idle: 5}, {CPU: "cpu3", User: 5, Iowait: 5}})
		if err != nil {
			t.Fatal(err)
		}
		if distributed[0].UsagePerc != 50 || distributed[1].UsagePerc != 50 {
			t.Fatalf("distributed: %+v", distributed)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name              string
			previous, current []cpu.TimesStat
		}{
			{name: "no_cpus"},
			{name: "new_cpu", current: []cpu.TimesStat{{CPU: "cpu4", User: 20}}},
			{name: "reset", previous: []cpu.TimesStat{{CPU: "cpu0", User: 20}}, current: []cpu.TimesStat{{CPU: "cpu0", User: 10}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if _, err := systemCPUUsage(tc.previous, tc.current); err == nil {
					t.Fatal("expected CPU baseline error")
				}
			})
		}
	})
}

func TestSystemStats(t *testing.T) {
	t.Parallel()
	t.Run("immediate_pressure_and_failure", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
			h, ch := s.health()
			if !h.Time.IsZero() || h.Error == "" || h.CPUHigh || h.MemHigh {
				t.Fatalf("warmup = %+v", h)
			}
			sample := v1.SystemSample{CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 100}, {ID: "cpu1", UsagePerc: 60}}, MemUsed: 800, MemTotal: 1000, MemPerc: 80}
			sample.Time = time.Now()
			s.record(&sample, nil)
			h, _ = s.health()
			if !h.CPUHigh || !h.MemHigh {
				t.Fatalf("first high sample = %+v", h)
			}
			select {
			case <-ch:
			default:
				t.Fatal("collection did not notify initial subscriber")
			}
			if h.CPUPerc != 160 || h.CPUCount != 2 || h.MemUsed != 800 {
				t.Fatalf("aggregate units = %+v", h)
			}
			sample.CPUs[1].UsagePerc = 50
			sample.MemPerc = 75
			sample.Time = time.Now()
			s.record(&sample, nil)
			h, _ = s.health()
			if h.CPUHigh || h.MemHigh {
				t.Fatalf("recovery below threshold = %+v", h)
			}
			s.record(&v1.SystemSample{}, errors.New("memory unavailable"))
			h, _ = s.health()
			if h.Error != "memory unavailable" || h.Time.IsZero() || h.CPUHigh || h.MemHigh {
				t.Fatalf("failure = %+v", h)
			}
		})
	})
	t.Run("warnings_survive_silence_errors_and_gaps", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
			sample := v1.SystemSample{CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 95}}, MemPerc: 95, MemUsed: 950, MemTotal: 1000}
			sample.Time = time.Now()
			s.record(&sample, nil)
			h, _ := s.health()
			if !h.CPUHigh || !h.MemHigh {
				t.Fatalf("high = %+v", h)
			}
			time.Sleep(time.Minute)
			h, _ = s.health()
			if h.Error != "" || !h.CPUHigh || !h.MemHigh {
				t.Fatalf("retained warning after silence = %+v", h)
			}
			s.record(&v1.SystemSample{}, errors.New("sampler failed"))
			h, _ = s.health()
			if h.Error != "sampler failed" || !h.CPUHigh || !h.MemHigh {
				t.Fatalf("retained warning after error = %+v", h)
			}
			sample.CPUs[0].UsagePerc, sample.MemPerc = 77, 77
			sample.Time = time.Now()
			s.record(&sample, nil)
			h, _ = s.health()
			if h.Error != "" || h.CPUHigh || h.MemHigh {
				t.Fatalf("fresh sample after gap = %+v", h)
			}
		})
	})
	t.Run("single_busy_cpu_and_error_recovery", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
			sample := v1.SystemSample{CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 100}, {ID: "cpu1", UsagePerc: 0}}, MemPerc: 95}
			sample.Time = time.Now()
			s.record(&sample, nil)
			h, _ := s.health()
			if h.CPUHigh || !h.MemHigh {
				t.Fatalf("whole machine pressure = %+v", h)
			}
			s.record(&v1.SystemSample{}, errors.New("reset"))
			sample.Time = time.Now()
			s.record(&sample, nil)
			h, _ = s.health()
			if !h.MemHigh {
				t.Fatalf("first high sample after failure = %+v", h)
			}
		})
	})
	t.Run("concurrent_subscribers", func(t *testing.T) {
		t.Parallel()
		s := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for range 100 {
					s.health()
					s.snapshot()
				}
			})
		}
		for range 100 {
			s.record(&v1.SystemSample{Time: time.Now(), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 50}}}, nil)
		}
		wg.Wait()
		if len(s.snapshot().Samples) != 60 {
			t.Fatal("history no longer bounded")
		}
	})
	t.Run("threshold_crossings", func(t *testing.T) {
		t.Parallel()
		s := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
		for i, tc := range []struct {
			percent float64
			high    bool
		}{
			{79.9, false}, {80, true}, {79.9, false}, {80.1, true},
		} {
			sample := v1.SystemSample{Time: time.Now(), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: tc.percent}}, MemPerc: tc.percent}
			s.record(&sample, nil)
			h, _ := s.health()
			if h.CPUHigh != tc.high || h.MemHigh != tc.high {
				t.Fatalf("sample %d (%v%%) = %+v", i, tc.percent, h)
			}
		}
	})
}

func TestTaskListServerHealth(t *testing.T) {
	t.Parallel()
	t.Run("initial_updates_reconnect_and_cleanup", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		stats := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
		h := testTaskHandlers(s)
		h.systemStats = stats
		for attempt := range 2 {
			ctx, cancel := context.WithCancel(testHTTPContext(t))
			t.Cleanup(cancel)
			var events []v1.ServerHealth
			w := &flushRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
			w.onWrite = func(raw []byte) {
				if !bytes.Contains(raw, []byte(`"kind":"health"`)) {
					return
				}
				for line := range strings.SplitSeq(string(raw), "\n") {
					data, ok := strings.CutPrefix(line, "data: ")
					if !ok {
						continue
					}
					var event v1.TaskListEvent
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						t.Fatal(err)
					}
					if event.Health.Time.IsZero() && strings.Contains(data, `"time"`) {
						t.Fatalf("unsampled health must omit time: %s", data)
					}
					events = append(events, *event.Health)
					switch len(events) {
					case 1:
						stats.record(&v1.SystemSample{Time: time.Now(), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 75}}, MemUsed: 20, MemTotal: 100}, nil)
					case 2:
						stats.record(&v1.SystemSample{}, errors.New("memory unavailable"))
					default:
						cancel()
					}
				}
			}
			h.handleTaskListEvents(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/events", nil))
			if len(events) != 3 || events[1].CPUPerc != 75 || events[1].CPUCount != 1 || events[2].Error != "memory unavailable" {
				t.Fatalf("attempt %d events = %+v", attempt, events)
			}
			if attempt == 0 && !events[0].Time.IsZero() {
				t.Fatalf("initial sample = %+v", events[0])
			}
			if attempt == 1 && events[0].Time.IsZero() {
				t.Fatal("reconnect lost latest sample")
			}
		}
	})
	t.Run("authorization", func(t *testing.T) {
		t.Parallel()
		s := newTestRouter(t, nil)
		for _, tc := range []struct {
			name      string
			principal *mcpPrincipal
			allowed   bool
		}{
			{"basic", &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeRead}}, false},
			{"tasks", &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeTasksRead}}, false},
			{"task_cannot_escalate", &mcpPrincipal{Remote: true, TaskID: ksid.NewID(), Scopes: []string{mcpScopeServerAdmin}}, false},
			{"admin", &mcpPrincipal{Remote: true, Scopes: []string{mcpScopeServerAdmin}}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				ctx := newMCPPrincipalContext(testHTTPContext(t), tc.principal)
				ctx, cancel := context.WithCancel(ctx)
				cancel()
				w := httptest.NewRecorder()
				testTaskHandlers(s).handleTaskListEvents(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/events", nil))
				if got := strings.Contains(w.Body.String(), `"kind":"health"`); got != tc.allowed {
					t.Fatalf("health present = %v: %s", got, w.Body.String())
				}
			})
		}
	})
	t.Run("collector_stall_keeps_warning_and_sends_heartbeats", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			s := newTestRouter(t, nil)
			stats := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
			stats.record(&v1.SystemSample{Time: time.Now(), CPUs: []v1.CPUUsage{{ID: "cpu0", UsagePerc: 95}}, MemPerc: 95}, nil)
			testTaskHandlers(s).systemStats = stats
			ctx, cancel := context.WithCancel(testHTTPContext(t))
			t.Cleanup(cancel)
			w := &flushRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
			var health []v1.ServerHealth
			var heartbeats int
			w.onWrite = func(raw []byte) {
				for line := range strings.SplitSeq(string(raw), "\n") {
					data, ok := strings.CutPrefix(line, "data: ")
					if !ok {
						continue
					}
					var event v1.TaskListEvent
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						t.Fatal(err)
					}
					if event.Health != nil {
						health = append(health, *event.Health)
					}
					if event.Kind == "heartbeat" {
						heartbeats++
						if heartbeats == 3 {
							cancel()
						}
					}
				}
			}
			testTaskHandlers(s).handleTaskListEvents(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/events", nil))
			if len(health) != 1 || !health[0].CPUHigh || !health[0].MemHigh || heartbeats != 3 {
				t.Fatalf("health=%+v, heartbeats=%d", health, heartbeats)
			}
		})
	})
}

type flushRecordingWriter struct {
	*httptest.ResponseRecorder

	onWrite func(data []byte)
	writes  []time.Time
	flushes []time.Time
}

func (w *flushRecordingWriter) Write(data []byte) (int, error) {
	w.writes = append(w.writes, time.Now())
	if w.onWrite != nil {
		w.onWrite(data)
	}
	return w.ResponseRecorder.Write(data)
}

func (w *flushRecordingWriter) Flush() {
	w.flushes = append(w.flushes, time.Now())
	w.ResponseRecorder.Flush()
}
