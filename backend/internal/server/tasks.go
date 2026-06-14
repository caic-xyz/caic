// Task HTTP route handlers.

package server

import (
	"bytes"
	"context"
	"encoding/json"
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
	terminal := state == task.StatePurged || state == task.StateCrashed || state == task.StateFailed
	loadedTask := entry.LoadedTask()
	if terminal && loadedTask != nil {
		s.streamHistoryFromDisk(w, flusher, entry)
		return
	}

	// Lazily load messages for entries that do not have a disk stream path.
	if loadedTask == nil {
		s.taskMgr.LoadMessagesOnDemand(entry)
	}

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
	if loadedTask != nil {
		s.streamHistoryFromDiskWithTracker(w, flusher, entry, tracker, &idx)
	} else {
		for _, msg := range filterHistoryForReplay(history) {
			writeEvents(tracker.ConvertMessage(msg, now))
		}
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

	if state == task.StatePurged || state == task.StateCrashed || state == task.StateFailed {
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
	idx := 0
	s.streamHistoryFromDiskWithTracker(w, flusher, entry, tracker, &idx)
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
}

func (s *taskHTTPHandlers) streamHistoryFromDiskWithTracker(w http.ResponseWriter, flusher http.Flusher, entry *tasks.Entry, tracker *v1conv.ToolTimingTracker, idx *int) {
	lt := entry.LoadedTask()
	if lt == nil {
		return
	}
	now := time.Now()
	bytesSinceFlush := 0
	emit := func(msg agent.Message) {
		evs := tracker.ConvertMessage(msg, now)
		for i := range evs {
			data, err := v1conv.MarshalEvent(&evs[i])
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			n, _ := fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, *idx)
			(*idx)++
			bytesSinceFlush += n
			if bytesSinceFlush >= 65536 {
				flusher.Flush()
				bytesSinceFlush = 0
			}
		}
	}
	push, flush := newReplayFilter(emit)
	for msg, err := range lt.StreamMessages() {
		if err != nil {
			slog.Warn("stream history from disk", "task", entry.Task().ID, "err", err)
			break
		}
		push(msg)
	}
	flush()
}

// handleTaskListEvents streams patch events for the task list as SSE. On first
// iteration it sends a full snapshot; thereafter it sends only upsert/delete
// events for changed or removed tasks. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch runner-internal state transitions.
func (s *taskHTTPHandlers) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, api.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// With GitHub App configured, CI updates arrive via check_suite webhooks;
	// use a nil channel so the ticker case is never selected. With no CI service
	// wired (e.g. a router built without automation), never poll.
	var ciTickerC <-chan time.Time
	if s.ciService != nil && s.forge.GitHubApp() == nil {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		ciTickerC = t.C
	}

	// Seed CI status immediately on connect (once); subsequent updates come from
	// webhooks (App) or the ciTicker (polling).
	ctx := r.Context()
	if s.ciService != nil {
		go s.ciService.PollCIForActiveRepos(context.WithoutCancel(ctx))
	}

	// prevByID tracks the last marshalled JSON for each task ID.
	prevByID := map[string][]byte{}
	var prevReposJSON []byte
	var lastWarnTime time.Time
	first := true

	for {
		out := s.service.taskListSnapshot(ctx)
		ch := s.taskMgr.Changed()
		repoList := repoListFromSnapshot(s.repos.SnapshotWithCI())
		var newWarnings []serverWarning
		if s.warnings != nil {
			newWarnings = s.warnings.Since(lastWarnTime)
		}

		reposJSON, err := json.Marshal(repoList)
		if err != nil {
			slog.Warn("marshal repos", "err", err)
			return
		}

		if first {
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "snapshot", Snapshot: out}); err != nil {
				slog.Warn("marshal task list snapshot", "err", err)
				return
			}
			if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
				slog.Warn("marshal repos snapshot", "err", err)
				return
			}
			for i := range out {
				data, err := json.Marshal(&out[i])
				if err != nil {
					slog.Warn("marshal task entry", "err", err)
					continue
				}
				prevByID[out[i].ID.String()] = data
			}
			prevReposJSON = reposJSON
			first = false
		} else {
			// Emit upserts/patches for new or changed tasks.
			currentIDs := make(map[string]struct{}, len(out))
			for i := range out {
				id := out[i].ID.String()
				currentIDs[id] = struct{}{}
				data, err := json.Marshal(&out[i])
				if err != nil {
					slog.Warn("marshal task", "id", id, "err", err)
					continue
				}
				if !bytes.Equal(data, prevByID[id]) {
					prev := prevByID[id]
					prevByID[id] = data
					if prev == nil {
						// New task: emit full object.
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "upsert", Upsert: &out[i]}); err != nil {
							slog.Warn("marshal task upsert", "id", id, "err", err)
							return
						}
					} else {
						// Existing task changed: emit only the diff.
						patch, err := computeTaskPatch(prev, data)
						if err != nil {
							slog.Warn("compute task patch", "id", id, "err", err)
							continue
						}
						if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "patch", Patch: patch}); err != nil {
							slog.Warn("marshal task patch", "id", id, "err", err)
							return
						}
					}
				}
			}
			// Emit deletes for removed tasks.
			for id := range prevByID {
				if _, ok := currentIDs[id]; !ok {
					if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "delete", Delete: id}); err != nil {
						slog.Warn("marshal task delete", "id", id, "err", err)
						return
					}
					delete(prevByID, id)
				}
			}
			// Emit any new warnings.
			for _, warn := range newWarnings {
				if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "warning", Warning: warn.msg}); err != nil {
					slog.Warn("marshal warning", "err", err)
					return
				}
				lastWarnTime = warn.ts
			}

			// Emit repos update when default-branch CI status has changed.
			if !bytes.Equal(reposJSON, prevReposJSON) {
				prevReposJSON = reposJSON
				if err := emitTaskListEvent(w, flusher, v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
					slog.Warn("marshal repos update", "err", err)
					return
				}
			}
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		case <-ciTickerC:
			go s.ciService.PollCIForActiveRepos(context.WithoutCancel(r.Context()))
		}
	}
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
