// Package server provides the HTTP server serving the API and embedded
// frontend.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

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
	tasks    []*taskEntry
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
		maxTurns: maxTurns,
		logDir:   logDir,
	}

	for _, abs := range absPaths {
		rel, err := filepath.Rel(absRoot, abs)
		if err != nil {
			rel = filepath.Base(abs)
		}
		branch, err := gitutil.CurrentBranch(ctx, abs)
		if err != nil {
			slog.Warn("skipping repo, cannot determine branch", "path", abs, "err", err)
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

	s.adoptContainers(ctx)
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
	mux.HandleFunc("POST /api/v1/tasks/{id}/finish", handleWithTask(s, s.finishTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/end", handleWithTask(s, s.endTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/pull", handleWithTask(s, s.pullTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/push", handleWithTask(s, s.pushTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/reconnect", handleWithTask(s, s.reconnectTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/takeover", handleWithTask(s, s.takeoverTask))

	// Serve embedded frontend.
	dist, err := fs.Sub(frontend.Files, "dist")
	if err != nil {
		return err
	}
	mux.Handle("GET /", http.FileServerFS(dist))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	slog.Info("listening", "addr", addr)
	return srv.ListenAndServe()
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
	out := make([]dto.TaskJSON, len(s.tasks))
	for i, e := range s.tasks {
		out[i] = toJSON(i, e)
	}
	s.mu.Unlock()
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

		t := &task.Task{Prompt: req.Prompt, Repo: req.Repo}
		entry := &taskEntry{task: t, done: make(chan struct{})}

		s.mu.Lock()
		id := len(s.tasks)
		s.tasks = append(s.tasks, entry)
		s.mu.Unlock()

		// Run in background using the server context, not the request context.
		go func() {
			defer close(entry.done)
			if err := runner.Start(ctx, t); err != nil {
				result := task.Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: task.StateFailed, Err: err}
				s.mu.Lock()
				entry.result = &result
				s.mu.Unlock()
				return
			}
			result := runner.Finish(ctx, t)
			s.mu.Lock()
			entry.result = &result
			s.mu.Unlock()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(dto.CreateTaskResp{Status: "accepted", ID: id})
	}
}

// handleTaskEvents streams agent messages as SSE.
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

	// Phase 1: replay full history.
	idx := 0
	for _, msg := range history {
		data, err := agent.MarshalMessage(msg)
		if err != nil {
			slog.Warn("marshal SSE message", "err", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
		idx++
	}
	flusher.Flush()

	// Phase 2: stream live messages.
	for msg := range live {
		data, err := agent.MarshalMessage(msg)
		if err != nil {
			slog.Warn("marshal SSE message", "err", err)
			continue
		}
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
		flusher.Flush()
		idx++
	}
}

func (s *Server) sendInput(_ context.Context, entry *taskEntry, req *dto.InputReq) (*dto.StatusResp, error) {
	if err := entry.task.SendInput(req.Prompt); err != nil {
		return nil, dto.Conflict(err.Error())
	}
	return &dto.StatusResp{Status: "sent"}, nil
}

func (s *Server) finishTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	state := entry.task.State
	if state != task.StateWaiting && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.Finish()
	return &dto.StatusResp{Status: "finishing"}, nil
}

func (s *Server) endTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	switch entry.task.State {
	case task.StateDone, task.StateFailed, task.StateEnded:
		return nil, dto.Conflict("task is already in a terminal state")
	case task.StatePending, task.StateStarting, task.StateRunning, task.StateWaiting, task.StatePulling, task.StatePushing:
	}
	entry.task.End()
	return &dto.StatusResp{Status: "ending"}, nil
}

func (s *Server) pullTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.PullResp, error) {
	t := entry.task
	switch t.State {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateDone, task.StateFailed, task.StateEnded:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateStarting, task.StateRunning, task.StateWaiting, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]
	diffStat, err := runner.PullChanges(ctx, t.Branch)
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	return &dto.PullResp{Status: "pulled", DiffStat: diffStat}, nil
}

func (s *Server) reconnectTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	t := entry.task
	if t.State != task.StateWaiting {
		return nil, dto.Conflict("task is not in waiting state")
	}
	runner := s.runners[t.Repo]
	if err := runner.Reconnect(ctx, t); err != nil {
		return nil, dto.InternalError(err.Error())
	}
	return &dto.StatusResp{Status: "reconnected"}, nil
}

func (s *Server) takeoverTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	t := entry.task
	if t.State != task.StateWaiting {
		return nil, dto.Conflict("task is not in waiting state")
	}
	runner := s.runners[t.Repo]
	if err := runner.Takeover(ctx, t); err != nil {
		return nil, dto.InternalError(err.Error())
	}
	return &dto.StatusResp{Status: "taken_over"}, nil
}

func (s *Server) pushTask(ctx context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	t := entry.task
	switch t.State {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateDone, task.StateFailed, task.StateEnded:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateStarting, task.StateRunning, task.StateWaiting, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]
	if err := runner.PushChanges(ctx, t.Branch); err != nil {
		return nil, dto.InternalError(err.Error())
	}
	return &dto.StatusResp{Status: "pushed"}, nil
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI and can be ended.
func (s *Server) adoptContainers(ctx context.Context) {
	entries, err := container.List(ctx)
	if err != nil {
		slog.Warn("failed to list containers on startup", "err", err)
		return
	}

	for _, ri := range s.repos {
		repoName := filepath.Base(ri.AbsPath)
		runner := s.runners[ri.RelPath]
		for _, e := range entries {
			branch, ok := container.BranchFromContainer(e.Name, repoName)
			if !ok {
				continue
			}
			t := &task.Task{
				Prompt:    "(adopted) " + branch,
				Repo:      ri.RelPath,
				Branch:    branch,
				Container: e.Name,
				State:     task.StateWaiting,
			}
			t.InitDoneCh()
			entry := &taskEntry{task: t, done: make(chan struct{})}

			s.mu.Lock()
			s.tasks = append(s.tasks, entry)
			s.mu.Unlock()

			slog.Info("adopted preexisting container", "repo", ri.RelPath, "container", e.Name, "branch", branch)

			// Reuse the standard Finish path: waits for Finish/End, then
			// does pull→push→kill (or just kill if End).
			go func() {
				defer close(entry.done)
				result := runner.Finish(ctx, t)
				s.mu.Lock()
				entry.result = &result
				s.mu.Unlock()
			}()
		}
	}
}

// getTask looks up a task by the {id} path parameter.
func (s *Server) getTask(r *http.Request) (*taskEntry, error) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return nil, dto.BadRequest("invalid task id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 0 || id >= len(s.tasks) {
		return nil, dto.NotFound("task")
	}
	return s.tasks[id], nil
}

func toJSON(id int, e *taskEntry) dto.TaskJSON {
	j := dto.TaskJSON{
		ID:        id,
		Task:      e.task.Prompt,
		Repo:      e.task.Repo,
		Branch:    e.task.Branch,
		Container: e.task.Container,
		State:     e.task.State.String(),
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
