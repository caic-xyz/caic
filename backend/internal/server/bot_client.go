// Implements bot.Client for the Server, providing task creation and status tracking for the bot.

package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// ResolveRepo maps a forge full name ("owner/repo") to repo info.
// Returns nil if the forge name does not match any managed repo.
func (s *Server) ResolveRepo(forgeFullName string) *bot.RepoInfo {
	owner, repo, ok := strings.Cut(forgeFullName, "/")
	if !ok {
		return nil
	}
	info, found := s.repoReg.byForge(owner, repo)
	if !found {
		return nil
	}
	return &bot.RepoInfo{
		RelPath:    info.RelPath,
		ForgeKind:  info.ForgeKind,
		ForgeOwner: info.ForgeOwner,
		ForgeRepo:  info.ForgeRepo,
	}
}

// CreateTask implements bot.Client. It creates and starts a task using the
// same code path as the HTTP API handler, returning the new task ID.
func (s *Server) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	runner, ok := s.GetRunner(req.Repo)
	if !ok {
		return "", fmt.Errorf("runner not found for repo %s", req.Repo)
	}
	// Pick harness: prefer agent.Claude if available, otherwise the
	// lexicographically first available harness. Sorting keeps the choice
	// deterministic regardless of map iteration order.
	var harness agent.Harness
	if _, ok := runner.Backends[agent.Claude]; ok {
		harness = agent.Claude
	} else if avail := slices.Sorted(maps.Keys(runner.Backends)); len(avail) > 0 {
		harness = avail[0]
	}
	if harness == "" {
		return "", fmt.Errorf("no backend available for repo %s", req.Repo)
	}

	// Resolve the forge owner/repo so ListPendingBotTasks can resolve the
	// commenter. Only relevant for issue-triggered tasks.
	var ownerResolved, repoResolved string
	if req.IssueNumber > 0 {
		if info, ok := s.repoReg.infoFor(req.Repo); ok && info.ForgeOwner != "" {
			ownerResolved = info.ForgeOwner
			repoResolved = info.ForgeRepo
		}
	}

	// Bot tasks always get the GitHub token if available (needed for pushing CI
	// fixes). Resolve it in the request ctx so multi-user deployments use the
	// caller's OAuth token rather than the server PAT.
	ghToken := s.resolveGitHubContainerToken(ctx, true)

	id, err := s.taskMgr.Create(ctx, tasks.CreateParams{
		OwnerID:             req.OwnerID,
		Prompt:              agent.Prompt{Text: req.Prompt},
		Repos:               []tasks.CreateRepo{{Name: req.Repo}},
		Harness:             harness,
		GitHubToken:         true,
		ResolvedGitHubToken: ghToken,
		ForgeIssue:          req.IssueNumber,
		ForgeOwner:          ownerResolved,
		ForgeRepo:           repoResolved,
	})
	if err != nil {
		return "", err
	}
	slog.Info("bot task created", "id", id, "repo", req.Repo, "harness", harness)
	return id, nil
}

// WatchTaskCompletion implements bot.Client.
func (s *Server) WatchTaskCompletion(ctx context.Context, taskID string) (state, result string, err error) {
	return s.taskMgr.WatchTaskCompletion(ctx, taskID)
}

// ListPendingBotTasks implements bot.Client.
func (s *Server) ListPendingBotTasks() []bot.PendingBotTask {
	pending := s.taskMgr.ListPendingBotTasks()
	out := make([]bot.PendingBotTask, len(pending))
	for i, p := range pending {
		out[i] = bot.PendingBotTask{
			TaskID:      p.TaskID,
			ForgeOwner:  p.ForgeOwner,
			ForgeRepo:   p.ForgeRepo,
			IssueNumber: p.IssueNumber,
		}
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
