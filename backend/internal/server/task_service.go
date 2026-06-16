// Task command orchestration and API DTO assembly.

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

// FakeCIHook generate fake tasks.
//
// TODO: Cleaner interface.
type FakeCIHook func(ctx context.Context, t *task.Task)

// taskService owns task command orchestration and API DTO assembly.
//
// HTTP handlers use it after route-level request decoding and task lookup, so
// server route code stays focused on protocol concerns.
type taskService struct {
	ctx       context.Context
	taskMgr   *tasks.Manager
	prefs     *preferences.Store
	repos     *repos.Service
	forge     *forgemanager.Manager
	ciService *ci.Service
	authStore *auth.Store
	fakeCI    FakeCIHook
}

func (s *taskService) authEnabled() bool {
	return s.authStore != nil
}

func (s *taskService) maybeFakeCI(t *task.Task) {
	if s.fakeCI == nil {
		return
	}
	s.fakeCI(s.ctx, t)
}

func (s *taskService) listTasks(ctx context.Context, _ *api.EmptyReq) (*[]v1.Task, error) {
	out := s.taskListSnapshot(ctx)
	return &out, nil
}

func (s *taskService) taskListSnapshot(ctx context.Context) []v1.Task {
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

// getTask returns a single task by id. The route resolves the entry (404 for
// unknown ids, 403 across owners), giving clients an authoritative existence
// check and initial state without depending on the eventually-consistent task
// list snapshot.
func (s *taskService) getTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.Task, error) {
	dto := v1conv.Task(ctx, entry, s.taskResolvers())
	return &dto, nil
}

func (s *taskService) createTask(ctx context.Context, req *v1.CreateTaskReq) (*v1.Task, error) {
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

	// Return the full task so clients can seed their store and render the detail
	// view immediately, without waiting for the SSE upsert to deliver it.
	dto := v1conv.Task(ctx, entry, s.taskResolvers())
	return &dto, nil
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
func (s *taskService) sendInput(ctx context.Context, entry *tasks.Entry, req *v1.InputReq) (*v1.StatusResp, error) {
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
	slog.WarnContext(ctx, "no active session",
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

func (s *taskService) restartTask(ctx context.Context, entry *tasks.Entry, req *v1.RestartReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Restart(ctx, entry, v1conv.PromptToAgent(req.Prompt)); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "restarted"}, nil
}

func (s *taskService) clearContext(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.ClearContext(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "cleared"}, nil
}

func (s *taskService) compactContext(ctx context.Context, entry *tasks.Entry, req *v1.CompactReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Compact(ctx, entry, req.Instructions); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "compacting"}, nil
}

func (s *taskService) stopTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Stop(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "stopping"}, nil
}

func (s *taskService) purgeTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Purge(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "purging"}, nil
}

func (s *taskService) reviveTask(ctx context.Context, entry *tasks.Entry, _ *api.EmptyReq) (*v1.StatusResp, error) {
	if err := s.taskMgr.Revive(ctx, entry); err != nil {
		return nil, toDTO(err)
	}
	return &v1.StatusResp{Status: "provisioning"}, nil
}

func (s *taskService) forkTask(ctx context.Context, entry *tasks.Entry, req *v1.ForkTaskReq) (*v1.Task, error) {
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
	dto := v1conv.Task(ctx, forkEntry, s.taskResolvers())
	return &dto, nil
}

func (s *taskService) taskToolInput(ctx context.Context, entry *tasks.Entry, toolUseID string) (*v1.TaskToolInputResp, error) {
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

func (s *taskService) taskDiff(ctx context.Context, entry *tasks.Entry, path string) (*v1.DiffResp, error) {
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

func (s *taskService) syncTask(ctx context.Context, entry *tasks.Entry, req *v1.SyncReq) (*v1.SyncResp, error) {
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
					slog.WarnContext(ctx, "sync: create PR", "repo", info.ForgeRepo, "branch", syncPrimaryBranch, "err", err)
				} else {
					resp.PRNumber = prNumber
				}
			} else {
				slog.WarnContext(ctx, "sync: no forge client available, skipping PR flow", "repo", syncPrimaryName, "forge", info.ForgeKind)
			}
		} else {
			slog.WarnContext(ctx, "sync: repo not found in server list, skipping PR flow", "repo", syncPrimaryName)
		}
	}
	return resp, nil
}

// resolveGitHubContainerToken returns the GitHub token to inject into a
// runtime instance when enabled is true, otherwise returns empty.
func (s *taskService) resolveGitHubContainerToken(ctx context.Context, enabled bool) string {
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

func (s *taskService) taskResolvers() v1conv.TaskResolvers {
	return newTaskResolvers(s.taskMgr, s.repos, s.authStore)
}

// newTaskResolvers builds the resolver set used to convert task entries into API
// DTOs. It is a free function so any HTTP concern object that creates a task
// (task and CI handlers) can assemble the full Task DTO it returns to clients
// from the same shared dependencies.
func newTaskResolvers(taskMgr *tasks.Manager, repoSvc *repos.Service, authStore *auth.Store) v1conv.TaskResolvers {
	return v1conv.TaskResolvers{
		RepoURL: func(rel string) string {
			if info, ok := repoSvc.InfoFor(rel); ok {
				return gitutil.RemoteToHTTPS(info.Remote)
			}
			return ""
		},
		RepoForge: func(rel string) v1.Forge {
			if info, ok := repoSvc.InfoFor(rel); ok {
				return v1.Forge(info.ForgeKind)
			}
			return ""
		},
		SudoPassword: taskMgr.SudoPassword,
		OwnerName: func(ownerID string) string {
			if authStore == nil || ownerID == "" {
				return ""
			}
			if u, ok := authStore.FindByID(ownerID); ok {
				return u.Username
			}
			return ""
		},
		ContextWindowLimit: func(repo string, harnessName harness.Name, model string) int {
			r, ok := taskMgr.Runner(repo)
			if !ok {
				return 0
			}
			b := r.Backends[harnessName]
			if b == nil {
				return 0
			}
			return b.ContextWindowLimit(model)
		},
	}
}
