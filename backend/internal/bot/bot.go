// Package bot implements forge event-driven task automation: prompt
// construction, task dispatch via the API, and result comment posting.
//
// It is essentially a state machine.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

// RepoInfo is the forge identity of a managed repository, returned by Client.ResolveRepo.
type RepoInfo struct {
	RelPath    string
	ForgeKind  forge.Kind
	ForgeOwner string
	ForgeRepo  string
}

// IssueEvent describes an issue-opened event from any forge.
type IssueEvent struct {
	ForgeFullName string // "owner/repo"
	Number        int
	Title         string
	Body          string
	HTMLURL       string
	Labels        []string
}

// PREvent describes a pull/merge request opened event from any forge.
type PREvent struct {
	ForgeFullName string // "owner/repo"
	Number        int
	Title         string
	Body          string
	HTMLURL       string
	HeadRef       string
	BaseRef       string
}

// CommentEvent describes a comment mentioning @caic on an issue or PR.
type CommentEvent struct {
	ForgeFullName string // "owner/repo"
	IssueNumber   int
	IssueTitle    string
	CommentBody   string
	CommentURL    string
}

// TaskRequest holds the parameters for creating a task via the API.
type TaskRequest struct {
	Repo        string // repo relative path
	Prompt      string
	OwnerID     string
	IssueNumber int // originating issue/PR number for completion comment callbacks
}

// PendingBotTask describes a non-terminal task with a forge issue callback.
// Used by ResumePendingComments to re-attach watchers after restart.
type PendingBotTask struct {
	TaskID      string
	ForgeOwner  string
	ForgeRepo   string
	IssueNumber int
}

// Client is the bot's interface to the task management system.
// It mirrors the public API contract without importing server types.
type Client interface {
	// ResolveRepo maps a forge full name ("owner/repo") to repo info.
	// Returns nil if the forge name does not match any managed repo.
	ResolveRepo(forgeFullName string) *RepoInfo
	// CreateTask creates a new task and returns its ID.
	CreateTask(ctx context.Context, req TaskRequest) (string, error)
	// WatchTaskCompletion blocks until the task reaches a terminal state,
	// then returns the final state name and agent result text.
	WatchTaskCompletion(ctx context.Context, taskID string) (state string, result string, err error)
	// ListPendingBotTasks returns non-terminal tasks that have a ForgeIssue set.
	ListPendingBotTasks() []PendingBotTask
	// ResolveCommenter returns a forge.Commenter for the given forge owner, or nil.
	ResolveCommenter(ctx context.Context, owner string) forge.Commenter
}

// Bot handles forge event-driven task automation.
type Bot struct {
	// Immutable.
	log    *slog.Logger
	ctx    context.Context // server-lifetime context for background goroutines
	client Client
}

// New returns a Bot backed by the given client.
// ctx must be the server-lifetime context (outlives individual requests).
func New(ctx context.Context, c Client) *Bot {
	return &Bot{log: slog.Default().With(slog.String("cmp", "bot")), ctx: ctx, client: c}
}

// ResumePendingComments re-attaches watchAndComment goroutines for tasks
// that were created by the bot before a server restart. Call after adoption
// is complete and the task map is populated.
func (b *Bot) ResumePendingComments() {
	pending := b.client.ListPendingBotTasks()
	for _, pt := range pending {
		commenter := b.client.ResolveCommenter(b.ctx, pt.ForgeOwner)
		if commenter == nil {
			b.log.WarnContext(b.ctx, "no commenter for owner on resume", "owner", pt.ForgeOwner, "task", pt.TaskID)
			continue
		}
		b.log.InfoContext(b.ctx, "resuming comment watcher", "task", pt.TaskID, "issue", pt.IssueNumber)
		go b.watchAndComment(pt.TaskID, commenter, pt.ForgeOwner, pt.ForgeRepo, pt.IssueNumber)
	}
}

// OnIssueOpened creates a task when an issue with the "caic" label is opened.
// commenter is used to post a completion comment; may be nil.
func (b *Bot) OnIssueOpened(ctx context.Context, ev *IssueEvent, commenter forge.Commenter) {
	hasCaicLabel := slices.Contains(ev.Labels, "caic")
	if !hasCaicLabel {
		return
	}
	repo := b.client.ResolveRepo(ev.ForgeFullName)
	if repo == nil {
		b.log.WarnContext(ctx, "no repo for forge", "full_name", ev.ForgeFullName)
		return
	}
	prompt := fmt.Sprintf("Fix the following GitHub issue:\n\nTitle: %s\nURL: %s\n\n%s",
		ev.Title, ev.HTMLURL, ev.Body)
	b.dispatch(ctx, repo, prompt, commenter, ev.Number, "")
}

// OnPROpened creates a task when a pull/merge request is opened or reopened.
func (b *Bot) OnPROpened(ctx context.Context, ev *PREvent) {
	repo := b.client.ResolveRepo(ev.ForgeFullName)
	if repo == nil {
		b.log.WarnContext(ctx, "no repo for forge", "full_name", ev.ForgeFullName)
		return
	}
	prompt := fmt.Sprintf("Review and fix the following pull request:\n\nTitle: %s\nBranch: %s → %s\nURL: %s\n\n%s",
		ev.Title, ev.HeadRef, ev.BaseRef, ev.HTMLURL, ev.Body)
	b.dispatch(ctx, repo, prompt, nil, 0, "")
}

// OnIssueComment creates a task when @caic is mentioned in a comment.
// commenter is used to post a completion comment; may be nil.
func (b *Bot) OnIssueComment(ctx context.Context, ev CommentEvent, commenter forge.Commenter) {
	if !strings.Contains(ev.CommentBody, "@caic") {
		return
	}
	repo := b.client.ResolveRepo(ev.ForgeFullName)
	if repo == nil {
		b.log.WarnContext(ctx, "no repo for forge", "full_name", ev.ForgeFullName)
		return
	}
	prompt := fmt.Sprintf("A user mentioned @caic in a comment on issue #%d:\n\nIssue: %s\nComment URL: %s\n\n%s",
		ev.IssueNumber, ev.IssueTitle, ev.CommentURL, ev.CommentBody)
	b.dispatch(ctx, repo, prompt, commenter, ev.IssueNumber, "")
}

// postTaskComment posts a completion comment on the originating issue or PR.
func (b *Bot) postTaskComment(ctx context.Context, commenter forge.Commenter, owner, repo string, issueNumber int, state, agentResult string) {
	var body string
	if agentResult != "" {
		body = fmt.Sprintf("caic task completed (state: %s)\n\n%s", state, agentResult)
	} else {
		body = fmt.Sprintf("caic task completed (state: %s)", state)
	}
	if err := commenter.PostComment(ctx, owner, repo, issueNumber, body); err != nil {
		b.log.WarnContext(ctx, "post comment failed", "owner", owner, "repo", repo, "issue", issueNumber, "err", err)
	}
}

func (b *Bot) dispatch(ctx context.Context, repo *RepoInfo, prompt string, commenter forge.Commenter, issueNumber int, ownerID string) {
	taskID, err := b.client.CreateTask(ctx, TaskRequest{
		Repo:        repo.RelPath,
		Prompt:      prompt,
		OwnerID:     ownerID,
		IssueNumber: issueNumber,
	})
	if err != nil {
		b.log.WarnContext(ctx, "create task failed", "repo", repo.RelPath, "err", err)
		return
	}
	b.log.InfoContext(ctx, "task created", "id", taskID, "repo", repo.RelPath)
	if commenter != nil && issueNumber > 0 {
		go b.watchAndComment(taskID, commenter, repo.ForgeOwner, repo.ForgeRepo, issueNumber)
	}
}

// watchAndComment blocks until the task completes, then posts a comment.
func (b *Bot) watchAndComment(taskID string, commenter forge.Commenter, owner, repo string, issueNumber int) {
	state, result, err := b.client.WatchTaskCompletion(b.ctx, taskID)
	if err != nil {
		b.log.WarnContext(b.ctx, "watch task failed", "id", taskID, "err", err)
		return
	}
	b.postTaskComment(b.ctx, commenter, owner, repo, issueNumber, state, result)
}
