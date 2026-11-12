// Package mcpschema owns MCP contracts not published through the caic SDK.
// SDK-visible task-detail DTOs remain in server/api/v1.

package mcpschema

import (
	"encoding/json"
	"errors"
	"strconv"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

// TaskReference accepts a JSON task number or a string task reference.
type TaskReference string

// UnmarshalJSON preserves numeric task numbers and string IDs without resolving them.
func (id *TaskReference) UnmarshalJSON(data []byte) error {
	var number int
	if err := json.Unmarshal(data, &number); err == nil {
		*id = TaskReference(strconv.Itoa(number))
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("task must be a task number or stable task ID")
	}
	*id = TaskReference(raw)
	return nil
}

// ServiceNotification is one alert in the bare gomode://notifications array.
type ServiceNotification struct {
	v1.ServiceActiveTask

	ID        string    `json:"id"`
	ItemID    string    `json:"itemId"`
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// TaskListArgs selects a bounded tasks_list page.
type TaskListArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema_description:"Opaque cursor returned by the previous page"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"minimum=1,maximum=50"                                    jsonschema_description:"Maximum tasks to return; defaults to 20, cannot exceed 50, and a response-size limit may shorten the page"`
}

// TaskSummary is one bounded tasks_list entry.
type TaskSummary struct {
	v1.ServiceActive

	HasPendingInput bool         `json:"hasPendingInput" jsonschema_description:"Whether a pending prompt record exists; task_get_detail shows its text and delivery status"`
	TaskNumber      int          `json:"taskNumber"              jsonschema_description:"Current session task number used by other task tools"`
	TaskID          string       `json:"taskID"                  jsonschema_description:"Stable task ID"`
	Title           string       `json:"title"                   jsonschema_description:"Task title"`
	State           v1.TaskState `json:"state"                   jsonschema_description:"Current task lifecycle state"`
	Harness         v1.Harness   `json:"harness"                 jsonschema_description:"Coding-agent harness"`
	Model           string       `json:"model,omitempty"         jsonschema_description:"Configured model, or absent for the harness default"`
	Effort          string       `json:"effort,omitempty"        jsonschema_description:"Configured reasoning effort, or absent for the harness default"`
	RuntimeName     string       `json:"runtimeName,omitempty"   jsonschema_description:"Runtime backend name"`
	StatusMessage   string       `json:"statusMessage,omitempty" jsonschema_description:"Bounded result or failure context for terminal tasks"`
	ChangedFiles    int          `json:"changedFiles"            jsonschema_description:"Number of changed files"`
	Additions       int          `json:"additions"               jsonschema_description:"Added lines"`
	Deletions       int          `json:"deletions"               jsonschema_description:"Deleted lines"`
	ForgePR         int          `json:"forgePR,omitempty"       jsonschema_description:"Pull request number"`
	CIStatus        v1.CIStatus  `json:"ciStatus,omitempty"      jsonschema_description:"Continuous integration status"`
	DurationSeconds float64      `json:"durationSeconds"         jsonschema_description:"Task duration in seconds"`
	CostUSD         float64      `json:"costUSD"                 jsonschema_description:"Task cost in US dollars"`
	CostKnown       bool         `json:"costKnown"               jsonschema_description:"Whether any cost is known; costUSD may be a subtotal"`
	CostComplete    bool         `json:"costComplete"            jsonschema_description:"Whether costUSD covers all observed billed usage"`
}

// TaskListOutput carries a tasks_list page and its continuation cursor.
type TaskListOutput struct {
	Tasks      []TaskSummary `json:"tasks"                jsonschema_description:"Tasks in this page"`
	NextCursor string        `json:"nextCursor,omitempty" jsonschema_description:"Opaque cursor for the next page"`
}

// RepoListArgs selects a bounded repos_list page.
type RepoListArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema_description:"Opaque cursor returned by the previous page"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"minimum=1,maximum=200"                                   jsonschema_description:"Maximum repositories to return; defaults to 200, cannot exceed 200, and a response-size limit may shorten the page"`
}

// RepoListOutput carries repositories, continuation, and presentation truncation.
type RepoListOutput struct {
	Repositories    []RepoSummary `json:"repositories"              jsonschema_description:"Repositories in this page"`
	NextCursor      string        `json:"nextCursor,omitempty"      jsonschema_description:"Opaque cursor for the next page"`
	FieldsTruncated bool          `json:"fieldsTruncated,omitempty" jsonschema_description:"Whether presentation-only repository fields were shortened to the MCP response limit"`
}

// HarnessModel advertises a model and its supported reasoning efforts.
type HarnessModel struct {
	ID            string   `json:"id"                      jsonschema_description:"Model identifier accepted by the harness"`
	EffortOptions []string `json:"effortOptions,omitempty" jsonschema_description:"Reasoning effort values the model accepts; absent means the harness default only"`
}

// HarnessInfo advertises one harness and its models.
type HarnessInfo struct {
	Name   v1.Harness     `json:"name"   jsonschema_description:"Harness identifier accepted by task_create"`
	Models []HarnessModel `json:"models" jsonschema_description:"Models this harness accepts, in harness order"`
}

// HarnessListOutput is the harnesses_list response.
type HarnessListOutput struct {
	Harnesses []HarnessInfo `json:"harnesses" jsonschema_description:"Available harnesses and their supported models"`
}

// TaskCreatedOutput identifies the task accepted by task_create.
type TaskCreatedOutput struct {
	Result     string `json:"result"               jsonschema_description:"Human-readable task creation result"`
	TaskNumber int    `json:"taskNumber,omitempty" jsonschema_description:"Current session task number"`
	TaskID     string `json:"taskID"               jsonschema_description:"Stable task ID"`
}

// TaskCreateArgs contains the user-controlled task_create options.
type TaskCreateArgs struct {
	Prompt      string   `json:"prompt"                jsonschema_description:"The task description/prompt for the coding agent"`
	Repos       []string `json:"repos,omitempty"       jsonschema_description:"Repositories to work in (zero or more); omit or pass an empty list for a task without repositories"`
	Harness     string   `json:"harness,omitempty"     jsonschema_description:"Agent harness to use (optional)"`
	Model       string   `json:"model,omitempty"       jsonschema_description:"Model to use (optional)"`
	Effort      string   `json:"effort,omitempty"      jsonschema_description:"Thinking effort to use (optional)"`
	RuntimeName string   `json:"runtimeName,omitempty" jsonschema_description:"Runtime backend name to use, such as docker or podman (optional)"`
	Display     bool     `json:"display,omitempty"     jsonschema_description:"Enable virtual display (VNC) for this task"`
	Tailscale   bool     `json:"tailscale,omitempty"   jsonschema_description:"Enable Tailscale networking for this task"`
	USB         bool     `json:"usb,omitempty"         jsonschema_description:"Enable USB passthrough for this task"`
	Sudo        bool     `json:"sudo,omitempty"        jsonschema_description:"Enable root access via sudo with a random password"`
	GitHubToken bool     `json:"gitHubToken,omitempty" jsonschema_description:"Enable GitHub token injection for this task"`
}

// InputDeliveryMode selects how task_send_message delivers a prompt.
type InputDeliveryMode string

// Input delivery modes preserve steering, appending, and replacement semantics.
const (
	InputAppend  InputDeliveryMode = "append"
	InputReplace InputDeliveryMode = "replace"
	InputSteer   InputDeliveryMode = "steer"
)

// TaskSendMessageArgs contains a task reference, prompt, and delivery choice.
type TaskSendMessageArgs struct {
	TaskNumber   TaskReference     `json:"task_number" jsonschema:"oneof_type=integer;string"                    jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
	Message      string            `json:"message"     jsonschema_description:"The message to send to the agent"`
	DeliveryMode InputDeliveryMode `json:"delivery_mode,omitempty" jsonschema:"enum=append,enum=replace,enum=steer" jsonschema_description:"steer (default) delivers immediately without clearing pending input; append preserves and extends pending input; replace cancels pending input by ID, including attachments, then queues new text. Cancellation and sending are separate operations: if sending fails, the old prompt remains canceled; concurrent queued messages may append. Both queued modes send immediately when waiting with no pending prompt. Changes are rejected once delivery starts. Use tasks_list or task_get_detail to inspect pending input"`
}

// TaskAnswerQuestionArgs contains the answer for a task question.
type TaskAnswerQuestionArgs struct {
	TaskNumber TaskReference `json:"task_number" jsonschema:"oneof_type=integer;string"                      jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
	Answer     string        `json:"answer"      jsonschema_description:"The answer to the agent's question"`
}

// TaskPushBranchArgs selects a task branch synchronization destination.
type TaskPushBranchArgs struct {
	TaskNumber TaskReference `json:"task_number"      jsonschema:"oneof_type=integer;string"                      jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
	Force      bool          `json:"force,omitempty"  jsonschema_description:"Force sync even with safety issues"`
	Target     string        `json:"target,omitempty" jsonschema:"enum=branch,enum=default,enum=main,enum=master" jsonschema_description:"Where to push: branch (default) or main"`
}

// TaskForkOutput identifies the task accepted by task_fork.
type TaskForkOutput struct {
	Result string `json:"result" jsonschema_description:"Human-readable fork result"`
	TaskID string `json:"taskID" jsonschema_description:"Stable task ID for the forked task"`
}

// TaskForkArgs selects a task snapshot and its follow-up prompt.
type TaskForkArgs struct {
	TaskNumber TaskReference `json:"task_number"       jsonschema:"oneof_type=integer;string"                                                jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID to fork"`
	Prompt     string        `json:"prompt"            jsonschema_description:"The initial prompt for the forked task"`
	Harness    string        `json:"harness,omitempty" jsonschema_description:"Override harness (optional, inherits from source if omitted)"`
	Model      string        `json:"model,omitempty"   jsonschema_description:"Model override (optional, inherits from source if omitted)"`
}

// CloneRepoArgs identifies a remote repository and optional local path.
type CloneRepoArgs struct {
	URL  string `json:"url"            jsonschema_description:"The git repository URL to clone"`
	Path string `json:"path,omitempty" jsonschema_description:"Local directory name (optional, derived from URL if omitted)"`
}

// TaskFixPRArgs identifies the task whose pull request needs repair.
type TaskFixPRArgs struct {
	TaskNumber TaskReference `json:"task_number" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number or stable task ID whose PR CI should be fixed"`
}

// BotFixCIArgs identifies the repository whose CI needs repair.
type BotFixCIArgs struct {
	Repo string `json:"repo" jsonschema_description:"Repository to fix CI for"`
}

// TaskNumberArgs identifies a task for tools that need only a reference.
type TaskNumberArgs struct {
	TaskNumber TaskReference `json:"task_number" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
}

// TaskGetDetailArgs selects a task and optional diffstat details.
type TaskGetDetailArgs struct {
	Task            TaskReference `json:"task" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number from tasks_list (for example 1 or #1) or stable task ID; use your stable task ID to inspect your own workspace"`
	IncludeDiffstat bool          `json:"include_diffstat,omitempty" jsonschema_description:"Include committed and uncommitted file diffstats, branch, upstream, and ahead/behind counts"`
}

// RepoSummary is the compact repository view shared by tools and resources.
type RepoSummary struct {
	Path       string        `json:"path"                jsonschema_description:"Repository path used by task_create"`
	BaseBranch v1.BranchInfo `json:"baseBranch"          jsonschema_description:"Configured base branch"`
	Forge      v1.Forge      `json:"forge,omitempty"     jsonschema_description:"Repository forge provider"`
	RemoteURL  string        `json:"remoteURL,omitempty" jsonschema_description:"HTTPS remote URL"`
	CI         v1.CIStatus   `json:"ci,omitempty"        jsonschema_description:"Aggregate continuous integration status"`
}

// TaskCursor binds the last emitted task ID to a task-ordering revision.
type TaskCursor struct {
	Revision string `json:"revision"`
	TaskID   string `json:"taskID"`
}

// ServiceItem is caic public task context; MCP instructions own its interpretation.
// References identify positions in the original task snapshot, independently
// of the priority order in which items are delivered.
type ServiceItem struct {
	CIStatus       v1.CIStatus `json:"ciStatus,omitempty"`
	ForgePR        int         `json:"forgePR,omitempty"`
	ID             string      `json:"id"`
	Reference      string      `json:"reference,omitempty"`
	Title          string      `json:"title"`
	State          string      `json:"state,omitempty"`
	NeedsAttention bool        `json:"needsAttention"`
}

// ServiceItemsOutput carries a bounded baseline and guidance
// for fetching tasks omitted by either the item-count or encoded-byte limit.
type ServiceItemsOutput struct {
	Items         []ServiceItem `json:"items"`
	MoreItemsHint string        `json:"moreItemsHint"`
	OmittedCount  int           `json:"omittedCount,omitempty"`
}
