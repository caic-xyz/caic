// Owns bounded task-history projections and leases across task lifecycle changes.

package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/server/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

type taskHistorySource struct {
	entry      *taskmgr.Entry
	generation string
	path       string
	source     *history.Source
	users      int
	used       uint64
	retired    bool
}

// taskHistory retains at most eight idle projections in addition to currently
// leased sources. Retirement prevents new use; the last caller closes the
// projector after its request finishes, including requests spanning compression.
type taskHistory struct {
	ctx     context.Context
	log     *slog.Logger
	manager *taskmgr.Manager
	mu      sync.Mutex
	sources map[ksid.ID]*taskHistorySource
	clock   uint64
}

func newTaskHistory(ctx context.Context, log *slog.Logger, manager *taskmgr.Manager, workers *backgroundWorkers) *taskHistory {
	h := &taskHistory{ctx: ctx, log: log, manager: manager, sources: make(map[ksid.ID]*taskHistorySource)}
	workers.Go(func() {
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
	})
	return h
}

// acquire leases a shared projector until release. Callers authorize the task
// before acquisition and must release once, after all source operations finish.
func (h *taskHistory) acquire(ctx context.Context, entry *taskmgr.Entry) (*history.Source, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := h.ctx.Err(); err != nil {
		return nil, nil, err
	}
	t := entry.Task()
	id, generation, path := t.ID, t.TimelineID(), entry.LogPath.Get()
	cached := h.sources[id]
	if cached != nil && (cached.entry != entry || cached.generation != generation || cached.path != path) {
		h.retireLocked(id, cached)
		cached = nil
	}
	if cached == nil {
		source, err := history.NewSource(ctx, path, h.manager.Backends)
		if err != nil {
			return nil, nil, err
		}
		cached = &taskHistorySource{entry: entry, generation: generation, path: path, source: source}
		h.sources[id] = cached
	}
	h.clock++
	cached.used = h.clock
	cached.users++
	return cached.source, func() { h.release(cached) }, nil
}

// isCurrent compares the acquired source with its bound entry, path, and
// generation, including replacement that occurred while the source opened.
func (h *taskHistory) isCurrent(entry *taskmgr.Entry, source *history.Source) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := entry.Task()
	cached := h.sources[t.ID]
	current, exists := h.manager.GetEntry(t.ID)
	return exists && current == entry && cached != nil && cached.source == source && cached.path == entry.LogPath.Get() && cached.generation == t.TimelineID()
}

func (h *taskHistory) release(cached *taskHistorySource) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cached.users--
	if cached.retired && cached.users == 0 {
		h.closeSource(cached.source)
	}
	h.evictLocked()
}

// invalidate withdraws a failed projector. Existing leases remain safe until
// their requests finish; a late failure cannot remove a replacement source.
func (h *taskHistory) invalidate(entry *taskmgr.Entry, source *history.Source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := entry.Task().ID
	if cached := h.sources[id]; cached != nil && cached.source == source {
		h.retireLocked(id, cached)
	}
}

func (h *taskHistory) retireLocked(id ksid.ID, cached *taskHistorySource) {
	delete(h.sources, id)
	cached.retired = true
	if cached.users == 0 {
		h.closeSource(cached.source)
	}
}

func (h *taskHistory) evictLocked() {
	for {
		idle := 0
		var oldest *taskHistorySource
		var key ksid.ID
		for id, cached := range h.sources {
			if cached.users != 0 {
				continue
			}
			idle++
			if oldest == nil || cached.used < oldest.used {
				oldest, key = cached, id
			}
		}
		if idle <= 8 {
			return
		}
		h.retireLocked(key, oldest)
	}
}

func (h *taskHistory) prune(all bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, cached := range h.sources {
		entry, exists := h.manager.GetEntry(id)
		if all || !exists || entry != cached.entry || entry.Task().TimelineID() != cached.generation || entry.LogPath.Get() != cached.path {
			h.retireLocked(id, cached)
		}
	}
}

func (h *taskHistory) closeSource(source *history.Source) {
	if err := source.Close(); err != nil {
		h.log.Warn("close task history projection", "err", err)
	}
}
