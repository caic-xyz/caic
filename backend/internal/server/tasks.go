// Task HTTP route handlers.

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// taskHTTPHandlers owns task HTTP protocol concerns.
//
// It handles SSE streams, WebSocket proxying, route-level task lookup, and
// raw HTTP response writing. Task command orchestration and DTO assembly belong
// to taskAPIService.
type taskHTTPHandlers struct {
	taskMgr   *tasks.Manager
	repos     *repos.Service
	forge     *forgemanager.Manager
	ciService *ci.Service
	authStore *auth.Store
	service   *taskAPIService

	warnings *WarningStore
}

func (h *taskHTTPHandlers) notifyTaskChange() {
	h.taskMgr.NotifyTaskChange()
}

// handleTaskRawEvents delegates to handleTaskEvents — both endpoints now
// serve the same backend-neutral EventMessage stream.
func (s *taskHTTPHandlers) handleTaskRawEvents(w http.ResponseWriter, r *http.Request) {
	s.handleTaskEvents(w, r)
}

// handleTaskEvents streams agent messages as SSE using backend-neutral
// EventMessage DTOs. All tool invocations are emitted as toolUse events.
func (s *taskHTTPHandlers) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, api.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	// Terminal tasks have no live channel: replay their history straight from
	// the on-disk log without materializing it into memory. This keeps server
	// memory O(1) for the very large logs that previously failed to load.
	state := entry.Task().GetState()
	if (state == task.StatePurged || state == task.StateFailed) && entry.LoadedTask() != nil {
		s.streamHistoryFromDisk(w, flusher, entry)
		return
	}

	// Lazily load messages for purged tasks on first access.
	s.taskMgr.LoadMessagesOnDemand(entry)

	history, live, unsub := entry.Task().Subscribe(r.Context())
	defer unsub()
	statsHistory, statsLive, statsUnsub := entry.Task().SubscribeStats(r.Context())
	defer statsUnsub()

	tracker := v1conv.NewToolTimingTracker(entry.Task().Harness, FormatToolOutput)
	idx := 0

	writeEvents := func(events []v1.EventMessage) {
		for i := range events {
			data, err := v1conv.MarshalEvent(&events[i])
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
			idx++
		}
	}

	now := time.Now()
	for _, msg := range filterHistoryForReplay(history) {
		writeEvents(tracker.ConvertMessage(msg, now))
	}
	for i := range statsHistory {
		ev := v1conv.StatsEvent(&statsHistory[i])
		data, err := v1conv.MarshalEvent(&ev)
		if err == nil {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
			idx++
		}
	}
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	if state == task.StatePurged || state == task.StateFailed {
		return
	}

	liveCh := live
	statsCh := statsLive
	for liveCh != nil || statsCh != nil {
		select {
		case msg, ok := <-liveCh:
			if !ok {
				liveCh = nil
				continue
			}
			writeEvents(tracker.ConvertMessage(msg, time.Now()))
			flusher.Flush()
		case cs, ok := <-statsCh:
			if !ok {
				statsCh = nil
				continue
			}
			ev := v1conv.StatsEvent(&cs)
			data, err := v1conv.MarshalEvent(&ev)
			if err == nil {
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
				idx++
			}
			flusher.Flush()
		}
	}
}

// streamHistoryFromDisk replays a terminal task's conversation directly from
// its log file, converting and flushing one message at a time so the full
// history is never materialized in memory. It collapses streaming-delta runs
// (matching the live path's filterHistoryForReplay) and emits a trailing
// "ready" event. No subscriber is registered: terminal tasks produce no live
// messages.
func (s *taskHTTPHandlers) streamHistoryFromDisk(w http.ResponseWriter, flusher http.Flusher, entry *tasks.Entry) {
	tracker := v1conv.NewToolTimingTracker(entry.Task().Harness, FormatToolOutput)
	now := time.Now()
	idx := 0
	bytesSinceFlush := 0
	emit := func(msg agent.Message) {
		evs := tracker.ConvertMessage(msg, now)
		for i := range evs {
			data, err := v1conv.MarshalEvent(&evs[i])
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			n, _ := fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
			idx++
			bytesSinceFlush += n
			if bytesSinceFlush >= 65536 {
				flusher.Flush()
				bytesSinceFlush = 0
			}
		}
	}
	push, flush := newReplayFilter(emit)
	for msg, err := range entry.LoadedTask().StreamMessages() {
		if err != nil {
			slog.Warn("stream history from disk", "task", entry.Task().ID, "err", err)
			break
		}
		push(msg)
	}
	flush()
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
}

// handleTaskToolInput returns the full (untruncated) input for a tool call.
// It scans the task's message history for the ToolUseMessage with the given
// toolUseID and returns its Input field.
func (s *taskHTTPHandlers) handleTaskToolInput(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.service.taskToolInput(r.Context(), entry, r.PathValue("toolUseID"))
	writeJSONResponse(w, resp, err)
}

func (s *taskHTTPHandlers) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.service.taskDiff(r.Context(), entry, r.URL.Query().Get("path"))
	writeJSONResponse(w, resp, err)
}

// handleVNCWebSocket proxies a WebSocket connection to the instance's VNC
// TCP port via the Docker host port mapping. Used by noVNC in the frontend.
func (s *taskHTTPHandlers) handleVNCWebSocket(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.Task()
	snap := t.Snapshot()
	if snap.RuntimeInstanceID == "" || snap.VNCPort == 0 {
		writeError(w, api.BadRequest("task has no VNC display"))
		return
	}
	slog.Info("vnc proxy start", "task", t.ID, "instance", snap.RuntimeInstanceID, "port", snap.VNCPort)
	vncAddr := fmt.Sprintf("127.0.0.1:%d", snap.VNCPort)

	var d net.Dialer
	d.Timeout = 10 * time.Second
	vncConn, err := d.DialContext(r.Context(), "tcp", vncAddr)
	if err != nil {
		slog.Error("vnc websocket: dial failed", "addr", vncAddr, "err", err)
		writeError(w, api.InternalError("cannot reach instance VNC"))
		return
	}
	defer func() { _ = vncConn.Close() }()

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // same-origin, no Origin check needed
	})
	if err != nil {
		slog.Warn("vnc websocket: accept failed", "task", t.ID, "err", err)
		return
	}
	defer func() { _ = wsConn.Close(websocket.StatusNormalClosure, "") }()

	// Bidirectional copy: WebSocket ↔ TCP.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var written atomic.Int64
	go func() {
		defer cancel()
		for {
			_, buf, err := wsConn.Read(ctx)
			if err != nil {
				slog.Debug("vnc ws→tcp done", "task", t.ID, "err", err)
				return
			}
			if _, err := vncConn.Write(buf); err != nil {
				slog.Debug("vnc ws→tcp write failed", "task", t.ID, "err", err)
				return
			}
		}
	}()
	n, cpErr := io.Copy(wsNetConn{wsConn, ctx}, vncConn)
	written.Store(n)
	slog.Info("vnc proxy done", "task", t.ID, "vnc→ws_bytes", n, "err", cpErr)
}

// wsNetConn adapts a coder/websocket connection to net.Conn for io.Copy.
type wsNetConn struct {
	*websocket.Conn

	ctx context.Context
}

func (w wsNetConn) Read(b []byte) (int, error) {
	_, buf, err := w.Conn.Read(w.ctx)
	if err != nil {
		return 0, err
	}
	n := copy(b, buf)
	if n < len(buf) {
		return n, io.ErrShortBuffer
	}
	return n, nil
}

func (w wsNetConn) Write(b []byte) (int, error) {
	if err := w.Conn.Write(w.ctx, websocket.MessageBinary, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// getTask looks up a task by the {id} path parameter.
// When auth is enabled, returns 403 if the task belongs to a different user.
func (s *taskHTTPHandlers) getTask(r *http.Request) (*tasks.Entry, error) {
	return taskEntryFromRequest(r, s.taskMgr, s.authStore)
}

// taskEntryFromRequest looks up a task by the {id} path parameter.
// When auth is enabled, returns 403 if the task belongs to a different user.
func taskEntryFromRequest(r *http.Request, taskMgr *tasks.Manager, authStore *auth.Store) (*tasks.Entry, error) {
	id := r.PathValue("id")
	entry, ok := taskMgr.GetEntry(id)
	if !ok {
		return nil, api.NotFound("task")
	}
	if authStore != nil {
		if u, ok := auth.UserFromContext(r.Context()); ok {
			if owner := entry.Task().OwnerID; owner != "" && owner != u.ID {
				return nil, api.Forbidden("task")
			}
		}
	}
	return entry, nil
}

// SetUsageFetchers replaces the provider usage fetchers used by the usage
// endpoints. Intended for e2e tests to inject fake fetchers that return
// canned data without real API credentials.
func (s *Router) SetUsageFetchers(fetchers []usage.ProviderFetcher) {
	s.usageHandlers.fetchers = fetchers
}
