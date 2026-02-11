// Package server provides the HTTP server serving the API and embedded
// frontend.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/maruel/ksid"
	"github.com/maruel/wmao/backend/frontend"
	"github.com/maruel/wmao/backend/internal/agent"
	"github.com/maruel/wmao/backend/internal/container"
	"github.com/maruel/wmao/backend/internal/gitutil"
	"github.com/maruel/wmao/backend/internal/server/dto"
	"github.com/maruel/wmao/backend/internal/task"
)

type repoInfo struct {
	RelPath    string // e.g. "github/wmao" — used as API ID.
	AbsPath    string
	BaseBranch string
}

// Server is the HTTP server for the wmao web UI.
type Server struct {
	repos    []repoInfo
	runners  map[string]*task.Runner // keyed by RelPath
	mu       sync.Mutex
	tasks    map[string]*taskEntry
	changed  chan struct{} // closed on task mutation; replaced under mu
	maxTurns int
	logDir   string
}

type taskEntry struct {
	task   *task.Task
	result *task.Result
	done   chan struct{}
}

// New creates a new Server. It discovers repos under rootDir, creates a Runner
// per repo, and adopts preexisting containers.
func New(ctx context.Context, rootDir string, maxTurns int, logDir string) (*Server, error) {
	absPaths, err := gitutil.DiscoverRepos(rootDir, 3)
	if err != nil {
		return nil, fmt.Errorf("discover repos: %w", err)
	}
	if len(absPaths) == 0 {
		return nil, fmt.Errorf("no git repos found under %s", rootDir)
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	s := &Server{
		runners:  make(map[string]*task.Runner, len(absPaths)),
		tasks:    make(map[string]*taskEntry),
		changed:  make(chan struct{}),
		maxTurns: maxTurns,
		logDir:   logDir,
	}

	for _, abs := range absPaths {
		rel, err := filepath.Rel(absRoot, abs)
		if err != nil {
			rel = filepath.Base(abs)
		}
		branch, err := gitutil.DefaultBranch(ctx, abs)
		if err != nil {
			slog.Warn("skipping repo, cannot determine default branch", "path", abs, "err", err)
			continue
		}
		ri := repoInfo{RelPath: rel, AbsPath: abs, BaseBranch: branch}
		s.repos = append(s.repos, ri)
		runner := &task.Runner{
			BaseBranch: branch,
			Dir:        abs,
			MaxTurns:   maxTurns,
			LogDir:     logDir,
		}
		if err := runner.Init(ctx); err != nil {
			slog.Warn("failed to init runner nextID", "path", abs, "err", err)
		}
		s.runners[rel] = runner
		slog.Info("discovered repo", "path", rel, "branch", branch)
	}

	if len(s.repos) == 0 {
		return nil, fmt.Errorf("no usable git repos found under %s", rootDir)
	}

	s.loadTerminatedTasks()
	if err := s.adoptContainers(ctx); err != nil {
		return nil, fmt.Errorf("adopt containers: %w", err)
	}
	return s, nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos", handle(s.listRepos))
	mux.HandleFunc("GET /api/v1/tasks", handle(s.listTasks))
	mux.HandleFunc("POST /api/v1/tasks", s.handleCreateTask(ctx))
	mux.HandleFunc("GET /api/v1/tasks/{id}/events", s.handleTaskEvents)
	mux.HandleFunc("POST /api/v1/tasks/{id}/input", handleWithTask(s, s.sendInput))
	mux.HandleFunc("POST /api/v1/tasks/{id}/terminate", handleWithTask(s, s.terminateTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/pull", handleWithTask(s, s.pullTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/push", handleWithTask(s, s.pushTask))
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)

	// Serve embedded frontend with SPA fallback: serve the file if it exists,
	// otherwise serve index.html for client-side routing.
	dist, err := fs.Sub(frontend.Files, "dist")
	if err != nil {
		return err
	}
	fileServer := http.FileServerFS(dist)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Try to stat the requested path. Serve the file if it exists.
		p := r.URL.Path
		if p != "/" {
			if _, err := fs.Stat(dist, p[1:]); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback: serve index.html.
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		mux.ServeHTTP(rw, r)
		slog.InfoContext(r.Context(), "http",
			"m", r.Method,
			"p", r.URL.Path,
			"s", rw.status,
			"d", roundDuration(time.Since(start)),
			"b", rw.size,
		)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		// Use Background because the parent ctx is already cancelled.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutdownCtx) //nolint:contextcheck // parent ctx is already cancelled at shutdown time
		shutdownCancel()
	}()
	slog.Info("listening", "addr", addr)
	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

func (s *Server) listRepos(_ context.Context, _ *dto.EmptyReq) (*[]dto.RepoJSON, error) {
	out := make([]dto.RepoJSON, len(s.repos))
	for i, r := range s.repos {
		out[i] = dto.RepoJSON{Path: r.RelPath, BaseBranch: r.BaseBranch}
	}
	return &out, nil
}

func (s *Server) listTasks(_ context.Context, _ *dto.EmptyReq) (*[]dto.TaskJSON, error) {
	s.mu.Lock()
	out := make([]dto.TaskJSON, 0, len(s.tasks))
	for _, e := range s.tasks {
		out = append(out, toJSON(e))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out, nil
}

func (s *Server) handleCreateTask(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dto.CreateTaskReq
		if !readAndDecodeBody(w, r, &req) {
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, err)
			return
		}

		runner, ok := s.runners[req.Repo]
		if !ok {
			writeError(w, dto.BadRequest("unknown repo: "+req.Repo))
			return
		}

		t := &task.Task{ID: ksid.NewID(), Prompt: req.Prompt, Repo: req.Repo}
		entry := &taskEntry{task: t, done: make(chan struct{})}

		s.mu.Lock()
		s.tasks[t.ID.String()] = entry
		s.taskChanged()
		s.mu.Unlock()

		// Run in background using the server context, not the request context.
		go func() {
			defer close(entry.done)
			if err := runner.Start(ctx, t); err != nil {
				result := task.Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: task.StateFailed, Err: err}
				s.mu.Lock()
				entry.result = &result
				s.taskChanged()
				s.mu.Unlock()
				return
			}
			result := runner.Kill(ctx, t)
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(dto.CreateTaskResp{Status: "accepted", ID: t.ID})
	}
}

// handleTaskEvents streams agent messages as SSE using typed EventMessage DTOs.
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	history, live, unsub := entry.task.Subscribe(r.Context())
	defer unsub()

	tracker := newToolTimingTracker()
	idx := 0

	writeEvents := func(events []dto.EventMessage) {
		for _, ev := range events {
			data, err := json.Marshal(ev)
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
			idx++
		}
	}

	// Phase 1: replay full history. Tool durations are 0 for replayed
	// messages because original timestamps are not stored.
	now := time.Now()
	for _, msg := range history {
		writeEvents(tracker.convertMessage(msg, now))
	}
	flusher.Flush()

	// Phase 2: stream live messages with accurate timestamps.
	for msg := range live {
		writeEvents(tracker.convertMessage(msg, time.Now()))
		flusher.Flush()
	}
}

// handleEvents streams the task list as SSE. It pushes immediately when a
// server-handled mutation fires the changed channel, and falls back to a
// 2-second ticker to catch runner-internal state transitions.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var prev []byte
	for {
		s.mu.Lock()
		out := make([]dto.TaskJSON, 0, len(s.tasks))
		for _, e := range s.tasks {
			out = append(out, toJSON(e))
		}
		ch := s.changed
		s.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

		data, err := json.Marshal(out)
		if err != nil {
			slog.Warn("marshal task list", "err", err)
			return
		}
		if !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: tasks\ndata: %s\n\n", data)
			flusher.Flush()
			prev = data
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-ticker.C:
		}
	}
}

func (s *Server) sendInput(_ context.Context, entry *taskEntry, req *dto.InputReq) (*dto.StatusResp, error) {
	if err := entry.task.SendInput(req.Prompt); err != nil {
		return nil, dto.Conflict(err.Error())
	}
	return &dto.StatusResp{Status: "sent"}, nil
}

func (s *Server) terminateTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	state := entry.task.State
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.Terminate()
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	select {
	case <-entry.done:
	case <-ctx.Done():
		return nil, dto.InternalError("timeout waiting for termination")
	}
	return &dto.StatusResp{Status: "terminated"}, nil
}

func (s *Server) pullTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.PullResp, error) {
	t := entry.task
	switch t.State {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateTerminating, task.StateFailed, task.StateTerminated:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]
	diffStat, err := runner.PullChanges(ctx, t.Branch)
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	return &dto.PullResp{Status: "pulled", DiffStat: diffStat}, nil
}

func (s *Server) pushTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	t := entry.task
	switch t.State {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateTerminating, task.StateFailed, task.StateTerminated:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]
	if err := runner.PushChanges(ctx, t.Branch); err != nil {
		return nil, dto.InternalError(err.Error())
	}
	return &dto.StatusResp{Status: "pushed"}, nil
}

// SetRunnerOps overrides container and agent operations on all runners.
func (s *Server) SetRunnerOps(c container.Ops, agentStart func(ctx context.Context, ctr string, maxTurns int, msgCh chan<- agent.Message, logW io.Writer, resumeSessionID string) (*agent.Session, error)) {
	for _, r := range s.runners {
		if c != nil {
			r.Container = c
		}
		if agentStart != nil {
			r.AgentStartFn = agentStart
		}
	}
}

// loadTerminatedTasks loads the last 10 terminated tasks from JSONL logs so
// they appear in the UI immediately after a server restart.
func (s *Server) loadTerminatedTasks() {
	if s.logDir == "" {
		return
	}
	loaded := task.LoadTerminated(s.logDir, 10)
	if len(loaded) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lt := range loaded {
		t := &task.Task{
			ID:        ksid.NewID(),
			Prompt:    lt.Prompt,
			Repo:      lt.Repo,
			Branch:    lt.Branch,
			State:     lt.State,
			StartedAt: lt.StartedAt,
		}
		if lt.Msgs != nil {
			t.RestoreMessages(lt.Msgs)
		}
		done := make(chan struct{})
		close(done)
		entry := &taskEntry{task: t, result: lt.Result, done: done}
		s.tasks[t.ID.String()] = entry
	}
	s.taskChanged()
	slog.Info("loaded terminated tasks from logs", "count", len(loaded))
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI.
func (s *Server) adoptContainers(ctx context.Context) error {
	entries, err := container.List(ctx)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	// Map branches loaded from terminated task logs to their ID in
	// s.tasks so we can replace stale entries with live containers.
	s.mu.Lock()
	branchID := make(map[string]string, len(s.tasks))
	for id, e := range s.tasks {
		if e.task.Branch != "" {
			branchID[e.task.Branch] = id
		}
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, ri := range s.repos {
		repoName := filepath.Base(ri.AbsPath)
		runner := s.runners[ri.RelPath]
		for _, e := range entries {
			branch, ok := container.BranchFromContainer(e.Name, repoName)
			if !ok {
				continue
			}
			wg.Go(func() {
				if err := s.adoptOne(ctx, ri, runner, e, branch, branchID); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
	}
	wg.Wait()
	return errors.Join(errs...)
}

// adoptOne investigates a single container and registers it as a task.
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, e container.Entry, branch string, branchID map[string]string) error {
	// Only adopt containers that wmao actually started. The
	// relay directory (deployed by DeployRelay) is the only
	// reliable proof that wmao owns this specific container
	// instance. Host-side log files alone are not sufficient
	// because they persist across container lifetimes.
	hasRelay, relayDirErr := agent.HasRelayDir(ctx, e.Name)
	if relayDirErr != nil {
		return fmt.Errorf("relay dir check for %s: %w", e.Name, relayDirErr)
	}
	if !hasRelay {
		slog.Info("skipping non-wmao container", "repo", ri.RelPath, "container", e.Name, "branch", branch)
		return nil
	}
	lt := task.LoadBranchLogs(s.logDir, branch)

	prompt := branch
	var startedAt time.Time
	var stateUpdatedAt time.Time

	// Check whether the relay daemon is alive in this container.
	relayAlive, relayErr := agent.IsRelayRunning(ctx, e.Name)
	if relayErr != nil {
		slog.Warn("relay check failed during adopt", "repo", ri.RelPath, "branch", branch, "container", e.Name, "err", relayErr)
	}

	var relayMsgs []agent.Message
	var relaySize int64
	if relayAlive {
		// Relay is alive — read authoritative output from container.
		relayMsgs, relaySize, relayErr = agent.ReadRelayOutput(ctx, e.Name)
		if relayErr != nil {
			slog.Warn("failed to read relay output", "repo", ri.RelPath, "branch", branch, "container", e.Name, "err", relayErr)
			relayAlive = false
		}
	}

	if lt != nil && lt.Prompt != "" {
		prompt = lt.Prompt
		startedAt = lt.StartedAt
		stateUpdatedAt = lt.LastStateUpdateAt
	}

	if stateUpdatedAt.IsZero() {
		stateUpdatedAt = time.Now().UTC()
	}
	t := &task.Task{
		Prompt:         prompt,
		Repo:           ri.RelPath,
		Branch:         branch,
		Container:      e.Name,
		State:          task.StateWaiting,
		StateUpdatedAt: stateUpdatedAt,
		StartedAt:      startedAt,
	}

	if relayAlive && len(relayMsgs) > 0 {
		// Relay output is authoritative — zero loss.
		t.RestoreMessages(relayMsgs)
		t.RelayOffset = relaySize
		slog.Info("restored conversation from relay", "repo", ri.RelPath, "branch", branch, "container", e.Name, "messages", len(relayMsgs))
	} else if lt != nil && len(lt.Msgs) > 0 {
		t.RestoreMessages(lt.Msgs)
		slog.Info("restored conversation from logs", "repo", ri.RelPath, "branch", branch, "container", e.Name, "messages", len(lt.Msgs))
	}
	t.ID = ksid.NewID()
	t.InitDoneCh()
	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	if oldID, ok := branchID[branch]; ok {
		// Replace the stale terminated entry with the live container.
		delete(s.tasks, oldID)
	}
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	slog.Info("adopted preexisting container", "repo", ri.RelPath, "container", e.Name, "branch", branch, "relay", relayAlive)

	// If relay is alive, auto-attach in background so messages
	// resume streaming immediately.
	if relayAlive {
		go func() {
			if err := runner.Reconnect(ctx, t); err != nil {
				slog.Warn("auto-attach to relay failed", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "err", err)
			}
		}()
	}

	// Reuse the standard Kill path: terminates the agent,
	// then kills the container.
	go func() {
		defer close(entry.done)
		result := runner.Kill(ctx, t)
		s.mu.Lock()
		entry.result = &result
		s.taskChanged()
		s.mu.Unlock()
	}()
	return nil
}

// getTask looks up a task by the {id} path parameter.
func (s *Server) getTask(r *http.Request) (*taskEntry, error) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tasks[id]
	if !ok {
		return nil, dto.NotFound("task")
	}
	return entry, nil
}

// taskChanged closes the current changed channel and replaces it. Must be
// called while holding s.mu.
func (s *Server) taskChanged() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// notifyTaskChange signals that task data may have changed.
func (s *Server) notifyTaskChange() {
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
}

func toJSON(e *taskEntry) dto.TaskJSON {
	j := dto.TaskJSON{
		ID:                e.task.ID,
		Task:              e.task.Prompt,
		Repo:              e.task.Repo,
		Branch:            e.task.Branch,
		Container:         e.task.Container,
		State:             e.task.State.String(),
		StateUpdatedAt:    float64(e.task.StateUpdatedAt.UnixMilli()) / 1e3,
		Model:             e.task.Model,
		ClaudeCodeVersion: e.task.ClaudeCodeVersion,
		SessionID:         e.task.SessionID,
	}
	if !e.task.StartedAt.IsZero() {
		j.ContainerUptimeMs = time.Since(e.task.StartedAt).Milliseconds()
	}
	if e.result != nil {
		j.DiffStat = e.result.DiffStat
		j.CostUSD = e.result.CostUSD
		j.DurationMs = e.result.DurationMs
		j.NumTurns = e.result.NumTurns
		j.Result = e.result.AgentResult
		if e.result.Err != nil {
			j.Error = e.result.Err.Error()
		}
	}
	return j
}

// responseWriter wraps http.ResponseWriter to capture status code and response size.
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Flush implements http.Flusher so SSE handlers can flush through the wrapper.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.NewResponseController
// can discover interfaces like http.Flusher.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// roundDuration rounds d to 3 significant digits with minimum 1us precision.
func roundDuration(d time.Duration) time.Duration {
	for t := 100 * time.Second; t >= 100*time.Microsecond; t /= 10 {
		if d >= t {
			return d.Round(t / 100)
		}
	}
	return d.Round(time.Microsecond)
}
