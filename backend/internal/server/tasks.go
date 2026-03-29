// Task lifecycle: create, list, stop, purge, revive, restart, sync, and event streaming.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/server/dto"
	v1 "github.com/caic-xyz/caic/backend/internal/server/dto/v1"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/md/gitutil"
	"github.com/maruel/ksid"
)

// reposLocked builds the current repo list including live CI status.
// Must be called with s.mu held.
func (s *Server) reposLocked() *[]v1.Repo {
	out := make([]v1.Repo, len(s.repos))
	for i, r := range s.repos {
		repo := v1.Repo{Path: r.RelPath, BaseBranch: r.BaseBranch, RemoteURL: gitutil.RemoteToHTTPS(r.Remote), Forge: v1.Forge(r.ForgeKind)}
		if ci, ok := s.repoCIStatus[r.RelPath]; ok {
			repo.DefaultBranchCIStatus = v1.CIStatus(ci.Status)
			repo.DefaultBranchChecks = ci.Checks
		}
		out[i] = repo
	}
	return &out
}

func (s *Server) listTasks(ctx context.Context, _ *dto.EmptyReq) (*[]v1.Task, error) {
	var ownerID string
	if s.authEnabled() {
		if u, ok := auth.UserFromContext(ctx); ok {
			ownerID = u.ID
		}
	}
	s.mu.Lock()
	out := make([]v1.Task, 0, len(s.tasks))
	for _, e := range s.tasks {
		if ownerID != "" && e.task.OwnerID != "" && e.task.OwnerID != ownerID {
			continue
		}
		out = append(out, s.toJSON(e))
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return &out, nil
}

func (s *Server) createTask(ctx context.Context, req *v1.CreateTaskReq) (*v1.CreateTaskResp, error) {
	// Resolve primary runner (first repo, or no-repo).
	var primaryRunner *task.Runner
	if len(req.Repos) > 0 {
		r, ok := s.runners[req.Repos[0].Name]
		if !ok {
			return nil, dto.BadRequest("unknown repo: " + req.Repos[0].Name)
		}
		primaryRunner = r
	} else {
		r, ok := s.runners[""]
		if !ok {
			return nil, dto.InternalError("no-repo runner not available")
		}
		primaryRunner = r
	}

	// Validate and resolve extra repo runners.
	extraRunners := make([]*task.Runner, 0, max(0, len(req.Repos)-1))
	for _, rs := range req.Repos[min(1, len(req.Repos)):] {
		er, ok := s.runners[rs.Name]
		if !ok {
			return nil, dto.BadRequest("unknown extra repo: " + rs.Name)
		}
		extraRunners = append(extraRunners, er)
	}

	harness := toAgentHarness(req.Harness)
	backend, ok := primaryRunner.Backends[harness]
	if !ok {
		return nil, dto.BadRequest("unknown harness: " + string(req.Harness))
	}

	if req.Model != "" && !slices.Contains(backend.Models(), req.Model) {
		return nil, dto.BadRequest("unsupported model for " + string(req.Harness) + ": " + req.Model)
	}

	if len(req.InitialPrompt.Images) > 0 && !backend.SupportsImages() {
		return nil, dto.BadRequest(string(req.Harness) + " does not support images")
	}

	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}

	// Build RepoMount slice — GitRoot filled immediately from runner.Dir.
	mounts := make([]task.RepoMount, len(req.Repos))
	for i, rs := range req.Repos {
		r := s.runners[rs.Name]
		mounts[i] = task.RepoMount{Name: rs.Name, BaseBranch: rs.BaseBranch, GitRoot: r.Dir}
	}

	// Resolve docker image and GitHub token access from user preferences.
	prefs := s.prefs.Get(userIDFromCtx(ctx))
	dockerImage := prefs.Settings.BaseImage
	ghToken := s.resolveGitHubContainerToken(ctx, prefs.Settings.GitHubTokenAccess)

	t := &task.Task{
		ID:            ksid.NewID(),
		InitialPrompt: v1PromptToAgent(req.InitialPrompt),
		Repos:         mounts,
		Harness:       harness,
		Model:         req.Model,
		DockerImage:   dockerImage,
		GitHubToken:   ghToken,
		Tailscale:     req.Tailscale,
		USB:           req.USB,
		Display:       req.Display,
		StartedAt:     time.Now().UTC(),
		OwnerID:       ownerID,
		Provider:      s.provider,
	}
	t.SetTitle(req.InitialPrompt.Text)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}

	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()

	// Run in background using the server context, not the request context.
	go func() {
		// Allocate branches for extra repos before starting the container.
		for i, er := range extraRunners {
			branch, err := er.AllocateBranch(s.ctx)
			if err != nil {
				result := task.Result{State: task.StateFailed, Err: fmt.Errorf("allocate branch for extra repo: %w", err)}
				s.mu.Lock()
				entry.result = &result
				s.taskChanged()
				s.mu.Unlock()
				close(entry.done)
				return
			}
			t.Repos[i+1].Branch = branch
		}

		h, err := primaryRunner.Start(s.ctx, t)
		if err != nil {
			result := task.Result{State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.taskChanged()
			s.mu.Unlock()
			close(entry.done)
			return
		}
		s.watchSession(entry, primaryRunner, h)
	}()

	go s.maybeFakeCI(t)

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
			return nil, dto.InternalError("save preferences: " + err.Error())
		}
	}

	return &v1.CreateTaskResp{Status: "accepted", ID: t.ID}, nil
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
		writeError(w, dto.InternalError("streaming not supported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	history, live, unsub := entry.task.Subscribe(r.Context())
	defer unsub()
	statsHistory, statsLive, statsUnsub := entry.task.SubscribeStats(r.Context())
	defer statsUnsub()

	tracker := newToolTimingTracker(entry.task.Harness)
	idx := 0

	writeEvents := func(events []v1.EventMessage) {
		for i := range events {
			data, err := marshalEvent(&events[i])
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
		writeEvents(tracker.convertMessage(msg, now))
	}
	for i := range statsHistory {
		ev := statsToEvent(&statsHistory[i])
		data, err := marshalEvent(&ev)
		if err == nil {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
			idx++
		}
	}
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	state := entry.task.GetState()
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
			writeEvents(tracker.convertMessage(msg, time.Now()))
			flusher.Flush()
		case cs, ok := <-statsCh:
			if !ok {
				statsCh = nil
				continue
			}
			ev := statsToEvent(&cs)
			data, err := marshalEvent(&ev)
			if err == nil {
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\nid: %d\n\n", data, idx)
				idx++
			}
			flusher.Flush()
		}
	}
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
		writeError(w, dto.BadRequest("toolUseID required"))
		return
	}
	history, _, unsub := entry.task.Subscribe(r.Context())
	unsub()
	for _, msg := range history {
		if tu, ok := msg.(*agent.ToolUseMessage); ok && tu.ToolUseID == toolUseID {
			writeJSONResponse(w, &v1.TaskToolInputResp{ToolUseID: tu.ToolUseID, Input: tu.Input}, nil)
			return
		}
	}
	writeError(w, dto.NotFound("tool use"))
}

// sendInput forwards user input to the agent session. On failure, it probes
// the relay daemon's liveness over SSH and returns diagnostic details in the
// 409 response so the frontend can show the user what went wrong.
//
// The relay probe uses the server context (not the request context) because the
// SSH round-trip may outlive a cancelled HTTP request, and we want the log line
// regardless.
func (s *Server) sendInput(ctx context.Context, entry *taskEntry, req *v1.InputReq) (*v1.StatusResp, error) {
	if len(req.Prompt.Images) > 0 {
		primaryName := ""
		if p := entry.task.Primary(); p != nil {
			primaryName = p.Name
		}
		runner := s.runners[primaryName]
		if b := runner.Backends[entry.task.Harness]; b != nil && !b.SupportsImages() {
			return nil, dto.BadRequest(string(entry.task.Harness) + " does not support images")
		}
	}
	if err := entry.task.SendInput(ctx, v1PromptToAgent(req.Prompt)); err != nil {
		t := entry.task
		rs := relayNoContainer
		if t.Container != "" {
			probeCtx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
			alive, relayErr := agent.IsRelayRunning(probeCtx, t.Container) //nolint:contextcheck // diagnostic probe; must outlive request
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
			"ctr", t.Container,
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
	if state := t.GetState(); state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan {
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
	primaryName := ""
	if p := t.Primary(); p != nil {
		primaryName = p.Name
	}
	runner := s.runners[primaryName]
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

func (s *Server) stopTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan && state != task.StateRunning {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.SetState(task.StateStopping)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	stopPrimaryName := ""
	if p := entry.task.Primary(); p != nil {
		stopPrimaryName = p.Name
	}
	runner := s.runners[stopPrimaryName]
	go func() {
		runner.StopTask(s.ctx, entry.task)
		s.mu.Lock()
		s.taskChanged()
		s.mu.Unlock()
	}()
	return &v1.StatusResp{Status: "stopping"}, nil
}

func (s *Server) purgeTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateWaiting && state != task.StateAsking && state != task.StateHasPlan && state != task.StateRunning && state != task.StateStopping && state != task.StateStopped {
		return nil, dto.Conflict("task is not running or waiting")
	}
	entry.task.SetState(task.StatePurging)
	s.mu.Lock()
	s.taskChanged()
	s.mu.Unlock()
	purgePrimaryName := ""
	if p := entry.task.Primary(); p != nil {
		purgePrimaryName = p.Name
	}
	runner := s.runners[purgePrimaryName]
	go s.cleanupTask(entry, runner, task.StatePurged)
	return &v1.StatusResp{Status: "purging"}, nil
}

func (s *Server) reviveTask(_ context.Context, entry *taskEntry, _ *dto.EmptyReq) (*v1.StatusResp, error) {
	state := entry.task.GetState()
	if state != task.StateStopped {
		return nil, dto.Conflict("task is not stopped")
	}
	revivePrimaryName := ""
	if p := entry.task.Primary(); p != nil {
		revivePrimaryName = p.Name
	}
	runner := s.runners[revivePrimaryName]
	entry.task.SetState(task.StateProvisioning)
	s.mu.Lock()
	// Reset done channel so watchSession works on the revived task.
	entry.done = make(chan struct{})
	entry.result = nil
	entry.cleanupOnce = sync.Once{}
	s.taskChanged()
	s.mu.Unlock()
	go func() {
		h, err := runner.ReviveTask(s.ctx, entry.task)
		if err != nil {
			slog.Warn("revive failed", "task", entry.task.ID, "err", err)
			return
		}
		s.watchSession(entry, runner, h)
	}()
	return &v1.StatusResp{Status: "provisioning"}, nil
}

func (s *Server) syncTask(ctx context.Context, entry *taskEntry, req *v1.SyncReq) (*v1.SyncResp, error) {
	t := entry.task
	switch t.GetState() {
	case task.StatePending:
		return nil, dto.Conflict("task has no container yet")
	case task.StateStopping, task.StateStopped, task.StatePurging, task.StateFailed, task.StatePurged:
		return nil, dto.Conflict("task is in a terminal state")
	case task.StateBranching, task.StateProvisioning, task.StateStarting, task.StateRunning, task.StateWaiting, task.StateAsking, task.StateHasPlan, task.StatePulling, task.StatePushing:
	}
	syncPrimaryName := ""
	syncPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		syncPrimaryName = p.Name
		syncPrimaryBranch = p.Branch
	}
	runner := s.runners[syncPrimaryName]

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
		ds, issues, err := runner.SyncToDefault(ctx, syncPrimaryBranch, t.Container, message, t.ExtraMDRepos())
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
	ds, issues, err := runner.SyncToOrigin(ctx, syncPrimaryBranch, t.Container, req.Force, t.ExtraMDRepos())
	if err != nil {
		return nil, dto.InternalError(err.Error())
	}
	status := "synced"
	if len(ds) == 0 {
		status = "empty"
	} else if len(issues) > 0 && !req.Force {
		status = "blocked"
	}
	resp := &v1.SyncResp{Status: status, Branch: syncPrimaryBranch, DiffStat: toV1DiffStat(ds), SafetyIssues: toV1SafetyIssues(issues)}
	if status != "blocked" {
		if info := s.repoInfoFor(syncPrimaryName); info != nil {
			if f := s.forge.forgeForInfo(ctx, info); f != nil {
				prNumber, err := s.startPRFlow(ctx, entry, f, info, syncPrimaryBranch, s.effectiveBaseBranch(t))
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
	t := entry.task
	if t.Container == "" {
		writeError(w, dto.Conflict("task has no container"))
		return
	}
	diffPrimaryName := ""
	diffPrimaryBranch := ""
	if p := t.Primary(); p != nil {
		diffPrimaryName = p.Name
		diffPrimaryBranch = p.Branch
	}
	runner, ok := s.runners[diffPrimaryName]
	if !ok {
		writeError(w, dto.InternalError("unknown repo"))
		return
	}
	path := r.URL.Query().Get("path")
	diff, err := runner.DiffContent(r.Context(), diffPrimaryBranch, path)
	if err != nil {
		writeError(w, dto.InternalError(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v1.DiffResp{Diff: diff})
}

// watchSession monitors a single active session. When the session's SSH
// process exits, it transitions the task to StateWaiting (the container and
// relay daemon may still be alive — see Flow 2 in the relay shutdown protocol
// in package agent). If entry.done fires first, the goroutine exits silently.
func (s *Server) watchSession(entry *taskEntry, runner *task.Runner, h *task.SessionHandle) {
	_ = runner // kept for interface consistency
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
			// Close the dispatch goroutine. CloseMsgCh is idempotent so this
			// is safe even if StopTask races and closes MsgCh concurrently.
			h.CloseMsgCh()
			<-h.DispatchDone
			if h.LogW != nil {
				_ = h.LogW.Close()
			}
			watchPrimaryName := ""
			watchPrimaryBranch := ""
			if p := t.Primary(); p != nil {
				watchPrimaryName = p.Name
				watchPrimaryBranch = p.Branch
			}
			attrs := []any{"repo", watchPrimaryName, "br", watchPrimaryBranch, "ctr", t.Container}
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
			// Asking (agent asked a question) or the task is Purging,
			// don't clobber that state.
			t.SetStateIf(task.StateRunning, task.StateWaiting)
			s.notifyTaskChange()
		case <-entry.done:
		}
	}()
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

// resolveGitHubContainerToken returns the GitHub token to inject into a
// container based on the user's access preference. Default ("" or "none")
// returns empty. "read-write" passes the parent token.
func (s *Server) resolveGitHubContainerToken(ctx context.Context, access preferences.GitHubTokenAccess) string {
	if access != preferences.GitHubTokenReadWrite {
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
func (s *Server) getTask(r *http.Request) (*taskEntry, error) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tasks[id]
	if !ok {
		return nil, dto.NotFound("task")
	}
	if s.authEnabled() {
		if u, ok := auth.UserFromContext(r.Context()); ok {
			if entry.task.OwnerID != "" && entry.task.OwnerID != u.ID {
				return nil, dto.Forbidden("task")
			}
		}
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

func (s *Server) toJSON(e *taskEntry) v1.Task {
	// Read all volatile fields in a single locked snapshot to avoid
	// data races with addMessage/RestoreMessages.
	snap := e.task.Snapshot()

	// Build Repos slice for API response.
	taskRepos := make([]v1.TaskRepo, len(e.task.Repos))
	for i, r := range e.task.Repos {
		taskRepos[i] = v1.TaskRepo{Name: r.Name, BaseBranch: r.BaseBranch, Branch: r.Branch, RemoteURL: s.repoURL(r.Name), Forge: s.repoForge(r.Name)}
	}
	if len(taskRepos) == 0 {
		taskRepos = nil
	}

	// Derive primary name for context window lookup.
	var primaryName string
	if p := e.task.Primary(); p != nil {
		primaryName = p.Name
	}

	j := v1.Task{
		ID:             e.task.ID,
		InitialPrompt:  e.task.InitialPrompt.Text,
		Title:          snap.Title,
		Repos:          taskRepos,
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
		j.StartedAt = float64(e.task.StartedAt.UnixMilli()) / 1e3
	}
	if !snap.TurnStartedAt.IsZero() {
		j.TurnStartedAt = float64(snap.TurnStartedAt.UnixMilli()) / 1e3
	}
	j.CumulativeInputTokens = snap.Usage.InputTokens
	j.CumulativeOutputTokens = snap.Usage.OutputTokens
	j.CumulativeCacheCreationInputTokens = snap.Usage.CacheCreationInputTokens
	j.CumulativeCacheReadInputTokens = snap.Usage.CacheReadInputTokens
	// Active tokens = last API call's context window fill (not the per-query sum).
	j.ActiveInputTokens = snap.LastAPIUsage.InputTokens + snap.LastAPIUsage.CacheCreationInputTokens
	j.ActiveCacheReadTokens = snap.LastAPIUsage.CacheReadInputTokens
	if snap.ContextWindowLimit > 0 {
		j.ContextWindowLimit = snap.ContextWindowLimit
	} else if primaryName != "" {
		if r := s.runners[primaryName]; r != nil {
			if b := r.Backends[e.task.Harness]; b != nil {
				j.ContextWindowLimit = b.ContextWindowLimit(snap.Model)
			}
		}
	}
	if e.result != nil {
		j.DiffStat = toV1DiffStat(e.result.DiffStat)
		j.Result = e.result.AgentResult
		if e.result.Err != nil {
			j.Error = e.result.Err.Error()
		}
	} else {
		j.DiffStat = toV1DiffStat(snap.DiffStat)
	}
	j.ForgeOwner = snap.ForgeOwner
	j.ForgeRepo = snap.ForgeRepo
	j.ForgePR = snap.ForgePR
	j.ForgeIssue = snap.ForgeIssue
	j.CIStatus = v1.CIStatus(snap.CIStatus)
	if len(snap.CIChecks) > 0 {
		j.CIChecks = make([]v1.ForgeCheck, len(snap.CIChecks))
		for i := range snap.CIChecks {
			j.CIChecks[i] = checkToDTO(&snap.CIChecks[i])
		}
	}
	if s.authStore != nil && e.task.OwnerID != "" {
		if u, ok := s.authStore.FindByID(e.task.OwnerID); ok {
			j.Owner = u.Username
		}
	}
	return j
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
