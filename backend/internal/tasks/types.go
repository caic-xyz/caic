// Exported parameter and result types for the Manager API.

package tasks

import (
	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/harness"
	"github.com/caic-xyz/caic/backend/internal/repowork"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/task"
)

// CreateParams bundles the HTTP path's task-creation parameters.
// The caller (Server) reads docker image, GitHub token access, and per-repo
// prefs from the user's preferences store before calling Create.
type CreateParams struct {
	OwnerID           string // empty in no-auth mode
	Prompt            agent.Prompt
	Repos             []CreateRepo // first entry is primary; empty = no-repo
	Harness           harness.Name
	Model             string // "" = harness default
	Effort            string // thinking effort; empty = default
	Tailscale         bool
	USB               bool
	Display           bool
	Sudo              bool
	GitHubToken       bool                 // inject GitHub token into the runtime environment
	BaseImage         string               // resolved base image
	ContainerPlatform string               // resolved container platform; empty means use host default
	MaxCPUs           int                  // max CPU cores; 0 means use the default
	CacheMounts       []runtime.CacheMount // resolved build cache mounts
	Mounts            []runtime.Mount      // resolved runtime bind mounts

	// ResolvedGitHubToken is the actual token string, resolved by the caller in
	// the request ctx; passed to workspace.Start. The caller resolves it (preferring
	// the logged-in user's OAuth token) because the Manager's serverCtx carries
	// no user identity.
	ResolvedGitHubToken string

	// Bot-task fields. The HTTP path leaves these zero (no-op); the bot sets them
	// so ListPendingBotTasks and commenter resolution work. When ForgeOwner is
	// non-empty, Create calls t.SetPR(ForgeOwner, ForgeRepo, 0).
	ForgeIssue int
	ForgeOwner string
	ForgeRepo  string
}

// CreateRepo describes a repo to mount in the new task.
type CreateRepo struct {
	Name       string // relative path
	BaseBranch string // empty = workspace default
}

// ForkParams bundles fork task creation parameters.
//
// The GitHubToken, Tailscale, USB, Display, and Sudo fields are resolved
// booleans: the HTTP handler dereferences the request's *bool overrides
// (falling back to the source task's value when nil) before calling Fork.
type ForkParams struct {
	OwnerID     string
	Prompt      agent.Prompt
	Harness     harness.Name // empty = use source's harness
	Model       string       // empty = use source's model
	Effort      string
	ExtraRepos  []ForkRepo
	GitHubToken bool // resolved override (handler derefs *bool, defaults to source)
	Tailscale   bool // resolved override (handler derefs *bool, defaults to source)
	USB         bool // resolved override
	Display     bool // resolved override
	Sudo        bool // resolved override

	// ResolvedGitHubToken is the actual token string, resolved by the caller in
	// the request ctx; passed to workspace.ForkTask. The caller resolves it
	// (preferring the logged-in user's OAuth token) because the Manager's
	// serverCtx carries no user identity.
	ResolvedGitHubToken string
}

// ForkRepo describes an extra repo to add to the fork.
type ForkRepo struct {
	Name       string
	BaseBranch string
}

// AdoptRepo describes a repo known to the manager for runtime instance adoption.
type AdoptRepo struct {
	RelPath    string
	AbsPath    string
	ForgeKind  string
	ForgeOwner string
	ForgeRepo  string
}

// AdoptedTask holds the result of adopting a single runtime instance.
type AdoptedTask struct {
	Entry          *Entry
	Task           *task.Task
	RelPath        string
	ForgeKind      string
	ForgeOwner     string
	ForgeRepo      string
	Branch         string
	FoundPRFromLog bool
}

// BotPendingTask is returned by ListPendingBotTasks.
type BotPendingTask struct {
	TaskID      string
	ForgeOwner  string
	ForgeRepo   string
	IssueNumber int
}

// SyncTarget specifies where to push changes.
type SyncTarget string

const (
	// SyncTargetOrigin pushes to the task's own branch on origin.
	SyncTargetOrigin SyncTarget = "origin"
	// SyncTargetDefault pushes to the repo's default branch.
	SyncTargetDefault SyncTarget = "default"
)

// SyncResult holds the outcome of a Sync operation.
type SyncResult struct {
	Status       string // "synced", "empty", "blocked"
	Branch       string
	DiffStat     agent.DiffStat
	SafetyIssues []repowork.SafetyIssue
}
