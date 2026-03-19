// PR creation flow and forge client resolution for synced branches.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/github"
	"github.com/caic-xyz/caic/backend/internal/forge/gitlab"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
	"github.com/maruel/roundtrippers"
)

// forgeManager resolves forge clients for repos, manages per-user rate-limit
// throttles, and caches GitHub App installation IDs.
type forgeManager struct {
	githubToken string
	gitlabToken string
	githubApp   githubAppClient // nil when GitHub App not configured

	mu                   sync.Mutex
	githubOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	githubPATThrottle    http.RoundTripper
	githubAppThrottle    http.RoundTripper
	gitlabOAuthThrottles map[string]http.RoundTripper // keyed by user ID
	gitlabPATThrottle    http.RoundTripper
	githubInstallations  map[string]int64 // owner (lowercase) → installation ID; -1 = not installed
}

func newForgeManager(githubToken, gitlabToken string, githubApp githubAppClient) *forgeManager {
	return &forgeManager{
		githubToken:          githubToken,
		gitlabToken:          gitlabToken,
		githubApp:            githubApp,
		githubOAuthThrottles: make(map[string]http.RoundTripper),
		githubPATThrottle:    newThrottle(),
		githubAppThrottle:    newThrottle(),
		gitlabOAuthThrottles: make(map[string]http.RoundTripper),
		gitlabPATThrottle:    newThrottle(),
		githubInstallations:  make(map[string]int64),
	}
}

// forgeForInfo returns the appropriate forge.Forge for the repo's remote, using
// the configured tokens. Falls back to a GitHub App installation token when no
// user OAuth token or PAT is available. Returns nil if no token is available.
func (m *forgeManager) forgeForInfo(ctx context.Context, info *repoInfo) forge.Forge {
	if f := m.forgeFor(ctx, info.ForgeKind); f != nil {
		return f
	}
	if info.ForgeKind == forge.KindGitHub && m.githubApp != nil {
		installID := m.installationID(info.ForgeOwner)
		if installID == 0 {
			id, err := m.githubApp.RepoInstallation(ctx, info.ForgeOwner, info.ForgeRepo)
			if err != nil {
				// Cache -1 to avoid repeating the lookup on every call.
				m.storeInstallationID(info.ForgeOwner, -1)
				return nil
			}
			m.storeInstallationID(info.ForgeOwner, id)
			installID = id
		}
		if installID < 0 {
			return nil // app not installed for this owner
		}
		client, err := m.githubApp.ForgeClient(ctx, installID)
		if err != nil {
			slog.Warn("forgeForInfo: app forge client", "err", err)
			return nil
		}
		return client
	}
	return nil
}

// forgeFor returns a Forge client for the given kind.
// In OAuth mode the authenticated user's access token is used.
// In PAT mode (no OAuth) the global token is used.
// Config.Validate ensures these two modes are never mixed.
// Returns nil if no token is available.
func (m *forgeManager) forgeFor(ctx context.Context, kind forge.Kind) forge.Forge {
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == kind && u.AccessToken != "" {
		switch kind {
		case forge.KindGitHub:
			return github.NewClient(u.AccessToken, m.githubOAuthThrottle(u.ID))
		case forge.KindGitLab:
			return gitlab.NewClient(u.AccessToken, m.gitlabOAuthThrottle(u.ID))
		}
	}
	switch kind {
	case forge.KindGitHub:
		if m.githubToken != "" {
			return github.NewClient(m.githubToken, m.githubPATThrottle)
		}
	case forge.KindGitLab:
		if m.gitlabToken != "" {
			return gitlab.NewClient(m.gitlabToken, m.gitlabPATThrottle)
		}
	}
	return nil
}

// storeInstallationID caches the GitHub App installation ID for the given owner.
// id == -1 means the app is not installed for that owner.
func (m *forgeManager) storeInstallationID(owner string, id int64) {
	if id == 0 {
		return
	}
	m.mu.Lock()
	m.githubInstallations[strings.ToLower(owner)] = id
	m.mu.Unlock()
}

// installationID returns the cached installation ID for the given owner, or 0 if unknown.
// Returns -1 if the app is known to not be installed for that owner.
func (m *forgeManager) installationID(owner string) int64 {
	m.mu.Lock()
	id := m.githubInstallations[strings.ToLower(owner)]
	m.mu.Unlock()
	return id
}

// githubOAuthThrottle returns the per-user throttle for GitHub OAuth.
// Each OAuth user has a separate GitHub rate-limit bucket; throttles must not be shared.
func (m *forgeManager) githubOAuthThrottle(userID string) http.RoundTripper {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.githubOAuthThrottles[userID]; ok {
		return t
	}
	t := newThrottle()
	m.githubOAuthThrottles[userID] = t
	return t
}

// gitlabOAuthThrottle returns the per-user throttle for GitLab OAuth.
func (m *forgeManager) gitlabOAuthThrottle(userID string) http.RoundTripper {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.gitlabOAuthThrottles[userID]; ok {
		return t
	}
	t := newThrottle()
	m.gitlabOAuthThrottles[userID] = t
	return t
}

// commenterFor returns a bot.Commenter for posting comments via the GitHub App
// (when installationID is non-zero) or the configured PAT, or nil if neither
// is available.
func (m *forgeManager) commenterFor(installationID int64) bot.Commenter {
	if m.githubApp != nil && installationID != 0 {
		return &appInstallCommenter{app: m.githubApp, installationID: installationID}
	}
	if m.githubToken != "" {
		return github.NewClient(m.githubToken, m.githubPATThrottle)
	}
	return nil
}

// newThrottle returns a Throttle transport at 1 QPS backed by http.DefaultTransport.
func newThrottle() http.RoundTripper {
	return &roundtrippers.Throttle{QPS: 1, Transport: http.DefaultTransport}
}

// startPRFlow creates a PR/MR for the synced branch, records it on the task,
// and launches CI monitoring in a goroutine. Returns the PR number on success.
func (s *Server) startPRFlow(ctx context.Context, entry *taskEntry, f forge.Forge, info *repoInfo, branch, baseBranch string) (int, error) {
	t := entry.task
	title := t.Title()
	if title == "" {
		title = t.InitialPrompt.Text
	}
	var body string
	if entry.result != nil {
		body = entry.result.AgentResult
	}
	pr, err := f.CreatePR(ctx, info.ForgeOwner, info.ForgeRepo, branch, baseBranch, title, body)
	if err != nil {
		return 0, err
	}
	slog.Info("PR created", "task", t.ID, "forge", f.Name(), "owner", info.ForgeOwner, "repo", info.ForgeRepo, "pr", pr.Number)
	t.SetPR(info.ForgeOwner, info.ForgeRepo, pr.Number)
	t.WriteToLog(&agent.MetaPRMessage{
		MessageType: "caic_pr",
		ForgeOwner:  info.ForgeOwner,
		ForgeRepo:   info.ForgeRepo,
		ForgePR:     pr.Number,
	})
	s.mu.Lock()
	entry.monitorBranch = branch
	s.mu.Unlock()
	s.notifyTaskChange()
	go s.monitorCI(s.ctx, entry, f, info.ForgeOwner, info.ForgeRepo, pr.HeadSHA) //nolint:contextcheck // CI monitoring must outlive the request
	return pr.Number, nil
}

// repoInfoFor returns the repoInfo for relPath, or nil if not found.
// Safe to call without the mutex (s.repos is immutable after construction).
func (s *Server) repoInfoFor(relPath string) *repoInfo {
	for i := range s.repos {
		if s.repos[i].RelPath == relPath {
			return &s.repos[i]
		}
	}
	return nil
}

// effectiveBaseBranch returns the branch the task was forked from: the task's
// own override if set, otherwise the runner's configured default.
func (s *Server) effectiveBaseBranch(t *task.Task) string {
	p := t.Primary()
	if p == nil {
		return ""
	}
	if p.BaseBranch != "" {
		return p.BaseBranch
	}
	if runner, ok := s.runners[p.Name]; ok {
		return runner.BaseBranch
	}
	return ""
}

// ResolveRepo implements bot.Client. It maps a forge full name to repo info.
func (s *Server) ResolveRepo(forgeFullName string) *bot.RepoInfo {
	owner, repo, ok := strings.Cut(forgeFullName, "/")
	if !ok {
		return nil
	}
	for _, ri := range s.repos {
		if strings.EqualFold(ri.ForgeOwner, owner) && strings.EqualFold(ri.ForgeRepo, repo) {
			return &bot.RepoInfo{
				RelPath:    ri.RelPath,
				ForgeKind:  ri.ForgeKind,
				ForgeOwner: ri.ForgeOwner,
				ForgeRepo:  ri.ForgeRepo,
			}
		}
	}
	return nil
}

// CreateTask implements bot.Client. It creates and starts a task using the
// same code path as the HTTP API handler, returning the new task ID.
func (s *Server) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	runner, ok := s.runners[req.Repo]
	if !ok {
		return "", fmt.Errorf("runner not found for repo %s", req.Repo)
	}
	// Pick harness: prefer agent.Claude if available, otherwise take the first one.
	var harness agent.Harness
	if _, ok := runner.Backends[agent.Claude]; ok {
		harness = agent.Claude
	} else {
		for h := range runner.Backends {
			harness = h
			break
		}
	}
	if harness == "" {
		return "", fmt.Errorf("no backend available for repo %s", req.Repo)
	}
	t := &task.Task{
		ID:            ksid.NewID(),
		InitialPrompt: agent.Prompt{Text: req.Prompt},
		Repos:         []task.RepoMount{{Name: req.Repo, GitRoot: runner.Dir}},
		Harness:       harness,
		StartedAt:     time.Now().UTC(),
		Provider:      s.provider,
		OwnerID:       req.OwnerID,
		ForgeIssue:    req.IssueNumber,
	}
	if req.IssueNumber > 0 {
		// Set forge owner/repo so ListPendingBotTasks can resolve the commenter.
		for _, ri := range s.repos {
			if ri.RelPath == req.Repo && ri.ForgeOwner != "" {
				t.SetPR(ri.ForgeOwner, ri.ForgeRepo, 0)
				break
			}
		}
	}
	t.SetTitle(req.Prompt)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}
	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.taskChanged()
	s.mu.Unlock()
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
	slog.Info("bot task created", "id", t.ID, "repo", req.Repo, "harness", harness)
	return t.ID.String(), nil
}

// WatchTaskCompletion implements bot.Client. It blocks until the task reaches
// a state where the agent has finished (waiting, stopped, failed, or purged),
// then returns the state name and the agent's result text.
func (s *Server) WatchTaskCompletion(ctx context.Context, taskID string) (state, result string, err error) {
	s.mu.Lock()
	entry, ok := s.tasks[taskID]
	s.mu.Unlock()
	if !ok {
		return "", "", fmt.Errorf("task %s not found", taskID)
	}
	for {
		st := entry.task.GetState()
		switch st { //nolint:exhaustive // only terminal/idle states are relevant
		case task.StateWaiting, task.StateStopped, task.StateFailed, task.StatePurged:
			return st.String(), lastResultText(entry.task), nil
		}
		s.mu.Lock()
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
}

// ListPendingBotTasks implements bot.Client. It returns non-terminal tasks
// that have a ForgeIssue set (i.e. tasks the bot should post a completion
// comment on). Called during startup to resume comment watchers.
func (s *Server) ListPendingBotTasks() []bot.PendingBotTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []bot.PendingBotTask
	for id, entry := range s.tasks {
		snap := entry.task.Snapshot()
		if snap.ForgeIssue <= 0 {
			continue
		}
		st := snap.State
		if st == task.StateWaiting || st == task.StateStopped || st == task.StateFailed || st == task.StatePurged {
			continue // already terminal for bot purposes
		}
		out = append(out, bot.PendingBotTask{
			TaskID:      id,
			ForgeOwner:  snap.ForgeOwner,
			ForgeRepo:   snap.ForgeRepo,
			IssueNumber: snap.ForgeIssue,
		})
	}
	return out
}

// ResolveCommenter implements bot.Client. It returns a Commenter for the
// given forge owner by looking up the cached GitHub App installation ID,
// falling back to the PAT. Returns nil if no commenter is available.
func (s *Server) ResolveCommenter(ctx context.Context, owner string) bot.Commenter {
	installID := s.forge.installationID(owner)
	if installID == 0 && s.forge.githubApp != nil {
		// Try to discover the installation ID via the API.
		id, err := s.forge.githubApp.RepoInstallation(ctx, owner, "")
		if err == nil && id > 0 {
			s.forge.storeInstallationID(owner, id)
			installID = id
		}
	}
	return s.forge.commenterFor(installID)
}
