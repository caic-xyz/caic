// Bot client adapter for task creation and forge comment resolution.

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
	"github.com/caic-xyz/caic/backend/internal/repos"
	"github.com/caic-xyz/caic/backend/internal/tasks"
)

type tokenResolver func(ctx context.Context, enabled bool) string

type botClientDeps struct {
	repos     *repos.Service
	taskMgr   *tasks.Manager
	forge     *ForgeManager
	tokenFunc tokenResolver
}

// BotClient adapts task and forge stores to bot.Client.
type BotClient struct {
	repos     *repos.Service
	taskMgr   *tasks.Manager
	forge     *ForgeManager
	tokenFunc tokenResolver
}

func newBotClient(d botClientDeps) *BotClient {
	return &BotClient{
		repos:     d.repos,
		taskMgr:   d.taskMgr,
		forge:     d.forge,
		tokenFunc: d.tokenFunc,
	}
}

// ResolveRepo maps a forge full name ("owner/repo") to repo info.
// Returns nil if the forge name does not match any managed repo.
func (c *BotClient) ResolveRepo(forgeFullName string) *bot.RepoInfo {
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
func (c *BotClient) CreateTask(ctx context.Context, req bot.TaskRequest) (string, error) {
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
	ghToken := ""
	if c.tokenFunc != nil {
		ghToken = c.tokenFunc(ctx, true)
	}

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
func (c *BotClient) WatchTaskCompletion(ctx context.Context, taskID string) (state, result string, err error) {
	return c.taskMgr.WatchTaskCompletion(ctx, taskID)
}

// ListPendingBotTasks returns non-terminal bot-created tasks.
func (c *BotClient) ListPendingBotTasks() []bot.PendingBotTask {
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
func (c *BotClient) ResolveCommenter(ctx context.Context, owner string) bot.Commenter {
	installID := c.forge.installationID(owner)
	if installID == 0 && c.forge.githubApp != nil {
		// Try to discover the installation ID via the API.
		id, err := c.forge.githubApp.RepoInstallation(ctx, owner, "")
		if err == nil && id > 0 {
			c.forge.storeInstallationID(owner, id)
			installID = id
		}
	}
	return c.forge.commenterFor(installID)
}
