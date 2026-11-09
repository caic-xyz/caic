// CI task contracts, categorized polling warnings, and PR creation flow.

package ci

import (
	"context"

	"github.com/maruel/ksid"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/forge/forgecache"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
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

// WarningCategory identifies a CI failure independently of its display text.
type WarningCategory string

// WarningCategoryCIPollFailed identifies a failed CI polling round.
const WarningCategoryCIPollFailed WarningCategory = "ci_poll_failed"

// WarningDetail describes a failed CI operation on a repository.
type WarningDetail struct {
	Repo  string
	Error string
}

// TaskEntry is an abstract task handle for CI monitoring.
type TaskEntry interface {
	Task() *taskrun.Task
	// AutomationContext admits CI work on the task and returns a lease: a
	// context and its release function. It fails when the task owner's
	// account is disabled, or when the task is stopping, stopped, or terminal.
	// Disabling the owner cancels the context, then waits for release before
	// the account counts as suspended. Call release when the work ends,
	// including after a cancellation. Work that outlives its caller, such as
	// a worker goroutine, takes its own lease.
	AutomationContext(ctx context.Context) (context.Context, context.CancelFunc, error)
	SendInput(ctx context.Context, prompt agent.Prompt) error
	RecordHost(ctx context.Context, m agent.Message) error
	RecordRepositorySummary(ctx context.Context, read *repo.GitSnapshot) (bool, error)
	MonitorBranch() string
	SetMonitorBranch(branch string)
	Result() *task.Result
}

// Backend is the single dependency the CI service needs from the server.
type Backend interface {
	// Forge.
	GitHubApp() GitHubAppClient
	ForgeForInfo(ctx context.Context, info *RepoInfo) forge.Forge

	// Tasks.
	CreateTask(ctx context.Context, req task.CreateRequest) (ksid.ID, error)
	// SyncTask pushes the task's commits to its own branch as a user sync does,
	// and reports whether the push was blocked by safety issues. It fails while
	// another sync of the task runs.
	SyncTask(ctx context.Context, entry TaskEntry) (blocked bool, err error)
	SetTaskMonitorBranch(entry TaskEntry, branch string)

	// Repos.
	RepoInfoFor(relPath string) RepoInfo
	ListActiveRepos() []RepoInfo
	SetRepoCIStatusIfChanged(relPath, sha string, result forgecache.Result) bool

	// Notifications.
	NotifyTaskChange(id ksid.ID)
	UpdateWarning(ctx context.Context, category WarningCategory, message string, details []WarningDetail) error
	ResolveWarning(ctx context.Context, category WarningCategory) error

	// Preferences.
	Prefs() *preferences.Store
}

// lastResultText returns the Result field of the most recent ResultMessage in
// the task's message history. Used as the squash-merge commit body.
func lastResultText(t *taskrun.Task) string {
	for msg := range t.BackwardMessages() {
		if rm, ok := msg.(*agent.ResultMessage); ok {
			return rm.Result
		}
	}
	return ""
}
