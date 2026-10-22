// Samples bounded machine history and publishes last-observed CPU/memory pressure without additional sampling.

package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

type systemStats struct {
	mu      sync.Mutex
	samples []v1.SystemSample
	err     string
	changed chan struct{}
}

func newSystemStats(ctx context.Context) *systemStats {
	s := &systemStats{samples: make([]v1.SystemSample, 0, 60), changed: make(chan struct{})}
	go s.run(ctx)
	return s
}

// health subscribes before returning the current observation, preventing missed
// updates. One broadcast channel serves every stream; slow readers coalesce.
func (s *systemStats) health() (h v1.ServerHealth, changed <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h = v1.ServerHealth{Error: s.err}
	if len(s.samples) == 0 {
		if h.Error == "" {
			h.Error = "Waiting for the first system sample"
		}
		return h, s.changed
	}
	v := &s.samples[len(s.samples)-1]
	h.Time = v.Time
	h.CPUCount = len(v.CPUs)
	for _, c := range v.CPUs {
		h.CPUPerc += c.UsagePerc
	}
	h.MemUsed, h.MemTotal = v.MemUsed, v.MemTotal
	h.CPUHigh = h.CPUCount > 0 && h.CPUPerc/float64(h.CPUCount) >= 80
	h.MemHigh = v.MemPerc >= 80
	return h, s.changed
}

func (s *systemStats) record(sample *v1.SystemSample, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.err = err.Error()
	} else {
		s.err = ""
		if len(s.samples) == cap(s.samples) {
			copy(s.samples, s.samples[1:])
			s.samples = s.samples[:len(s.samples)-1]
		}
		s.samples = append(s.samples, *sample)
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *systemStats) snapshot() v1.SystemStatsResp {
	s.mu.Lock()
	defer s.mu.Unlock()
	samples := append([]v1.SystemSample{}, s.samples...)
	for i := range samples {
		samples[i].CPUs = slices.Clone(samples[i].CPUs)
	}
	return v1.SystemStatsResp{Samples: samples, Error: s.err}
}

func (s *systemStats) run(ctx context.Context) {
	// Establish a CPU baseline before publishing the first interval.
	previous, err := cpu.TimesWithContext(ctx, true)
	if err != nil {
		s.record(&v1.SystemSample{}, fmt.Errorf("CPU: %w", err))
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample, next, err := readSystemSample(ctx, previous)
			previous = next
			s.record(&sample, err)
		}
	}
}

func readSystemSample(ctx context.Context, previous []cpu.TimesStat) (v1.SystemSample, []cpu.TimesStat, error) {
	current, err := cpu.TimesWithContext(ctx, true)
	if err != nil {
		return v1.SystemSample{}, previous, fmt.Errorf("CPU: %w", err)
	}
	usages, err := systemCPUUsage(previous, current)
	if err != nil {
		return v1.SystemSample{}, current, err
	}
	m, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return v1.SystemSample{}, current, fmt.Errorf("memory: %w", err)
	}
	n, err := net.IOCountersWithContext(ctx, false)
	if err != nil {
		return v1.SystemSample{}, current, fmt.Errorf("network: %w", err)
	}
	d, err := disk.UsageWithContext(ctx, ".")
	if err != nil {
		return v1.SystemSample{}, current, fmt.Errorf("filesystem: %w", err)
	}
	if len(n) != 1 || m.Total == 0 {
		return v1.SystemSample{}, current, errors.New("incomplete system measurements")
	}
	used := m.Total - m.Available
	return v1.SystemSample{
		Time: time.Now().UTC(), CPUs: usages, MemUsed: used, MemTotal: m.Total,
		MemPerc: 100 * float64(used) / float64(m.Total), NetRx: n[0].BytesRecv, NetTx: n[0].BytesSent,
		DiskUsed: d.Used, DiskTotal: d.Total,
	}, current, nil
}

// systemCPUUsage matches stable CPU identifiers, including sparse CPU IDs.
// New or reset counters require a fresh baseline; never invent an idle reading.
func systemCPUUsage(previous, current []cpu.TimesStat) ([]v1.CPUUsage, error) {
	if len(current) == 0 {
		return nil, errors.New("no logical CPU counters available")
	}
	before := make(map[string]cpu.TimesStat, len(previous))
	for _, c := range previous {
		before[c.CPU] = c
	}
	out := make([]v1.CPUUsage, 0, len(current))
	for _, c := range current {
		p, ok := before[c.CPU]
		if !ok {
			return nil, fmt.Errorf("CPU %s is waiting for a counter baseline", c.CPU)
		}
		// Guest time is already included in User/Nice. Iowait is not busy time.
		busy := c.User + c.Nice + c.System + c.Irq + c.Softirq + c.Steal
		prevBusy := p.User + p.Nice + p.System + p.Irq + p.Softirq + p.Steal
		total := busy + c.Idle + c.Iowait
		prevTotal := prevBusy + p.Idle + p.Iowait
		if total < prevTotal || busy < prevBusy {
			return nil, fmt.Errorf("CPU %s counters reset", c.CPU)
		}
		used := 0.0
		if total > prevTotal {
			used = math.Max(0, math.Min(100, 100*(busy-prevBusy)/(total-prevTotal)))
		}
		out = append(out, v1.CPUUsage{ID: c.CPU, UsagePerc: used})
	}
	return out, nil
}
