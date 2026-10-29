// HTTP handlers for task-list/global-health SSE and file streams, WebSocket proxying, and route-level task lookup.

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/maruel/ksid"

	"github.com/maruel/gomode/sse"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

// taskHandlers owns task HTTP protocol concerns.
//
// It handles SSE streams, WebSocket proxying, route-level task lookup, and
// raw HTTP response writing. Task command orchestration and DTO assembly belong
// to taskService.
type taskHandlers struct {
	workers    *backgroundWorkers
	log        *slog.Logger
	taskMgr    *taskmgr.Manager
	checkouts  *repo.Registry
	repoStatus *ci.RepoStatusStore
	forgeMgr   *forgemgr.Manager
	ciSvc      *ci.Service
	taskSvc    *taskService

	warnings    *WarningStore
	systemStats *systemStats
}

func (h *taskHandlers) pollCI(ctx context.Context) {
	h.workers.Go(func() {
		pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		stop := context.AfterFunc(h.workers.ctx, cancel) //nolint:contextcheck // preserve request identity while binding polling to router shutdown
		defer stop()
		defer cancel()
		h.ciSvc.PollCIForActiveRepos(pollCtx)
	})
}

func (h *taskHandlers) notifyTaskChange() {
	h.taskMgr.NotifyTaskChange()
}

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
	var healthChanged <-chan struct{}
	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()
	emitHealth := func() error {
		health, changed := h.systemStats.health()
		healthChanged = changed
		return emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "health", Health: &health})
	}
	if h.ciSvc != nil {
		h.pollCI(ctx)
	}

	// prevByID tracks the last marshalled JSON for each task ID.
	prevByID := map[string][]byte{}
	// prevStateSeq tracks the state transition sequence already delivered for
	// each task, so a replayed transition is never resent.
	prevStateSeq := map[string]uint64{}
	var prevReposJSON []byte
	ownerID := ""
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}
	focusCh, unsubscribeFocus := h.taskSvc.subscribeTaskFocus(ctx)
	defer unsubscribeFocus()
	var lastWarnSeq uint64
	var prevSettledLoading bool
	var prevSettledError string
	first := true
	prevComplete := false

	for {
		// Subscribe before reading the snapshot. If a task changes while the
		// snapshot is being assembled, this channel remains closed and the next
		// loop emits the newer state instead of missing the transition.
		ch := h.taskMgr.Changed()
		settledLoading, settledError := h.taskMgr.RestorationStatus()
		complete := h.taskMgr.TaskListComplete()
		out, replays, membership := h.taskSvc.taskListSnapshotWithReplay(ctx, prevStateSeq)
		currentIDs := make(map[string]struct{}, len(membership))
		for _, id := range membership {
			currentIDs[id] = struct{}{}
		}
		complete = complete && len(out) == len(membership)
		wasFirst := first
		repoList := repoListFromSnapshot(h.log, h.checkouts.Checkouts(), h.repoStatus)
		newWarnings := h.warnings.Since(ownerID, lastWarnSeq)

		reposJSON, err := json.Marshal(repoList)
		if err != nil {
			h.log.WarnContext(ctx, "marshal repos", "err", err)
			return
		}

		if first {
			// Establish the runtime/history restoration state before the snapshot so the
			// client's connection indicator and empty-list handling are correct on
			// the first paint. The snapshot is a different union variant and cannot
			// carry the status payload, so it arrives as its own event.
			if err := emitRestorationStatusEvent(stream, settledLoading, settledError); err != nil {
				h.log.WarnContext(ctx, "marshal settled status", "err", err)
				return
			}
			if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "snapshot", Snapshot: out, Complete: &complete}); err != nil {
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
			if mcpHasScope(ctx, mcpScopeServerAdmin) {
				if err := emitHealth(); err != nil {
					h.log.WarnContext(ctx, "emit server health", "err", err)
					return
				}
			}
			first = false
		} else {
			// Emit a status event when runtime/history restoration transitions
			// (in-progress -> completed | failed).
			if settledLoading != prevSettledLoading || settledError != prevSettledError {
				prevSettledLoading = settledLoading
				prevSettledError = settledError
				if err := emitRestorationStatusEvent(stream, settledLoading, settledError); err != nil {
					h.log.WarnContext(ctx, "marshal settled status", "err", err)
					return
				}
			}
			// Emit upserts/patches for new or changed tasks.
			for i := range out {
				id := out[i].ID.String()
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

			// Emit repos update when default-branch CI status has changed.
			if !bytes.Equal(reposJSON, prevReposJSON) {
				prevReposJSON = reposJSON
				if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "repos", Repos: *repoList}); err != nil {
					h.log.WarnContext(ctx, "marshal repos update", "err", err)
					return
				}
			}
		}

		// Keep rendering partial data during restoration. Once absence becomes
		// authoritative, a fresh full snapshot cleans missed disconnect deletes,
		// including tasks that never produced a delta on this connection.
		if !wasFirst && complete && !prevComplete {
			if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "snapshot", Snapshot: out, Complete: &complete}); err != nil {
				h.log.WarnContext(ctx, "marshal complete task-list snapshot", "err", err)
				return
			}
		}
		prevComplete = complete

		// Replay active alerts on connect, then emit diagnostic updates by revision.
		for _, warn := range newWarnings {
			if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "warning", Warning: &warn.Warning}); err != nil {
				h.log.WarnContext(ctx, "marshal warning", "err", err)
				return
			}
			lastWarnSeq = warn.seq
		}

	waitForTaskChange:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-healthChanged:
				if err := emitHealth(); err != nil {
					h.log.WarnContext(ctx, "emit server health", "err", err)
					return
				}
			case <-heartbeatTicker.C:
				if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "heartbeat"}); err != nil {
					h.log.WarnContext(ctx, "emit task-list heartbeat", "err", err)
					return
				}
			case id := <-focusCh:
				if err := emitTaskListEvent(stream, &v1.TaskListEvent{Kind: "focus", Focus: id}); err != nil {
					h.log.WarnContext(ctx, "emit task focus", "err", err)
					return
				}
			case <-ch:
				break waitForTaskChange
			case <-ticker.C:
				break waitForTaskChange
			case <-ciTickerC:
				h.pollCI(r.Context())
			}
		}
	}
}

// handleTaskToolInput returns the full (untruncated) input for a tool call.
// Source query parameters distinguish provider-local IDs across owners and
// turns. An unscoped request must identify exactly one invocation.
func (h *taskHandlers) handleTaskToolInput(w http.ResponseWriter, r *http.Request) {
	entry, err := h.getTask(r)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	q := r.URL.Query()
	scope := v1.TaskToolInputQuery{AgentID: q.Get("agentID"), DelegationToolUseID: q.Get("delegationToolUseID"), ThreadID: q.Get("threadID"), TurnID: q.Get("turnID")}
	resp, err := h.taskSvc.taskToolInput(r.Context(), entry, r.PathValue("toolUseID"), scope)
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

	var workers sync.WaitGroup
	workers.Go(func() {
		defer cancel()
		defer func() { _ = vncConn.SetDeadline(time.Now()) }()
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
	})
	n, cpErr := io.Copy(wsNetConn{wsConn, ctx}, vncConn)
	cancel()
	_ = vncConn.SetDeadline(time.Now())
	workers.Wait()
	log.InfoContext(ctx, "VNC proxy done", "vnc_to_ws_bytes", n, "err", cpErr)
}

// getTask looks up a task by the {id} path parameter.
// It returns 403 when the caller cannot access the task.
// It implements taskEntryResolver for route wrappers that require task lookup.
func (h *taskHandlers) getTask(r *http.Request) (*taskmgr.Entry, error) {
	return taskEntryFromRequest(r, h.taskMgr)
}

// taskEntryFromRequest looks up a task by the {id} path parameter, waiting
// for runtime restoration before treating an unresolved task as missing.
// It returns 403 when the caller cannot access the task.
func taskEntryFromRequest(r *http.Request, taskMgr *taskmgr.Manager) (*taskmgr.Entry, error) {
	id, err := ksid.Parse(r.PathValue("id"))
	if err != nil {
		return nil, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task" + " not found"}
	}
	entry, ok := taskMgr.GetEntry(id)
	if !ok {
		if err := taskMgr.WaitForRuntimeImport(r.Context()); err != nil {
			return nil, err
		}
		entry, ok = taskMgr.GetEntry(id)
	}
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
	hh := &historyHandlers{taskHandlers: h}
	m.HandleFunc("GET /tasks/{id}/history/bootstrap", hh.handleHistoryBootstrap)
	m.HandleFunc("GET /tasks/{id}/history/{scope}/{version}/{generation}/chunks/{first}/{last}", hh.handleHistoryChunk)
	m.HandleFunc("GET /tasks/{id}/history/costs", hh.handleHistoryCosts)
	m.HandleFunc("GET /tasks/{id}/history/events", hh.handleHistoryEvents)
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
	m.HandleFunc("GET /tasks/{id}/attachments/{hash}", h.handleTaskAttachment)
	m.HandleFunc("GET /tasks/{id}/raw", h.handleTaskRaw)
	m.HandleFunc("GET /tasks/{id}/repo-status", h.handleTaskRepoStatus)
	m.HandleFunc("GET /tasks/{id}/vnc/ws", h.handleVNCWebSocket)
	m.HandleFunc("GET /tasks/{id}/tool/{toolUseID}", h.handleTaskToolInput)
	return m
}

// handleTaskRaw is a raw browser resource endpoint, like the VNC WebSocket;
// it is intentionally outside the generated JSON SDK. Active document formats
// download as attachments so container content cannot run on caic's origin.
func (h *taskHandlers) handleTaskRaw(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	entry, err := h.getTask(r)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	name := r.URL.Query().Get("path")
	w.Header().Set("Accept-Ranges", "bytes")
	offset, length := int64(0), int64(-1)
	size := int64(0)
	contentType := ""
	// Ignore multipart ranges and unverifiable If-Range validators. A fresh full
	// response is permitted for either and avoids mixing mutable file versions.
	rangeHeader := r.Header.Get("Range")
	if strings.HasPrefix(rangeHeader, "bytes=") && !strings.Contains(rangeHeader, ",") && r.Header.Get("If-Range") == "" {
		var err error
		size, err = h.taskSvc.taskFileSize(ctx, entry, name)
		if err != nil {
			writeError(ctx, w, err)
			return
		}
		offset, length, err = parseTaskFileRange(strings.TrimPrefix(rangeHeader, "bytes="), size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeError(ctx, w, &api.Error{Status: http.StatusRequestedRangeNotSatisfiable, Code: api.CodeBadRequest, Message: err.Error()})
			return
		}
		// Sniff the file prefix even when serving a range from its middle.
		for prefix, err := range h.taskSvc.taskFile(ctx, entry, name, 0, 512) {
			if err != nil {
				writeError(ctx, w, err)
				return
			}
			contentType = taskFileContentType(prefix)
			break
		}
	}
	remaining := length
	started := false
	for data, err := range h.taskSvc.taskFile(ctx, entry, name, offset, length) {
		if err != nil {
			if !started {
				writeError(ctx, w, err)
				return
			}
			httpLogger(ctx).WarnContext(ctx, "task file stream failed", "err", err)
			// Headers are already sent. Abort the transfer so a partial file cannot
			// look like a complete successful response or contain an appended error.
			panic(http.ErrAbortHandler)
		}
		if !started {
			if contentType == "" {
				contentType = taskFileContentType(data)
			}
			disposition := "attachment"
			switch contentType {
			case "image/avif", "image/bmp", "image/gif", "image/jpeg", "image/png", "image/webp", "text/plain; charset=utf-8":
				disposition = "inline"
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": path.Base(name)}))
			w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if length >= 0 {
				// Byte offsets describe the original file representation.
				w.Header().Set("Content-Encoding", "identity")
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size))
				w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
				w.WriteHeader(http.StatusPartialContent)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			started = true
			if r.Method == http.MethodHead {
				return
			}
		}
		n, err := w.Write(data)
		if err != nil {
			httpLogger(ctx).WarnContext(ctx, "write task file chunk", "err", err)
			panic(http.ErrAbortHandler)
		}
		if n != len(data) {
			panic(http.ErrAbortHandler)
		}
		if remaining >= 0 {
			remaining -= int64(n)
		}
	}
	if remaining > 0 {
		httpLogger(ctx).WarnContext(ctx, "task file shrank during range read", "missing_bytes", remaining)
		panic(http.ErrAbortHandler)
	}
}

// handleTaskAttachment serves one prompt image from the task log by content
// digest, optionally downscaled. It is a raw browser resource endpoint, like
// the file stream, and intentionally outside the generated JSON SDK.
func (h *taskHandlers) handleTaskAttachment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entry, err := h.getTask(r)
	if err != nil {
		writeError(ctx, w, err)
		return
	}
	// A failed lookup must not stay cached: only a served image is immutable.
	w.Header().Set("Cache-Control", "no-store")
	raw := strings.ToLower(r.PathValue("hash"))
	digestBytes, err := hex.DecodeString(raw)
	if err != nil || len(digestBytes) != sha256.Size {
		writeError(ctx, w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "image hash must be 64 hexadecimal characters"})
		return
	}
	var digest [sha256.Size]byte
	copy(digest[:], digestBytes)

	width := 0
	if value := r.URL.Query().Get("w"); value != "" && value != "0" {
		parsed, err := strconv.Atoi(value)
		if err != nil || (parsed != attachmentConversationWidth && parsed != attachmentPaneWidth) {
			writeError(ctx, w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "unsupported image width"})
			return
		}
		width = parsed
	}

	etag := `"` + raw + "-" + strconv.Itoa(width) + "-" + attachmentVariant + `"`
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("Cache-Control", attachmentCacheControl)
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	logPath := entry.LogPath.Get()
	if logPath == "" {
		writeError(ctx, w, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "image not found"})
		return
	}
	mediaType, data, err := taskslog.FindAttachment(ctx, logPath, digest)
	if err != nil {
		writeError(ctx, w, attachmentError(err))
		return
	}
	if width > 0 {
		if scaled, ok := downscaleImage(data, mediaType, width); ok {
			data = scaled
		}
	}

	w.Header().Set("Cache-Control", attachmentCacheControl)
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", "inline; filename=\"attachment-"+raw[:12]+imageExtension(mediaType)+"\"")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func taskFileContentType(data []byte) string {
	// The standard MIME sniffer omits AVIF and BMP. These signatures select
	// inert image decoders; active document types still download as attachments.
	if len(data) >= 16 && string(data[4:8]) == "ftyp" && (string(data[8:12]) == "avif" || string(data[8:12]) == "avis") {
		return "image/avif"
	}
	if len(data) >= 14 && string(data[:2]) == "BM" {
		return "image/bmp"
	}
	ct := http.DetectContentType(data)
	// Go identifies only the Ogg container. Its first packet identifies the
	// codec, so audio and video get their specific media types (RFC 5334).
	if ct == "application/ogg" && len(data) > 27 {
		off := 27 + int(data[26]) // Fixed page header plus the segment table.
		if off < len(data) {
			pkt := data[off:]
			if bytes.HasPrefix(pkt, []byte("\x80theora")) {
				return "video/ogg"
			}
			for _, sig := range []string{"\x01vorbis", "\x7fFLAC", "OpusHead", "Speex   "} {
				if bytes.HasPrefix(pkt, []byte(sig)) {
					return "audio/ogg"
				}
			}
		}
	}
	return ct
}

// parseTaskFileRange resolves one byte-range specification against current size.
func parseTaskFileRange(spec string, size int64) (offset, length int64, err error) {
	start, end, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok || size == 0 {
		return 0, 0, errors.New("unsatisfiable byte range")
	}
	if start == "" {
		count, err := strconv.ParseInt(strings.TrimSpace(end), 10, 64)
		if err != nil || count <= 0 {
			return 0, 0, errors.New("invalid suffix range")
		}
		count = min(count, size)
		return size - count, count, nil
	}
	offset, err = strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil || offset < 0 || offset >= size {
		return 0, 0, errors.New("unsatisfiable byte range")
	}
	last := size - 1
	if end != "" {
		last, err = strconv.ParseInt(strings.TrimSpace(end), 10, 64)
		if err != nil || last < offset {
			return 0, 0, errors.New("invalid byte range end")
		}
		last = min(last, size-1)
	}
	return offset, last - offset + 1, nil
}

// emitRestorationStatusEvent sends a kind=="status" event carrying runtime/history
// restoration state (loading flag and error). It is the only event that can
// carry the status variant, so the initial state and every transition go
// through it.
func emitRestorationStatusEvent(stream *sse.Stream, loading bool, errStr string) error {
	return emitTaskListEvent(stream, &v1.TaskListEvent{
		Kind:   "status",
		Status: &v1.TaskListRestorationStatus{Loading: loading, Error: errStr},
	})
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
