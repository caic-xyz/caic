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
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/frontend"
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/container"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md"
	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"github.com/maruel/ksid"
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

// Config bundles environment-derived values read once at startup and threaded
// into the server instead of calling os.Getenv at runtime.
type Config struct {
	// GeminiAPIKey is necessary to support Gemini Live audio.
	GeminiAPIKey string
	// LLMProvider is the genai provider for LLM features (title generation, commit descriptions).
	LLMProvider string
	// LLMModel is the model for LLM features.
	LLMModel string
	// TailscaleAPIKey is necessary to support Tailscale networking inside the container.
	TailscaleAPIKey string
}

// Server is the HTTP server for the caic web UI.
type Server struct {
	ctx          context.Context // server-lifetime context; outlives individual HTTP requests
	repos        []repoInfo
	runners      map[string]*task.Runner // keyed by RelPath
	mdClient     *md.Client
	mu           sync.Mutex
	tasks        map[string]*taskEntry
	changed      chan struct{} // closed on task mutation; replaced under mu
	maxTurns     int
	logDir       string
	usage        *usageFetcher // nil if no OAuth token available
	geminiAPIKey string
	provider     genai.Provider
}

// mdBackend adapts *md.Client to task.ContainerBackend.
type mdBackend struct {
	client      *md.Client
	llmProvider string
	llmModel    string
}

func (b *mdBackend) Start(ctx context.Context, dir, branch string, labels []string, opts task.StartOptions) (name, tailscaleFQDN string, err error) {
	slog.Info("md start", "dir", dir, "branch", branch, "tailscale", opts.Tailscale, "usb", opts.USB, "display", opts.Display)
	image := opts.DockerImage
	if image == "" {
		image = md.DefaultBaseImage + ":latest"
	}
	c := b.client.Container(dir, branch)
	sr, err := c.Start(ctx, &md.StartOpts{Quiet: true, BaseImage: image, Labels: labels, USB: opts.USB, Tailscale: opts.Tailscale, Display: opts.Display})
	if err != nil {
		return "", "", err
	}
	return c.Name, sr.TailscaleFQDN, nil
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
	return b.client.Container(dir, branch).Fetch(ctx, b.llmProvider, b.llmModel)
}

func (b *mdBackend) Kill(ctx context.Context, dir, branch string) error {
	slog.Info("md kill", "dir", dir, "branch", branch)
	return b.client.Container(dir, branch).Kill(ctx)
}

type taskEntry struct {
	task        *task.Task
	result      *task.Result
	done        chan struct{}
	cleanupOnce sync.Once // ensures exactly one cleanup runs per task
}

// New creates a new Server. It discovers repos under rootDir, creates a Runner
// per repo, and adopts preexisting containers.
//
// Startup sequence:
//  1. Initialize container client (instant).
//  2. Parallel I/O phase: discover repos, load terminated task logs, and list
//     containers concurrently.
//  3. Runner init phase: create a Runner per repo with container and agent backends
//     (runs parallel within after repos are discovered).
//  4. Adopt containers using pre-fetched list and logs. If a container's relay
//     is alive, auto-attach to resume streaming.
func New(ctx context.Context, rootDir string, maxTurns int, logDir string, cfg *Config) (*Server, error) {
	if logDir == "" {
		return nil, errors.New("logDir is required")
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}

	// container.New is instant; run it serially to simplify.
	mdClient, err := container.New(cfg.TailscaleAPIKey)
	if err != nil {
		return nil, fmt.Errorf("init container library: %w", err)
	}

	// Phase 1: Parallel I/O — repos discovery, logs loading, and container listing.
	type reposResult struct {
		paths []string
		err   error
	}
	type logsResult struct {
		logs []*task.LoadedTask
		err  error
	}
	type containersResult struct {
		containers []*md.Container
		err        error
	}

	repoCh := make(chan reposResult, 1)
	logCh := make(chan logsResult, 1)
	contCh := make(chan containersResult, 1)

	go func() {
		paths, err := gitutil.DiscoverRepos(rootDir, 3)
		repoCh <- reposResult{paths, err}
	}()
	go func() {
		logs, err := task.LoadLogs(logDir)
		logCh <- logsResult{logs, err}
	}()
	go func() {
		containers, err := mdClient.List(ctx)
		contCh <- containersResult{containers, err}
	}()

	repoRes := <-repoCh
	logRes := <-logCh
	contRes := <-contCh

	// Check for errors.
	if repoRes.err != nil {
		return nil, fmt.Errorf("discover repos: %w", repoRes.err)
	}
	if len(repoRes.paths) == 0 {
		return nil, fmt.Errorf("no git repos found under %s", rootDir)
	}

	s := &Server{
		ctx:          ctx,
		runners:      make(map[string]*task.Runner, len(repoRes.paths)),
		tasks:        make(map[string]*taskEntry),
		changed:      make(chan struct{}),
		maxTurns:     maxTurns,
		logDir:       logDir,
		usage:        newUsageFetcher(ctx),
		geminiAPIKey: cfg.GeminiAPIKey,
		mdClient:     mdClient,
	}
	if cfg.LLMProvider != "" {
		if c, ok := providers.All[cfg.LLMProvider]; !ok || c.Factory == nil {
			slog.Warn("unknown LLM provider for title generation", "provider", cfg.LLMProvider)
		} else {
			var opts []genai.ProviderOption
			if cfg.LLMModel != "" {
				opts = append(opts, genai.ProviderOptionModel(cfg.LLMModel))
			} else {
				opts = append(opts, genai.ModelCheap)
			}
			if p, err := c.Factory(ctx, opts...); err != nil {
				slog.Warn("failed to create LLM provider for title generation", "provider", cfg.LLMProvider, "err", err)
			} else {
				slog.Info("title generation enabled", "provider", p.Name(), "model", p.ModelID())
				s.provider = p
			}
		}
	}

	backend := &mdBackend{client: mdClient, llmProvider: cfg.LLMProvider, llmModel: cfg.LLMModel}

	// Phase 2: Runner init (parallel per-repo).
	type repoResult struct {
		info   repoInfo
		runner *task.Runner
	}
	results := make([]repoResult, len(repoRes.paths))
	var wg sync.WaitGroup
	for i, abs := range repoRes.paths {
		wg.Go(func() {
			rel, err := filepath.Rel(absRoot, abs)
			if err != nil {
				rel = filepath.Base(abs)
			}
			branch, err := gitutil.DefaultBranch(ctx, abs, "origin")
			if err != nil {
				slog.Warn("skipping repo, cannot determine default branch", "path", abs, "err", err)
				return
			}
			repoURL := gitutil.RemoteToHTTPS(gitutil.RemoteOriginURL(ctx, abs))
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
			results[i] = repoResult{
				info:   repoInfo{RelPath: rel, AbsPath: abs, BaseBranch: branch, RepoURL: repoURL},
				runner: runner,
			}
			slog.Info("discovered repo", "path", rel, "branch", branch)
		})
	}
	wg.Wait()
	for _, r := range results {
		if r.runner == nil {
			continue
		}
		s.repos = append(s.repos, r.info)
		s.runners[r.info.RelPath] = r.runner
	}

	if len(s.repos) == 0 {
		return nil, fmt.Errorf("no usable git repos found under %s", rootDir)
	}

	// Phase 3: Load terminated tasks from pre-loaded logs.
	if logRes.err != nil {
		slog.Warn("failed to load logs at startup", "err", logRes.err)
	} else {
		if err := s.loadTerminatedTasksFrom(logRes.logs); err != nil {
			return nil, fmt.Errorf("load terminated tasks: %w", err)
		}
	}

	// Phase 4: Adopt containers (using pre-fetched list).
	if contRes.err != nil {
		slog.Warn("cannot list containers, skipping adoption", "err", contRes.err)
	} else {
		if err := s.adoptContainers(ctx, contRes.containers, logRes.logs); err != nil {
			return nil, fmt.Errorf("adopt containers: %w", err)
		}
	}

	s.watchContainerEvents(ctx)
	return s, nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/server/config", handle(s.getConfig))
	mux.HandleFunc("GET /api/v1/server/harnesses", handle(s.listHarnesses))
	mux.HandleFunc("GET /api/v1/server/repos", handle(s.listRepos))
	mux.HandleFunc("GET /api/v1/tasks", handle(s.listTasks))
	mux.HandleFunc("POST /api/v1/tasks", handle(s.createTask))
	mux.HandleFunc("GET /api/v1/tasks/{id}/raw_events", s.handleTaskRawEvents)
	mux.HandleFunc("GET /api/v1/tasks/{id}/events", s.handleTaskEvents)
	mux.HandleFunc("POST /api/v1/tasks/{id}/input", handleWithTask(s, s.sendInput))
	mux.HandleFunc("POST /api/v1/tasks/{id}/restart", handleWithTask(s, s.restartTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/terminate", handleWithTask(s, s.terminateTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/sync", handleWithTask(s, s.syncTask))
	mux.HandleFunc("GET /api/v1/tasks/{id}/diff", s.handleGetDiff)
	mux.HandleFunc("GET /api/v1/usage", s.handleGetUsage)
	mux.HandleFunc("GET /api/v1/voice/token", handle(s.getVoiceToken))
	mux.HandleFunc("GET /api/v1/server/tasks/events", s.handleTaskListEvents)
	mux.HandleFunc("GET /api/v1/server/usage/events", s.handleUsageEvents)

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

func (s *Server) getConfig(_ context.Context, _ *dto.EmptyReq) (*v1.Config, error) {
	return &v1.Config{
		TailscaleAvailable: s.mdClient.TailscaleAPIKey != "",
		USBAvailable:       runtime.GOOS == "linux",
		DisplayAvailable:   true,
	}, nil
}

func (s *Server) listHarnesses(_ context.Context, _ *dto.EmptyReq) (*[]v1.HarnessInfo, error) {
	// Collect unique harness backends from all runners.
	seen := make(map[agent.Harness]agent.Backend)
	for _, r := range s.runners {
		for h, b := range r.Backends {
			seen[h] = b
		}
	}
	out := make([]v1.HarnessInfo, 0, len(seen))
	for h, b := range seen {
		out = append(out, v1.HarnessInfo{Name: string(h), Models: b.Models(), SupportsImages: b.SupportsImages()})
	}
	slices.SortFunc(out, func(a, b v1.HarnessInfo) int {
		return strings.Compare(a.Name, b.Name)
	})
	return &out, nil
}

func (s *Server) listRepos(_ context.Context, _ *dto.EmptyReq) (*[]v1.Repo, error) {
	out := make([]v1.Repo, len(s.repos))
	for i, r := range s.repos {
		out[i] = v1.Repo{Path: r.RelPath, BaseBranch: r.BaseBranch, RepoURL: r.RepoURL}
	}
	return &out, nil
}

func (s *Server) listTasks(_ context.Context, _ *dto.EmptyReq) (*[]v1.Task, error) {
	s.mu.Lock()
	out := make([]v1.Task, 0, len(s.tasks))
	for _, e := range s.tasks {
		out = append(out, s.toJSON(e))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out, nil
}

func (s *Server) createTask(ctx context.Context, req *v1.CreateTaskReq) (*v1.CreateTaskResp, error) {
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

	if len(req.InitialPrompt.Images) > 0 && !backend.SupportsImages() {
		return nil, dto.BadRequest(string(req.Harness) + " does not support images")
	}

	t := &task.Task{ID: ksid.NewID(), InitialPrompt: v1PromptToAgent(req.InitialPrompt), Repo: req.Repo, BaseBranch: req.BaseBranch, Harness: harness, Model: req.Model, DockerImage: req.Image, Tailscale: req.Tailscale, USB: req.USB, Display: req.Display, StartedAt: time.Now().UTC(), Provider: s.provider}
	t.SetTitle(req.InitialPrompt.Text)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	// Run in background using the server context, not the request context.
	go func() {
		h, err := runner.Start(s.ctx, t)
		if err != nil {
			result := task.Result{State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
			close(entry.done)
			return
		}
		s.watchSession(entry, runner, h)
	}()

	return &v1.CreateTaskResp{Status: "accepted", ID: t.ID}, nil
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

	writeEvents := func(events []v1.ClaudeEventMessage) {
		for i := range events {
			data, err := json.Marshal(&events[i])
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx) //nolint:gosec // SSE stream, data is json.Marshal output
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
	state := entry.task.GetState()
	if state == task.StateTerminated || state == task.StateFailed {
		return
	}

	// Phase 2: stream live messages with accurate timestamps.
	for msg := range live {
		writeEvents(tracker.convertMessage(msg, time.Now()))
		flusher.Flush()
	}
}

// handleTaskEvents streams agent messages as SSE using backend-neutral
// EventMessage DTOs. All tool invocations are emitted as toolUse events.
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

	tracker := newGenericToolTimingTracker(entry.task.Harness)
	idx := 0

	writeEvents := func(events []v1.EventMessage) {
		for i := range events {
			data, err := marshalEvent(&events[i])
			if err != nil {
				slog.Warn("marshal SSE event", "err", err)
				continue
			}
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx) //nolint:gosec // SSE stream, data is json.Marshal output
			idx++
		}
	}

	now := time.Now()
	for _, msg := range history {
		writeEvents(tracker.convertMessage(msg, now))
	}
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	state := entry.task.GetState()
	if state == task.StateTerminated || state == task.StateFailed {
		return
	}

	for msg := range live {
		writeEvents(tracker.convertMessage(msg, time.Now()))
		flusher.Flush()
	}
}

// handleTaskListEvents streams the full task list as SSE. It pushes
// immediately when a server-handled mutation fires the changed channel, and
// falls back to a 2-second ticker to catch runner-internal state transitions.
func (s *Server) handleTaskListEvents(w http.ResponseWriter, r *http.Request) {
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
		out := make([]v1.Task, 0, len(s.tasks))
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
		if !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
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

// handleUsageEvents streams usage snapshots as SSE every 30 seconds (or on
// change). Each message is a single UsageResp JSON object.
func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var prev []byte

	emit := func() {
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
		if !bytes.Equal(data, prev) {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
			prev = data
		}
	}

	// Send initial usage immediately.
	emit()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			emit()
		}
	}
}

// relayStatus describes the state of the in-container relay daemon, probed
// over SSH when SendInput fails. Combined with the task state and session
// status (from task.SendInput's error), the three values pinpoint why input
// delivery failed:
//
//   - state=waiting session=none  relay=dead → relay died, reconnect failed.
//   - state=waiting session=exited relay=alive → SSH attach exited but relay
//     is still running; reconnect should recover.
//   - state=running session=none  relay=alive → state-machine bug: state says
//     running but no Go-side session object exists.
//   - state=pending session=none  relay=no-container → task never started.
type relayStatus string

const (
	relayAlive       relayStatus = "alive"        // Relay socket exists; daemon is running.
	relayDead        relayStatus = "dead"         // No socket; daemon exited or was never started.
	relayCheckFailed relayStatus = "check-failed" // SSH probe failed (container unreachable).
	relayNoContainer relayStatus = "no-container" // Task has no container yet.
)

// sendInput forwards user input to the agent session. On failure, it probes
// the relay daemon's liveness over SSH and returns diagnostic details in the
// 409 response so the frontend can show the user what went wrong.
//
// The relay probe uses the server context (not the request context) because the
// SSH round-trip may outlive a cancelled HTTP request, and we want the log line
// regardless.
func (s *Server) sendInput(ctx context.Context, entry *taskEntry, req *v1.InputReq) (*v1.StatusResp, error) {
	if len(req.Prompt.Images) > 0 {
		runner := s.runners[entry.task.Repo]
		if b := runner.Backends[entry.task.Harness]; b != nil && !b.SupportsImages() {
			return nil, dto.BadRequest(string(entry.task.Harness) + " does not support images")
		}
	}
	if err := entry.task.SendInput(ctx, v1PromptToAgent(req.Prompt)); err != nil {
		t := entry.task
		rs := relayNoContainer
		if t.Container != "" {
			ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			alive, relayErr := agent.IsRelayRunning(ctx, t.Container) //nolint:contextcheck // diagnostic probe; must outlive request
			cancel()
			switch {
			case relayErr != nil:
				rs = relayCheckFailed
			case alive:
				rs = relayAlive
			default:
				rs = relayDead
			}
		}
		taskState := t.GetState()
		slog.Warn("sendInput: no active session",
			"task", t.ID,
			"branch", t.Branch,
			"container", t.Container,
			"state", taskState,
			"relay", rs,
		)
		return nil, dto.Conflict(err.Error()).
			WithDetail("state", taskState.String()).
			WithDetail("relay", string(rs))
	}
	return &v1.StatusResp{Status: "sent"}, nil
}

func (s *Server) restartTask(_ context.Context, entry *taskEntry, req *v1.RestartReq) (*v1.StatusResp, error) {
	t := entry.task
	if state := t.GetState(); state != task.StateWaiting && state != task.StateAsking {
		return nil, dto.Conflict("task is not waiting or asking")
	}
	prompt := v1PromptToAgent(req.Prompt)
	if prompt.Text == "" {
		// Read the plan file from the container.
		plan, err := agent.ReadPlan(s.ctx, t.Container, t.GetPlanFile()) //nolint:contextcheck // intentionally using server context
		if err != nil {
			return nil, dto.BadRequest("no prompt provided and failed to read plan from container: " + err.Error())
		}
		prompt.Text = plan
	}
	runner := s.runners[t.Repo]
	// Use the server-lifetime context, not the HTTP request context.
	// The new agent session must outlive this request.
	h, err := runner.RestartSession(s.ctx, t, prompt) //nolint:contextcheck // intentionally using server context
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	s.watchSession(entry, runner, h)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	return &v1.StatusResp{Status: "restarted"}, nil
}

func (s *Server) terminateTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.SetState(task.StateTerminating)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	runner := s.runners[entry.task.Repo]
	go s.cleanupTask(entry, runner, task.StateTerminated)
	return &v1.StatusResp{Status: "terminating"}, nil
}

func (s *Server) syncTask(ctx context.Context, entry *taskEntry, req *v1.SyncReq) (*v1.SyncResp, error) {
	t := entry.task
	switch t.GetState() {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateTerminating, task.StateFailed, task.StateTerminated:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StatePulling, task.StatePushing:
	}
	runner := s.runners[t.Repo]

	if req.Target == v1.SyncTargetDefault {
		if req.Force {
			return nil, dto.BadRequest("force is not supported for default-branch sync")
		}
		// Look up the base branch for the response.
		baseBranch := runner.BaseBranch
		// Build commit message from task title, falling back to prompt.
		message := t.Title()
		if message == "" {
			message = t.InitialPrompt.Text
		}
		ds, issues, err := runner.SyncToDefault(ctx, t.Branch, t.Container, message)
		if err != nil {
			return nil, dto.InternalError(err.Error())
		}
		status := "synced"
		if len(ds) == 0 {
			status = "empty"
		} else if len(issues) > 0 {
			status = "blocked"
		}
		return &v1.SyncResp{Status: status, Branch: baseBranch, DiffStat: toV1DiffStat(ds), SafetyIssues: toV1SafetyIssues(issues)}, nil
	}

	// Default: push to the task's own branch.
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
	return &v1.SyncResp{Status: status, Branch: t.Branch, DiffStat: toV1DiffStat(ds), SafetyIssues: toV1SafetyIssues(issues)}, nil
}

func (s *Server) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.task
	if t.Container == "" {
		writeError(w, dto.Conflict("task has no container"))
		return
	}
	runner, ok := s.runners[t.Repo]
	if !ok {
		writeError(w, dto.InternalError("unknown repo"))
		return
	}
	path := r.URL.Query().Get("path")
	diff, err := runner.DiffContent(r.Context(), t.Branch, path)
	if err != nil {
		writeError(w, dto.InternalError(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v1.DiffResp{Diff: diff})
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

// getVoiceToken returns a Gemini API credential for the Android voice client.
//
// Currently returns the raw GEMINI_API_KEY (ephemeral=false) because the
// v1alpha ephemeral endpoint produces lower-quality responses. The client uses
// the ephemeral field to decide the WebSocket URL and auth parameter.
//
// TODO(security): Switch back to ephemeral tokens once v1beta supports
// auth_tokens or v1alpha quality improves. See getVoiceTokenEphemeral.
func (s *Server) getVoiceToken(_ context.Context, _ *dto.EmptyReq) (*v1.VoiceTokenResp, error) {
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	slog.Info("voice token", "api_key_len", len(apiKey), "mode", "raw_key")
	expireTime := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)
	return &v1.VoiceTokenResp{
		Token:     apiKey,
		ExpiresAt: expireTime,
	}, nil
}

// getVoiceTokenEphemeral creates a short-lived Gemini ephemeral token via
// POST /v1alpha/auth_tokens. Ephemeral tokens are v1alpha only; v1beta
// returns 404. The client must use v1alpha + BidiGenerateContentConstrained
// with ?access_token=.
//
// This path works but produces lower-quality voice responses than the v1beta
// BidiGenerateContent endpoint with a raw key. Kept for future use once Google
// stabilises v1beta ephemeral tokens.
//
// See https://ai.google.dev/gemini-api/docs/ephemeral-tokens
func (s *Server) getVoiceTokenEphemeral(ctx context.Context, _ *dto.EmptyReq) (*v1.VoiceTokenResp, error) { //nolint:unused // kept for future use
	apiKey := s.geminiAPIKey
	if apiKey == "" {
		return nil, dto.InternalError("GEMINI_API_KEY not configured")
	}
	slog.Info("voice token", "api_key_len", len(apiKey), "mode", "ephemeral")
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

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // URL is a hardcoded constant
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

	tokenPrefix := tokenResp.Name
	if len(tokenPrefix) > 16 {
		tokenPrefix = tokenPrefix[:16]
	}
	slog.Info("voice token", "token_prefix", tokenPrefix, "token_len", len(tokenResp.Name))

	return &v1.VoiceTokenResp{
		Token:     tokenResp.Name,
		ExpiresAt: expireTime,
		Ephemeral: true,
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

// loadTerminatedTasks loads the last 10 terminated tasks from JSONL logs on disk.
// Exported for testing; New() uses the parallelized variant.
func (s *Server) loadTerminatedTasks() error {
	all, err := task.LoadLogs(s.logDir)
	if err != nil {
		return err
	}
	return s.loadTerminatedTasksFrom(all)
}

// loadTerminatedTasksFrom populates s.tasks from pre-loaded log data. It filters
// to tasks with an explicit caic_result trailer and keeps the most recent 10.
func (s *Server) loadTerminatedTasksFrom(all []*task.LoadedTask) error {
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
			ID:            ksid.NewID(),
			InitialPrompt: agent.Prompt{Text: lt.Prompt},
			Repo:          lt.Repo,
			Harness:       lt.Harness,
			Branch:        lt.Branch,
			StartedAt:     lt.StartedAt,
		}
		t.SetState(lt.State)
		if lt.Title != "" {
			t.SetTitle(lt.Title)
		} else {
			t.SetTitle(lt.Prompt)
		}
		// TODO: Figure out when it was terminated.
		if lt.Msgs != nil {
			t.RestoreMessages(lt.Msgs)
		}
		// Backfill result stats from restored messages when the trailer
		// has zero cost (e.g. session exited without a final ResultMessage).
		if lt.Result.CostUSD == 0 {
			lt.Result.CostUSD, lt.Result.NumTurns, lt.Result.Duration, lt.Result.Usage, _ = t.LiveStats()
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
//
// Flow:
//  1. Map branches from terminated tasks to their IDs so live containers
//     can replace stale entries.
//  2. For each container matching a caic repo, call adoptOne concurrently.
//
// containers and allLogs are pre-loaded to avoid redundant I/O. If containers
// is nil (due to a container client error), adoption is skipped.
func (s *Server) adoptContainers(ctx context.Context, containers []*md.Container, allLogs []*task.LoadedTask) error {
	if containers == nil {
		return nil
	}

	// Map repo+branch loaded from terminated task logs to their ID in
	// s.tasks so we can replace stale entries with live containers.
	// The key is "repo\x00branch" because different repos can share a
	// branch name.
	s.mu.Lock()
	branchID := make(map[string]string, len(s.tasks))
	for id, e := range s.tasks {
		if e.task.Branch != "" {
			branchID[e.task.Repo+"\x00"+e.task.Branch] = id
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
				if err := s.adoptOne(ctx, ri, runner, c, branch, branchID, allLogs); err != nil {
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
//
// It verifies the container has a "caic" label (proving caic started it),
// restores messages from either the relay output or JSONL logs, checks
// whether the relay is alive, and registers the task. If the relay is
// alive, it spawns a background goroutine to reattach. allLogs is the
// pre-loaded set of JSONL log files (shared across all adoptOne calls).
func (s *Server) adoptOne(ctx context.Context, ri repoInfo, runner *task.Runner, c *md.Container, branch string, branchID map[string]string, allLogs []*task.LoadedTask) error {
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

	// Find the most recent log file for this repo+branch from the pre-loaded
	// logs. Both repo and branch must match: different repos can share a
	// branch name (e.g. "caic-0"), and matching on branch alone would return
	// another repo's log, corrupting the title, prompt, and timestamps.
	var lt *task.LoadedTask
	for i := len(allLogs) - 1; i >= 0; i-- {
		if allLogs[i].Branch == branch && allLogs[i].Repo == ri.RelPath {
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
		ID:            taskID,
		InitialPrompt: agent.Prompt{Text: prompt},
		Repo:          ri.RelPath,
		Harness:       harnessName,
		Branch:        branch,
		Container:     c.Name,
		StartedAt:     startedAt,
		Tailscale:     c.Tailscale,
		TailscaleFQDN: c.TailscaleFQDN(ctx),
		USB:           c.USB,
		Display:       c.Display,
		Provider:      s.provider,
	}
	t.SetStateAt(task.StateRunning, stateUpdatedAt)
	// Set an immediate fallback title; GenerateTitle is fired async below
	// after messages are restored so the LLM sees the full conversation.
	if lt != nil && lt.Title != "" {
		t.SetTitle(lt.Title)
	} else {
		t.SetTitle(prompt)
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

	// When the relay is dead (agent subprocess already exited) and
	// RestoreMessages didn't infer a terminal turn (no trailing
	// ResultMessage), the task would be stuck as "running" with no
	// session and no way to interact. Transition to StateWaiting so
	// the user can restart with a new prompt or terminate.
	if !relayAlive {
		relayLog := agent.ReadRelayLog(ctx, c.Name, 4096)
		if relayLog != "" {
			slog.Warn("relay log from dead relay", "container", c.Name, "branch", branch, "log", relayLog)
		}
		if t.GetState() == task.StateRunning {
			t.SetState(task.StateWaiting)
			slog.Warn("adopted container with dead relay, marking as waiting",
				"repo", ri.RelPath, "branch", branch, "container", c.Name,
				"sessionID", t.GetSessionID(), "messages", len(t.Messages()))
		}
	}

	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	if oldID, ok := branchID[ri.RelPath+"\x00"+branch]; ok {
		// Replace the stale terminated entry with the live container.
		delete(s.tasks, oldID)
	}
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	slog.Info("adopted preexisting container",
		"repo", ri.RelPath, "container", c.Name, "branch", branch,
		"relay", relayAlive, "state", t.GetState(), "sessionID", t.GetSessionID())

	// Regenerate title async — relay may have new conversation data since the
	// log was written. The fallback title is already set above.
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive adoption

	// Auto-reconnect in background: relay alive → attach; relay dead
	// but SessionID present → --resume. Reconnect handles both paths
	// and reverts to StateWaiting if all strategies fail.
	if relayAlive || t.GetSessionID() != "" {
		strategy := "attach"
		if !relayAlive {
			strategy = "resume"
		}
		slog.Info("auto-reconnect starting", "repo", ri.RelPath, "branch", branch, "container", c.Name, "strategy", strategy)
		go func() {
			h, err := runner.Reconnect(ctx, t)
			if err != nil {
				slog.Warn("auto-reconnect failed, task is waiting",
					"repo", t.Repo, "branch", t.Branch, "container", t.Container,
					"strategy", strategy, "err", err)
				s.notifyTaskChange()
				return
			}
			slog.Info("auto-reconnect succeeded", "repo", t.Repo, "branch", t.Branch, "container", t.Container, "strategy", strategy)
			s.watchSession(entry, runner, h)
		}()
	} else if !relayAlive {
		slog.Warn("adopted orphaned task: relay dead and no session ID to resume",
			"repo", ri.RelPath, "branch", branch, "container", c.Name,
			"state", t.GetState())
	}
	return nil
}

// cleanupTask runs runner.Cleanup exactly once per task (guarded by
// entry.cleanupOnce), stores the result, notifies SSE, and closes entry.done.
func (s *Server) cleanupTask(entry *taskEntry, runner *task.Runner, reason task.State) {
	entry.cleanupOnce.Do(func() {
		result := runner.Cleanup(s.ctx, entry.task, reason)
		s.mu.Lock()
		entry.result = &result
		s.taskChanged()
		s.mu.Unlock()
		close(entry.done)
	})
}

// watchSession monitors a single active session. When the session's SSH
// process exits, it transitions the task to StateWaiting (the container and
// relay daemon may still be alive — see Flow 2 in the relay shutdown protocol
// in package agent). If entry.done fires first, the goroutine exits silently.
func (s *Server) watchSession(entry *taskEntry, runner *task.Runner, h *task.SessionHandle) {
	_ = runner // kept for interface consistency; may be used for future auto-reconnect
	go func() {
		done := h.Session.Done()
		select {
		case <-done:
			// Session died. Check if this handle is still the task's current
			// handle (restart may have replaced it). If stale, exit silently.
			current := entry.task.SessionDone()
			if current != done {
				return
			}
			t := entry.task
			t.DetachSession()
			result, sessionErr := h.Session.Wait()
			attrs := []any{"repo", t.Repo, "branch", t.Branch, "container", t.Container}
			if result != nil {
				attrs = append(attrs, "result", result.Subtype)
			}
			if sessionErr != nil {
				attrs = append(attrs, "err", sessionErr)
				slog.Warn("session exited with error", attrs...)
			} else {
				slog.Info("session exited", attrs...)
			}
			// Only transition Running→Waiting. If addMessage() already set
			// Asking (agent asked a question) or the task is Terminating,
			// don't clobber that state.
			t.SetStateIf(task.StateRunning, task.StateWaiting)
			s.notifyTaskChange()
		case <-entry.done:
		}
	}()
}

// watchContainerEvents starts a single goroutine that listens for Docker
// container die events and triggers cleanup for the corresponding task.
func (s *Server) watchContainerEvents(ctx context.Context) {
	go func() {
		for {
			ch, err := container.WatchEvents(ctx, "caic")
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("docker events failed, retrying in 5s", "err", err)
				select {
				case <-time.After(5 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
			for ev := range ch {
				s.handleContainerDeath(ev.Name)
			}
			// Stream ended. Reconnect unless context cancelled.
			if ctx.Err() != nil {
				return
			}
			slog.Warn("docker events stream ended, reconnecting in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// handleContainerDeath looks up a task by container name and triggers cleanup.
func (s *Server) handleContainerDeath(containerName string) {
	s.mu.Lock()
	var found *taskEntry
	var runner *task.Runner
	for _, e := range s.tasks {
		if e.task.Container == containerName {
			found = e
			runner = s.runners[e.task.Repo]
			break
		}
	}
	s.mu.Unlock()
	if found == nil || runner == nil {
		return
	}
	slog.Info("container died, cleaning up task", "container", containerName, "task", found.task.ID, "branch", found.task.Branch)
	go s.cleanupTask(found, runner, task.StateFailed)
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

// tailscaleURL returns the Tailscale URL for the task, or "true" if enabled
// but FQDN not yet known, or "" if disabled.
func tailscaleURL(t *task.Task) string {
	if t.TailscaleFQDN != "" {
		return "https://" + t.TailscaleFQDN
	}
	if t.Tailscale {
		return "true"
	}
	return ""
}

// effectiveBaseBranch returns the branch the task was forked from: the task's
// own override if set, otherwise the runner's configured default.
func (s *Server) effectiveBaseBranch(t *task.Task) string {
	if t.BaseBranch != "" {
		return t.BaseBranch
	}
	if runner, ok := s.runners[t.Repo]; ok {
		return runner.BaseBranch
	}
	return ""
}

func (s *Server) toJSON(e *taskEntry) v1.Task {
	// Read all volatile fields in a single locked snapshot to avoid
	// data races with addMessage/RestoreMessages.
	snap := e.task.Snapshot()
	j := v1.Task{
		ID:             e.task.ID,
		InitialPrompt:  e.task.InitialPrompt.Text,
		Title:          snap.Title,
		Repo:           e.task.Repo,
		RepoURL:        s.repoURL(e.task.Repo),
		BaseBranch:     s.effectiveBaseBranch(e.task),
		Branch:         e.task.Branch,
		Container:      e.task.Container,
		State:          snap.State.String(),
		StateUpdatedAt: float64(snap.StateUpdatedAt.UnixMilli()) / 1e3,
		Harness:        toV1Harness(e.task.Harness),
		Model:          snap.Model,
		AgentVersion:   snap.AgentVersion,
		SessionID:      snap.SessionID,
		InPlanMode:     snap.InPlanMode,
		PlanContent:    snap.PlanContent,
		Tailscale:      tailscaleURL(e.task),
		USB:            e.task.USB,
		Display:        e.task.Display,
		CostUSD:        snap.CostUSD,
		NumTurns:       snap.NumTurns,
		Duration:       snap.Duration.Seconds(),
	}
	if !e.task.StartedAt.IsZero() {
		j.ContainerUptimeMs = time.Since(e.task.StartedAt).Milliseconds()
	}
	j.CumulativeInputTokens = snap.Usage.InputTokens
	j.CumulativeOutputTokens = snap.Usage.OutputTokens
	j.CumulativeCacheCreationInputTokens = snap.Usage.CacheCreationInputTokens
	j.CumulativeCacheReadInputTokens = snap.Usage.CacheReadInputTokens
	// Active input tokens includes ephemeral input + cache creation (anything sent over wire).
	j.ActiveInputTokens = snap.LastUsage.InputTokens + snap.LastUsage.CacheCreationInputTokens
	j.ActiveCacheReadTokens = snap.LastUsage.CacheReadInputTokens
	if e.result != nil {
		j.DiffStat = toV1DiffStat(e.result.DiffStat)
		j.Result = e.result.AgentResult
		if e.result.Err != nil {
			j.Error = e.result.Err.Error()
		}
	} else {
		j.DiffStat = toV1DiffStat(snap.DiffStat)
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
