// Tests whole-machine measurements and detached system-history API snapshots.

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
