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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/sse"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// taskHandlers owns task HTTP protocol concerns.
//
// It handles SSE streams, WebSocket proxying, route-level task lookup, and
// raw HTTP response writing. Task command orchestration and DTO assembly belong
// to taskService.
type taskHandlers struct {
	log        *slog.Logger
	taskMgr    *taskmgr.Manager
	checkouts  *repo.Registry
	repoStatus *ci.RepoStatusStore
	forgeMgr   *forgemgr.Manager
	ciSvc      *ci.Service
	taskSvc    *taskService

	warnings *WarningStore
}

func (h *taskHandlers) notifyTaskChange() {
	h.taskMgr.NotifyTaskChange()
}

// handleTaskEvents streams agent messages as SSE using backend-neutral
// EventMessage DTOs. All tool invocations are emitted as toolUse events.
func (h *taskHandlers) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	log := h.log.With("task", entry.Task().ID)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "streaming not supported"})
		return
	}

	stream := taskEventStream{
		ctx:     r.Context(),
		w:       w,
		flusher: flusher,
		writer:  sse.New(w),
		resume:  taskEventResume{lastEventID: r.Header.Get("Last-Event-ID")},
	}
	if err := stream.beginWrite(); err != nil {
		log.WarnContext(r.Context(), "set initial task SSE write deadline", "err", err)
		return
	}
	if _, err := fmt.Fprint(w, "retry: 500\n\n"); err != nil {
		_ = stream.clearWriteDeadline()
		log.WarnContext(r.Context(), "write SSE retry interval", "err", err)
		return
	}
	if err := stream.flush(); err != nil {
		log.WarnContext(r.Context(), "start SSE stream", "err", err)
		return
	}

	// Terminal tasks have no live channel. Stream their parsed raw-log history
	// directly, keeping memory bounded even for very large task logs.
	state := entry.Task().GetState()
	loadedTask := entry.LoadedTask()
	if isTaskEventTerminal(state) || state == taskslog.StateStopped {
		var sourceErr error
		loadedTask, sourceErr = h.taskMgr.HistorySource(entry)
		if sourceErr != nil {
			log.WarnContext(r.Context(), "load SSE history source", "err", sourceErr)
			if state != taskslog.StateStopped && r.Context().Err() == nil {
				writeReplayHistoryError(w, flusher)
			}
			return
		}
	}
	if isTaskEventTerminal(state) && loadedTask != nil && entry.LogPath.Get() != "" {
		if err := stream.resume.prepare(stream.w, entry.Task().TimelineID(), taskEventSourceDisk); err != nil {
			log.WarnContext(r.Context(), "prepare terminal SSE resume", "err", err)
			return
		}
		stream.tracker = newHistoryTracker(entry.Task())
		if err := h.replayHistoryFromDisk(&stream, entry, loadedTask); err != nil {
			log.WarnContext(r.Context(), "stream terminal SSE history", "err", err)
			if r.Context().Err() == nil {
				writeReplayHistoryError(w, flusher)
			}
			return
		}
		if err := stream.writeReady(); err != nil {
			log.WarnContext(r.Context(), "write terminal SSE ready", "err", err)
			return
		}
		if err := stream.flush(); err != nil {
			log.WarnContext(r.Context(), "flush terminal SSE stream", "err", err)
		}
		return
	}

	if err := h.streamTaskEvents(&stream, entry, state, loadedTask); err != nil {
		log.WarnContext(r.Context(), "stream SSE events", "err", err)
		var historyErr *historyLoadError
		// A stopped task can be revived while its raw log is being scanned.
		// That scan is intentionally invalidated by growth; close so the client
		// reconnects against the new lifecycle rather than publishing a terminal
		// history failure. Terminal raw-log corruption remains explicit below.
		if state != taskslog.StateStopped && r.Context().Err() == nil && errors.As(err, &historyErr) {
			writeReplayHistoryError(w, flusher)
		}
	}
}

func (h *taskHandlers) streamTaskEvents(stream *taskEventStream, entry *taskmgr.Entry, state taskslog.State, loadedTask *taskslog.LoadedTask) error {
	rawHistory := shouldReplayHistoryFromDisk(state, loadedTask)
	source := taskEventSourceMemory
	if rawHistory {
		source = taskEventSourceDisk
	}
	if err := stream.resume.prepare(stream.w, entry.Task().TimelineID(), source); err != nil {
		return err
	}
	var history []task.TimelineMessage
	var statsHistory []runtime.Stats
	var live <-chan task.TimelineMessage
	var statsLive <-chan runtime.Stats
	var unsub, statsUnsub func()
	var liveAfter uint64
	if rawHistory {
		// Subscribe before scanning so a revive's new live output is buffered,
		// but do not copy stale in-memory messages or stats: raw disk is the
		// authoritative history for this stopped incarnation.
		liveAfter, live, unsub = entry.Task().SubscribeLiveMessages(stream.ctx)
		statsLive, statsUnsub = entry.Task().SubscribeLiveStats(stream.ctx)
	} else {
		history, live, unsub = entry.Task().Subscribe(stream.ctx)
		statsHistory, statsLive, statsUnsub = entry.Task().SubscribeStats(stream.ctx)
	}
	defer unsub()
	defer statsUnsub()

	stream.tracker = newHistoryTracker(entry.Task())

	now := time.Now()
	if rawHistory {
		// A stopped task no longer has reliable in-memory history. Parse and
		// stream its raw log directly rather than building a derived cache.
		if err := h.replayHistoryFromDisk(stream, entry, loadedTask); err != nil {
			return &historyLoadError{err: err}
		}
		// Revive may begin while the stopped incarnation is being scanned. Do
		// not publish ready or buffered live events from that new incarnation;
		// close so the client reconnects against its current lifecycle.
		if entry.Task().GetState() != state {
			return nil
		}
	} else {
		if err := h.replayMemoryHistory(stream, entry, history, now); err != nil {
			return err
		}
	}
	if !stream.resume.active() {
		if err := stream.writeStats(statsHistory); err != nil {
			return err
		}
	}
	if err := stream.writeReady(); err != nil {
		return err
	}
	if err := stream.flush(); err != nil {
		return fmt.Errorf("flush task SSE stream: %w", err)
	}

	if isTaskEventTerminal(state) {
		return nil
	}

	liveCh := live
	statsCh := statsLive
	for {
		select {
		case msg, ok := <-liveCh:
			if !ok {
				// A client disconnect cancels the request context and closes both
				// channels; that is a normal end. Any other close means the task
				// dropped this subscriber (slow-consumer backpressure). End the
				// response so the client reconnects with Last-Event-ID and resumes
				// from the retained timeline. Continuing here would leave an open
				// stream that only carries stats and silently never delivers
				// messages again.
				if stream.ctx.Err() != nil {
					return nil
				}
				return errTaskEventSubscriberDropped
			}
			sequence := msg.Sequence
			if rawHistory {
				sequence = stream.nextMessage + (msg.Sequence - liveAfter)
				liveAfter = msg.Sequence
			}
			at := msg.ObservedAt
			if at.IsZero() {
				at = time.Now()
			}
			if err := stream.writeMessage(msg.Message, sequence, at, false); err != nil {
				return err
			}
			if err := stream.flush(); err != nil {
				return fmt.Errorf("flush task SSE stream: %w", err)
			}
		case cs, ok := <-statsCh:
			if !ok {
				// The stats subscription is gone (request context cancelled), so
				// the response has no consumer left to serve.
				return nil
			}
			if err := stream.writeStats([]runtime.Stats{cs}); err != nil {
				return err
			}
			if err := stream.flush(); err != nil {
				return fmt.Errorf("flush task SSE stream: %w", err)
			}
		}
	}
}

func isTaskEventTerminal(state taskslog.State) bool {
	return state == taskslog.StatePurged || state == taskslog.StateCrashed || state == taskslog.StateFailed
}

// newHistoryTracker builds the per-stream API conversion tracker with the
// task's plan-content projection wired in.
func newHistoryTracker(t *task.Task) *apiconv.ToolTimingTracker {
	return apiconv.NewToolTimingTracker(t.Harness, t.PlanContentFor)
}

func (h *taskHandlers) replayMemoryHistory(stream *taskEventStream, entry *taskmgr.Entry, history []task.TimelineMessage, at time.Time) error {
	messages := make([]agent.Message, len(history))
	for i := range history {
		messages[i] = history[i].Message
	}
	skip := historyReplaySkip(messages)
	return h.replayWithCursorReset(stream, entry, func() error {
		if stream.resume.beyond(uint64(len(history))) {
			return errInvalidTaskEventID
		}
		for i, message := range history {
			messageTime := message.ObservedAt
			if messageTime.IsZero() {
				messageTime = at
			}
			if err := stream.writeMessage(message.Message, message.Sequence, messageTime, skip[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (h *taskHandlers) replayHistoryFromDisk(stream *taskEventStream, entry *taskmgr.Entry, lt *taskslog.LoadedTask) error {
	return h.replayWithCursorReset(stream, entry, func() error {
		return h.streamHistoryFromDisk(stream, entry, lt)
	})
}

func (h *taskHandlers) replayWithCursorReset(stream *taskEventStream, entry *taskmgr.Entry, replay func() error) error {
	err := replay()
	if !errors.Is(err, errInvalidTaskEventID) {
		return err
	}
	if err := stream.resume.reset(stream.w); err != nil {
		return err
	}
	stream.nextMessage = 0
	stream.tracker = newHistoryTracker(entry.Task())
	return replay()
}

// streamHistoryFromDisk parses and writes history one message at a time. The
// parser validates the scan through EOF while the stream emits incremental SSE
// frames, so task history is never retained as a derived cache or full slice.
func (h *taskHandlers) streamHistoryFromDisk(stream *taskEventStream, entry *taskmgr.Entry, lt *taskslog.LoadedTask) error {
	if lt == nil || lt.LogPath() == "" {
		return errors.New("task has no replayable log")
	}
	if stream.tracker == nil {
		stream.tracker = newHistoryTracker(entry.Task())
	}
	const historyFlushBytes = 64 << 10
	lastFlushBytes := stream.writtenBytes
	cleanTurnComplete := false
	emit := func(parsed agent.TimedMessage) error {
		message := parsed.Message
		if exit, ok := message.(*agent.ExitMessage); ok && exit.ExitCode != 0 && cleanTurnComplete {
			return nil
		}
		if task.ClearsExitError(message) {
			cleanTurnComplete = false
		}
		if result, ok := message.(*agent.ResultMessage); ok {
			cleanTurnComplete = !result.IsError
		}
		at := parsed.ProducerTime
		sequence := stream.nextMessage + 1
		if err := stream.writeMessage(message, sequence, at, false); err != nil {
			return err
		}
		if stream.writtenBytes-lastFlushBytes >= historyFlushBytes {
			if err := stream.flush(); err != nil {
				return fmt.Errorf("flush task history SSE stream: %w", err)
			}
			lastFlushBytes = stream.writtenBytes
		}
		return nil
	}
	for parsed, err := range lt.StreamMessages(stream.ctx) {
		if err != nil {
			return fmt.Errorf("stream raw task history: %w", err)
		}
		if err := emit(parsed); err != nil {
			return err
		}
	}
	return stream.resume.complete()
}

func writeReplayHistoryError(w io.Writer, flusher http.Flusher) {
	data, err := json.Marshal(v1.TaskHistoryStreamError{Message: "task history is unavailable"})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

func shouldReplayHistoryFromDisk(state taskslog.State, lt *taskslog.LoadedTask) bool {
	return state == taskslog.StateStopped && lt != nil && lt.LogPath() != ""
}

// emitSettledStatusEvent sends a kind=="status" event carrying the settled-
// history pass state (loading flag and error). It is the only event that can
// carry the status variant, so the initial state and every transition go
// through it.
func emitSettledStatusEvent(stream *sse.Stream, loading bool, errStr string) error {
	return emitTaskListEvent(stream, &v1.TaskListEvent{
		Kind:   "status",
		Status: &v1.TaskListSettledStatus{Loading: loading, Error: errStr},
	})
}

// handleTaskListEvents streams patch events for the task list as SSE. On first
// iteration it sends a full snapshot; thereafter it sends only upsert/delete
// events for changed or removed tasks. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch checkout-internal state transitions.
func (h *taskHandlers) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
	stream := sse.New(w)
	if err := stream.Flush(); err != nil {
		if errors.Is(err, http.ErrNotSupported) {
			writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "streaming not supported"})
			return
		}
		h.log.WarnContext(r.Context(), "start task-list SSE stream", "err", err)
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
	// prevStateSeq tracks the state transition sequence already delivered for
	// each task, so a replayed transition is never resent.
	prevStateSeq := map[string]uint64{}
	var prevReposJSON []byte
	var lastWarnTime time.Time
	var prevSettledLoading bool
	var prevSettledError string
	first := true

	for {
		// Subscribe before reading the snapshot. If a task changes while the
		// snapshot is being assembled, this channel remains closed and the next
		// loop emits the newer state instead of missing the transition.
		ch := h.taskMgr.Changed()
		settledLoading, settledError := h.taskMgr.SettledStatus()
		out, replays := h.taskSvc.taskListSnapshotWithReplay(ctx, prevStateSeq)
		repoList := repoListFromSnapshot(h.log, h.checkouts.Checkouts(), h.repoStatus)
		newWarnings := h.warnings.Since(lastWarnTime)

		reposJSON, err := json.Marshal(repoList)
		if err != nil {
			h.log.WarnContext(ctx, "marshal repos", "err", err)
			return
		}

		if first {
			// Establish the settled-history pass state before the snapshot so the
			// client's connection indicator and empty-list handling are correct on
			// the first paint. The snapshot is a different union variant and cannot
			// carry the status payload, so it arrives as its own event.
			if err := emitSettledStatusEvent(stream, settledLoading, settledError); err != nil {
				h.log.WarnContext(ctx, "marshal settled status", "err", err)
				return
			}
			if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "snapshot", Snapshot: out}); err != nil {
				h.log.WarnContext(ctx, "marshal task list snapshot", "err", err)
				return
			}
			if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
				h.log.WarnContext(ctx, "marshal repos snapshot", "err", err)
				return
			}
			for i := range out {
				data, err := json.Marshal(&out[i])
				if err != nil {
					h.log.WarnContext(ctx, "marshal task entry", "task", out[i].ID, "err", err)
					continue
				}
				id := out[i].ID.String()
				prevByID[id] = data
				// The snapshot carries the current state, so start the replay
				// cursor there instead of resending retained transitions.
				prevStateSeq[id] = replays[id].seq
			}
			prevReposJSON = reposJSON
			prevSettledLoading = settledLoading
			prevSettledError = settledError
			first = false
		} else {
			// Emit a status event when the settled-history pass transitions
			// (in-progress -> completed | failed).
			if settledLoading != prevSettledLoading || settledError != prevSettledError {
				prevSettledLoading = settledLoading
				prevSettledError = settledError
				if err := emitSettledStatusEvent(stream, settledLoading, settledError); err != nil {
					h.log.WarnContext(ctx, "marshal settled status", "err", err)
					return
				}
			}
			// Emit upserts/patches for new or changed tasks.
			currentIDs := make(map[string]struct{}, len(out))
			for i := range out {
				id := out[i].ID.String()
				currentIDs[id] = struct{}{}
				data, err := json.Marshal(&out[i])
				if err != nil {
					h.log.WarnContext(ctx, "marshal task", "task", id, "err", err)
					continue
				}
				if !bytes.Equal(data, prevByID[id]) {
					prev := prevByID[id]
					prevByID[id] = data
					replay := replays[id]
					if prev == nil {
						// New task: emit the full object. It carries the current
						// state, so advance the replay cursor past the retained
						// history instead of resending it.
						prevStateSeq[id] = replay.seq
						if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "upsert", Upsert: &out[i]}); err != nil {
							h.log.WarnContext(ctx, "marshal task upsert", "task", id, "err", err)
							return
						}
						continue
					}
					// Replay short-lived states the snapshot already overwrote before
					// applying the current diff, so the client observes every
					// transition instead of only its net result.
					for _, tr := range replay.history {
						state, err := apiconv.TaskState(tr.State)
						if err != nil {
							h.log.WarnContext(ctx, "convert task state", "task", id, "state", tr.State, "err", err)
							continue
						}
						patch, err := taskStatePatch(id, state, tr.At)
						if err != nil {
							h.log.WarnContext(ctx, "marshal task state patch", "task", id, "err", err)
							continue
						}
						if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "patch", Patch: patch}); err != nil {
							h.log.WarnContext(ctx, "marshal task patch", "task", id, "err", err)
							return
						}
					}
					prevStateSeq[id] = replay.seq
					// Existing task changed: emit only the diff.
					patch, err := computeTaskPatch(prev, data)
					if err != nil {
						h.log.WarnContext(ctx, "compute task patch", "task", id, "err", err)
						continue
					}
					// The replay already delivered the state and its timestamp.
					if len(replay.history) > 0 {
						delete(patch, "state")
						delete(patch, "stateUpdatedAt")
						if len(patch) == 1 { // Only "id" remains; the replay carried it all.
							continue
						}
					}
					if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "patch", Patch: patch}); err != nil {
						h.log.WarnContext(ctx, "marshal task patch", "task", id, "err", err)
						return
					}
				}
			}
			// Emit deletes for removed tasks.
			for id := range prevByID {
				if _, ok := currentIDs[id]; !ok {
					if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "delete", Delete: id}); err != nil {
						h.log.WarnContext(ctx, "marshal task delete", "task", id, "err", err)
						return
					}
					delete(prevByID, id)
					delete(prevStateSeq, id)
				}
			}
			// Emit any new warnings.
			for _, warn := range newWarnings {
				if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "warning", Warning: warn.msg}); err != nil {
					h.log.WarnContext(ctx, "marshal warning", "err", err)
					return
				}
				lastWarnTime = warn.ts
			}

			// Emit repos update when default-branch CI status has changed.
			if !bytes.Equal(reposJSON, prevReposJSON) {
				prevReposJSON = reposJSON
				if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
					h.log.WarnContext(ctx, "marshal repos update", "err", err)
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
		writeError(r.Context(), w, err)
		return
	}
	resp, err := h.taskSvc.taskToolInput(r.Context(), entry, r.PathValue("toolUseID"))
	writeJSONResponse(r.Context(), w, resp, err)
}

func (h *taskHandlers) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	resp, err := h.taskSvc.taskDiff(r.Context(), entry, r.URL.Query().Get("path"))
	writeJSONResponse(r.Context(), w, resp, err)
}

func (h *taskHandlers) handleGetDiffIndex(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	resp, err := h.taskSvc.taskDiffIndex(r.Context(), entry)
	writeJSONResponse(r.Context(), w, resp, err)
}

func (h *taskHandlers) handleGetFileDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	repository, err := strconv.Atoi(r.URL.Query().Get("repository"))
	if err != nil {
		writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "repository must be an integer"})
		return
	}
	resp, err := h.taskSvc.taskFileDiff(r.Context(), entry, v1.FileDiffReq{
		Repository:   repository,
		Commit:       r.URL.Query().Get("commit"),
		Path:         r.URL.Query().Get("path"),
		OriginalPath: r.URL.Query().Get("originalPath"),
	})
	writeJSONResponse(r.Context(), w, resp, err)
}

func (h *taskHandlers) handleTaskRepoStatus(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	resp, err := h.taskSvc.taskRepoStatus(r.Context(), entry)
	writeJSONResponse(r.Context(), w, resp, err)
}

// handleVNCWebSocket proxies a WebSocket connection to the instance's VNC
// TCP port via the Docker host port mapping. Used by noVNC in the frontend.
func (h *taskHandlers) handleVNCWebSocket(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	t := entry.Task()
	log := h.log.With("task", t.ID)
	snap := t.Snapshot()
	if snap.RuntimeInstanceID == "" || snap.VNCPort == 0 {
		writeError(r.Context(), w, &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "task has no VNC display"})
		return
	}
	log.InfoContext(r.Context(), "VNC proxy start", "instance", snap.RuntimeInstanceID, "port", snap.VNCPort)
	vncAddr := fmt.Sprintf("127.0.0.1:%d", snap.VNCPort)

	var d net.Dialer
	d.Timeout = 10 * time.Second
	vncConn, err := d.DialContext(r.Context(), "tcp", vncAddr)
	if err != nil {
		log.ErrorContext(r.Context(), "dial VNC websocket", "addr", vncAddr, "err", err)
		writeError(r.Context(), w, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: "cannot reach instance VNC"})
		return
	}
	defer func() { _ = vncConn.Close() }()

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // same-origin, no Origin check needed
	})
	if err != nil {
		log.WarnContext(r.Context(), "accept VNC websocket", "err", err)
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
				log.DebugContext(ctx, "VNC websocket to TCP done", "err", err)
				return
			}
			if _, err := vncConn.Write(buf); err != nil {
				log.DebugContext(ctx, "VNC websocket to TCP write failed", "err", err)
				return
			}
		}
	}()
	n, cpErr := io.Copy(wsNetConn{wsConn, ctx}, vncConn)
	written.Store(n)
	log.InfoContext(ctx, "VNC proxy done", "vnc_to_ws_bytes", n, "err", cpErr)
}

// getTask looks up a task by the {id} path parameter.
// It returns 403 when the caller cannot access the task.
// It implements taskEntryResolver for route wrappers that require task lookup.
func (h *taskHandlers) getTask(r *http.Request) (*taskmgr.Entry, error) {
	return taskEntryFromRequest(r, h.taskMgr)
}

// taskEntryFromRequest looks up a task by the {id} path parameter.
// It returns 403 when the caller cannot access the task.
func taskEntryFromRequest(r *http.Request, taskMgr *taskmgr.Manager) (*taskmgr.Entry, error) {
	id := r.PathValue("id")
	entry, ok := taskMgr.GetEntry(id)
	if !ok {
		return nil, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task" + " not found"}
	}
	if !taskAccessFromContext(r.Context()).canAccess(entry.Task()) {
		return nil, &api.Error{Status: http.StatusForbidden, Code: api.CodeForbidden, Message: "task" + " access denied"}
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
	m.HandleFunc("GET /tasks/{id}/handoff", handleWithTask(h, h.taskSvc.getTaskHandoff))
	m.HandleFunc("POST /tasks/{id}/fork", handleWithTask(h, h.taskSvc.forkTask))
	m.HandleFunc("POST /tasks/{id}/stop", handleWithTask(h, h.taskSvc.stopTask))
	m.HandleFunc("POST /tasks/{id}/purge", handleWithTask(h, h.taskSvc.purgeTask))
	m.HandleFunc("POST /tasks/{id}/revive", handleWithTask(h, h.taskSvc.reviveTask))
	m.HandleFunc("POST /tasks/{id}/sync", handleWithTask(h, h.taskSvc.syncTask))
	m.HandleFunc("GET /tasks/{id}/diff", h.handleGetDiff)
	m.HandleFunc("GET /tasks/{id}/diff/index", h.handleGetDiffIndex)
	m.HandleFunc("GET /tasks/{id}/diff/file", h.handleGetFileDiff)
	m.HandleFunc("GET /tasks/{id}/repo-status", h.handleTaskRepoStatus)
	m.HandleFunc("GET /tasks/{id}/vnc/ws", h.handleVNCWebSocket)
	m.HandleFunc("GET /tasks/{id}/tool/{toolUseID}", h.handleTaskToolInput)
	return m
}

// historyLoadError marks a failure before any task history is available for SSE
// publication. It is the sole condition that can produce the history-unavailable
// frame; later stats, ready, flush, and live-stream errors remain their own errors.
type historyLoadError struct{ err error }

func (e *historyLoadError) Error() string { return e.err.Error() }

func (e *historyLoadError) Unwrap() error { return e.err }

// taskEventStream writes one task's ordered SSE event stream.
type taskEventStream struct {
	ctx          context.Context
	w            http.ResponseWriter
	flusher      http.Flusher
	writer       *sse.Stream
	tracker      *apiconv.ToolTimingTracker
	resume       taskEventResume
	nextMessage  uint64
	writtenBytes int
	idBuffer     []byte
}

// beginWrite bounds writes to a task SSE client. A client can disappear while
// its TCP connection remains open; without a deadline the stream goroutine
// would block forever and never let EventSource reconnect from its cursor.
func (s *taskEventStream) beginWrite() error {
	return s.writer.BeginWrite()
}

func (s *taskEventStream) clearWriteDeadline() error {
	return s.writer.ClearWriteDeadline()
}

func (s *taskEventStream) flush() error {
	return s.writer.Flush()
}

func (s *taskEventStream) writeMessage(msg agent.Message, sequence uint64, at time.Time, suppress bool) error {
	s.nextMessage = sequence
	events := s.tracker.ConvertMessage(msg, at)
	if err := s.resume.observe(sequence, events); err != nil {
		return err
	}
	if suppress || s.resume.precedes(sequence) {
		return nil
	}
	for i := range events {
		id := s.resume.eventID(sequence, uint64(i))
		if s.resume.acknowledged(id) {
			continue
		}
		if err := s.writeEvent(&events[i], id); err != nil {
			return err
		}
	}
	return nil
}

func (s *taskEventStream) writeStats(stats []runtime.Stats) error {
	for i := range stats {
		ev := apiconv.StatsEvent(&stats[i])
		if err := s.writeEvent(&ev, taskEventID{}); err != nil {
			return err
		}
	}
	return nil
}

func (s *taskEventStream) writeEvent(ev *v1.EventMessage, id taskEventID) error {
	data, err := apiconv.MarshalEvent(ev)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	if err := s.beginWrite(); err != nil {
		return err
	}
	var n int
	if id.message == 0 {
		n, err = fmt.Fprintf(s.w, "event: message\ndata: %s\n\n", data)
	} else {
		s.idBuffer = id.appendTo(s.idBuffer[:0])
		n, err = fmt.Fprintf(s.w, "id: %s\nevent: message\ndata: %s\n\n", s.idBuffer, data)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("write SSE event: %w", err), s.clearWriteDeadline())
	}
	s.writtenBytes += n
	return nil
}

func (s *taskEventStream) writeReady() error {
	if err := s.beginWrite(); err != nil {
		return err
	}
	if _, err := fmt.Fprint(s.w, "event: ready\ndata: {}\n\n"); err != nil {
		return errors.Join(fmt.Errorf("write SSE ready event: %w", err), s.clearWriteDeadline())
	}
	return nil
}

type taskEventSource string

const (
	taskEventSourceDisk   taskEventSource = "disk"
	taskEventSourceMemory taskEventSource = "memory"
)

type taskEventID struct {
	timeline string
	source   taskEventSource
	message  uint64
	event    uint64
}

func parseTaskEventID(value string) (taskEventID, bool) {
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[0] != "v1" || parts[1] == "" {
		return taskEventID{}, false
	}
	source := taskEventSource(parts[2])
	if source != taskEventSourceDisk && source != taskEventSourceMemory {
		return taskEventID{}, false
	}
	messageID, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil || messageID == 0 {
		return taskEventID{}, false
	}
	eventID, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil {
		return taskEventID{}, false
	}
	return taskEventID{timeline: parts[1], source: source, message: messageID, event: eventID}, true
}

func (id taskEventID) String() string {
	return string(id.appendTo(nil))
}

func (id taskEventID) appendTo(dst []byte) []byte {
	dst = append(dst, "v1/"...)
	dst = append(dst, id.timeline...)
	dst = append(dst, '/')
	dst = append(dst, id.source...)
	dst = append(dst, '/')
	dst = strconv.AppendUint(dst, id.message, 10)
	dst = append(dst, '/')
	return strconv.AppendUint(dst, id.event, 10)
}

var errInvalidTaskEventID = errors.New("invalid task SSE event ID")

// errTaskEventSubscriberDropped reports that the task dropped this stream's
// live subscriber because it could not keep up with the message rate. The
// handler ends the response so the client reconnects with Last-Event-ID and
// resumes from the retained timeline.
var errTaskEventSubscriberDropped = errors.New("task event subscriber dropped")

type taskEventResume struct {
	lastEventID string
	timelineID  string
	source      taskEventSource
	after       taskEventID
	cursorSeen  bool
}

func (r *taskEventResume) prepare(w io.Writer, timelineID string, source taskEventSource) error {
	r.timelineID = timelineID
	r.source = source
	if r.lastEventID == "" {
		return nil
	}
	id, ok := parseTaskEventID(r.lastEventID)
	if !ok || id.timeline != timelineID || id.source != source {
		return r.reset(w)
	}
	r.after = id
	return nil
}

func (r *taskEventResume) reset(w io.Writer) error {
	if _, err := fmt.Fprint(w, "id:\nevent: reset\ndata: {}\n\n"); err != nil {
		return fmt.Errorf("write SSE reset event: %w", err)
	}
	r.lastEventID = ""
	r.after = taskEventID{}
	r.cursorSeen = false
	return nil
}

func (r *taskEventResume) observe(sequence uint64, events []v1.EventMessage) error {
	if sequence != r.after.message {
		return nil
	}
	if r.after.event >= uint64(len(events)) {
		return errInvalidTaskEventID
	}
	r.cursorSeen = true
	return nil
}

func (r *taskEventResume) eventID(message, event uint64) taskEventID {
	return taskEventID{timeline: r.timelineID, source: r.source, message: message, event: event}
}

func (r *taskEventResume) acknowledged(id taskEventID) bool {
	return id.message == r.after.message && id.event <= r.after.event
}

func (r *taskEventResume) precedes(sequence uint64) bool {
	return sequence < r.after.message
}

func (r *taskEventResume) beyond(lastSequence uint64) bool {
	return r.after.message > lastSequence
}

func (r *taskEventResume) active() bool {
	return r.after.message != 0
}

func (r *taskEventResume) complete() error {
	if r.active() && !r.cursorSeen {
		return errInvalidTaskEventID
	}
	return nil
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
