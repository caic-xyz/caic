// Tests tool timing correlation and the distinction between measured and unknown durations.

package usagedb

import (
	"testing"
	"time"
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
