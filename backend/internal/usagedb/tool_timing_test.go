// Tests tool timing correlation and the distinction between measured and unknown durations.

package usagedb

import (
	"testing"
	"time"

	"github.com/maruel/ksid"
)

func TestToolTimingTracker(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, time.February, 5, 10, 0, 0, 0, time.UTC)
	var tracker ToolTimingTracker
	tracker.Start("native", "Bash", at)
	if name, ms, ok := tracker.Finish("native", at.Add(5*time.Second), 1200); !ok || name != "Bash" || ms != 1200 {
		t.Errorf("native timing = %q, %d, %t", name, ms, ok)
	}
	tracker.Start("clock", "Read", at)
	tracker.Start("clock", "Read", at.Add(time.Second)) // repeated assistant record keeps the first start
	if name, ms, ok := tracker.Finish("clock", at.Add(1500*time.Millisecond), 0); !ok || name != "Read" || ms != 1500 {
		t.Errorf("producer timing = %q, %d, %t", name, ms, ok)
	}
	tracker.Start("missing", "Edit", time.Time{})
	if _, _, ok := tracker.Finish("missing", at, 0); ok {
		t.Error("missing producer start gained a duration")
	}
	tracker.Start("regressed", "Edit", at)
	if _, _, ok := tracker.Finish("regressed", at.Add(-time.Second), 0); ok {
		t.Error("regressed producer clock gained a duration")
	}
	if _, _, ok := tracker.Finish("clock", at, 0); ok {
		t.Error("completed tool was counted twice")
	}
	tracker.Start("synthetic", "Read", time.Time{})
	if _, _, ok := tracker.Finish("synthetic", at, 0); ok {
		t.Error("server receipt time must not supply a producer duration")
	}
}

func TestStoreToolTiming(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newTestStore(t, dir)
	meta := testMeta(ksid.NewID())
	at := atUTC(5, 10, 0, 0)
	s.Observe(meta, &Event{At: at, ToolProducerTime: at, ToolStartID: "a", ToolName: "Bash", Delta: Delta{ToolCalls: map[string]int{"Bash": 1}}})
	s.Observe(meta, &Event{At: at.Add(2 * time.Second), ToolProducerTime: at.Add(2 * time.Second), ToolResultID: "a"})
	s.Observe(meta, &Event{At: at.Add(3 * time.Second), ToolProducerTime: at.Add(3 * time.Second), ToolStartID: "b", ToolName: "Bash", Delta: Delta{ToolCalls: map[string]int{"Bash": 1}}})
	s.Observe(meta, &Event{At: at.Add(4 * time.Second), TurnBoundary: true, Delta: Delta{Turns: 1}})
	day := s.Days()[0]
	if day.Tools["Bash"] != 2 || day.ToolTimings["Bash"] != (ToolTiming{Count: 1, DurationMs: 2000}) {
		t.Errorf("live tool totals = %v, %v", day.Tools, day.ToolTimings)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := newTestStore(t, dir)
	day = reopened.Days()[0]
	if day.Tools["Bash"] != 2 || day.ToolTimings["Bash"] != (ToolTiming{Count: 1, DurationMs: 2000}) {
		t.Errorf("recovered tool totals = %v, %v", day.Tools, day.ToolTimings)
	}
	// Adoption replays the start at the durable watermark before seeing its
	// unflushed result. The start restores correlation without recounting a call.
	reopened.Observe(meta, &Event{At: at.Add(3 * time.Second), ToolProducerTime: at.Add(3 * time.Second), Replayed: true, ToolStartID: "b", ToolName: "Bash", Delta: Delta{ToolCalls: map[string]int{"Bash": 1}}})
	reopened.Observe(meta, &Event{At: at.Add(5 * time.Second), ToolProducerTime: at.Add(5 * time.Second), Replayed: true, ToolResultID: "b"})
	reopened.Observe(meta, &Event{At: at.Add(6 * time.Second), Replayed: true, TurnBoundary: true, Delta: Delta{Turns: 1}})
	day = reopened.Days()[0]
	if day.Tools["Bash"] != 2 || day.ToolTimings["Bash"] != (ToolTiming{Count: 2, DurationMs: 4000}) {
		t.Errorf("resumed tool totals = %v, %v", day.Tools, day.ToolTimings)
	}
	reopened.Observe(meta, &Event{At: at.Add(7 * time.Second), ToolStartID: "c", ToolName: "Bash", Delta: Delta{ToolCalls: map[string]int{"Bash": 1}}})
	reopened.Observe(meta, &Event{At: at.Add(8 * time.Second), ToolResultID: "c"})
	reopened.Observe(meta, &Event{At: at.Add(9 * time.Second), TurnBoundary: true, Delta: Delta{Turns: 1}})
	day = reopened.Days()[0]
	if day.Tools["Bash"] != 3 || day.ToolTimings["Bash"] != (ToolTiming{Count: 2, DurationMs: 4000}) {
		t.Errorf("timestamp-less tool totals = %v, %v", day.Tools, day.ToolTimings)
	}
}
