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
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgemanager"
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

// botClient adapts task and forge stores to bot.Client.
type botClient struct {
	repos   *repos.Service
	taskMgr *tasks.Manager
	forge   *forgemanager.Manager
}

// ResolveRepo maps a forge full name ("owner/repo") to repo info.
// Returns nil if the forge name does not match any managed repo.
func (c *botClient) ResolveRepo(forgeFullName string) *bot.RepoInfo {
	owner, repo, ok := strings.Cut(forgeFullName, "/")
	if !ok {
		return nil
	}
	info, found := c.repos.ByForge(owner, repo)
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
func (c *botClient) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
	runner, ok := c.taskMgr.Runner(req.Repo)
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
		if info, ok := c.repos.InfoFor(req.Repo); ok && info.ForgeOwner != "" {
			ownerResolved = info.ForgeOwner
			repoResolved = info.ForgeRepo
		}
	}

	// Bot tasks always get the GitHub token if available (needed for pushing CI
	// fixes). Resolve it in the request ctx so multi-user deployments use the
	// caller's OAuth token rather than the server PAT.
	ghToken := c.resolveGitHubContainerToken(ctx, true)

	id, err := c.taskMgr.Create(ctx, tasks.CreateParams{
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
func (c *botClient) ResolveCommenter(ctx context.Context, owner string) bot.Commenter {
	installID := c.forge.InstallationID(owner)
	if installID == 0 && c.forge.GitHubApp() != nil {
		// Try to discover the installation ID via the API.
		id, err := c.forge.GitHubApp().RepoInstallation(ctx, owner, "")
		if err == nil && id > 0 {
			c.forge.StoreInstallationID(owner, id)
			installID = id
		}
	}
	return c.forge.CommenterFor(installID)
}

func (c *botClient) resolveGitHubContainerToken(ctx context.Context, enabled bool) string {
	if !enabled {
		return ""
	}
	if u, ok := auth.UserFromContext(ctx); ok && u.Provider == forge.KindGitHub && u.AccessToken != "" {
		return u.AccessToken
	}
	if c.forge != nil {
		return c.forge.GitHubToken()
	}
	return ""
}
