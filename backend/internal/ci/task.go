// PR creation flow and forge client resolution for synced branches.

package ci

import (
	"context"
	"slices"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/bot"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// GitHubAppClient provides forge operations scoped to a GitHub App installation.
type GitHubAppClient interface {
	ForgeClient(ctx context.Context, installationID int64) (forge.Forge, error)
	DeleteInstallation(ctx context.Context, installationID int64) error
	RepoInstallation(ctx context.Context, owner, repo string) (int64, error)
	PostComment(ctx context.Context, installationID int64, owner, repo string, issueNumber int, body string) error
}

// RepoInfo identifies a repository managed by the CI service.
type RepoInfo struct {
	RelPath    string
	BaseBranch string
	ForgeKind  forge.Kind
	ForgeOwner string
	ForgeRepo  string
}

// TaskEntry is an abstract task handle for CI monitoring.
type TaskEntry interface {
	GetTask() *task.Task
	GetMonitorBranch() string
	SetMonitorBranch(branch string)
	SetResult(result *task.Result)
	GetResult() *task.Result
	CloseDone()
}

// RepoCIState is an in-memory CI status snapshot for a repository's default branch.
type RepoCIState struct {
	Status  forge.CIStatus
	Checks  []forge.Check
	HeadSHA string
}

// Backend is the single dependency the CI service needs from the server.
type Backend interface {
	// Forge.
	GitHubApp() GitHubAppClient
	ForgeForInfo(ctx context.Context, info *RepoInfo) forge.Forge

	// Tasks.
	CreateTask(ctx context.Context, req bot.TaskRequest) (string, error)
	GetRunner(relPath string) (*task.Runner, bool)
	SetTaskMonitorBranch(entry TaskEntry, branch string)

	// Repos.
	RepoInfoFor(relPath string) RepoInfo
	ListActiveRepos() []RepoInfo
	SetRepoCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool

	// Notifications.
	NotifyTaskChange()
	EmitWarning(msg string)

	// Preferences.
	Prefs() *preferences.Store
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
