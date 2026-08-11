// HTTP handlers for task SSE streams, WebSocket proxying, route-level task lookup, and raw response writing.

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/caic-xyz/caic/backend/internal/eventreplay"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/repo/repomgr"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// taskHandlers owns task HTTP protocol concerns.
//
// It handles SSE streams, WebSocket proxying, route-level task lookup, and
// raw HTTP response writing. Task command orchestration and DTO assembly belong
// to taskService.
type taskHandlers struct {
	taskMgr    *taskmgr.Manager
	repoSvc    *repomgr.Service
	repoStatus *ci.RepoStatusStore
	forgeMgr   *forgemgr.Manager
	ciSvc      *ci.Service
	authStore  *auth.Store
	taskSvc    *taskService

	warnings *WarningStore
}

// historyLoadError marks a failure before any task history is available for SSE
// publication. It is the sole condition that can produce the history-unavailable
// frame; later stats, ready, flush, and live-stream errors remain their own errors.
type historyLoadError struct{ err error }

func (e *historyLoadError) Error() string { return e.err.Error() }

func (e *historyLoadError) Unwrap() error { return e.err }

// taskEventStream writes one task's ordered SSE event stream.
type taskEventStream struct {
	ctx        context.Context
	w          http.ResponseWriter
	flusher    http.Flusher
	controller *http.ResponseController
	tracker    *v1conv.ToolTimingTracker
	nextID     int
}

func (s *taskEventStream) writeMessages(messages []agent.Message, at time.Time) error {
	for _, msg := range messages {
		events := s.tracker.ConvertMessage(msg, at)
		for i := range events {
			if err := s.writeEvent(&events[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *taskEventStream) writeStats(stats []runtime.Stats) error {
	for i := range stats {
		ev := v1conv.StatsEvent(&stats[i])
		if err := s.writeEvent(&ev); err != nil {
			return err
		}
	}
	return nil
}

func (s *taskEventStream) writeEvent(ev *v1.EventMessage) error {
	data, err := v1conv.MarshalEvent(ev)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	if _, err := fmt.Fprintf(s.w, "event: message\ndata: %s\nid: %d\n\n", data, s.nextID); err != nil {
		return fmt.Errorf("write SSE event: %w", err)
	}
	s.nextID++
	return nil
}

func (s *taskEventStream) writeReady() error {
	if _, err := fmt.Fprint(s.w, "event: ready\ndata: {}\n\n"); err != nil {
		return fmt.Errorf("write SSE ready event: %w", err)
	}
	return nil
}

func (h *taskHandlers) notifyTaskChange() {
	h.taskMgr.NotifyTaskChange()
}

// handleTaskEvents streams agent messages as SSE using backend-neutral
// EventMessage DTOs. All tool invocations are emitted as toolUse events.
func (h *taskHandlers) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
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
	stream := taskEventStream{
		ctx:        r.Context(),
		w:          w,
		flusher:    flusher,
		controller: http.NewResponseController(w),
	}
	if err := stream.controller.Flush(); err != nil {
		slog.WarnContext(r.Context(), "start task SSE stream", "err", err)
		return
	}

	// Terminal tasks have no live channel: replay their history straight from
	// the on-disk log without materializing it into memory. This keeps server
	// memory O(1) for the very large logs that previously failed to load.
	state := entry.Task().GetState()
	loadedTask := entry.LoadedTask()
	if isTaskEventTerminal(state) && loadedTask != nil {
		h.streamHistoryFromDisk(r.Context(), w, flusher, entry)
		return
	}
	if err := h.streamTaskEvents(&stream, entry, state, loadedTask); err != nil {
		slog.WarnContext(r.Context(), "stream task SSE events", "task", entry.Task().ID, "err", err)
		var historyErr *historyLoadError
		if r.Context().Err() == nil && errors.As(err, &historyErr) {
			writeReplayHistoryError(w, flusher)
		}
	}
}

func (h *taskHandlers) streamTaskEvents(stream *taskEventStream, entry *taskmgr.Entry, state task.State, loadedTask *task.LoadedTask) error {
	// Lazily load messages for entries that do not have a disk stream path.
	if loadedTask == nil {
		h.taskMgr.LoadMessagesOnDemand(entry)
	}

	history, live, unsub := entry.Task().Subscribe(stream.ctx)
	defer unsub()
	statsHistory, statsLive, statsUnsub := entry.Task().SubscribeStats(stream.ctx)
	defer statsUnsub()

	stream.tracker = v1conv.NewToolTimingTracker(entry.Task().Harness, FormatToolOutput)

	now := time.Now()
	if shouldReplayHistoryFromDisk(state, loadedTask) {
		// A stopped task has no reliable live history source. Its disk history is
		// published only through the EOF- and identity-gated replay store; a
		// failed rebuild returns an explicit stream error rather than a misleading
		// empty ready conversation.
		if err := h.streamReplayStore(stream.ctx, stream.w, stream.flusher, entry, &stream.nextID); err != nil {
			return &historyLoadError{err: err}
		}
	} else {
		if err := stream.writeMessages(filterHistoryForReplay(history), now); err != nil {
			return err
		}
	}
	if err := stream.writeStats(statsHistory); err != nil {
		return err
	}
	if err := stream.writeReady(); err != nil {
		return err
	}
	if err := stream.controller.Flush(); err != nil {
		return fmt.Errorf("flush task SSE stream: %w", err)
	}

	if isTaskEventTerminal(state) {
		return nil
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
			if err := stream.writeMessages([]agent.Message{msg}, time.Now()); err != nil {
				return err
			}
			if err := stream.controller.Flush(); err != nil {
				return fmt.Errorf("flush task SSE stream: %w", err)
			}
		case cs, ok := <-statsCh:
			if !ok {
				statsCh = nil
				continue
			}
			if err := stream.writeStats([]runtime.Stats{cs}); err != nil {
				return err
			}
			if err := stream.controller.Flush(); err != nil {
				return fmt.Errorf("flush task SSE stream: %w", err)
			}
		}
	}
	return nil
}

func isTaskEventTerminal(state task.State) bool {
	return state == task.StatePurged || state == task.StateCrashed || state == task.StateFailed
}

// streamHistoryFromDisk replays a terminal task without subscribing to live
// messages. It prefers the EventMessage sidecar: validated zstd JSONL lines are
// copied straight into SSE frames. Raw-log parsing is only the cold fallback used
// to regenerate a missing or stale sidecar.
func (h *taskHandlers) streamHistoryFromDisk(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, entry *taskmgr.Entry) {
	idx := 0
	if err := h.streamReplayStore(ctx, w, flusher, entry, &idx); err != nil {
		slog.Warn("stream terminal replay", "task", entry.Task().ID, "err", err)
		if ctx.Err() == nil {
			writeReplayHistoryError(w, flusher)
		}
		return
	}
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
}

func writeReplayHistoryError(w io.Writer, flusher http.Flusher) {
	data, err := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: "task history is unavailable"})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

// streamReplayStore serves the rebuildable DTO sidecar, regenerating it from
// the raw log on a miss. A successful cache hit never invokes a harness parser
// or DTO converter; it only decompresses lines and frames them as SSE.
func (h *taskHandlers) streamReplayStore(ctx context.Context, w io.Writer, flusher http.Flusher, entry *taskmgr.Entry, idx *int) error {
	lt := entry.LoadedTask()
	if lt == nil || lt.LogPath() == "" {
		return errors.New("task has no replayable log")
	}
	logPath := lt.LogPath()
	if replay, ok := eventreplay.OpenReplay(logPath, lt.CacheProofForLog); ok {
		if err := ctx.Err(); err != nil {
			replay.Close()
			return err
		}
		result := replay.WriteSSE(w, flusher, idx)
		replay.Close()
		switch result {
		case eventreplay.SSEComplete:
			return nil
		case eventreplay.SSEPartial:
			return errors.New("replay SSE write failed after history publication")
		case eventreplay.SSEUnpublished:
			// No bytes reached the client, so a full regeneration can start at ID 0.
		}
	}
	if err := eventreplay.RegenerateReplay(ctx, logPath, lt.CacheProofForLog, lt.ScanMessagesWithContext); err != nil {
		return fmt.Errorf("regenerate replay cache: %w", err)
	}
	replay, ok := eventreplay.OpenReplay(logPath, lt.CacheProofForLog)
	if !ok {
		return errors.New("regenerated replay cache was not publishable")
	}
	defer replay.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	switch replay.WriteSSE(w, flusher, idx) {
	case eventreplay.SSEComplete:
		return nil
	case eventreplay.SSEPartial:
		return errors.New("regenerated replay SSE write failed after history publication")
	default:
		return errors.New("regenerated replay cache could not be published")
	}
}

func shouldReplayHistoryFromDisk(state task.State, lt *task.LoadedTask) bool {
	if lt == nil {
		return false
	}
	return state == task.StateStopped
}

// handleTaskListEvents streams patch events for the task list as SSE. On first
// iteration it sends a full snapshot; thereafter it sends only upsert/delete
// events for changed or removed tasks. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch workspace-internal state transitions.
func (h *taskHandlers) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if err := controller.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			writeError(w, api.InternalError("streaming not supported"))
			return
		}
		slog.WarnContext(r.Context(), "start task-list SSE stream", "err", err)
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	// With GitHub App configured, CI updates arrive via check_suite webhooks;
	// use a nil channel so the ticker case is never selected. With no CI service
	// wired (e.g. a router built without automation), never poll.
	var ciTickerC <-chan time.Time
	if h.ciSvc != nil && h.forgeMgr.GitHubApp() == nil {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		ciTickerC = t.C
	}

	// Seed CI status immediately on connect (once); subsequent updates come from
	// webhooks (App) or the ciTicker (polling).
	ctx := r.Context()
	if h.ciSvc != nil {
		go h.ciSvc.PollCIForActiveRepos(context.WithoutCancel(ctx))
	}

	// prevByID tracks the last marshalled JSON for each task ID.
	prevByID := map[string][]byte{}
	var prevReposJSON []byte
	var lastWarnTime time.Time
	first := true

	for {
		// Subscribe before reading the snapshot. If a task changes while the
		// snapshot is being assembled, this channel remains closed and the next
		// loop emits the newer state instead of missing the transition.
		ch := h.taskMgr.Changed()
		out := h.taskSvc.taskListSnapshot(ctx)
		repoList := repoListFromSnapshot(h.repoSvc.Snapshot(), h.repoStatus)
		var newWarnings []serverWarning
		if h.warnings != nil {
			newWarnings = h.warnings.Since(lastWarnTime)
		}

		reposJSON, err := json.Marshal(repoList)
		if err != nil {
			slog.WarnContext(ctx, "marshal repos", "err", err)
			return
		}

		if first {
			if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "snapshot", Snapshot: out}); err != nil {
				slog.WarnContext(ctx, "marshal task list snapshot", "err", err)
				return
			}
			if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
				slog.WarnContext(ctx, "marshal repos snapshot", "err", err)
				return
			}
			for i := range out {
				data, err := json.Marshal(&out[i])
				if err != nil {
					slog.WarnContext(ctx, "marshal task entry", "err", err)
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
					slog.WarnContext(ctx, "marshal task", "id", id, "err", err)
					continue
				}
				if !bytes.Equal(data, prevByID[id]) {
					prev := prevByID[id]
					prevByID[id] = data
					if prev == nil {
						// New task: emit full object.
						if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "upsert", Upsert: &out[i]}); err != nil {
							slog.WarnContext(ctx, "marshal task upsert", "id", id, "err", err)
							return
						}
					} else {
						// Existing task changed: emit only the diff.
						patch, err := computeTaskPatch(prev, data)
						if err != nil {
							slog.WarnContext(ctx, "compute task patch", "id", id, "err", err)
							continue
						}
						if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "patch", Patch: patch}); err != nil {
							slog.WarnContext(ctx, "marshal task patch", "id", id, "err", err)
							return
						}
					}
				}
			}
			// Emit deletes for removed tasks.
			for id := range prevByID {
				if _, ok := currentIDs[id]; !ok {
					if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "delete", Delete: id}); err != nil {
						slog.WarnContext(ctx, "marshal task delete", "id", id, "err", err)
						return
					}
					delete(prevByID, id)
				}
			}
			// Emit any new warnings.
			for _, warn := range newWarnings {
				if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "warning", Warning: warn.msg}); err != nil {
					slog.WarnContext(ctx, "marshal warning", "err", err)
					return
				}
				lastWarnTime = warn.ts
			}

			// Emit repos update when default-branch CI status has changed.
			if !bytes.Equal(reposJSON, prevReposJSON) {
				prevReposJSON = reposJSON
				if err := emitTaskListEvent(ctx, w, controller, &v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
					slog.WarnContext(ctx, "marshal repos update", "err", err)
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
			go h.ciSvc.PollCIForActiveRepos(context.WithoutCancel(r.Context()))
		}
	}
}

// handleTaskToolInput returns the full (untruncated) input for a tool call.
// It scans the task's message history for the ToolUseMessage with the given
// toolUseID and returns its Input field.
func (h *taskHandlers) handleTaskToolInput(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := h.taskSvc.taskToolInput(r.Context(), entry, r.PathValue("toolUseID"))
	writeJSONResponse(w, resp, err)
}

func (h *taskHandlers) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := h.taskSvc.taskDiff(r.Context(), entry, r.URL.Query().Get("path"))
	writeJSONResponse(w, resp, err)
}

// handleVNCWebSocket proxies a WebSocket connection to the instance's VNC
// TCP port via the Docker host port mapping. Used by noVNC in the frontend.
func (h *taskHandlers) handleVNCWebSocket(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
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
	slog.InfoContext(r.Context(), "vnc proxy start", "task", t.ID, "instance", snap.RuntimeInstanceID, "port", snap.VNCPort)
	vncAddr := fmt.Sprintf("127.0.0.1:%d", snap.VNCPort)

	var d net.Dialer
	d.Timeout = 10 * time.Second
	vncConn, err := d.DialContext(r.Context(), "tcp", vncAddr)
	if err != nil {
		slog.ErrorContext(r.Context(), "vnc websocket: dial failed", "addr", vncAddr, "err", err)
		writeError(w, api.InternalError("cannot reach instance VNC"))
		return
	}
	defer func() { _ = vncConn.Close() }()

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // same-origin, no Origin check needed
	})
	if err != nil {
		slog.WarnContext(r.Context(), "vnc websocket: accept failed", "task", t.ID, "err", err)
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
				slog.DebugContext(ctx, "vnc ws→tcp done", "task", t.ID, "err", err)
				return
			}
			if _, err := vncConn.Write(buf); err != nil {
				slog.DebugContext(ctx, "vnc ws→tcp write failed", "task", t.ID, "err", err)
				return
			}
		}
	}()
	n, cpErr := io.Copy(wsNetConn{wsConn, ctx}, vncConn)
	written.Store(n)
	slog.InfoContext(ctx, "vnc proxy done", "task", t.ID, "vnc→ws_bytes", n, "err", cpErr)
}

// getTask looks up a task by the {id} path parameter.
// When auth is enabled, returns 403 if the task belongs to a different user.
func (h *taskHandlers) getTask(r *http.Request) (*taskmgr.Entry, error) {
	return taskEntryFromRequest(r, h.taskMgr, h.authStore)
}

// taskEntryFromRequest looks up a task by the {id} path parameter.
// When auth is enabled, returns 403 if the task belongs to a different user.
func taskEntryFromRequest(r *http.Request, taskMgr *taskmgr.Manager, authStore *auth.Store) (*taskmgr.Entry, error) {
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

// routes returns the handler for the task collection and per-task endpoints,
// plus the global task-list SSE stream. Patterns are relative to the
// /api/caic/v1 version prefix, stripped at mount time.
func (h *taskHandlers) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /tasks", handle(h.taskSvc.listTasks))
	m.HandleFunc("POST /tasks", handle(h.taskSvc.createTask))
	m.HandleFunc("GET /tasks/events", h.handleTaskListEvents)
	m.HandleFunc("GET /tasks/{id}", handleWithTask(h, h.taskSvc.getTask))
	m.HandleFunc("GET /tasks/{id}/info", handleWithTask(h, h.taskSvc.getTaskInfo))
	m.HandleFunc("GET /tasks/{id}/raw_events", h.handleTaskEvents)
	m.HandleFunc("GET /tasks/{id}/events", h.handleTaskEvents)
	m.HandleFunc("POST /tasks/{id}/input", handleWithTask(h, h.taskSvc.sendInput))
	m.HandleFunc("POST /tasks/{id}/restart", handleWithTask(h, h.taskSvc.restartTask))
	m.HandleFunc("POST /tasks/{id}/clear-context", handleWithTask(h, h.taskSvc.clearContext))
	m.HandleFunc("POST /tasks/{id}/compact", handleWithTask(h, h.taskSvc.compactContext))
	m.HandleFunc("POST /tasks/{id}/fork", handleWithTask(h, h.taskSvc.forkTask))
	m.HandleFunc("POST /tasks/{id}/stop", handleWithTask(h, h.taskSvc.stopTask))
	m.HandleFunc("POST /tasks/{id}/purge", handleWithTask(h, h.taskSvc.purgeTask))
	m.HandleFunc("POST /tasks/{id}/revive", handleWithTask(h, h.taskSvc.reviveTask))
	m.HandleFunc("POST /tasks/{id}/sync", handleWithTask(h, h.taskSvc.syncTask))
	m.HandleFunc("GET /tasks/{id}/diff", h.handleGetDiff)
	m.HandleFunc("GET /tasks/{id}/vnc/ws", h.handleVNCWebSocket)
	m.HandleFunc("GET /tasks/{id}/tool/{toolUseID}", h.handleTaskToolInput)
	return m
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
