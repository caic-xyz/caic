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
	"strconv"
	"sync"
	"time"

	"github.com/maruel/wmao/backend/frontend"
	"github.com/maruel/wmao/backend/internal/agent"
	"github.com/maruel/wmao/backend/internal/container"
	"github.com/maruel/wmao/backend/internal/gitutil"
	"github.com/maruel/wmao/backend/internal/task"
)

// Server is the HTTP server for the wmao web UI.
type Server struct {
	runner *task.Runner
	mu     sync.Mutex
	tasks  []*taskEntry
}

type taskEntry struct {
	task   *task.Task
	result *task.Result
	done   chan struct{}
}

// taskJSON is the JSON representation sent to the frontend.
type taskJSON struct {
	ID         int     `json:"id"`
	Task       string  `json:"task"`
	Branch     string  `json:"branch"`
	Container  string  `json:"container"`
	State      string  `json:"state"`
	DiffStat   string  `json:"diffStat"`
	CostUSD    float64 `json:"costUSD"`
	DurationMs int64   `json:"durationMs"`
	NumTurns   int     `json:"numTurns"`
	Error      string  `json:"error,omitempty"`
	Result     string  `json:"result,omitempty"`
}

// New creates a new Server. It discovers preexisting containers and adopts
// them as tasks.
func New(ctx context.Context, maxTurns int, logDir string) (*Server, error) {
	branch, err := gitutil.CurrentBranch(ctx)
	if err != nil {
		return nil, err
	}
	s := &Server{
		runner: &task.Runner{BaseBranch: branch, MaxTurns: maxTurns, LogDir: logDir},
	}
	s.adoptContainers(ctx)
	return s, nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask(ctx))
	mux.HandleFunc("GET /api/tasks/{id}/events", s.handleTaskEvents)
	mux.HandleFunc("POST /api/tasks/{id}/input", s.handleTaskInput)
	mux.HandleFunc("POST /api/tasks/{id}/finish", s.handleTaskFinish)
	mux.HandleFunc("POST /api/tasks/{id}/end", s.handleTaskEnd)

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

func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := make([]taskJSON, len(s.tasks))
	for i, e := range s.tasks {
		out[i] = toJSON(i, e)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateTask(ctx context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Prompt string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Prompt == "" {
			http.Error(w, "prompt is required", http.StatusBadRequest)
			return
		}

		t := &task.Task{Prompt: req.Prompt}
		entry := &taskEntry{task: t, done: make(chan struct{})}

		s.mu.Lock()
		id := len(s.tasks)
		s.tasks = append(s.tasks, entry)
		s.mu.Unlock()

		// Run in background using the server context, not the request context.
		go func() {
			defer close(entry.done)
			if err := s.runner.Start(ctx, t); err != nil {
				result := task.Result{Task: t.Prompt, Branch: t.Branch, Container: t.Container, State: task.StateFailed, Err: err}
				s.mu.Lock()
				entry.result = &result
				s.mu.Unlock()
				return
			}
			result := s.runner.Finish(ctx, t)
			s.mu.Lock()
			entry.result = &result
			s.mu.Unlock()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "id": id})
	}
}

// handleTaskEvents streams agent messages as SSE.
func (s *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.getTask(w, r)
	if !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ch, unsub := entry.task.Subscribe(r.Context())
	defer unsub()

	idx := 0
	for msg := range ch {
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

// handleTaskInput accepts user input for a running task.
func (s *Server) handleTaskInput(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.getTask(w, r)
	if !ok {
		return
	}

	var req struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}

	if err := entry.task.SendInput(req.Prompt); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "sent"})
}

// handleTaskFinish signals a task to finish its session and proceed to
// pull/push/kill.
func (s *Server) handleTaskFinish(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.getTask(w, r)
	if !ok {
		return
	}

	state := entry.task.State
	if state != task.StateWaiting && state != task.StateRunning {
		http.Error(w, "task is not running or waiting", http.StatusConflict)
		return
	}

	entry.task.Finish()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "finishing"})
}

// handleTaskEnd force-kills a task, skipping pull/push.
func (s *Server) handleTaskEnd(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.getTask(w, r)
	if !ok {
		return
	}

	switch entry.task.State {
	case task.StateDone, task.StateFailed, task.StateEnded:
		http.Error(w, "task is already in a terminal state", http.StatusConflict)
		return
	case task.StatePending, task.StateStarting, task.StateRunning, task.StateWaiting, task.StatePulling, task.StatePushing:
	}

	entry.task.End()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ending"})
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI and can be ended.
func (s *Server) adoptContainers(ctx context.Context) {
	entries, err := container.List(ctx)
	if err != nil {
		slog.Warn("failed to list containers on startup", "err", err)
		return
	}
	repo, err := gitutil.RepoName(ctx)
	if err != nil {
		slog.Warn("failed to get repo name for container adoption", "err", err)
		return
	}
	for _, e := range entries {
		branch, ok := container.BranchFromContainer(e.Name, repo)
		if !ok {
			continue
		}
		t := &task.Task{
			Prompt:    "(adopted) " + branch,
			Branch:    branch,
			Container: e.Name,
			State:     task.StateWaiting,
		}
		t.InitDoneCh()
		entry := &taskEntry{task: t, done: make(chan struct{})}

		s.mu.Lock()
		s.tasks = append(s.tasks, entry)
		s.mu.Unlock()

		slog.Info("adopted preexisting container", "container", e.Name, "branch", branch)

		// Goroutine waits for End, then kills the container.
		go func() {
			defer close(entry.done)
			select {
			case <-t.Done():
			case <-ctx.Done():
				return
			}
			t.State = task.StateEnded
			slog.Info("ending adopted container", "container", t.Container)
			if err := s.runner.KillContainer(ctx, t.Branch); err != nil {
				slog.Warn("failed to kill adopted container", "container", t.Container, "err", err)
			}
			result := task.Result{Task: t.Prompt, Branch: t.Branch, Container: t.Container, State: task.StateEnded}
			s.mu.Lock()
			entry.result = &result
			s.mu.Unlock()
		}()
	}
}

// getTask looks up a task by the {id} path parameter.
func (s *Server) getTask(w http.ResponseWriter, r *http.Request) (*taskEntry, bool) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 0 || id >= len(s.tasks) {
		http.Error(w, "task not found", http.StatusNotFound)
		return nil, false
	}
	return s.tasks[id], true
}

func toJSON(id int, e *taskEntry) taskJSON {
	j := taskJSON{
		ID:        id,
		Task:      e.task.Prompt,
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
