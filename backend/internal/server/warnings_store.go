// Account-scoped warning store coalesces categorized failure episodes for SSE clients.

package server

import (
	"slices"
	"sync"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

type serverWarning struct {
	ci.Warning

	ownerID string
	seq     uint64
}

// WarningStore retains active alerts by account and category. Recovery removes
// an alert, so a later failure receives a new ID. Diagnostic updates keep ID.
type WarningStore struct {
	taskMgr *taskmgr.Manager

	mu       sync.Mutex
	warnings []serverWarning
	seq      uint64
}

// NewWarningStore creates a warning store that wakes task-list subscribers.
func NewWarningStore(taskMgr *taskmgr.Manager) *WarningStore {
	return &WarningStore{taskMgr: taskMgr}
}

// Update coalesces an account's alert by category, retaining its episode ID.
// Details are copied so callers cannot mutate a published warning.
func (w *WarningStore) Update(ownerID string, category ci.WarningCategory, message string, details []ci.WarningDetail) {
	w.mu.Lock()
	idx := slices.IndexFunc(w.warnings, func(item serverWarning) bool {
		return item.ownerID == ownerID && item.Category == category
	})
	var id string
	if idx >= 0 {
		previous := w.warnings[idx]
		if previous.Message == message && slices.Equal(previous.Details, details) {
			w.mu.Unlock()
			return
		}
		id = previous.ID
		w.warnings = slices.Delete(w.warnings, idx, idx+1)
	} else {
		id = ksid.NewID().String()
	}
	w.seq++
	w.warnings = append(w.warnings, serverWarning{
		ID: id, Category: category, Message: message, Details: slices.Clone(details),
		ownerID: ownerID,
		seq:     w.seq,
	})
	w.mu.Unlock()
	w.taskMgr.NotifyTaskChange()
}

// Resolve marks recovery for an account and category, removing the active alert.
func (w *WarningStore) Resolve(ownerID string, category ci.WarningCategory) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warnings = slices.DeleteFunc(w.warnings, func(item serverWarning) bool {
		return item.ownerID == ownerID && item.Category == category
	})
}

// Since returns active warnings for ownerID newer than the revision cursor.
// A zero cursor replays active warnings on reconnect, retaining their IDs.
func (w *WarningStore) Since(ownerID string, seq uint64) []serverWarning {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []serverWarning
	for _, item := range w.warnings {
		if item.ownerID == ownerID && item.seq > seq {
			item.Details = slices.Clone(item.Details)
			out = append(out, item)
		}
	}
	return out
}
