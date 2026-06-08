// Server warning ring buffer delivered to SSE clients.

package server

import (
	"slices"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/tasks"
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
	taskMgr *tasks.Manager

	mu       sync.Mutex
	warnings []serverWarning
}

// NewWarningStore creates a warning store that notifies taskMgr subscribers on
// each new warning.
func NewWarningStore(taskMgr *tasks.Manager) *WarningStore {
	return &WarningStore{taskMgr: taskMgr}
}

// Emit delivers a CI warning to connected SSE clients.
func (s *WarningStore) Emit(msg string) {
	s.mu.Lock()
	now := time.Now()
	// Deduplicate: skip if the same message was emitted recently.
	for _, w := range slices.Backward(s.warnings) {
		if now.Sub(w.ts) > warningDedup {
			break
		}
		if w.msg == msg {
			s.mu.Unlock()
			return
		}
	}
	s.warnings = append(s.warnings, serverWarning{msg: msg, ts: now})
	if len(s.warnings) > maxWarnings {
		s.warnings = s.warnings[len(s.warnings)-maxWarnings:]
	}
	s.mu.Unlock()
	if s.taskMgr != nil {
		s.taskMgr.NotifyTaskChange()
	}
}

// Since returns all warnings with a timestamp after t.
func (s *WarningStore) Since(t time.Time) []serverWarning {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []serverWarning
	for _, w := range s.warnings {
		if w.ts.After(t) {
			out = append(out, w)
		}
	}
	return out
}
