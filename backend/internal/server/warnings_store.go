// Server warning ring buffer delivered to SSE clients.

package server

import (
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// serverWarning is a timestamped warning message stored for SSE clients.
type serverWarning struct {
	msg string
	ts  time.Time
}

const (
	// maxWarnings caps the warning ring buffer.
	maxWarnings = 100
	// warningDedup suppresses duplicate messages within this window.
	warningDedup = 5 * time.Minute
)

// WarningStore is a ring buffer of timestamped server warnings delivered to SSE
// clients. CI automation (owned by internal/app) writes to it; the task-list SSE
// handler reads from it.
type WarningStore struct {
	taskMgr *taskmgr.Manager

	mu       sync.Mutex
	warnings []serverWarning
}

// NewWarningStore creates a warning store that notifies taskMgr subscribers on
// each new warning.
func NewWarningStore(taskMgr *taskmgr.Manager) *WarningStore {
	return &WarningStore{taskMgr: taskMgr}
}

// Emit delivers a CI warning to connected SSE clients.
func (w *WarningStore) Emit(msg string) {
	w.mu.Lock()
	now := time.Now()
	// Deduplicate: skip if the same message was emitted recently.
	for _, item := range slices.Backward(w.warnings) {
		if now.Sub(item.ts) > warningDedup {
			break
		}
		if item.msg == msg {
			w.mu.Unlock()
			return
		}
	}
	w.warnings = append(w.warnings, serverWarning{msg: msg, ts: now})
	if len(w.warnings) > maxWarnings {
		w.warnings = w.warnings[len(w.warnings)-maxWarnings:]
	}
	w.mu.Unlock()
	if w.taskMgr != nil {
		w.taskMgr.NotifyTaskChange()
	}
}

// Since returns all warnings with a timestamp after t.
func (w *WarningStore) Since(t time.Time) []serverWarning {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []serverWarning
	for _, item := range w.warnings {
		if item.ts.After(t) {
			out = append(out, item)
		}
	}
	return out
}
