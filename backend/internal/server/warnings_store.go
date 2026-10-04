// Warning store translates subsystem failures into account-scoped and public API warning episodes for SSE clients.

package server

import (
	"fmt"
	"slices"
	"sync"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/ci"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

type serverWarning struct {
	v1.Warning

	ownerID string
	global  bool
	seq     uint64
}

// WarningStore retains account-scoped warnings by owner and category, and public
// warnings by category. Recovery removes an episode, so a later failure receives
// a new ID. Diagnostic updates keep the ID.
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

// UpdateCI translates a CI warning and coalesces its account's failure episode.
// Details are copied so callers cannot mutate a published warning.
func (w *WarningStore) UpdateCI(ownerID string, category ci.WarningCategory, message string, details []ci.WarningDetail) error {
	c, err := ciWarningCategory(category)
	if err != nil {
		return err
	}
	d := make([]v1.WarningDetail, len(details))
	for i := range details {
		d[i] = v1.WarningDetail{Repo: details[i].Repo, Error: details[i].Error}
	}
	w.update(ownerID, false, c, message, d)
	return nil
}

// UpdateRuntimeRestore translates a runtime import failure into a public alert.
// It covers tasks with existing runtime instances, including idle or stopped
// instances, rather than purged history. Private diagnostics belong in logs.
func (w *WarningStore) UpdateRuntimeRestore(failure *taskmgr.ImportError) {
	message := fmt.Sprintf("%d tasks could not be restored.", failure.Failed)
	if failure.Failed == 1 {
		message = "1 task could not be restored."
	}
	w.update("", true, v1.WarningCategoryRuntimeRestoreFailed, message, []v1.WarningDetail{})
}

// ResolveCI translates a CI recovery and removes its account's active alert.
func (w *WarningStore) ResolveCI(ownerID string, category ci.WarningCategory) error {
	c, err := ciWarningCategory(category)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warnings = slices.DeleteFunc(w.warnings, func(item serverWarning) bool {
		return !item.global && item.ownerID == ownerID && item.Category == c
	})
	return nil
}

// Since returns active account and public server warnings newer than the revision cursor.
// A zero cursor replays active warnings on reconnect, retaining their IDs.
func (w *WarningStore) Since(ownerID string, seq uint64) []serverWarning {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []serverWarning
	for _, item := range w.warnings {
		if (item.global || item.ownerID == ownerID) && item.seq > seq {
			item.Details = slices.Clone(item.Details)
			out = append(out, item)
		}
	}
	return out
}

func (w *WarningStore) update(ownerID string, global bool, category v1.WarningCategory, message string, details []v1.WarningDetail) {
	w.mu.Lock()
	idx := slices.IndexFunc(w.warnings, func(item serverWarning) bool {
		return item.ownerID == ownerID && item.global == global && item.Category == category
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
		global:  global,
		seq:     w.seq,
	})
	w.mu.Unlock()
	w.taskMgr.NotifyTaskChange()
}

func ciWarningCategory(category ci.WarningCategory) (v1.WarningCategory, error) {
	switch category {
	case ci.WarningCategoryCIPollFailed:
		return v1.WarningCategoryCIPollFailed, nil
	default:
		return "", fmt.Errorf("unsupported CI warning category %q", category)
	}
}
