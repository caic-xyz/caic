// Task API service for task command and DTO assembly behavior.

package server

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/caic-xyz/md/gitutil"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/ci"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/api/v1conv"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// taskAPIService owns task command orchestration and API DTO assembly.
//
// HTTP handlers use it after route-level request decoding and task lookup, so
// server route code stays focused on protocol concerns.
type taskAPIService struct {
	ctx       context.Context
	taskMgr   *tasks.Manager
	prefs     *preferences.Store
	repos     *repos.Service
	forge     *forgemanager.Manager
	ciService *ci.Service
	authStore *auth.Store
	fakeCI    fakeCIHook
}

func (s *taskAPIService) authEnabled() bool {
	return s.authStore != nil
}

func (s *taskAPIService) maybeFakeCI(t *task.Task) {
	if s.fakeCI == nil {
		return
	}
	s.fakeCI(s.ctx, t)
}

func (s *taskAPIService) repoURL(rel string) string {
	if info, ok := s.repos.InfoFor(rel); ok {
		return gitutil.RemoteToHTTPS(info.Remote)
	}
	return ""
}

func (s *taskAPIService) repoForge(rel string) v1.Forge {
	if info, ok := s.repos.InfoFor(rel); ok {
		return v1.Forge(info.ForgeKind)
	}
	return ""
}

func (s *taskAPIService) listTasks(ctx context.Context, _ *api.EmptyReq) (*[]v1.Task, error) {
	out := s.taskListSnapshot(ctx)
	return &out, nil
}

func (s *taskAPIService) taskListSnapshot(ctx context.Context) []v1.Task {
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
	return out
}

func (s *taskAPIService) createTask(ctx context.Context, req *v1.CreateTaskReq) (*v1.CreateTaskResp, error) {
	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}

	// Docker image and max CPUs come from the user's preferences; the rest of
	// validation (unknown repo/harness/model/images) lives in Manager.Create.
	prefs := s.prefs.Get(userIDFromCtx(ctx))

	taskRepos := make([]tasks.CreateRepo, len(req.Repos))
	for i, r := range req.Repos {
		taskRepos[i] = tasks.CreateRepo{Name: r.Name, BaseBranch: r.BaseBranch}
	}

	id, err := s.taskMgr.Create(ctx, tasks.CreateParams{
		OwnerID:             ownerID,
		Prompt:              v1conv.PromptToAgent(req.InitialPrompt),
		Repos:               taskRepos,
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
		ContainerPlatform:   prefs.Settings.ContainerPlatform.String(),
		MaxCPUs:             prefs.Settings.MaxCPUs,
		CacheMounts:         cacheMountsFromSettings(&prefs.Settings),
		Mounts:              mountsFromSettings(&prefs.Settings),
	})
	if err != nil {
		return nil, toDTO(err)
	}

	entry, ok := s.taskMgr.GetEntry(id)
	if !ok {
		return nil, api.InternalError("created task not found")
	}

	go s.maybeFakeCI(entry.Task())

	if err := s.prefs.Update(userIDFromCtx(ctx), func(p *preferences.Preferences) {
		p.Harness = string(req.Harness)
		if req.Model == "" {
			delete(p.Models, string(req.Harness))
		} else {
			if p.Models == nil {
				p.Models = make(map[string]string)
			}
			p.Models[string(req.Harness)] = req.Model
		}
		harnessName := string(req.Harness)
		if req.Effort == "" {
			delete(p.Efforts[harnessName], req.Model)
			if len(p.Efforts[harnessName]) == 0 {
				delete(p.Efforts, harnessName)
			}
		} else {
			if p.Efforts == nil {
				p.Efforts = make(preferences.EffortPreferences)
			}
			if p.Efforts[harnessName] == nil {
				p.Efforts[harnessName] = make(map[string]string)
			}
			p.Efforts[harnessName][req.Model] = req.Effort
		}
		if len(req.Repos) > 0 {
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
			}
		}
	}); err != nil {
		return nil, api.InternalError("save preferences: " + err.Error())
	}

	return &v1.CreateTaskResp{Status: "accepted", ID: entry.Task().ID}, nil
}

// relayStatus describes the state of the runtime-instance relay daemon, probed
// over SSH when SendInput fails. Combined with the task state and session
// status (from task.SendInput's error), the three values pinpoint why input
// delivery failed:
//
//   - state=waiting session=none  relay=dead → relay died, reconnect failed.
//   - state=waiting session=exited relay=alive → SSH attach exited but relay
//     is still running; reconnect should recover.
//   - state=running session=none  relay=alive → state-machine bug: state says
//     running but no Go-side session object exists.
//   - state=pending session=none  relay=no-instance → task never started.
type relayStatus string

const (
	relayAlive       relayStatus = "alive"        // Relay socket exists; daemon is running.
	relayDead        relayStatus = "dead"         // No socket; daemon exited or was never started.
	relayCheckFailed relayStatus = "check-failed" // SSH probe failed (runtime instance unreachable).
	relayNoInstance  relayStatus = "no-instance"  // Task has no runtime instance yet.
)

// sendInput forwards user input to the agent session. On failure, it probes
// the relay daemon's liveness over SSH and returns diagnostic details in the
// 409 response so the frontend can show the user what went wrong.
//
// The relay probe uses the server context (not the request context) because the
// SSH round-trip may outlive a cancelled HTTP request, and we want the log line
// regardless.
func (s *taskAPIService) sendInput(ctx context.Context, entry *tasks.Entry, req *v1.InputReq) (*v1.StatusResp, error) {
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

func (s *taskAPIService) restartTask(ctx context.Context, entry *tasks.Entry, req *v1.RestartReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Restart(ctx, entry, v1conv.PromptToAgent(req.Prompt)); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "restarted"}, nil
}

func (s *taskAPIService) clearContext(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.ClearContext(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "cleared"}, nil
}

func (s *taskAPIService) compactContext(ctx context.Context, entry *tasks.Entry, req *v1.CompactReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Compact(ctx, entry, req.Instructions); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "compacting"}, nil
}

func (s *taskAPIService) stopTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Stop(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "stopping"}, nil
}

func (s *taskAPIService) purgeTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Purge(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "purging"}, nil
}

func (s *taskAPIService) reviveTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Revive(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "provisioning"}, nil
}

func (s *taskAPIService) forkTask(ctx context.Context, entry *tasks.Entry, req *v1.ForkTaskReq) (*v1.CreateTaskResp, error) {
	source := entry.Task()

	var ownerID string
	if u, ok := auth.UserFromContext(ctx); ok {
		ownerID = u.ID
	}

	var selectedHarness harness.Name
	if req.Harness != "" {
		selectedHarness = v1conv.AgentHarness(req.Harness)
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
		Harness:             selectedHarness,
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

func (s *taskAPIService) taskToolInput(ctx context.Context, entry *tasks.Entry, toolUseID string) (*v1.TaskToolInputResp, error) {
	if toolUseID == "" {
		return nil, api.BadRequest("toolUseID required")
	}
	s.taskMgr.LoadMessagesOnDemand(entry)
	history, _, unsub := entry.Task().Subscribe(ctx)
	unsub()
	for _, msg := range history {
		if tu, ok := msg.(*agent.ToolUseMessage); ok && tu.ToolUseID == toolUseID {
			return &v1.TaskToolInputResp{ToolUseID: tu.ToolUseID, Input: tu.Input}, nil
		}
	}
	return nil, api.NotFound("tool use")
}

func (s *taskAPIService) taskDiff(ctx context.Context, entry *tasks.Entry, path string) (*v1.DiffResp, error) {
	t := entry.Task()
	if t.RuntimeInstanceID() == "" {
		return nil, api.Conflict("task has no instance")
	}
	diffPrimaryName := ""
	if p := t.Primary(); p != nil {
		diffPrimaryName = p.Name
	}
	runner, ok := s.taskMgr.Runner(diffPrimaryName)
	if !ok {
		return nil, api.InternalError("unknown repo")
	}
	diff, err := runner.DiffContent(ctx, t, path)
	if err != nil {
		return nil, api.InternalError(err.Error())
	}
	return &v1.DiffResp{Diff: diff}, nil
}

func (s *taskAPIService) syncTask(ctx context.Context, entry *tasks.Entry, req *v1.SyncReq) (*v1.SyncResp, error) {
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
		if info, ok := s.repos.InfoFor(syncPrimaryName); ok {
			if f := s.forge.ForgeForInfo(ctx, &info); f != nil {
				ciInfo := ci.RepoInfo{
					RelPath:    info.RelPath,
					BaseBranch: info.BaseBranch,
					ForgeKind:  info.ForgeKind,
					ForgeOwner: info.ForgeOwner,
					ForgeRepo:  info.ForgeRepo,
				}
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

// resolveGitHubContainerToken returns the GitHub token to inject into a
// runtime instance when enabled is true, otherwise returns empty.
func (s *taskAPIService) resolveGitHubContainerToken(ctx context.Context, enabled bool) string {
	if !enabled {
		return ""
	}
	// Resolve the parent token: prefer the OAuth user's token, fall back to
	// the server-level PAT.
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == forge.KindGitHub && u.AccessToken != "" {
		return u.AccessToken
	}
	if s.forge != nil {
		return s.forge.GitHubToken()
	}
	return ""
}

func (s *taskAPIService) taskResolvers() v1conv.TaskResolvers {
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
		ContextWindowLimit: func(repo string, harness harness.Name, model string) int {
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
