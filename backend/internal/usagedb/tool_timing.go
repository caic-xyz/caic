// Tool timing pairs source-neutral tool starts and results for daily usage rollups.

package usagedb

import "time"

const maxPendingToolStarts = 1024

type toolStart struct {
	name string
	at   time.Time
}

// ToolTimingTracker bounds unmatched starts and measures completed tool calls.
// A native duration takes precedence; otherwise both producer timestamps must
// be present and ordered. Missing measurements never become zero-duration calls.
type ToolTimingTracker struct {
	pending map[string]toolStart
}

// Start remembers one tool call until its result arrives.
func (t *ToolTimingTracker) Start(id, name string, at time.Time) {
	if id == "" || name == "" {
		return
	}
	if t.pending == nil {
		t.pending = make(map[string]toolStart)
	}
	if _, exists := t.pending[id]; exists {
		return
	}
	if len(t.pending) >= maxPendingToolStarts {
		clear(t.pending)
	}
	t.pending[id] = toolStart{name: name, at: at}
}

// Finish returns the named duration for a matched tool result.
func (t *ToolTimingTracker) Finish(id string, at time.Time, nativeMs int64) (name string, durationMs int64, measured bool) {
	start, ok := t.pending[id]
	if !ok {
		return "", 0, false
	}
	delete(t.pending, id)
	if nativeMs > 0 {
		return start.name, nativeMs, true
	}
	if start.at.IsZero() || at.IsZero() || !at.After(start.at) {
		return "", 0, false
	}
	ms := at.Sub(start.at).Milliseconds()
	return start.name, ms, ms > 0
}
