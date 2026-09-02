// Tests for the background settled-history pass (runSettledHistory).

package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
	"github.com/maruel/ksid"
)

// writeSettledHistoryLog writes a minimal v1 task log (metadata header plus an
// optional result trailer) with the given mtime and returns its path. An empty
// state leaves the log non-terminal (running).
func writeSettledHistoryLog(t *testing.T, dir, name, state string, mtime time.Time) string {
	meta, err := json.Marshal(agent.MetaMessage{
		MessageType: "caic_meta",
		Version:     int(agent.LogVersionV1),
		Harness:     harness.Claude,
		Prompt:      "history",
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 0, len(meta)+len(state)+64)
	data = append(data, meta...)
	data = append(data, '\n')
	if state != "" {
		trailer, err := json.Marshal(agent.MetaResultMessage{
			MessageType: "caic_result",
			State:       state,
		})
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, trailer...)
		data = append(data, '\n')
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

func newSettledHistoryTestManager(t *testing.T, logStore *taskslog.Store) *taskmgr.Manager {
	router, err := runtime.NewRouter(slog.New(slog.DiscardHandler), []runtime.System{
		&modelRefreshSystem{testRuntimeBackend: &runtimetest.FakeBackend{}, Inventory: &runtimetest.FakeInventory{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := taskmgr.New(taskmgr.Config{
		ServerCtx:           t.Context(),
		Log:                 slog.New(slog.DiscardHandler),
		LogStore:            logStore,
		Runtimes:            router,
		Checkouts:           repo.NewRegistry(),
		RuntimeStartTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRunSettledHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logStore := taskslog.NewStore(testLogger(), dir)
	taskMgr := newSettledHistoryTestManager(t, logStore)

	now := time.Now().UTC()
	cutoffAgo := 2 * taskslog.SettledRetention
	// Task IDs are ksids: the loader only registers a parsed ID (insertLoadedTasks
	// randomises unparseable ones), and the filename base must equal the ID.
	idAlpha, idBeta, idGamma, idDelta := ksid.NewID().String(), ksid.NewID().String(), ksid.NewID().String(), ksid.NewID().String()
	recentTerminal := writeSettledHistoryLog(t, dir, idAlpha+".jsonl", "purged", now.Add(-time.Hour))
	running := writeSettledHistoryLog(t, dir, idBeta+".jsonl", "", now.Add(-time.Hour))
	staleTerminal := writeSettledHistoryLog(t, dir, idGamma+".jsonl", "purged", now.Add(-cutoffAgo))
	staleCompressed := writeSettledHistoryLog(t, dir, idDelta+".jsonl", "purged", now.Add(-cutoffAgo))
	if _, err := logStore.Compress(staleCompressed, nil, taskslog.StatePurged); err != nil {
		t.Fatal(err)
	}

	if err := runSettledHistory(t.Context(), testLogger(), logStore, taskMgr); err != nil {
		t.Fatal(err)
	}

	// A recent terminal plain log is settled by the pass and registered through
	// the settled scan.
	if _, err := os.Stat(recentTerminal); !os.IsNotExist(err) {
		t.Errorf("recent terminal plain log still exists: %v", err)
	}
	if _, err := os.Stat(recentTerminal + ".zst"); err != nil {
		t.Errorf("recent terminal log not compressed: %v", err)
	}
	if _, ok := taskMgr.GetEntry(idAlpha); !ok {
		t.Error("recent terminal task not registered")
	}

	// A non-terminal plain log is left plain and registered (coerced to a
	// terminal state because no live runtime owns it).
	if _, err := os.Stat(running); err != nil {
		t.Errorf("running plain log missing: %v", err)
	}
	if _, ok := taskMgr.GetEntry(idBeta); !ok {
		t.Error("running task not registered")
	}

	// A terminal plain log past the retention cutoff is still compressed (the
	// settle scan has no cutoff) but is not registered.
	if _, err := os.Stat(staleTerminal); !os.IsNotExist(err) {
		t.Errorf("stale terminal plain log still exists: %v", err)
	}
	if _, err := os.Stat(staleTerminal + ".zst"); err != nil {
		t.Errorf("stale terminal log not compressed: %v", err)
	}
	if _, ok := taskMgr.GetEntry(idGamma); ok {
		t.Error("stale terminal task registered despite retention cutoff")
	}

	// Compressed history past the retention cutoff is left in place and not
	// registered.
	if _, err := os.Stat(staleCompressed + ".zst"); err != nil {
		t.Errorf("stale compressed log missing: %v", err)
	}
	if _, ok := taskMgr.GetEntry(idDelta); ok {
		t.Error("stale compressed task registered despite retention cutoff")
	}
}
