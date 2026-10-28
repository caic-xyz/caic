// Owns live-task history sources and generation-scoped cleanup.

package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/server/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

type taskHistorySource struct {
	entry      *taskmgr.Entry
	generation string
	path       string
	source     *history.Source
}

// taskHistory owns source lifetime independently of individual HTTP requests.
// The manager change channel releases terminal/deleted tasks and invalidated
// generations; server context cancellation releases every remaining source.
type taskHistory struct {
	ctx     context.Context
	log     *slog.Logger
	manager *taskmgr.Manager

	mu      sync.Mutex
	sources map[ksid.ID]taskHistorySource
}

func newTaskHistory(ctx context.Context, log *slog.Logger, manager *taskmgr.Manager) *taskHistory {
	h := &taskHistory{ctx: ctx, log: log, manager: manager, sources: make(map[ksid.ID]taskHistorySource)}
	// This watcher ends with the owning server context; it has no independent
	// worker lifetime or shutdown wait contract.
	go func() {
		for {
			_, changed := manager.ChangeSnapshot()
			h.prune(ctx.Err() != nil)
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
			case <-changed:
			}
		}
	}()
	return h
}

// Source returns one shared projector for this live task incarnation. The
// caller must perform normal task authorization before using its payloads.
// Source.Refresh and Source.WriteRange accept request cancellation separately.
func (h *taskHistory) Source(ctx context.Context, entry *taskmgr.Entry) (*history.Source, error) {
	h.mu.Lock()
	var retired *history.Source
	defer func() {
		h.mu.Unlock()
		if retired != nil {
			h.closeSource(retired)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := h.ctx.Err(); err != nil {
		return nil, err
	}
	t := entry.Task()
	if state := t.GetState(); state.IsTerminal() || state == taskslog.StateStopped {
		return nil, errors.New("chunk projection requires a live task")
	}
	id, generation, path := t.ID, t.TimelineID(), entry.LogPath.Get()
	if cached, ok := h.sources[id]; ok {
		if cached.entry == entry && cached.generation == generation && cached.path == path {
			return cached.source, nil
		}
		retired = cached.source
		delete(h.sources, id)
	}
	source, err := history.NewSource(ctx, path, h.manager.Backends)
	if err != nil {
		return nil, err
	}
	h.sources[id] = taskHistorySource{entry: entry, generation: generation, path: path, source: source}
	return source, nil
}

// Invalidate withdraws a failed projector so the next request can reconstruct
// it. A late failure from an older generation cannot remove its replacement.
func (h *taskHistory) Invalidate(entry *taskmgr.Entry, source *history.Source) {
	h.mu.Lock()
	id := entry.Task().ID
	if cached, ok := h.sources[id]; ok && cached.source == source {
		delete(h.sources, id)
		h.mu.Unlock()
		h.closeSource(source)
		return
	}
	h.mu.Unlock()
}

func (h *taskHistory) prune(all bool) {
	h.mu.Lock()
	var retired []*history.Source
	for id, cached := range h.sources {
		entry, exists := h.manager.GetEntry(id)
		remove := all || !exists || entry != cached.entry
		if !remove {
			t := entry.Task()
			state := t.GetState()
			remove = state.IsTerminal() || state == taskslog.StateStopped || t.TimelineID() != cached.generation || entry.LogPath.Get() != cached.path
		}
		if remove {
			delete(h.sources, id)
			retired = append(retired, cached.source)
		}
	}
	h.mu.Unlock()
	for _, source := range retired {
		h.closeSource(source)
	}
}

func (h *taskHistory) closeSource(source *history.Source) {
	if err := source.Close(); err != nil {
		h.log.Warn("close task history projection", "err", err)
	}
}
