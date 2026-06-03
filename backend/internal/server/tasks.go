// Task lifecycle: create, list, stop, purge, revive, restart, sync, and event streaming.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/caic-xyz/md/gitutil"
	"github.com/coder/websocket"

	"github.com/caic-xyz/caic/backend/internal/agent"
	api "github.com/caic-xyz/caic/backend/internal/api"
	v1 "github.com/caic-xyz/caic/backend/internal/api/v1"
	"github.com/caic-xyz/caic/backend/internal/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// repoList builds the current repo list including live CI status. It snapshots
// the registry (repos + CI status captured atomically) and needs no external
// lock.
func (s *Server) repoList() *[]v1.Repo {
	snap := s.repoReg.snapshotWithCI()
	out := make([]v1.Repo, len(snap))
	for i := range snap {
		r := &snap[i].info
		repo := v1.Repo{Path: r.RelPath, Branch: r.BaseBranch, BaseBranch: v1.BranchInfo{Name: r.BaseBranch, Remote: r.BaseBranchRemote}, RemoteURL: gitutil.RemoteToHTTPS(r.Remote), Forge: v1.Forge(r.ForgeKind)}
		if snap[i].hasCI {
			cs := snap[i].ci
			repo.CI = v1.CIStatus(cs.Status)
			repo.CIChecks = make([]v1.ForgeCheck, len(cs.Checks))
			for j := range cs.Checks {
				repo.CIChecks[j] = v1conv.ForgeCheck(&cs.Checks[j])
			}
		}
		out[i] = repo
	}
	return &out
}

func (s *Server) listTasks(ctx context.Context, _ *api.EmptyReq) (*[]v1.Task, error) {
	var ownerID string
	if s.authEnabled() {
		if u, ok := auth.UserFromContext(ctx); ok {
			ownerID = u.ID
		}
	}
	var out []v1.Task
	s.taskMgr.Range(func(_ string, e *tasks.Entry) bool {
		if ownerID != "" && e.Task().OwnerID != "" && e.Task().OwnerID != ownerID {
			return true
		}
		out = append(out, v1conv.Task(ctx, e, s.taskResolvers()))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out, nil
}

func (s *Server) createTask(ctx context.Context, req *v1.CreateTaskReq) (*v1.CreateTaskResp, error) {
	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}

	// Docker image and max CPUs come from the user's preferences; the rest of
	// validation (unknown repo/harness/model/images) lives in Manager.Create.
	prefs := s.prefs.Get(userIDFromCtx(ctx))

	repos := make([]tasks.CreateRepo, len(req.Repos))
	for i, r := range req.Repos {
		repos[i] = tasks.CreateRepo{Name: r.Name, BaseBranch: r.BaseBranch}
	}

	id, err := s.taskMgr.Create(ctx, tasks.CreateParams{
		OwnerID:             ownerID,
		Prompt:              v1conv.PromptToAgent(req.InitialPrompt),
		Repos:               repos,
		Harness:             v1conv.AgentHarness(req.Harness),
		Model:               req.Model,
		Effort:              req.Effort,
		Tailscale:           req.Tailscale,
		USB:                 req.USB,
		Display:             req.Display,
		Sudo:                req.Sudo,
		GitHubToken:         req.GitHubToken,
		ResolvedGitHubToken: s.resolveGitHubContainerToken(ctx, req.GitHubToken),
		BaseImage:           prefs.Settings.BaseImage,
		MaxCPUs:             prefs.Settings.MaxCPUs,
		CacheMounts:         cacheMountsFromSettings(&prefs.Settings),
	})
	if err != nil {
		return nil, toDTO(err)
	}

	entry, ok := s.taskMgr.GetEntry(id)
	if !ok {
		return nil, api.InternalError("created task not found")
	}

	go s.maybeFakeCI(entry.Task())

	if len(req.Repos) > 0 {
		if err := s.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
			p.TouchRepo(req.Repos[0].Name, &preferences.RepoPrefs{
				BaseBranch: req.Repos[0].BaseBranch,
				Harness:    string(req.Harness),
				Model:      req.Model,
			})
			// When the user selects the default model (empty string),
			// TouchRepo won't clear the old value because empty means
			// "don't override". Clear it explicitly so the stale
			// non-default model doesn't persist.
			if req.Model == "" {
				p.Repositories[0].Model = ""
				delete(p.Models, string(req.Harness))
			}
		}); err != nil {
			return nil, api.InternalError("save preferences: " + err.Error())
		}
	}

	return &v1.CreateTaskResp{Status: "accepted", ID: entry.Task().ID}, nil
}

// handleTaskRawEvents delegates to handleTaskEvents — both endpoints now
// serve the same backend-neutral EventMessage stream.
func (s *Server) handleTaskRawEvents(w http.ResponseWriter, r *http.Request) {
	s.handleTaskEvents(w, r)
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
func (s *Server) streamHistoryFromDisk(w http.ResponseWriter, flusher http.Flusher, entry *tasks.Entry) {
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
func (s *Server) handleTaskToolInput(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	toolUseID := r.PathValue("toolUseID")
	if toolUseID == "" {
		writeError(w, api.BadRequest("toolUseID required"))
		return
	}
	s.taskMgr.LoadMessagesOnDemand(entry)
	history, _, unsub := entry.Task().Subscribe(r.Context())
	unsub()
	for _, msg := range history {
		if tu, ok := msg.(*agent.ToolUseMessage); ok && tu.ToolUseID == toolUseID {
			writeJSONResponse(w, &v1.TaskToolInputResp{ToolUseID: tu.ToolUseID, Input: tu.Input}, nil)
			return
		}
	}
	writeError(w, api.NotFound("tool use"))
}

// sendInput forwards user input to the agent session. On failure, it probes
// the relay daemon's liveness over SSH and returns diagnostic details in the
// 409 response so the frontend can show the user what went wrong.
//
// The relay probe uses the server context (not the request context) because the
// SSH round-trip may outlive a cancelled HTTP request, and we want the log line
// regardless.
func (s *Server) sendInput(ctx context.Context, entry *tasks.Entry, req *v1.InputReq) (*v1.StatusResp, error) {
	err := s.taskMgr.SendInput(ctx, entry, v1conv.PromptToAgent(req.Prompt))
	if err == nil {
		return &v1.StatusResp{Status: "sent"}, nil
	}
	if !errors.Is(err, tasks.ErrNoSession) {
		return nil, toDTO(err)
	}
	// No active session: probe the relay daemon's liveness over SSH and return
	// diagnostic details in the 409 response. NoSessionError.Error() preserves
	// the original task.SendInput message verbatim.
	t := entry.Task()
	rs := relayNoInstance
	instanceID := t.RuntimeInstanceID()
	if instanceID != "" {
		probeCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		alive, relayErr := agent.IsRelayRunning(probeCtx, string(instanceID)) //nolint:contextcheck // diagnostic probe; must outlive request
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
	var primaryBranchLog string
	if p := t.Primary(); p != nil {
		primaryBranchLog = p.Branch
	}
	slog.Warn("no active session",
		"task", t.ID,
		"br", primaryBranchLog,
		"instance", instanceID,
		"state", taskState,
		"relay", rs,
	)
	return nil, api.Conflict(err.Error()).
		WithDetail("state", taskState.String()).
		WithDetail("relay", string(rs))
}

func (s *Server) restartTask(ctx context.Context, entry *tasks.Entry, req *v1.RestartReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Restart(ctx, entry, v1conv.PromptToAgent(req.Prompt)); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "restarted"}, nil
}

func (s *Server) clearContext(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.ClearContext(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "cleared"}, nil
}

func (s *Server) compactContext(ctx context.Context, entry *tasks.Entry, req *v1.CompactReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Compact(ctx, entry, req.Instructions); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "compacting"}, nil
}

func (s *Server) stopTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Stop(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "stopping"}, nil
}

func (s *Server) purgeTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Purge(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "purging"}, nil
}

func (s *Server) reviveTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Revive(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "provisioning"}, nil
}

func (s *Server) forkTask(ctx context.Context, entry *tasks.Entry, req *v1.ForkTaskReq) (*v1.CreateTaskResp, error) {
	source := entry.Task()

	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}

	var harness agent.Harness
	if req.Harness != "" {
		harness = v1conv.AgentHarness(req.Harness)
	}

	extraRepos := make([]tasks.ForkRepo, len(req.ExtraRepos))
	for i, rs := range req.ExtraRepos {
		extraRepos[i] = tasks.ForkRepo{Name: rs.Name, BaseBranch: rs.BaseBranch}
	}

	// Deref the *bool overrides, defaulting to the source task's values.
	gh := source.GitHubTokenEnabled()
	if req.GitHubToken != nil {
		gh = *req.GitHubToken
	}
	tailscale := source.Tailscale
	if req.Tailscale != nil {
		tailscale = *req.Tailscale
	}
	usb := source.USB
	if req.USB != nil {
		usb = *req.USB
	}
	display := source.Display
	if req.Display != nil {
		display = *req.Display
	}
	sudo := source.Sudo
	if req.Sudo != nil {
		sudo = *req.Sudo
	}

	newID, err := s.taskMgr.Fork(ctx, entry, tasks.ForkParams{
		OwnerID:             ownerID,
		Prompt:              v1conv.PromptToAgent(req.Prompt),
		Harness:             harness,
		Model:               req.Model,
		Effort:              req.Effort,
		ExtraRepos:          extraRepos,
		GitHubToken:         gh,
		ResolvedGitHubToken: s.resolveGitHubContainerToken(ctx, gh),
		Tailscale:           tailscale,
		USB:                 usb,
		Display:             display,
		Sudo:                sudo,
	})
	if err != nil {
		return nil, toDTO(err)
	}

	forkEntry, ok := s.taskMgr.GetEntry(newID)
	if !ok {
		return nil, api.InternalError("forked task not found")
	}
	return &v1.CreateTaskResp{Status: "accepted", ID: forkEntry.Task().ID}, nil
}

func (s *Server) syncTask(ctx context.Context, entry *tasks.Entry, req *v1.SyncReq) (*v1.SyncResp, error) {
	s.initConcernAdapters()

	t := entry.Task()
	target := tasks.SyncTargetOrigin
	if req.Target == v1.SyncTargetDefault {
		target = tasks.SyncTargetDefault
	}
	res, err := s.taskMgr.Sync(ctx, entry, target, req.Force)
	if err != nil {
		return nil, toDTO(err)
	}
	resp := &v1.SyncResp{
		Status:       res.Status,
		Branch:       res.Branch,
		DiffStat:     v1conv.DiffStat(res.DiffStat),
		SafetyIssues: v1conv.SafetyIssues(res.SafetyIssues),
	}

	// Default-branch sync never starts a PR flow.
	if req.Target == v1.SyncTargetDefault {
		return resp, nil
	}

	syncPrimaryName := ""
	syncPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		syncPrimaryName = p.Name
		syncPrimaryBranch = p.Branch
	}
	if resp.Status != "blocked" {
		if info, ok := s.repoInfoFor(syncPrimaryName); ok {
			if f := s.forge.forgeForInfo(ctx, &info); f != nil {
				ciInfo := s.ciAdapter.RepoInfoFor(info.RelPath)
				prNumber, err := s.ciService.StartPRFlow(ctx, entry, f, &ciInfo, syncPrimaryBranch, s.taskMgr.EffectiveBaseBranch(t))
				if err != nil {
					slog.Warn("sync: create PR", "repo", info.ForgeRepo, "branch", syncPrimaryBranch, "err", err)
				} else {
					resp.PRNumber = prNumber
				}
			} else {
				slog.Warn("sync: no forge client available, skipping PR flow", "repo", syncPrimaryName, "forge", info.ForgeKind)
			}
		} else {
			slog.Warn("sync: repo not found in server list, skipping PR flow", "repo", syncPrimaryName)
		}
	}
	return resp, nil
}

func (s *Server) handleGetDiff(w http.ResponseWriter, r *http.Request) {
	entry, err := s.getTask(r)
	if err != nil {
		writeError(w, err)
		return
	}
	t := entry.Task()
	instanceID := t.RuntimeInstanceID()
	if instanceID == "" {
		writeError(w, api.Conflict("task has no instance"))
		return
	}
	diffPrimaryName := ""
	if p := t.Primary(); p != nil {
		diffPrimaryName = p.Name
	}
	runner, ok := s.taskMgr.Runner(diffPrimaryName)
	if !ok {
		writeError(w, api.InternalError("unknown repo"))
		return
	}
	path := r.URL.Query().Get("path")
	diff, err := runner.DiffContent(r.Context(), t, path)
	if err != nil {
		writeError(w, api.InternalError(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v1.DiffResp{Diff: diff}); err != nil {
		slog.WarnContext(r.Context(), "encode diff response", "err", err)
	}
}

// handleVNCWebSocket proxies a WebSocket connection to the instance's VNC
// TCP port via the Docker host port mapping. Used by noVNC in the frontend.
func (s *Server) handleVNCWebSocket(w http.ResponseWriter, r *http.Request) {
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

// resolveGitHubContainerToken returns the GitHub token to inject into a
// instance when enabled is true, otherwise returns empty.
func (s *Server) resolveGitHubContainerToken(ctx context.Context, enabled bool) string {
	if !enabled {
		return ""
	}
	// Resolve the parent token: prefer the OAuth user's token, fall back to
	// the server-level PAT.
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == forge.KindGitHub && u.AccessToken != "" {
		return u.AccessToken
	}
	if s.forge != nil {
		return s.forge.githubToken
	}
	return ""
}

// getTask looks up a task by the {id} path parameter.
// When auth is enabled, returns 403 if the task belongs to a different user.
func (s *Server) getTask(r *http.Request) (*tasks.Entry, error) {
	id := r.PathValue("id")
	entry, ok := s.taskMgr.GetEntry(id)
	if !ok {
		return nil, api.NotFound("task")
	}
	if s.authEnabled() {
		if u, ok := auth.UserFromContext(r.Context()); ok {
			if owner := entry.Task().OwnerID; owner != "" && owner != u.ID {
				return nil, api.Forbidden("task")
			}
		}
	}
	return entry, nil
}

func (s *Server) taskResolvers() v1conv.TaskResolvers {
	return v1conv.TaskResolvers{
		RepoURL:      s.repoURL,
		RepoForge:    s.repoForge,
		SudoPassword: s.taskMgr.SudoPassword,
		OwnerName: func(ownerID string) string {
			if s.authStore == nil || ownerID == "" {
				return ""
			}
			if u, ok := s.authStore.FindByID(ownerID); ok {
				return u.Username
			}
			return ""
		},
		ContextWindowLimit: func(repo string, harness agent.Harness, model string) int {
			r, ok := s.taskMgr.Runner(repo)
			if !ok {
				return 0
			}
			b := r.Backends[harness]
			if b == nil {
				return 0
			}
			return b.ContextWindowLimit(model)
		},
	}
}

// SetRunnerBackends updates the instance backend and agent runner backends
// for all runners.
func (s *Server) SetRunnerBackends(c runtime.Backend, backends map[agent.Harness]agent.Backend) {
	s.taskMgr.SetRunnerBackends(c, backends)
	if c != nil {
		s.runtimeBackend = c
	}
	if backends != nil {
		s.agentBackends = backends
	}
}

// SetUsageFetchers replaces the provider usage fetchers used by the usage
// endpoints. Intended for e2e tests to inject fake fetchers that return
// canned data without real API credentials.
func (s *Server) SetUsageFetchers(fetchers []usage.ProviderFetcher) {
	s.usageFetchers = fetchers
}
