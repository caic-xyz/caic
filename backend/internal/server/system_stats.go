// Samples per-logical-CPU utilization, memory, network, and filesystem usage into bounded machine history.

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
}

func newSystemStats(ctx context.Context) *systemStats {
	s := &systemStats{samples: make([]v1.SystemSample, 0, 60)}
	go s.run(ctx)
	return s
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
		s.mu.Lock()
		s.err = fmt.Sprintf("CPU: %v", err)
		s.mu.Unlock()
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
			s.mu.Lock()
			if err != nil {
				s.err = err.Error()
			} else {
				s.err = ""
				if len(s.samples) == cap(s.samples) {
					copy(s.samples, s.samples[1:])
					s.samples = s.samples[:len(s.samples)-1]
				}
				s.samples = append(s.samples, sample)
			}
			s.mu.Unlock()
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
