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
)

// historyProjectionVersion versions canonical normalized event bytes.
// Change it whenever event conversion, serialization, or line framing changes.
// Generation is the task object's random incarnation, invalidated on restart;
// deterministic same-generation reconstruction can reuse published range URLs.
const historyProjectionVersion = "2"

// historyHandlers serves task history for every task state.
type historyHandlers struct{ *taskHandlers }

// historySource uses normal task authorization and validates resource identity
// before opening/projecting logs. Failed responses never enter the HTTP cache.
func (h *historyHandlers) historySource(w http.ResponseWriter, r *http.Request, immutable bool) (*taskmgr.Entry, *history.Source, func(), error) {
	historyCacheHeaders(w)
	e, err := h.getTask(r)
	if err != nil {
		return nil, nil, nil, err
	}
	if immutable {
		if r.PathValue("scope") != historyScope(r.Context()) {
			return nil, nil, nil, &api.Error{Status: http.StatusForbidden, Code: api.CodeForbidden, Message: "history cache scope mismatch"}
		}
		if r.PathValue("version") != historyProjectionVersion || r.PathValue("generation") != e.Task().TimelineID() {
			return nil, nil, nil, historyConflict()
		}
	} else if g := r.URL.Query().Get("generation"); g != "" && g != e.Task().TimelineID() {
		return nil, nil, nil, historyConflict()
	}
	s, release, err := h.taskSvc.history.acquire(r.Context(), e)
	return e, s, release, err
}

func (h *historyHandlers) historySnapshot(ctx context.Context, e *taskmgr.Entry, s *history.Source, through uint64, latest bool) (history.Snapshot, error) {
	snap, err := s.Snapshot(ctx)
	if err == nil && (latest || snap.Through < through) {
		snap, err = s.Refresh(ctx)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		h.taskSvc.history.invalidate(e, s)
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

func (h *historyHandlers) handleHistoryManifest(w http.ResponseWriter, r *http.Request) {
	e, s, release, err := h.historySource(w, r, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	defer release()
	if _, err = h.historySnapshot(r.Context(), e, s, 0, true); err != nil {
		writeError(r.Context(), w, err)
		return
	}
	t := e.Task()
	snap, sections, metadata, err := s.Sections(r.Context(), t.InitialPrompt.Text)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	b := historyDiscovery(r.Context(), e, snap)
	writeHistoryJSON(w, r, v1.HistoryManifest{Bootstrap: b, Sections: sections, Metadata: metadata})
}

func (h *historyHandlers) handleHistorySlice(w http.ResponseWriter, r *http.Request) {
	e, s, release, err := h.historySource(w, r, true)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	defer release()
	body, err := historySlice(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	snap, err := h.historySnapshot(r.Context(), e, s, body.Last.RecordID, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if err = s.CheckSlice(r.Context(), snap, body); err != nil {
		writeError(r.Context(), w, historyBadRange())
		return
	}
	if immutableHistory(w, r) {
		return
	}
	if err = s.WriteSlice(r.Context(), snap, body, w); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (h *historyHandlers) handleHistoryActivity(w http.ResponseWriter, r *http.Request) {
	e, s, release, err := h.historySource(w, r, true)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	defer release()
	kind := v1.EventKind(r.PathValue("kind"))
	through, err := historyUint(r.PathValue("through"))
	if err != nil || kind != v1.EventKindNativeSubagent && kind != v1.EventKindBackgroundCommand {
		writeError(r.Context(), w, historyBadRange())
		return
	}
	var after history.Position
	query := r.URL.Query()
	if query.Has("afterRecordID") != query.Has("afterIndex") {
		writeError(r.Context(), w, historyBadRange())
		return
	}
	if query.Has("afterRecordID") {
		after.RecordID, err = historyUint(query.Get("afterRecordID"))
		index, parseErr := strconv.ParseUint(query.Get("afterIndex"), 10, 32)
		if err != nil || parseErr != nil || strconv.FormatUint(index, 10) != query.Get("afterIndex") || after.RecordID > through {
			writeError(r.Context(), w, historyBadRange())
			return
		}
		after.Index = uint32(index)
	}
	snap, err := h.historySnapshot(r.Context(), e, s, through, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	first, err := s.ActivityStart(r.Context(), kind, r.PathValue("activity"))
	if err != nil || first.RecordID > through {
		writeError(r.Context(), w, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "history activity not found"})
		return
	}
	if immutableHistory(w, r) {
		return
	}
	if err = s.WriteActivity(r.Context(), snap, kind, r.PathValue("activity"), through, after, w); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (h *historyHandlers) handleHistorySetup(w http.ResponseWriter, r *http.Request) {
	e, s, release, err := h.historySource(w, r, true)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	defer release()
	through, err := historyUint(r.PathValue("through"))
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	snap, err := h.historySnapshot(r.Context(), e, s, through, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	if immutableHistory(w, r) {
		return
	}
	if err = s.WriteSetup(r.Context(), snap, through, w); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func historySlice(r *http.Request) (v1.HistorySlice, error) {
	first, err := historyUint(r.PathValue("first"))
	if err != nil {
		return v1.HistorySlice{}, err
	}
	last, err := historyUint(r.PathValue("last"))
	if err != nil {
		return v1.HistorySlice{}, err
	}
	fi, err := historyIndex(r.PathValue("firstIndex"))
	if err != nil {
		return v1.HistorySlice{}, err
	}
	li := history.RecordEndIndex
	if r.PathValue("lastIndex") != "end" {
		li, err = historyIndex(r.PathValue("lastIndex"))
		if err != nil {
			return v1.HistorySlice{}, err
		}
	}
	if last < first || last == first && li < fi {
		return v1.HistorySlice{}, historyBadRange()
	}
	return v1.HistorySlice{First: v1.HistoryPosition{RecordID: first, Index: fi}, Last: v1.HistoryPosition{RecordID: last, Index: li}}, nil
}

func historyIndex(value string) (uint32, error) {
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n == uint64(history.RecordEndIndex) || strconv.FormatUint(n, 10) != value {
		return 0, historyBadRange()
	}
	return uint32(n), nil
}

// immutableHistory returns true when the validator completed the response.
func immutableHistory(w http.ResponseWriter, r *http.Request) bool {
	sum := sha256.Sum256([]byte(r.URL.RequestURI()))
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func historyDiscovery(ctx context.Context, e *taskmgr.Entry, snap history.Snapshot) v1.HistoryBootstrap {
	t := e.Task()
	scope := historyScope(ctx)
	base := fmt.Sprintf("/api/caic/v1/tasks/%s/history/%s/%s/%s", t.ID, scope, historyProjectionVersion, t.TimelineID())
	return v1.HistoryBootstrap{ProjectionVersion: historyProjectionVersion, Generation: t.TimelineID(), Scope: scope, RecordsPerChunk: history.RecordsPerChunk, Through: snap.Through, Cursor: historyCursor{generation: t.TimelineID(), record: snap.Through, end: true}.String(), ResourceBase: base}
}

func historyEstimate(c history.TurnEstimate) v1.HistoryTurnEstimate {
	return v1.HistoryTurnEstimate{HistoryPosition: c.Position, Model: c.Model, Cost: v1.EventCost{USD: c.Cost.USD, Source: v1.EventCostSource(c.Cost.Source)}, TotalUSD: c.TotalUSD}
}

func (h *historyHandlers) handleHistoryCosts(w http.ResponseWriter, r *http.Request) {
	e, s, release, err := h.historySource(w, r, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	defer release()
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
	return &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "history reference expired; fetch history/manifest"}
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

func (h *historyHandlers) handleHistoryEvents(w http.ResponseWriter, r *http.Request) {
	e, s, release, err := h.historySource(w, r, false)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	defer release()
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("cursor")
	}
	cursor, err := parseHistoryCursor(value)
	stream := sse.New(w)
	w.Header().Set("Cache-Control", "private, no-store")
	reset := func(ctx context.Context) {
		if err := historyFrame(stream, "reset", "", v1.HistoryReset{ManifestURL: fmt.Sprintf("/api/caic/v1/tasks/%s/history/manifest", e.Task().ID)}); err != nil {
			h.log.DebugContext(ctx, "write history reset", "err", err)
		}
	}
	if err != nil || cursor.generation != e.Task().TimelineID() || !h.taskSvc.history.isCurrent(e, s) {
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
		if !h.taskSvc.history.isCurrent(e, s) {
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
	var manifest *v1.HistoryManifest
	if !cursor.hasMetadata || snapshot.MetadataRevision != cursor.metadataRevision {
		current, sections, metadata, err := s.Sections(ctx, e.Task().InitialPrompt.Text)
		if err != nil {
			return err
		}
		snapshot = current
		bootstrap := historyDiscovery(ctx, e, snapshot)
		manifest = &v1.HistoryManifest{Bootstrap: bootstrap, Sections: sections, Metadata: metadata}
	}
	first := cursor.record
	if cursor.end {
		first++
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
	*cursor = historyCursor{generation: cursor.generation, record: snapshot.Through, end: true, metadataRevision: snapshot.MetadataRevision, hasMetadata: true}
	if err := historyFrame(stream, "checkpoint", cursor.String(), v1.HistoryCheckpoint{Cursor: cursor.String(), Through: cursor.record}); err != nil {
		return err
	}
	if manifest != nil {
		return historyFrame(stream, "manifest", "", manifest)
	}
	return nil
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
	generation       string
	record           uint64
	index            uint32
	end              bool
	metadataRevision uint64
	hasMetadata      bool
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
