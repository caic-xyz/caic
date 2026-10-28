// Serves account-isolated immutable history, mutable discovery/analytics, and exact raw-cursor SSE.

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maruel/gomode/sse"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/server/history"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// historyProjectionVersion versions canonical normalized event bytes.
// Change it whenever event conversion or serialization changes.
// Generation is the task object's random incarnation, invalidated on restart;
// deterministic same-generation reconstruction can reuse published range URLs.
const historyProjectionVersion = "1"

// historyHandlers owns the new history protocol independently of legacy SSE.
type historyHandlers struct{ *taskHandlers }

// historySource uses normal task authorization and validates resource identity
// before opening/projecting logs. Failed responses never enter the HTTP cache.
func (h *historyHandlers) historySource(w http.ResponseWriter, r *http.Request, immutable bool) (*taskmgr.Entry, *history.Source, error) {
	historyCacheHeaders(w)
	e, err := h.getTask(r)
	if err != nil {
		return nil, nil, err
	}
	if immutable {
		if r.PathValue("scope") != historyScope(r.Context()) {
			return nil, nil, &api.Error{Status: http.StatusForbidden, Code: api.CodeForbidden, Message: "history cache scope mismatch"}
		}
		if r.PathValue("version") != historyProjectionVersion || r.PathValue("generation") != e.Task().TimelineID() {
			return nil, nil, historyConflict()
		}
	} else if g := r.URL.Query().Get("generation"); g != "" && g != e.Task().TimelineID() {
		return nil, nil, historyConflict()
	}
	state := e.Task().GetState()
	if state.IsTerminal() || state == taskslog.StateStopped {
		return nil, nil, historyConflict()
	}
	s, err := h.taskSvc.history.Source(r.Context(), e)
	return e, s, err
}

func (h *historyHandlers) historySnapshot(ctx context.Context, e *taskmgr.Entry, s *history.Source, through uint64, latest bool) (history.Snapshot, error) {
	snap, err := s.Snapshot(ctx)
	if err == nil && (latest || snap.Through < through) {
		snap, err = s.Refresh(ctx)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		h.taskSvc.history.Invalidate(e, s)
	}
	if err == nil && snap.Through < through {
		return history.Snapshot{}, historyConflict()
	}
	return snap, err
}

func writeHistoryJSON(w http.ResponseWriter, r *http.Request, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, err := w.Write(data); err != nil {
		return
	}
}

func (h *historyHandlers) handleHistoryBootstrap(w http.ResponseWriter, r *http.Request) {
	e, s, err := h.historySource(w, r, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	snap, err := h.historySnapshot(r.Context(), e, s, 0, true)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	t := e.Task()
	scope := historyScope(r.Context())
	base := fmt.Sprintf("/api/caic/v1/tasks/%s/history/%s/%s/%s", t.ID, scope, historyProjectionVersion, t.TimelineID())
	b := v1.HistoryBootstrap{ProjectionVersion: historyProjectionVersion, Generation: t.TimelineID(), Scope: scope, RecordsPerChunk: history.RecordsPerChunk, Through: snap.Through, Cursor: historyCursor{generation: t.TimelineID(), record: snap.Through, end: true}.String(), ResourceBase: base}
	writeHistoryJSON(w, r, b)
}

func (h *historyHandlers) handleHistoryChunk(w http.ResponseWriter, r *http.Request) {
	e, s, err := h.historySource(w, r, true)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	first, last, err := historyRange(r.PathValue("first"), r.PathValue("last"))
	if err == nil && (first-1)%history.RecordsPerChunk != 0 {
		err = historyBadRange()
	}
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	snap, err := h.historySnapshot(r.Context(), e, s, last, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	// Identity-derived validators require no body decoding or unrelated raw tail
	// projection. Vary also prevents cached bodies crossing cookie/token changes.
	sum := sha256.Sum256([]byte(r.URL.Path))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	err = s.WriteRange(r.Context(), snap, first, last, w)
	// A streaming failure must terminate the response, never publish a truncated
	// JSON body as a successful immutable resource in a browser cache.
	if err != nil {
		panic(http.ErrAbortHandler)
	}
}

func historyEstimate(c history.TurnEstimate) v1.HistoryTurnEstimate {
	return v1.HistoryTurnEstimate{HistoryPosition: c.Position, Model: c.Model, Cost: v1.EventCost{USD: c.Cost.USD, Source: v1.EventCostSource(c.Cost.Source)}, TotalUSD: c.TotalUSD}
}

func (h *historyHandlers) handleHistoryCosts(w http.ResponseWriter, r *http.Request) {
	e, s, err := h.historySource(w, r, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	through, err := historyUint(r.URL.Query().Get("through"))
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	snap, err := h.historySnapshot(r.Context(), e, s, through, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Type", "application/json")
	if _, err = io.WriteString(w, "["); err != nil {
		return
	}
	enc := json.NewEncoder(w)
	comma := false
	for c, err := range s.TurnCosts(r.Context(), snap, through, e.Task().Pricer) {
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		if comma {
			if _, err := io.WriteString(w, ","); err != nil {
				return
			}
		}
		comma = true
		if err := enc.Encode(historyEstimate(c)); err != nil {
			return
		}
	}
	if _, err := io.WriteString(w, "]"); err != nil {
		return
	}
}

func historyCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Cookie")
	w.Header().Add("Vary", "Authorization")
}

func historyScope(ctx context.Context) string {
	access := taskAccessFromContext(ctx)
	sum := sha256.Sum256([]byte(access.userID + ":" + access.delegatingTaskID.String()))
	return hex.EncodeToString(sum[:16])
}

func historyConflict() error {
	return &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "history reference expired; fetch history/bootstrap"}
}

func historyBadRange() error {
	return &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "invalid physical history range"}
}

func historyUint(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	// Canonical decimal paths prevent aliases and arithmetic overflow at the API boundary.
	if err != nil || n == 0 || n > (1<<53)-1 || strconv.FormatUint(n, 10) != value {
		return 0, historyBadRange()
	}
	return n, nil
}

func historyRange(first, last string) (firstID, lastID uint64, err error) {
	f, err := historyUint(first)
	if err != nil {
		return 0, 0, err
	}
	l, err := historyUint(last)
	if err != nil {
		return 0, 0, err
	}
	// Context and overlays may start at a retained record inside a fixed chunk.
	// Chunk bodies additionally enforce their aligned first record at the route.
	if l < f || (f-1)/history.RecordsPerChunk != (l-1)/history.RecordsPerChunk {
		return 0, 0, historyBadRange()
	}
	return f, l, nil
}

func (h *historyHandlers) handleHistoryEvents(w http.ResponseWriter, r *http.Request) {
	e, s, err := h.historySource(w, r, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("cursor")
	}
	cursor, err := parseHistoryCursor(value)
	stream := sse.New(w)
	w.Header().Set("Cache-Control", "private, no-store")
	reset := func(ctx context.Context) {
		if err := historyFrame(stream, "reset", "", v1.HistoryReset{BootstrapURL: fmt.Sprintf("/api/caic/v1/tasks/%s/history/bootstrap", e.Task().ID)}); err != nil {
			h.log.DebugContext(ctx, "write history reset", "err", err)
		}
	}
	if err != nil || cursor.generation != e.Task().TimelineID() {
		reset(r.Context())
		return
	}
	// Subscribe before inspecting raw history. The semantic timeline is only a
	// wakeup: all bytes and IDs come from the shared authoritative raw projector.
	// A timer also detects physical controls that emit no semantic notification.
	_, live, unsub := e.Task().SubscribeLiveMessages(r.Context())
	defer unsub()
	stats, statsLive, statsUnsub := e.Task().SubscribeStats(r.Context())
	defer statsUnsub()
	if err := historyFrame(stream, "statsSnapshot", "", apiconv.StatsSnapshot(stats)); err != nil {
		return
	}
	snap, err := h.historySnapshot(r.Context(), e, s, cursor.record, true)
	if err != nil {
		h.historyStreamError(r, stream, err)
		return
	}
	if !cursor.end {
		found := false
		for event, err := range s.Events(r.Context(), snap, cursor.record, cursor.record) {
			if err != nil {
				h.historyStreamError(r, stream, err)
				return
			}
			if event.Index == cursor.index {
				found = true
				break
			}
		}
		if !found {
			reset(r.Context())
			return
		}
	}
	if err := h.writeHistorySuffix(r.Context(), stream, e, s, snap, &cursor); err != nil {
		h.historyStreamError(r, stream, err)
		return
	}
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-live:
			if !ok {
				return
			}
		case sample, ok := <-statsLive:
			if !ok {
				return
			}
			// Never attach a raw ID: same-timestamp disk revisions must still arrive.
			if err := historyFrame(stream, "stats", "", apiconv.Stats(&sample)); err != nil {
				return
			}
			continue
		case <-heartbeat.C:
			if err := stream.KeepAlive(); err != nil {
				return
			}
			continue
		case <-poll.C:
		}
		current, ok := h.taskMgr.GetEntry(e.Task().ID)
		if !ok || current != e || e.Task().TimelineID() != cursor.generation || e.Task().GetState().IsTerminal() || e.Task().GetState() == taskslog.StateStopped {
			reset(r.Context())
			return
		}
		snap, err = h.historySnapshot(r.Context(), e, s, cursor.record, true)
		if err != nil {
			h.historyStreamError(r, stream, err)
			return
		}
		if cursor.end && cursor.record == snap.Through {
			continue
		}
		if err := h.writeHistorySuffix(r.Context(), stream, e, s, snap, &cursor); err != nil {
			h.historyStreamError(r, stream, err)
			return
		}
	}
}

func (h *historyHandlers) writeHistorySuffix(ctx context.Context, stream *sse.Stream, e *taskmgr.Entry, s *history.Source, snapshot history.Snapshot, cursor *historyCursor) error {
	first := cursor.record
	if cursor.end {
		first++
	}
	if first > snapshot.Through {
		return historyFrame(stream, "checkpoint", cursor.String(), v1.HistoryCheckpoint{Cursor: cursor.String()})
	}
	for event, err := range s.Events(ctx, snapshot, first, snapshot.Through) {
		if err != nil {
			return err
		}
		if event.RecordID == cursor.record && !cursor.end && event.Index <= cursor.index {
			continue
		}
		next := historyCursor{generation: e.Task().TimelineID(), record: event.RecordID, index: event.Index}
		if err := historyFrame(stream, "", next.String(), event); err != nil {
			return err
		}
		*cursor = next
	}
	*cursor = historyCursor{generation: cursor.generation, record: snapshot.Through, end: true}
	return historyFrame(stream, "checkpoint", cursor.String(), v1.HistoryCheckpoint{Cursor: cursor.String()})
}

func (h *historyHandlers) historyStreamError(r *http.Request, stream *sse.Stream, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	h.log.WarnContext(r.Context(), "stream raw task history", "err", err)
	if err := historyFrame(stream, "error", "", v1.TaskHistoryStreamError{Message: "history unavailable; rediscover bootstrap and retry"}); err != nil {
		h.log.DebugContext(r.Context(), "write history stream error", "err", err)
	}
}

type historyCursor struct {
	generation string
	record     uint64
	index      uint32
	end        bool
}

func parseHistoryCursor(value string) (historyCursor, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != historyProjectionVersion || parts[1] == "" || len(parts[1]) > 128 {
		return historyCursor{}, historyConflict()
	}
	n, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || n > (1<<53)-1 || strconv.FormatUint(n, 10) != parts[2] {
		return historyCursor{}, historyConflict()
	}
	c := historyCursor{generation: parts[1], record: n, end: parts[3] == "end"}
	if !c.end {
		i, err := strconv.ParseUint(parts[3], 10, 32)
		if err != nil || n == 0 || strconv.FormatUint(i, 10) != parts[3] {
			return historyCursor{}, historyConflict()
		}
		c.index = uint32(i)
	}
	return c, nil
}

func (c historyCursor) String() string {
	output := "end"
	if !c.end {
		output = strconv.FormatUint(uint64(c.index), 10)
	}
	return fmt.Sprintf("%s.%s.%d.%s", historyProjectionVersion, c.generation, c.record, output)
}

func historyFrame(stream *sse.Stream, name, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if name != "" {
		if err := stream.Writef("event: %s\n", name); err != nil {
			return err
		}
	}
	if id != "" {
		if err := stream.Writef("id: %s\n", id); err != nil {
			return err
		}
	}
	if err := stream.Writef("data: %s\n\n", data); err != nil {
		return err
	}
	return stream.Flush()
}
