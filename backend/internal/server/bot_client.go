// Implements bot.Client for the Server, providing task creation and status tracking for the bot.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/maruel/ksid"
)

// ResolveRepo maps a forge full name ("owner/repo") to repo info.
// Returns nil if the forge name does not match any managed repo.
func (s *Server) ResolveRepo(forgeFullName string) *bot.RepoInfo {
	owner, repo, ok := strings.Cut(forgeFullName, "/")
	if !ok {
		return nil
	}
	for i := range s.repos {
		if strings.EqualFold(s.repos[i].ForgeOwner, owner) && strings.EqualFold(s.repos[i].ForgeRepo, repo) {
			return &bot.RepoInfo{
				RelPath:    s.repos[i].RelPath,
				ForgeKind:  s.repos[i].ForgeKind,
				ForgeOwner: s.repos[i].ForgeOwner,
				ForgeRepo:  s.repos[i].ForgeRepo,
			}
		}
	}
	return nil
}

// CreateTask implements bot.Client. It creates and starts a task using the
// same code path as the HTTP API handler, returning the new task ID.
func (s *Server) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	runner, ok := s.GetRunner(req.Repo)
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
	// Bot tasks always get the GitHub token if available (needed for pushing CI fixes).
	ghToken := s.resolveGitHubContainerToken(ctx, true)
	t := &task.Task{
		ID:            ksid.NewID(),
		InitialPrompt: agent.Prompt{Text: req.Prompt},
		Repos:         []task.RepoMount{{Name: req.Repo, GitRoot: runner.Dir, MountedPath: "~/src/" + req.Repo}},
		Harness:       harness,
		GitHubToken:   true,
		StartedAt:     time.Now().UTC(),
		Provider:      s.provider,
		OwnerID:       req.OwnerID,
		ForgeIssue:    req.IssueNumber,
	}
	if req.IssueNumber > 0 {
		// Set forge owner/repo so ListPendingBotTasks can resolve the commenter.
		for i := range s.repos {
			if s.repos[i].RelPath == req.Repo && s.repos[i].ForgeOwner != "" {
				t.SetPR(s.repos[i].ForgeOwner, s.repos[i].ForgeRepo, 0)
				break
			}
		}
	}
	t.SetTitle(req.Prompt)
	go t.GenerateTitle(s.ctx) //nolint:contextcheck // fire-and-forget; must outlive request
	entry := &taskEntry{task: t, done: make(chan struct{})}
	s.mu.Lock()
	s.tasks[t.ID.String()] = entry
	s.notifyTaskChange()
	s.mu.Unlock()
	go func() {
		h, err := runner.Start(s.ctx, t, ghToken)
		if err != nil {
			result := task.Result{State: task.StateFailed, Err: err}
			s.mu.Lock()
			entry.result = &result
			s.notifyTaskChange()
			s.mu.Unlock()
			close(entry.done)
			return
		}
		s.watchSession(entry, runner, h)
	}()
	slog.Info("bot task created", "id", t.ID, "repo", req.Repo, "harness", harness)
	return t.ID.String(), nil
}

// WatchTaskCompletion implements bot.Client.
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

// ListPendingBotTasks implements bot.Client.
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

// ResolveCommenter implements bot.Client.
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

// lastResultText returns the Result field of the most recent ResultMessage in
// the task's message history. Used as the squash-merge commit body.
func lastResultText(t *task.Task) string {
	msgs := t.Messages()
	for _, msg := range slices.Backward(msgs) {
		if rm, ok := msg.(*agent.ResultMessage); ok {
			return rm.Result
		}
	}
	return ""
}
