// Bot client adapter for task creation and forge comment resolution.

package app

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemgr"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
)

// botClient adapts task and forge stores to bot.Client.
type botClient struct {
	repoSvc  *repo.Service
	taskMgr  *taskmgr.Manager
	forgeMgr *forgemgr.Manager
}

// ResolveRepo maps a forge full name ("owner/repo") to repo info.
// Returns nil if the forge name does not match any managed repo.
func (c *botClient) ResolveRepo(forgeFullName string) *bot.RepoInfo {
	owner, repoName, ok := strings.Cut(forgeFullName, "/")
	if !ok {
		return nil
	}
	info, found := c.repoSvc.Repositories.RepositoryByForge(owner, repoName)
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

// CreateTask creates and starts a task for bot-driven automation.
func (c *botClient) CreateTask(ctx context.Context, req task.CreateRequest) (string, error) {
	if _, ok := c.taskMgr.Checkouts.Checkout(req.Repo); !ok {
		return "", fmt.Errorf("checkout not found for repo %s", req.Repo)
	}
	backends := c.taskMgr.Backends
	// Pick harness: prefer Claude if available, otherwise the
	// lexicographically first available harness. Sorting keeps the choice
	// deterministic regardless of map iteration order.
	var selectedHarness harness.Name
	if _, ok := backends[harness.Claude]; ok {
		selectedHarness = harness.Claude
	} else if avail := slices.Sorted(maps.Keys(backends)); len(avail) > 0 {
		selectedHarness = avail[0]
	}
	if selectedHarness == "" {
		return "", fmt.Errorf("no backend available for repo %s", req.Repo)
	}

	// Resolve the forge owner/repo so ListPendingBotTasks can resolve the
	// commenter. Only relevant for issue-triggered tasks.
	var ownerResolved, repoResolved string
	if req.ForgeIssue > 0 {
		if info, ok := c.repoSvc.Repositories.Repository(req.Repo); ok && info.ForgeOwner != "" {
			ownerResolved = info.ForgeOwner
			repoResolved = info.ForgeRepo
		}
	}

	// Bot tasks always get the GitHub token if available (needed for pushing CI
	// fixes). Resolve it in the request ctx so multi-user deployments use the
	// caller's OAuth token rather than the server PAT.
	ghToken := c.resolveGitHubContainerToken(ctx, true)

	id, err := c.taskMgr.Create(ctx, taskmgr.CreateParams{
		OwnerID:             req.OwnerID,
		Prompt:              agent.Prompt{Text: req.Prompt},
		Repos:               []taskmgr.CreateRepo{{Name: req.Repo}},
		Harness:             selectedHarness,
		GitHubToken:         true,
		ResolvedGitHubToken: ghToken,
		ForgeIssue:          req.ForgeIssue,
		ForgeOwner:          ownerResolved,
		ForgeRepo:           repoResolved,
	})
	if err != nil {
		return "", err
	}
	slog.InfoContext(ctx, "bot task created", "id", id, "repo", req.Repo, "harness", selectedHarness)
	return id, nil
}

// WatchTaskCompletion blocks until a task reaches a terminal state.
func (c *botClient) WatchTaskCompletion(ctx context.Context, taskID string) (state, result string, err error) {
	return c.taskMgr.WatchTaskCompletion(ctx, taskID)
}

// ListPendingBotTasks returns non-terminal bot-created tasks.
func (c *botClient) ListPendingBotTasks() []bot.PendingBotTask {
	pending := c.taskMgr.ListPendingBotTasks()
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

// ResolveCommenter returns a forge commenter for an owner.
func (c *botClient) ResolveCommenter(ctx context.Context, owner string) forge.Commenter {
	installID := c.forgeMgr.InstallationID(owner)
	if installID == 0 && c.forgeMgr.GitHubApp() != nil {
		// Try to discover the installation ID via the API.
		id, err := c.forgeMgr.GitHubApp().RepoInstallation(ctx, owner, "")
		if err == nil && id > 0 {
			c.forgeMgr.StoreInstallationID(owner, id)
			installID = id
		}
	}
	return c.forgeMgr.CommenterFor(installID)
}

func (c *botClient) resolveGitHubContainerToken(ctx context.Context, enabled bool) string {
	if !enabled {
		return ""
	}
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == auth.ProviderGitHub && u.AccessToken != "" {
		return u.AccessToken
	}
	return c.forgeMgr.GitHubToken()
}
