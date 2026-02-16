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
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/maruel/caic/backend/frontend"
	"github.com/maruel/caic/backend/internal/agent"
	"github.com/maruel/caic/backend/internal/container"
	"github.com/maruel/caic/backend/internal/gitutil"
	"github.com/maruel/caic/backend/internal/server/dto"
	"github.com/maruel/caic/backend/internal/task"
	"github.com/maruel/ksid"
	"github.com/maruel/md"
)

type repoInfo struct {
	RelPath    string // e.g. "github/caic" — used as API ID.
	AbsPath    string
	BaseBranch string
	RepoURL    string // HTTPS browse URL derived from origin remote.
}

// CreateAuthTokenConfig is the request for POST /v1alpha/auth_tokens.
// See https://github.com/googleapis/go-genai/blob/main/types.go
type CreateAuthTokenConfig struct {
	Uses                 int    `json:"uses"`
	ExpireTime           string `json:"expireTime"`
	NewSessionExpireTime string `json:"newSessionExpireTime"`
}

// AuthToken is the response from POST /v1alpha/auth_tokens.
// See https://github.com/googleapis/go-genai/blob/main/types.go
type AuthToken struct {
	Name string `json:"name"`
}

// Server is the HTTP server for the caic web UI.
type Server struct {
	ctx      context.Context // server-lifetime context; outlives individual HTTP requests
	repos    []repoInfo
	runners  map[string]*task.Runner // keyed by RelPath
	mdClient *md.Client
	mu       sync.Mutex
	tasks    map[string]*taskEntry
	changed  chan struct{} // closed on task mutation; replaced under mu
	maxTurns int
	logDir   string
	usage    *usageFetcher // nil if no OAuth token available
}

// mdBackend adapts *md.Client to task.ContainerBackend.
type mdBackend struct{ client *md.Client }

func (b *mdBackend) Start(ctx context.Context, dir, branch string, labels []string, image string) (string, error) {
	slog.Info("md start", "dir", dir, "branch", branch)
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	c := b.client.Container(dir, branch)
	if err := c.Start(ctx, &md.StartOpts{NoSSH: true, Quiet: true, BaseImage: image, Labels: labels, USB: runtime.GOOS == "linux"}); err != nil {
		return "", err
	}
	return c.Name, nil
}

func (b *mdBackend) Diff(ctx context.Context, dir, branch string, args ...string) (string, error) {
	slog.Info("md diff", "dir", dir, "branch", branch, "args", args)
	var stdout bytes.Buffer
	if err := b.client.Container(dir, branch).Diff(ctx, &stdout, io.Discard, args); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func (b *mdBackend) Fetch(ctx context.Context, dir, branch string) error {
	slog.Info("md fetch", "dir", dir, "branch", branch)
	return b.client.Container(dir, branch).Fetch(ctx, os.Getenv("ASK_PROVIDER"), os.Getenv("ASK_MODEL"))
}

func (b *mdBackend) Kill(ctx context.Context, dir, branch string) error {
	slog.Info("md kill", "dir", dir, "branch", branch)
	return b.client.Container(dir, branch).Kill(ctx)
}

type taskEntry struct {
	task   *task.Task
	result *task.Result
	done   chan struct{}
}

// New creates a new Server. It discovers repos under rootDir, creates a Runner
// per repo, and adopts preexisting containers.
func New(ctx context.Context, rootDir string, maxTurns int, logDir string) (*Server, error) {
	if logDir == "" {
		return nil, errors.New("logDir is required")
	}
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
		ctx:      ctx,
		runners:  make(map[string]*task.Runner, len(absPaths)),
		tasks:    make(map[string]*taskEntry),
		changed:  make(chan struct{}),
		maxTurns: maxTurns,
		logDir:   logDir,
		usage:    newUsageFetcher(ctx),
	}

	mdClient, err := container.New()
	if err != nil {
		return nil, fmt.Errorf("init container library: %w", err)
	}
	s.mdClient = mdClient
	backend := &mdBackend{client: mdClient}

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
		repoURL := gitutil.RemoteToHTTPS(gitutil.RemoteOriginURL(ctx, abs))
		ri := repoInfo{RelPath: rel, AbsPath: abs, BaseBranch: branch, RepoURL: repoURL}
		s.repos = append(s.repos, ri)
		runner := &task.Runner{
			BaseBranch: branch,
			Dir:        abs,
			MaxTurns:   maxTurns,
			LogDir:     logDir,
			Container:  backend,
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

	if err := s.loadTerminatedTasks(); err != nil {
		return nil, fmt.Errorf("load terminated tasks: %w", err)
	}
	if err := s.adoptContainers(ctx); err != nil {
		return nil, fmt.Errorf("adopt containers: %w", err)
	}
	return s, nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/harnesses", handle(s.listHarnesses))
	mux.HandleFunc("GET /api/v1/repos", handle(s.listRepos))
	mux.HandleFunc("GET /api/v1/tasks", handle(s.listTasks))
	mux.HandleFunc("POST /api/v1/tasks", handle(s.createTask))
	mux.HandleFunc("GET /api/v1/tasks/{id}/raw_events", s.handleTaskRawEvents)
	mux.HandleFunc("POST /api/v1/tasks/{id}/input", handleWithTask(s, s.sendInput))
	mux.HandleFunc("POST /api/v1/tasks/{id}/restart", handleWithTask(s, s.restartTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/terminate", handleWithTask(s, s.terminateTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/sync", handleWithTask(s, s.syncTask))
	mux.HandleFunc("GET /api/v1/usage", s.handleGetUsage)
	mux.HandleFunc("GET /api/v1/voice/token", handle(s.getVoiceToken))
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)

	// Serve embedded frontend with SPA fallback and precompressed variants.
	dist, err := fs.Sub(frontend.Files, "dist")
	if err != nil {
		return err
	}
	mux.HandleFunc("GET /", newStaticHandler(dist))

	// Middleware chain: logging → decompress → compress → mux.
	// Logging sees compressed bytes (accurate wire-size reporting).
	var inner http.Handler = mux
	inner = compressMiddleware(inner)
	inner = decompressMiddleware(inner)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		inner.ServeHTTP(rw, r)
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

func (s *Server) listHarnesses(_ context.Context, _ *dto.EmptyReq) (*[]dto.HarnessJSON, error) {
	// Collect unique harness backends from all runners.
	seen := make(map[agent.Harness]agent.Backend)
	for _, r := range s.runners {
		for h, b := range r.Backends {
			seen[h] = b
		}
	}
	out := make([]dto.HarnessJSON, 0, len(seen))
	for h, b := range seen {
		out = append(out, dto.HarnessJSON{Name: string(h), Models: b.Models()})
	}
	slices.SortFunc(out, func(a, b dto.HarnessJSON) int {
		return strings.Compare(a.Name, b.Name)
	})
	return &out, nil
}

func (s *Server) listRepos(_ context.Context, _ *dto.EmptyReq) (*[]dto.RepoJSON, error) {
	out := make([]dto.RepoJSON, len(s.repos))
	for i, r := range s.repos {
		out[i] = dto.RepoJSON{Path: r.RelPath, BaseBranch: r.BaseBranch, RepoURL: r.RepoURL}
	}
	return &out, nil
}

func (s *Server) listTasks(_ context.Context, _ *dto.EmptyReq) (*[]dto.TaskJSON, error) {
	s.mu.Lock()
	out := make([]dto.TaskJSON, 0, len(s.tasks))
	for _, e := range s.tasks {
		out = append(out, s.toJSON(e))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out, nil
}

func (s *Server) createTask(_ context.Context, req *dto.CreateTaskReq) (*dto.CreateTaskResp, error) {
	runner, ok := s.runners[req.Repo]
	if !ok {
		return nil, dto.BadRequest("unknown repo: " + req.Repo)
	}

	harness := toAgentHarness(req.Harness)
	backend, ok := runner.Backends[harness]
	if !ok {
		return nil, dto.BadRequest("unknown harness: " + string(req.Harness))
	}

	if req.Model != "" && !slices.Contains(backend.Models(), req.Model) {
		return nil, dto.BadRequest("unsupported model for " + string(req.Harness) + ": " + req.Model)
	}

	t := &task.Task{ID: ksid.NewID(), Prompt: req.Prompt, Repo: req.Repo, Harness: harness, Model: req.Model, Image: req.Image}
	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	// Run in background using the server context, not the request context.
	go func() {
		defer close(entry.done)
		if err := runner.Start(s.ctx, t); err != nil {
			result := task.Result{Task: t.Prompt, Repo: t.Repo, Branch: t.Branch, Container: t.Container, State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
			return
		}
		result := runner.Kill(s.ctx, t)
		s.mu.Lock()
		entry.result = &result
		s.taskChanged()
		s.mu.Unlock()
	}()

	return &dto.CreateTaskResp{Status: "accepted", ID: t.ID}, nil
}

// handleTaskRawEvents streams agent messages as SSE using typed EventMessage DTOs.
func (s *Server) handleTaskRawEvents(w http.ResponseWriter, r *http.Request) {
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
	// Signal the client that history replay is complete so it can swap
	// the buffered messages atomically, avoiding a flash of empty content.
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	// Terminal tasks (terminated, failed) will never produce new messages.
	// Return immediately so the client receives history without blocking.
	state := entry.task.State
	if state == task.StateTerminated || state == task.StateFailed {
		return
	}

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

	taskTicker := time.NewTicker(2 * time.Second)
	defer taskTicker.Stop()

	usageTicker := time.NewTicker(30 * time.Second)
	defer usageTicker.Stop()

	var prevTasks []byte
	var prevUsage []byte

	emitUsage := func() {
		if s.usage == nil || !s.usage.hasToken() {
			return
		}
		u := s.usage.get()
		if u == nil {
			return
		}
		data, err := json.Marshal(u)
		if err != nil {
			return
		}
		if !bytes.Equal(data, prevUsage) {
			_, _ = fmt.Fprintf(w, "event: usage\ndata: %s\n\n", data)
			flusher.Flush()
			prevUsage = data
		}
	}

	// Send initial usage immediately.
	emitUsage()

	for {
		s.mu.Lock()
		out := make([]dto.TaskJSON, 0, len(s.tasks))
		for _, e := range s.tasks {
			out = append(out, s.toJSON(e))
		}
		ch := s.changed
		s.mu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

		data, err := json.Marshal(out)
		if err != nil {
			slog.Warn("marshal task list", "err", err)
			return
		}
		if !bytes.Equal(data, prevTasks) {
			_, _ = fmt.Fprintf(w, "event: tasks\ndata: %s\n\n", data)
			flusher.Flush()
			prevTasks = data
		}

		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-taskTicker.C:
		case <-usageTicker.C:
			emitUsage()
		}
	}
}

func (s *Server) sendInput(_ context.Context, entry *taskEntry, req *dto.InputReq) (*dto.StatusResp, error) {
	if err := entry.task.SendInput(req.Prompt); err != nil {
		return nil, dto.Conflict(err.Error())
	}
	return &dto.StatusResp{Status: "sent"}, nil
}

func (s *Server) restartTask(_ context.Context, entry *taskEntry, req *dto.RestartReq) (*dto.StatusResp, error) {
	t := entry.task
	if t.State != task.StateWaiting && t.State != task.StateAsking {
		return nil, dto.Conflict("task is not waiting or asking")
	}
	prompt := req.Prompt
	if prompt == "" {
		// Read the plan file from the container.
		plan, err := agent.ReadPlan(s.ctx, t.Container, t.PlanFile) //nolint:contextcheck // intentionally using server context
		if err != nil {
			return nil, dto.BadRequest("no prompt provided and failed to read plan from container: " + err.Error())
		}
		prompt = plan
	}
	runner := s.runners[t.Repo]
	// Use the server-lifetime context, not the HTTP request context.
	// The new agent session must outlive this request.
	if err := runner.RestartSession(s.ctx, t, prompt); err != nil { //nolint:contextcheck // intentionally using server context
		return nil, dto.InternalError(err.Error())
	}
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	return &dto.StatusResp{Status: "restarted"}, nil
}

func (s *Server) terminateTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*dto.StatusResp, error) {
	state := entry.task.State
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.Terminate()
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	// Don't block on entry.done: the Kill goroutine may take 10+ seconds
	// (agent graceful shutdown + container kill). The request context is
	// derived from the server's BaseContext, which is already cancelled
	// during graceful shutdown—making the wait always fail. The client
	// observes the final state transition via SSE.
	return &dto.StatusResp{Status: "terminating"}, nil
}

func (s *Server) syncTask(ctx context.Context, entry *taskEntry, req *dto.SyncReq) (*dto.SyncResp, error) {
	t := entry.task
	switch t.State {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateTerminating, task.StateFailed, task.StateTerminated:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]
	ds, issues, err := runner.SyncToOrigin(ctx, t.Branch, t.Container, req.Force)
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	status := "synced"
	if len(ds) == 0 {
		status = "empty"
	} else if len(issues) > 0 && !req.Force {
		status = "blocked"
	}
	return &dto.SyncResp{Status: status, DiffStat: toDTODiffStat(ds), SafetyIssues: toDTOSafetyIssues(issues)}, nil
}

func (s *Server) handleGetUsage(w http.ResponseWriter, _ *http.Request) {
	if s.usage == nil || !s.usage.hasToken() {
		writeError(w, dto.InternalError("usage not available: no OAuth token"))
		return
	}
	resp := s.usage.get()
	if resp == nil {
		writeError(w, dto.InternalError("usage data unavailable"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) getVoiceToken(ctx context.Context, _ *dto.EmptyReq) (*dto.VoiceTokenResp, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	now := time.Now().UTC()
	expireTime := now.Add(30 * time.Minute).Format(time.RFC3339)
	newSessionExpire := now.Add(2 * time.Minute).Format(time.RFC3339)

	reqBody := CreateAuthTokenConfig{
		Uses:                 1,
		ExpireTime:           expireTime,
		NewSessionExpireTime: newSessionExpire,
	}
	bodyBytes, err := json.Marshal(&reqBody)
	if err != nil {
		return nil, dto.InternalError("failed to marshal token request").Wrap(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1alpha/auth_tokens",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, dto.InternalError("failed to create token request").Wrap(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, dto.InternalError("failed to fetch ephemeral token").Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, dto.InternalError(fmt.Sprintf("Gemini auth_tokens returned %d: %s", resp.StatusCode, string(body)))
	}

	var tokenResp AuthToken
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, dto.InternalError("failed to decode token response").Wrap(err)
	}

	return &dto.VoiceTokenResp{
		Token:     tokenResp.Name,
		ExpiresAt: expireTime,
	}, nil
}

// SetRunnerOps overrides container and agent backends on all runners.
func (s *Server) SetRunnerOps(c task.ContainerBackend, backends map[agent.Harness]agent.Backend) {
	for _, r := range s.runners {
		if c != nil {
			r.Container = c
		}
		if backends != nil {
			r.Backends = backends
		}
	}
}

// loadTerminatedTasks loads the last 10 terminated tasks from JSONL logs so
// they appear in the UI immediately after a server restart.
func (s *Server) loadTerminatedTasks() error {
	all, err := task.LoadLogs(s.logDir)
	if err != nil {
		return err
	}
	// Filter to tasks with an explicit caic_result trailer.
	// Log files without a trailer may belong to still-running tasks.
	var terminated []*task.LoadedTask
	for _, lt := range all {
		if lt.Result != nil {
			terminated = append(terminated, lt)
		}
	}
	// LoadLogs returns ascending; reverse for most-recent-first, keep last 10.
	slices.Reverse(terminated)
	if len(terminated) > 10 {
		terminated = terminated[:10]
	}
	if len(terminated) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, lt := range terminated {
		t := &task.Task{
			ID:        ksid.NewID(),
			Prompt:    lt.Prompt,
			Repo:      lt.Repo,
			Harness:   lt.Harness,
			Branch:    lt.Branch,
			State:     lt.State,
			StartedAt: lt.StartedAt,
		}
		if lt.Msgs != nil {
			t.RestoreMessages(lt.Msgs)
		}
		// Backfill result stats from restored messages when the trailer
		// has zero cost (e.g. session exited without a final ResultMessage).
		if lt.Result.CostUSD == 0 {
			lt.Result.CostUSD, lt.Result.NumTurns, lt.Result.DurationMs, lt.Result.Usage = t.LiveStats()
		}
		done := make(chan struct{})
		close(done)
		entry := &taskEntry{task: t, result: lt.Result, done: done}
		s.tasks[t.ID.String()] = entry
	}
	s.taskChanged()
	slog.Info("loaded terminated tasks from logs", "count", len(terminated))
	return nil
}

// adoptContainers discovers preexisting md containers and creates task entries
// for them so they appear in the UI.
func (s *Server) adoptContainers(ctx context.Context) error {
	if s.mdClient == nil {
		return nil
	}
	containers, err := s.mdClient.List(ctx)
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
		for _, c := range containers {
			branch, ok := container.BranchFromContainer(c.Name, repoName)
			if !ok {
				continue
			}
			wg.Go(func() {
				if err := s.adoptOne(ctx, ri, runner, c, branch, branchID); err != nil {
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
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, c *md.Container, branch string, branchID map[string]string) error {
	// Only adopt containers that caic started. The caic label is set at
	// container creation and is the authoritative proof of ownership.
	labelVal, err := container.LabelValue(ctx, c.Name, "caic")
	if err != nil {
		return fmt.Errorf("label check for %s: %w", c.Name, err)
	}
	if labelVal == "" {
		slog.Info("skipping non-caic container", "repo", ri.RelPath, "container", c.Name, "branch", branch)
		return nil
	}
	taskID, err := ksid.Parse(labelVal)
	if err != nil {
		return fmt.Errorf("parse caic label %q on %s: %w", labelVal, c.Name, err)
	}

	// Find the most recent log file for this branch.
	allLogs, err := task.LoadLogs(s.logDir)
	if err != nil {
		return fmt.Errorf("load branch logs for %s: %w", branch, err)
	}
	var lt *task.LoadedTask
	for i := len(allLogs) - 1; i >= 0; i-- {
		if allLogs[i].Branch == branch {
			lt = allLogs[i]
			break
		}
	}

	prompt := branch
	var startedAt time.Time
	var stateUpdatedAt time.Time

	// Read the harness from the container label (authoritative), falling
	// back to the log file, then to Claude as the default.
	harnessLabel, _ := container.LabelValue(ctx, c.Name, "harness")
	harnessName := agent.Harness(harnessLabel)
	if harnessName == "" && lt != nil {
		harnessName = lt.Harness
	}
	if harnessName == "" {
		harnessName = agent.Claude
	}

	// Check whether the relay daemon is alive in this container.
	relayAlive, relayErr := agent.IsRelayRunning(ctx, c.Name)
	if relayErr != nil {
		slog.Warn("relay check failed during adopt", "repo", ri.RelPath, "branch", branch, "container", c.Name, "err", relayErr)
	}

	var relayMsgs []agent.Message
	var relaySize int64
	if relayAlive {
		// Relay is alive — read authoritative output from container.
		relayMsgs, relaySize, relayErr = runner.ReadRelayOutput(ctx, c.Name, harnessName)
		if relayErr != nil {
			slog.Warn("failed to read relay output", "repo", ri.RelPath, "branch", branch, "container", c.Name, "err", relayErr)
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
		ID:             taskID,
		Prompt:         prompt,
		Repo:           ri.RelPath,
		Harness:        harnessName,
		Branch:         branch,
		Container:      c.Name,
		State:          task.StateRunning,
		StateUpdatedAt: stateUpdatedAt,
		StartedAt:      startedAt,
	}

	if relayAlive && len(relayMsgs) > 0 {
		// Relay output is authoritative — zero loss. It contains both
		// Claude Code stdout and user inputs (logged by the relay).
		t.RestoreMessages(relayMsgs)
		t.RelayOffset = relaySize
		slog.Info("restored conversation from relay", "repo", ri.RelPath, "branch", branch, "container", c.Name, "messages", len(relayMsgs))
	} else if lt != nil && len(lt.Msgs) > 0 {
		t.RestoreMessages(lt.Msgs)
		slog.Info("restored conversation from logs", "repo", ri.RelPath, "branch", branch, "container", c.Name, "messages", len(lt.Msgs))
	}
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

	slog.Info("adopted preexisting container", "repo", ri.RelPath, "container", c.Name, "branch", branch, "relay", relayAlive)

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

func (s *Server) repoURL(rel string) string {
	for _, r := range s.repos {
		if r.RelPath == rel {
			return r.RepoURL
		}
	}
	return ""
}

func (s *Server) toJSON(e *taskEntry) dto.TaskJSON {
	j := dto.TaskJSON{
		ID:             e.task.ID,
		Task:           e.task.Prompt,
		Repo:           e.task.Repo,
		RepoURL:        s.repoURL(e.task.Repo),
		Branch:         e.task.Branch,
		Container:      e.task.Container,
		State:          e.task.State.String(),
		StateUpdatedAt: float64(e.task.StateUpdatedAt.UnixMilli()) / 1e3,
		Harness:        toDTOHarness(e.task.Harness),
		Model:          e.task.Model,
		AgentVersion:   e.task.AgentVersion,
		SessionID:      e.task.SessionID,
		InPlanMode:     e.task.InPlanMode,
	}
	if !e.task.StartedAt.IsZero() {
		j.ContainerUptimeMs = time.Since(e.task.StartedAt).Milliseconds()
	}
	// Token usage is per-query in ResultMessage but cumulative in LiveStats.
	// Always use LiveStats for token totals.
	var usage agent.Usage
	j.CostUSD, j.NumTurns, j.DurationMs, usage = e.task.LiveStats()
	j.InputTokens = usage.InputTokens
	j.OutputTokens = usage.OutputTokens
	j.CacheCreationInputTokens = usage.CacheCreationInputTokens
	j.CacheReadInputTokens = usage.CacheReadInputTokens
	if e.result != nil {
		j.DiffStat = toDTODiffStat(e.result.DiffStat)
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
