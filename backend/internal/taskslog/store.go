// Store owns the on-disk task-log directory and its startup load recipe: header scanning, terminal compression, and settled-task selection.

package taskslog

import (
	"slices"
	"time"
)

// SettledRetention bounds how far back a task log's last state update can be
// and still reload as a settled (purged) task entry at startup.
const SettledRetention = 14 * 24 * time.Hour

// MaxSettledPerRepo caps how many settled task entries reload per repository,
// most recently updated first.
const MaxSettledPerRepo = 5

// Store owns one task-log directory.
type Store struct {
	LogDir string
	Writer *Writer
}

// NewStore creates a Store rooted at logDir.
func NewStore(logDir string) *Store {
	return &Store{LogDir: logDir, Writer: &Writer{LogDir: logDir}}
}

// LoadPlain scans LogDir and loads every task log header. See LoadLogs.
func (s *Store) LoadPlain() ([]*LoadedTask, error) {
	return LoadLogs(s.LogDir)
}

// CompressTerminal compresses loaded non-revivable task logs in place. See
// Writer.CompressTerminalLogs.
func (s *Store) CompressTerminal(logs []*LoadedTask) error {
	return s.Writer.CompressTerminalLogs(logs)
}

// Settled selects logs eligible to reload as settled task entries: updated
// within SettledRetention of now, capped at MaxSettledPerRepo per repository,
// most recently updated first.
func (s *Store) Settled(logs []*LoadedTask, now time.Time) []*LoadedTask {
	eligible := make([]*LoadedTask, 0, len(logs))
	for _, lt := range logs {
		if now.Sub(lt.LastStateUpdateAt) > SettledRetention {
			continue
		}
		eligible = append(eligible, lt)
	}
	slices.SortFunc(eligible, func(a, b *LoadedTask) int {
		return b.LastStateUpdateAt.Compare(a.LastStateUpdateAt)
	})
	perRepo := make(map[string]int)
	kept := eligible[:0]
	for _, lt := range eligible {
		key := ""
		if p := lt.Primary(); p != nil {
			key = p.Name
		}
		if perRepo[key] < MaxSettledPerRepo {
			perRepo[key]++
			kept = append(kept, lt)
		}
	}
	return kept
}
