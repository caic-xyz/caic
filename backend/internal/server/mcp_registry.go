// MCP tool registry, schemas, resource catalog, subscription invalidation, and keepalives.

package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/caic-xyz/md/git"
	"github.com/invopop/jsonschema"
	"github.com/maruel/ksid"
	orderedmap "github.com/pb33f/ordered-map/v2"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	repodomain "github.com/caic-xyz/caic/backend/internal/repo"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	taskpkg "github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	providerusage "github.com/caic-xyz/caic/backend/internal/usage"
	"github.com/caic-xyz/caic/metrics"
	"github.com/caic-xyz/caic/oauth"
)

const caicVoiceSystemInstruction = `You are a voice assistant for caic, a system for managing AI coding agents.

## What caic does
caic runs coding agents (Claude Code, Codex, etc) inside isolated containers on a remote server. Each agent works autonomously on a git branch, writing code, running tests, and committing changes. The user is a software engineer who supervises multiple agents concurrently — often while away from the screen — and controls them by voice.

## Task lifecycle
A task has a prompt (what to build), a repo, a branch, and a state:
- pending: task is queued, waiting to start
- branching: creating git branch
- provisioning: starting container
- starting: launching agent session
- running: agent is actively working
- waiting: agent completed a turn, awaiting user input
- asking: agent asked a question, needs the user to answer
- has_plan: agent produced a plan, awaiting approval
- pulling: pulling changes from container
- pushing: pushing changes to remote
- stopping: graceful stop in progress
- stopped: container stopped and can be revived
- purging: cleanup in progress, container being deleted
- purged: container deleted; result contains the outcome
- crashed: agent session crashed; container is preserved and can be revived
- failed: unrecoverable failure; error has the reason

## Context you have
If the client provides a bounded current-task snapshot at session start, use it to answer questions about listed task status without calling tasks_list first. When that snapshot says tasks were omitted, call tasks_list and follow nextCursor until it is absent. Without a snapshot, call tasks_list. Call task_get_detail when the user asks for specifics (recent events, diffs).

## Behavior guidelines
- Reply only to the current request, in one or two short sentences unless the user explicitly asks for more detail. Speak quickly and omit background, explanations, and summaries that were not requested.
- Never ask a follow-up or confirmation, including "would you like me to…" or "should I also…". Ask one clarifying question only when missing information makes the request impossible or safety-critical; after it is answered, perform the request without asking again.
- Do not volunteer ideas, next steps, related actions, or offers. Do not comment on tasks or invoke tools until the user asks.
- Notify the user only when an agent enters the waiting or asking state. Do not notify on any other state transition.
- Be concise. The user is often away from the screen.
- Summarize task status: state and what the agent is doing. Only mention elapsed time or cost when the user specifically asks.
- When an agent is asking, read the question and options clearly, wait for the verbal answer, then call task_answer_question.
- When creating a task, omit harness, model, and effort unless the user explicitly asks for an override. caic fills an omitted harness from saved preferences; omitted model and effort use the selected harness defaults. Never pair one harness's model or effort with another harness. Confirm repo and prompt before creating.
- Refer to tasks by title.
- Free tools: agent_last_message, repos_list, tasks_list, task_get_detail, get_usage. Call them whenever useful without asking.
- When the user asks for a status update, call agent_last_message for each waiting/asking task to get latest output.
- For safety issues during sync, describe each issue and ask whether to force.`

const subscriptionHeartbeatInterval = 15 * time.Second

const (
	// mcpListJSONMaxBytes bounds each compact structured list result. When a
	// count-based page would exceed it, the result ends at the last item that
	// fits and its cursor resumes at the following item.
	mcpListJSONMaxBytes     = 256 << 10
	mcpRepoPageSizeDefault  = 200
	mcpRepoPageSizeMax      = 200
	mcpResourceJSONMaxBytes = 256 << 10
	mcpResourcePageSize     = 100
	mcpServiceItemMaxCount  = 50
	mcpTextOutputMaxBytes   = 32 << 10
	mcpTaskPageSizeDefault  = 20
	mcpTaskPageSizeMax      = 50
	mcpTaskCursorMaxBytes   = 1024
	maxMCPSafetyIssues      = 50
	maxMCPRepoField         = 512
	maxTaskDetailPaths      = 50
	maxTaskSummaryEffort    = 64
	maxTaskSummaryMessage   = 512
	maxTaskSummaryModel     = 256
	maxTaskSummaryRuntime   = 128
	maxMCPTaskTitle         = 512
)

const mcpTruncatedMetaKey = "xyz.caic/truncated"

var (
	errMCPRepositoryListChanged     = errors.New("repository list changed while paging")
	errMCPRepositoryPathTooLong     = errors.New("repository path exceeds the 512-byte MCP limit; shorten the configured path")
	errMCPRepositoryPathInvalidUTF8 = errors.New("repository path is not valid UTF-8; rename the configured path")
)

type mcpRegistry struct {
	// All fields are required by the full MCP registry.
	serverConfig  *serverHandlers
	taskSvc       *taskService
	ci            *ciHandlers
	usage         *usageHandlers
	notifications *notificationFeed
	audit         *auditStore
	metrics       metrics.Recorder
}

func (m *mcpRegistry) Instructions(ctx context.Context) (string, error) {
	out := caicVoiceSystemInstruction
	if defaults := m.voiceSessionDefaults(ctx); defaults != "" {
		out += "\n\n" + defaults
	}
	return out, nil
}

func (m *mcpRegistry) Tools(ctx context.Context) ([]mcp.ToolDescriptor, error) {
	specs := m.specs()
	tools := make([]mcp.ToolDescriptor, 0, len(specs))
	for _, s := range specs {
		if _, ok := authorizeToolScope(ctx, s.Name); !ok {
			continue
		}
		inputSchema := s.InputSchema
		description := s.Description
		if s.Name == "task_create" {
			if _, ok := taskMCPTaskID(ctx); ok {
				inputSchema = buildDelegatedTaskCreateSchema()
				description = "Create a child task from this task's snapshot. Provide only the child prompt; caic derives the repository, runtime, and parent from this task."
			}
		}
		if s.Name == "task_fork" {
			if _, ok := taskMCPTaskID(ctx); ok {
				inputSchema = buildDelegatedTaskForkSchema()
				description = "Fork this task or one of its child tasks. Use task_number 0 for this task, or a child task number returned by tasks_list. Provide only the fork prompt."
			}
		}
		tools = append(tools, mcp.ToolDescriptor{Name: s.Name, Title: s.Title, Description: description, InputSchema: inputSchema, OutputSchema: s.OutputSchema, Annotations: s.Annotations})
	}
	return tools, nil
}

func (m *mcpRegistry) CallTool(ctx context.Context, name string, argsJSON json.RawMessage) (mcp.RawToolResult, error) {
	for _, s := range m.specs() {
		if s.Name != name {
			continue
		}
		if authResult, ok := m.authorizeTool(ctx, name); !ok {
			m.audit.record(ctx, &auditEvent{Operation: "tools/call", Name: name, Args: auditArgsSummary(argsJSON), Decision: authResult})
			return mcp.RawToolResult{Meta: mcp.MetaObject{
				"mcp/www_authenticate":   []string{mcpScopeChallenge(requiredScopeForTool(name))},
				mcp.ToolErrorCodeMetaKey: string(api.CodeUnauthorized),
			}, Structured: mcp.ErrorOutput{Error: authResult}, IsError: true}, nil
		}
		start := time.Now()
		res, err := s.Handler(ctx, argsJSON)
		// TODO(observability): Record pre-wire logical result size and truncation,
		// keyed by tool name and outcome, at this registry boundary. Final encoded
		// response bytes belong to the MCP transport writer.
		status := "ok"
		outcome := metrics.OutcomeOK
		switch {
		case err != nil:
			status, outcome = "error", metrics.OutcomeError
		case res.IsError:
			status, outcome = "tool_error", metrics.OutcomeError
		}
		m.metrics.Record(ctx, "mcp.tool."+name, outcome, metrics.Duration(time.Since(start)))
		m.audit.record(ctx, &auditEvent{Operation: "tools/call", Name: name, Args: auditArgsSummary(argsJSON), Decision: "allow", Status: status})
		return res, err
	}
	return mcp.RawToolResult{}, mcp.ErrInvalidParams("unknown tool: %s", name)
}

func (m *mcpRegistry) ListResources(ctx context.Context, cursor string) (mcp.ResourcesListResult, error) {
	keys, err := m.resourceKeys(ctx)
	if err != nil {
		return mcp.ResourcesListResult{}, err
	}
	page, next, err := paginateMCPResourceKeys(keys, cursor)
	if err != nil {
		return mcp.ResourcesListResult{}, mcp.ErrInvalidParams("invalid cursor: %w", err)
	}
	resources := make([]mcp.ResourceDescriptor, 0, len(page))
	for resource, projectionErr := range m.resourceDescriptors(ctx, page) {
		if projectionErr != nil {
			return mcp.ResourcesListResult{}, projectionErr
		}
		resources = append(resources, resource)
	}
	return mcp.ResourcesListResult{ResultType: mcp.ResultTypeComplete, NextCursor: next, Resources: resources, TTLMS: mcp.DefaultTTLMS, CacheScope: mcp.CacheScopePrivate}, nil
}

func (m *mcpRegistry) Resources(ctx context.Context) iter.Seq2[mcp.ResourceDescriptor, error] {
	keys, err := m.resourceKeys(ctx)
	if err != nil {
		return func(yield func(mcp.ResourceDescriptor, error) bool) {
			yield(mcp.ResourceDescriptor{}, err)
		}
	}
	return m.resourceDescriptors(ctx, keys)
}

func (m *mcpRegistry) ReadResource(ctx context.Context, uri string) (mcp.ResourcesReadResult, error) {
	if authResult, ok := m.authorizeResource(ctx, uri); !ok {
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: authResult})
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("%s", authResult)
	}
	switch {
	case uri == "caic://usage":
		usage := m.usage.buildResp(ctx)
		return m.resourceJSON(ctx, uri, usage)
	case uri == "gomode://items":
		taskList := m.taskSvc.taskListSnapshot(ctx)
		items, err := boundedServiceItems(taskList, mcpResourceJSONMaxBytes)
		if err != nil {
			return mcp.ResourcesReadResult{}, err
		}
		return m.resourceJSON(ctx, uri, items)
	case uri == "gomode://notifications":
		taskList := m.taskSvc.taskListSnapshot(ctx)
		notifications := m.notifications.notifications(ctx, taskList, m.usage.buildResp(ctx))
		return m.resourceJSON(ctx, uri, notifications)
	case strings.HasPrefix(uri, "caic://repos/"):
		name, err := url.PathUnescape(strings.TrimPrefix(uri, "caic://repos/"))
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("invalid repo uri: %w", err)
		}
		checkout, ok := m.serverConfig.checkouts.Checkout(name)
		if !ok {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("repo not found: %s", name)
		}
		repository, truncated, err := m.repositorySummary(checkout)
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("%s", err)
		}
		result, err := m.resourceJSON(ctx, uri, repository)
		if truncated {
			result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
		}
		return result, err
	case strings.HasPrefix(uri, "caic://tasks/"):
		rawID := strings.TrimPrefix(uri, "caic://tasks/")
		id, err := mcpTaskResourceID(uri)
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("task not found: %s", rawID)
		}
		entry, ok := m.visibleTaskEntry(ctx, id)
		if !ok {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("task not found: %s", rawID)
		}
		task, err := taskDTO(ctx, entry, m.taskSvc.taskMgr, m.taskSvc.checkouts, m.taskSvc.authStore)
		if err != nil {
			return mcp.ResourcesReadResult{}, err
		}
		return m.resourceJSON(ctx, uri, task)
	default:
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("unknown resource: %s", uri)
	}
}

func (m *mcpRegistry) SubscribeResourceUpdates(ctx context.Context, filter mcp.SubscriptionFilter) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	sources, err := m.subscriptionSources(ctx, filter)
	if err != nil {
		return nil, err
	}
	taskC := sources.taskC
	repoC := sources.repoC
	repoStatusC := sources.repoStatusC
	return func(yield func(mcp.ResourceUpdate, error) bool) {
		heartbeatTicker := time.NewTicker(subscriptionHeartbeatInterval)
		defer heartbeatTicker.Stop()
		var usageC <-chan time.Time
		if sources.usagePolling {
			usageTicker := time.NewTicker(providerusage.CacheTTL)
			defer usageTicker.Stop()
			usageC = usageTicker.C
		}
		pendingTaskUpdate := false
		for {
			if pendingTaskUpdate {
				pendingTaskUpdate = false
				if !yield(sources.taskUpdate(ctx, m), nil) {
					return
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-taskC:
				// Re-arm before yielding. yield may block while another task mutation
				// closes the replacement channel; retaining the versioned snapshot
				// makes that later mutation observable after the callback returns.
				previousVersion := sources.taskVersion
				sources.taskVersion, taskC = m.taskSvc.taskMgr.ChangeSnapshot()
				pendingTaskUpdate = sources.taskVersion > previousVersion+1
				if !yield(sources.taskUpdate(ctx, m), nil) {
					return
				}
			case <-repoC:
				if !yield(sources.repoUpdate(), nil) {
					return
				}
				repoC = m.serverConfig.checkouts.Changed()
			case <-repoStatusC:
				if !yield(sources.repoUpdate(), nil) {
					return
				}
				repoStatusC = m.serverConfig.repoStatus.Changed()
			case <-usageC:
				if !yield(sources.usageUpdate(), nil) {
					return
				}
			case <-heartbeatTicker.C:
				if !yield(mcp.ResourceUpdate{KeepAlive: true}, nil) {
					return
				}
			}
		}
	}, nil
}

// ForTask returns m scoped to id's server-owned task principal.
func (m *mcpRegistry) ForTask(id ksid.ID) mcp.Registry {
	return scopedMCPRegistry{Registry: m, principal: &mcpPrincipal{TaskID: id, Remote: true}}
}

func (m *mcpRegistry) voiceSessionDefaults(ctx context.Context) string {
	if !mcpHasScope(ctx, mcpScopeTasksRead) {
		return ""
	}
	parts := make([]string, 0, 2)
	prefs := m.serverConfig.prefs.Get(userIDFromCtx(ctx))
	if len(prefs.Repositories) > 0 {
		parts = append(parts, "[Default repo: "+prefs.Repositories[0].Path+"]")
	}
	if prefs.Harness != "" {
		parts = append(parts, "[Default harness: "+prefs.Harness+"]")
	}
	return strings.Join(parts, "\n")
}

// specs returns the caic MCP tool catalog.
//
// Voice tool-call mode: the Gemini Live adapter currently declares every tool
// as BLOCKING, because the provider-neutral ToolDeclaration has no per-tool
// execution mode and the gateway tool round trip is synchronous. Server-side
// handler durations are recorded as "mcp.tool.<name>" and shown in Settings,
// but they exclude the client round trip the model waits through. Once
// voice-path durations are measured, a tool whose call exceeds one second
// should become asynchronous (Gemini behavior NON_BLOCKING) so the assistant
// can keep talking while it runs. Switching an individual tool needs a mode
// hint carried from this catalog through ToolDeclaration to the adapter; see
// gomode/voicegateway/voicertc/AGENTS.md.
func (m *mcpRegistry) specs() []mcp.ToolSpec {
	createSpec := mcp.NewToolSpec("task_create", "Create task", "Create a new coding task. Confirm repo and prompt with the user before calling. Omit harness, model, and effort unless the user explicitly asks for an override; caic resolves an omitted harness from saved preferences and leaves omitted model/effort to harness defaults.", m.handleTaskCreate)
	createSpec.InputSchema = buildTaskCreateSchema()
	createSpec.Annotations = &mcp.ToolAnnotations{Title: "Create task", DestructiveHint: true, OpenWorldHint: false}

	forkSpec := mcp.NewToolSpec("task_fork", "Fork task", "Fork a running or waiting task, creating a snapshot of its container on a new branch. The prompt describes what the forked task should do. Optionally override the harness and model.", m.handleTaskFork)
	forkSpec.InputSchema = buildTaskForkSchema()
	forkSpec.Annotations = &mcp.ToolAnnotations{Title: "Fork task", DestructiveHint: true, OpenWorldHint: false}

	botFixCISpec := mcp.NewToolSpec("bot_fix_ci", "Fix repository CI", "Create a task to investigate and fix a failing CI on a repository's default branch.", m.handleBotFixCI)
	botFixCISpec.InputSchema = buildBotFixCISchema()
	botFixCISpec.Annotations = &mcp.ToolAnnotations{Title: "Fix repository CI", DestructiveHint: true, OpenWorldHint: false}

	return []mcp.ToolSpec{
		annotateTool(mcp.NewToolSpec("tasks_list", "List tasks", "List current coding tasks in stable active-first order. A response-size limit may return fewer tasks than limit; pass nextCursor as cursor until it is absent.", m.handleTasksList), mcp.ToolAnnotations{Title: "List tasks", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("repos_list", "List repositories", "List repositories in stable path order. Defaults to at most 200 repositories; a response-size limit may shorten the page, so pass nextCursor as cursor until it is absent.", m.handleReposList), mcp.ToolAnnotations{Title: "List repositories", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		createSpec,
		annotateTool(mcp.NewToolSpec("task_get_detail", "Get task detail", "Get recent activity and status details by task number from tasks_list, or by stable task ID.", m.handleTaskGetDetail), mcp.ToolAnnotations{Title: "Get task detail", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_send_message", "Send task message", "Send a text message to a waiting or asking agent by task number.", m.handleTaskSendMessage), mcp.ToolAnnotations{Title: "Send task message", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_answer_question", "Answer task question", "Answer an agent's question by task number. The agent is in 'asking' state.", m.handleTaskAnswerQuestion), mcp.ToolAnnotations{Title: "Answer task question", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_push_branch_to_remote", "Push task branch", "Sync or push a task's changes to GitHub. Push to task branch (default) or squash-push to main.", m.handleTaskPushBranchToRemote), mcp.ToolAnnotations{Title: "Push task branch", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("task_stop", "Stop task", "Stop a running or waiting task. The container is preserved and can be revived later.", m.handleTaskStop), mcp.ToolAnnotations{Title: "Stop task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_purge", "Purge task", "Stop a task and schedule its container for deletion after the recovery window. Reviving the task during the window cancels deletion.", m.handleTaskPurge), mcp.ToolAnnotations{Title: "Purge task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_revive", "Revive task", "Revive a stopped or crashed task, restarting its container and agent session.", m.handleTaskRevive), mcp.ToolAnnotations{Title: "Revive task", DestructiveHint: false, OpenWorldHint: false}),
		forkSpec,
		annotateTool(mcp.NewToolSpec("get_usage", "Get usage", "Check current API quota utilization and limits.", m.handleGetUsage), mcp.ToolAnnotations{Title: "Get usage", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("clone_repo", "Clone repository", "Clone a git repository by URL. Optionally specify a local path.", m.handleCloneRepo), mcp.ToolAnnotations{Title: "Clone repository", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("agent_last_message", "Get last agent message", "Get latest agent message, question, or result. Call to check what the agent needs or relay to user.", m.handleAgentLastMessage), mcp.ToolAnnotations{Title: "Get last agent message", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_fix_pr", "Fix task PR", "Inject a fix-PR command into an existing task to fix its failing PR CI in auto mode.", m.handleTaskFixPR), mcp.ToolAnnotations{Title: "Fix task PR", DestructiveHint: true, OpenWorldHint: true}),
		botFixCISpec,
	}
}

func annotateTool(spec mcp.ToolSpec, annotations mcp.ToolAnnotations) mcp.ToolSpec { //nolint:gocritic // Tool specs are immutable catalog entries; value style keeps call sites simple.
	spec.Annotations = &annotations
	return spec
}

func (m *mcpRegistry) subscriptionSources(ctx context.Context, filter mcp.SubscriptionFilter) (subscriptionSources, error) {
	var sources subscriptionSources
	hasFilter := false
	for _, uri := range filter.ResourceSubscriptions {
		if decision, ok := m.authorizeResource(ctx, uri); !ok {
			return subscriptionSources{}, mcp.ErrInvalidParams("%s", decision)
		}
		hasFilter = true
		switch {
		case strings.HasPrefix(uri, "caic://tasks/"):
			id, err := mcpTaskResourceID(uri)
			if err != nil {
				return subscriptionSources{}, mcp.ErrInvalidParams("task not found")
			}
			sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
			sources.taskResourceIDs = append(sources.taskResourceIDs, id)
		case strings.HasPrefix(uri, "caic://repos/"):
			sources.repoC = m.serverConfig.checkouts.Changed()
			sources.repoStatusC = m.serverConfig.repoStatus.Changed()
			sources.repoResourceURIs = append(sources.repoResourceURIs, uri)
		case uri == "gomode://items":
			sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
			sources.taskUpdateURIs = append(sources.taskUpdateURIs, uri)
		case uri == "gomode://notifications":
			sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
			sources.taskUpdateURIs = append(sources.taskUpdateURIs, uri)
			sources.usagePolling = true
			sources.usageResourceURIs = append(sources.usageResourceURIs, uri)
		default:
			return subscriptionSources{}, mcp.ErrInvalidParams("unsupported resource subscription: %s", uri)
		}
	}
	if filter.ResourcesListChanged {
		if !mcpHasScope(ctx, mcpScopeRead) {
			return subscriptionSources{}, mcp.ErrInvalidParams("missing required MCP scope: %s", mcpScopeRead)
		}
		hasFilter = true
		sources.taskVersion, sources.taskC = m.taskSvc.taskMgr.ChangeSnapshot()
		sources.repoC = m.serverConfig.checkouts.Changed()
		sources.repoStatusC = m.serverConfig.repoStatus.Changed()
		sources.resourcesListChanged = true
	}
	if !hasFilter {
		return subscriptionSources{}, mcp.ErrInvalidParams("subscription filter is empty")
	}
	return sources, nil
}

type mcpTaskListArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema_description:"Opaque cursor returned by the previous page"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"minimum=1,maximum=50"                                    jsonschema_description:"Maximum tasks to return; defaults to 20, cannot exceed 50, and a response-size limit may shorten the page"`
}

type mcpTaskSummary struct {
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
}

type mcpTaskListOutput struct {
	Tasks      []mcpTaskSummary `json:"tasks"                jsonschema_description:"Tasks in this page"`
	NextCursor string           `json:"nextCursor,omitempty" jsonschema_description:"Opaque cursor for the next page"`
}

func (m *mcpRegistry) handleTasksList(ctx context.Context, args mcpTaskListArgs) mcp.ToolResult[mcpTaskListOutput] {
	pageSize := args.Limit
	if pageSize == 0 {
		pageSize = mcpTaskPageSizeDefault
	}
	if pageSize < 1 || pageSize > mcpTaskPageSizeMax {
		return mcp.ToolErrorWithMeta[mcpTaskListOutput]("Invalid limit. Use an integer from 1 through 50.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	keys, revision := m.taskKeys(ctx)
	page, next, start, err := paginateMCPTaskKeys(keys, revision, args.Cursor, pageSize)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpTaskListOutput]("Invalid cursor. Omit cursor to restart task listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	tasks, err := m.taskSummaries(ctx, page, start, revision)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpTaskListOutput]("Task ordering changed while listing. Omit cursor to restart task listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
	}
	tasks, next, err = fitMCPJSONPage(tasks, next != "", mcpListJSONMaxBytes,
		func(count int) (string, error) { return encodeMCPTaskCursor(revision, page[count-1].ID) },
		func(page []mcpTaskSummary, cursor string) any {
			return mcpTaskListOutput{Tasks: page, NextCursor: cursor}
		},
	)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpTaskListOutput]("A task summary exceeds the MCP response-size limit.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeInternalError)})
	}
	// TODO(observability): Record requested/effective page size, cursor outcomes,
	// returned task count, and pre-wire logical payload size here. Do not use
	// cursor, task ID, title, or user identity as metric labels.
	return mcp.TypedToolResult(mcpTaskListOutput{Tasks: tasks, NextCursor: next})
}

type mcpRepoListArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema_description:"Opaque cursor returned by the previous page"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"minimum=1,maximum=200"                                   jsonschema_description:"Maximum repositories to return; defaults to 200, cannot exceed 200, and a response-size limit may shorten the page"`
}

type mcpRepoSummary struct {
	Path       string        `json:"path"                jsonschema_description:"Repository path used by task_create"`
	BaseBranch v1.BranchInfo `json:"baseBranch"          jsonschema_description:"Configured base branch"`
	Forge      v1.Forge      `json:"forge,omitempty"     jsonschema_description:"Repository forge provider"`
	RemoteURL  string        `json:"remoteURL,omitempty" jsonschema_description:"HTTPS remote URL"`
	CI         v1.CIStatus   `json:"ci,omitempty"        jsonschema_description:"Aggregate continuous integration status"`
}

type mcpRepoListOutput struct {
	Repositories    []mcpRepoSummary `json:"repositories"              jsonschema_description:"Repositories in this page"`
	NextCursor      string           `json:"nextCursor,omitempty"      jsonschema_description:"Opaque cursor for the next page"`
	FieldsTruncated bool             `json:"fieldsTruncated,omitempty" jsonschema_description:"Whether presentation-only repository fields were shortened to the MCP response limit"`
}

func (m *mcpRegistry) handleReposList(_ context.Context, args mcpRepoListArgs) mcp.ToolResult[mcpRepoListOutput] {
	pageSize := args.Limit
	if pageSize == 0 {
		pageSize = mcpRepoPageSizeDefault
	}
	if pageSize < 1 || pageSize > mcpRepoPageSizeMax {
		return mcp.ToolErrorWithMeta[mcpRepoListOutput]("Invalid limit. Use an integer from 1 through 200.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	page, next, err := paginateMCPRepositories(m.repositoryPaths(), args.Cursor, pageSize)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpRepoListOutput]("Invalid cursor. Omit cursor to restart repository listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	repositories, fieldsTruncated, err := m.repositorySummaries(page)
	if err != nil {
		if errors.Is(err, errMCPRepositoryPathTooLong) {
			return mcp.ToolErrorWithMeta[mcpRepoListOutput](err.Error(), mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
		}
		return mcp.ToolErrorWithMeta[mcpRepoListOutput]("Repository list changed while paging. Omit cursor to restart repository listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
	}
	repositories, next, err = fitMCPJSONPage(repositories, next != "", mcpListJSONMaxBytes,
		func(count int) (string, error) { return mcpKeyCursor(page[count-1]), nil },
		func(page []mcpRepoSummary, cursor string) any {
			return mcpRepoListOutput{Repositories: page, NextCursor: cursor, FieldsTruncated: fieldsTruncated}
		},
	)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpRepoListOutput]("A repository summary exceeds the MCP response-size limit.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeInternalError)})
	}
	result := mcp.TypedToolResult(mcpRepoListOutput{Repositories: repositories, NextCursor: next, FieldsTruncated: fieldsTruncated})
	if fieldsTruncated {
		result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	}
	return result
}

func domainToolError[T any](err error) mcp.ToolResult[T] {
	if err == nil {
		return mcp.ToolResult[T]{}
	}
	if apiErr, ok := errors.AsType[*api.Error](err); ok {
		return mcp.ToolErrorWithMeta[T](apiErr.Error(), mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
	}
	return mcp.ToolErrorWithMeta[T](err.Error(), mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeInternalError)})
}

// taskCreateToolError adds a recovery step only when the rejected task-create
// override can be safely omitted or the caller can list available repositories.
func (m *mcpRegistry) taskCreateToolError(ctx context.Context, args mcpTaskCreateArgs, err error) mcp.ToolResult[mcpTaskCreatedOutput] { //nolint:gocritic // Recovery depends on request-shaped arguments.
	apiErr, ok := errors.AsType[*api.Error](err)
	if !ok {
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	message := apiErr.Error()
	switch apiErr.Code {
	case api.CodeUnknownRepository:
		if !m.taskCreateConfigurationValid(ctx, args) {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		message = repositoryRecoveryMessage(ctx, message, "task_create")
	case api.CodeUnknownHarness:
		if args.Harness == "" {
			return mcp.ToolErrorWithMeta[mcpTaskCreatedOutput](apiErr.Error(), mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
		}
		args.Harness = ""
		message += ". Omit harness to use caic's default harness, then retry task_create."
	case api.CodeUnsupportedModel:
		if args.Model == "" {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		args.Model = ""
		message += ". Omit model to use the selected harness's default model, then retry task_create."
	case api.CodeUnknownRuntime:
		if args.RuntimeName == "" {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		args.RuntimeName = ""
		message += ". Omit runtimeName to use caic's default runtime, then retry task_create."
	default:
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	if !m.taskCreateConfigurationValid(ctx, args) {
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	return mcp.ToolErrorWithMeta[mcpTaskCreatedOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
}

// repositoryRecoveryMessage adds a safe retry instruction for a rejected
// repository path. It only refers to repos_list when the caller can use it.
func repositoryRecoveryMessage(ctx context.Context, message, operation string) string {
	if mcpHasScope(ctx, mcpScopeRead) {
		return message + ". Call repos_list, use an exact returned path, then retry " + operation + "."
	}
	return message + ". The path must exactly match a configured repository."
}

// taskCreateConfigurationValid reports whether the effective harness, model,
// and runtime selections are valid after a rejected override is removed.
func (m *mcpRegistry) taskCreateConfigurationValid(ctx context.Context, args mcpTaskCreateArgs) bool { //nolint:gocritic // Validation consumes request-shaped arguments.
	apiHarness, err := m.resolveTaskCreateHarness(ctx, args.Harness)
	if err != nil {
		return false
	}
	harnessName, err := apiconv.AgentHarness(apiHarness)
	if err != nil {
		return false
	}
	backend, ok := m.serverConfig.taskMgr.Backends[harnessName]
	if !ok || (args.Model != "" && !slices.Contains(backend.ModelInventory().IDs(), args.Model)) {
		return false
	}
	runtimes := m.taskSvc.taskMgr.Runtimes
	if runtimes == nil {
		return false
	}
	if args.RuntimeName == "" {
		return len(runtimes.Runtimes) > 0
	}
	_, ok = runtimes.ByName[runtime.Name(args.RuntimeName)]
	return ok
}

type mcpTaskCreatedOutput struct {
	Result     string `json:"result"               jsonschema_description:"Human-readable task creation result"`
	TaskNumber int    `json:"taskNumber,omitempty" jsonschema_description:"Current session task number"`
	TaskID     string `json:"taskID"               jsonschema_description:"Stable task ID"`
}

type mcpTaskCreateArgs struct {
	Prompt      string   `json:"prompt"                jsonschema_description:"The task description/prompt for the coding agent"`
	Repos       []string `json:"repos"                 jsonschema:"minItems=1"                                                                   jsonschema_description:"Repositories to work in (one or more)"`
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

func (m *mcpRegistry) handleTaskCreate(ctx context.Context, args mcpTaskCreateArgs) mcp.ToolResult[mcpTaskCreatedOutput] { //nolint:gocritic // MCP tool handlers receive decoded argument values by API contract.
	if args.Prompt == "" {
		return domainToolError[mcpTaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: prompt"})
	}
	if _, ok := taskMCPTaskID(ctx); ok {
		if len(args.Repos) != 0 || args.Harness != "" || args.Model != "" || args.Effort != "" || args.RuntimeName != "" || args.Display || args.Tailscale || args.USB || args.Sudo || args.GitHubToken {
			return mcp.ToolError[mcpTaskCreatedOutput]("Task-scoped MCP may only set prompt")
		}
		resp, err := m.taskSvc.createTask(ctx, &v1.CreateTaskReq{InitialPrompt: v1.Prompt{Text: args.Prompt}})
		if err != nil {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: "Created child task: " + resp.ID.String(), TaskID: resp.ID.String()})
	}
	if len(args.Repos) == 0 {
		return domainToolError[mcpTaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: repos"})
	}
	apiHarness, err := m.resolveTaskCreateHarness(ctx, args.Harness)
	if err != nil {
		if args.Harness == "" {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		return m.taskCreateToolError(ctx, args, &api.Error{Status: http.StatusBadRequest, Code: api.CodeUnknownHarness, Message: err.Error()})
	}
	req := &v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: args.Prompt},
		Repos:         make([]v1.RepoSpec, len(args.Repos)),
		Harness:       apiHarness,
		Model:         args.Model,
		Effort:        args.Effort,
		RuntimeName:   args.RuntimeName,
		Display:       args.Display,
		Tailscale:     args.Tailscale,
		USB:           args.USB,
		Sudo:          args.Sudo,
		GitHubToken:   args.GitHubToken,
	}
	for i, repo := range args.Repos {
		req.Repos[i] = v1.RepoSpec{Name: repo}
	}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	resp, err := m.taskSvc.createTask(ctx, req)
	if err != nil {
		return m.taskCreateToolError(ctx, args, err)
	}
	taskList := m.taskSvc.taskListSnapshot(ctx)
	num := taskNumberForID(taskList, resp.ID.String())
	title := resp.ID.String()
	for i := range taskList {
		if taskList[i].ID == resp.ID {
			title = taskTitle(&taskList[i])
			break
		}
	}
	title, truncated := truncateMCPTaskTitle(title)
	var output mcpTaskCreatedOutput
	if num > 0 {
		output = mcpTaskCreatedOutput{Result: fmt.Sprintf("Created task #%d: %s", num, title), TaskNumber: num, TaskID: resp.ID.String()}
	} else {
		output = mcpTaskCreatedOutput{Result: "Created task: " + title, TaskID: resp.ID.String()}
	}
	result := mcp.TypedToolResult(output)
	if truncated {
		result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	}
	return result
}

func (m *mcpRegistry) resolveTaskCreateHarness(ctx context.Context, harness string) (v1.Harness, error) {
	if harness != "" {
		return apiconv.ParseHarness(harness)
	}
	prefs := m.serverConfig.prefs.Get(userIDFromCtx(ctx))
	if prefs.Harness != "" {
		apiHarness, err := apiconv.ParseHarness(prefs.Harness)
		if err != nil {
			return "", &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: err.Error()}
		}
		return apiHarness, nil
	}
	return m.firstHarness(ctx)
}

func (m *mcpRegistry) firstHarness(ctx context.Context) (v1.Harness, error) {
	harnesses, err := m.serverConfig.listHarnesses(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list available harnesses: %w", err)
	}
	if harnesses == nil || len(*harnesses) == 0 {
		return "", &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "no available harnesses"}
	}
	return (*harnesses)[0].Name, nil
}

func (m *mcpRegistry) handleTaskGetDetail(ctx context.Context, args mcpTaskGetDetailArgs) mcp.ToolResult[mcp.TextOutput] {
	keys, _ := m.taskKeys(ctx)
	id, number, err := args.Task.Decode(len(keys))
	if err != nil {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: err.Error()})
	}
	var t v1.Task
	if number != 0 {
		t, err = m.taskByNumber(ctx, number)
	} else {
		t, err = m.taskByID(ctx, id)
	}
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	title := fmt.Sprintf("## Task #%d: %s", number, taskTitle(&t))
	if number == 0 {
		title = fmt.Sprintf("## Task %s: %s", t.ID, taskTitle(&t))
	}
	lines := []string{
		title,
		"",
		fmt.Sprintf("State: %s  Elapsed: %s  Cost: %s", t.State, formatElapsed(time.Duration(t.Duration*float64(time.Second))), formatCost(t.CostUSD)),
	}
	if t.Runtime.RuntimeName != "" {
		lines = append(lines, "Runtime: "+t.Runtime.RuntimeName)
	}
	if t.State == v1.TaskStatePurged && t.Result != "" {
		lines = append(lines, "**Result:** "+t.Result)
	}
	if t.State == v1.TaskStateStopped {
		lines = append(lines, "**Stopped:** container stopped")
	}
	if t.State == v1.TaskStateCrashed && t.Error != "" {
		lines = append(lines, "**Crashed:** "+t.Error)
	}
	if t.State == v1.TaskStateFailed && t.Error != "" {
		lines = append(lines, "**Error:** "+t.Error)
	}
	if len(t.DiffStat) > 0 {
		pathCount := min(len(t.DiffStat), maxTaskDetailPaths)
		paths := make([]string, pathCount)
		for i, d := range t.DiffStat[:pathCount] {
			paths[i] = d.Path
		}
		changed := "**Changed:** " + strings.Join(paths, ", ")
		if omitted := len(t.DiffStat) - pathCount; omitted > 0 {
			changed += fmt.Sprintf(" … and %d more files", omitted)
		}
		lines = append(lines, changed)
	}
	return boundedTextToolResult(strings.TrimSpace(strings.Join(lines, "\n")))
}

type mcpTaskSendMessageArgs struct {
	TaskNumber int    `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
	Message    string `json:"message"     jsonschema_description:"The message to send to the agent"`
}

func (m *mcpRegistry) handleTaskSendMessage(ctx context.Context, args mcpTaskSendMessageArgs) mcp.ToolResult[mcp.TextOutput] {
	return m.sendTaskInput(ctx, mcpTaskInputArgs(args), "message", "Sent message to task #%d.")
}

type mcpTaskAnswerQuestionArgs struct {
	TaskNumber int    `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
	Answer     string `json:"answer"      jsonschema_description:"The answer to the agent's question"`
}

func (m *mcpRegistry) handleTaskAnswerQuestion(ctx context.Context, args mcpTaskAnswerQuestionArgs) mcp.ToolResult[mcp.TextOutput] {
	return m.sendTaskInput(ctx, mcpTaskInputArgs{TaskNumber: args.TaskNumber, Message: args.Answer}, "answer", "Answered task #%d.")
}

type mcpTaskPushBranchArgs struct {
	TaskNumber int    `json:"task_number"      jsonschema_description:"The task number, e.g. 1 for task #1"`
	Force      bool   `json:"force,omitempty"  jsonschema_description:"Force sync even with safety issues"`
	Target     string `json:"target,omitempty" jsonschema:"enum=branch,enum=default,enum=main,enum=master"  jsonschema_description:"Where to push: branch (default) or main"`
}

func (m *mcpRegistry) handleTaskPushBranchToRemote(ctx context.Context, args mcpTaskPushBranchArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	targetRaw := args.Target
	if targetRaw == "main" || targetRaw == "master" {
		targetRaw = string(v1.SyncTargetDefault)
	}
	req := &v1.SyncReq{Force: args.Force, Target: v1.SyncTarget(targetRaw)}
	if err := req.Validate(); err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	resp, err := m.taskSvc.syncTask(ctx, entry, req)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	verb := fmt.Sprintf("Synced task #%d", num)
	if req.Target == v1.SyncTargetDefault {
		verb = fmt.Sprintf("Pushed task #%d to main", num)
	}
	if len(resp.SafetyIssues) == 0 {
		return mcp.TextToolResult(verb + ".")
	}
	issueCount := min(len(resp.SafetyIssues), maxMCPSafetyIssues)
	issueLines := make([]string, 0, issueCount+1)
	for _, issue := range resp.SafetyIssues[:issueCount] {
		issueLines = append(issueLines, fmt.Sprintf("- **%s** %s: %s", issue.Kind, issue.File, issue.Detail))
	}
	if omitted := len(resp.SafetyIssues) - issueCount; omitted > 0 {
		issueLines = append(issueLines, fmt.Sprintf("- … %d more safety issues omitted", omitted))
	}
	return boundedTextToolResult(verb + " with safety issues:\n" + strings.Join(issueLines, "\n"))
}

func (m *mcpRegistry) handleTaskStop(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.taskScopedEntryByNumber(ctx, args.TaskNumber, false)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	_, err := m.taskSvc.stopTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Stopping task #%d.", num))
}

func (m *mcpRegistry) handleTaskPurge(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.taskScopedEntryByNumber(ctx, args.TaskNumber, false)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	_, err := m.taskSvc.purgeTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Stopped task #%d and scheduled it for purge.", num))
}

func (m *mcpRegistry) handleTaskRevive(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	_, err := m.taskSvc.reviveTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Reviving task #%d.", num))
}

type mcpTaskForkOutput struct {
	Result string `json:"result" jsonschema_description:"Human-readable fork result"`
	TaskID string `json:"taskID" jsonschema_description:"Stable task ID for the forked task"`
}

type mcpTaskForkArgs struct {
	TaskNumber int    `json:"task_number"       jsonschema_description:"The task number to fork, e.g. 1 for task #1"`
	Prompt     string `json:"prompt"            jsonschema_description:"The initial prompt for the forked task"`
	Harness    string `json:"harness,omitempty" jsonschema_description:"Override harness (optional, inherits from source if omitted)"`
	Model      string `json:"model,omitempty"   jsonschema_description:"Model override (optional, inherits from source if omitted)"`
}

// taskForkToolError adds a recovery step only when the rejected override can
// be omitted to inherit the source task's configuration.
func (m *mcpRegistry) taskForkToolError(args mcpTaskForkArgs, source *taskpkg.Task, err error) mcp.ToolResult[mcpTaskForkOutput] {
	apiErr, ok := errors.AsType[*api.Error](err)
	if !ok {
		return domainToolError[mcpTaskForkOutput](err)
	}
	message := apiErr.Error()
	switch apiErr.Code {
	case api.CodeUnknownHarness:
		if args.Harness == "" || !m.forkWithoutHarnessValid(source, args.Model) {
			return domainToolError[mcpTaskForkOutput](err)
		}
		message += ". Omit harness to inherit the source task's harness, then retry task_fork."
	case api.CodeUnsupportedModel:
		if args.Model == "" || !m.forkWithoutModelValid(source, args.Harness) {
			return domainToolError[mcpTaskForkOutput](err)
		}
		message += ". Omit model to inherit the source task's model, then retry task_fork."
	default:
		return domainToolError[mcpTaskForkOutput](err)
	}
	return mcp.ToolErrorWithMeta[mcpTaskForkOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
}

func (m *mcpRegistry) forkWithoutHarnessValid(source *taskpkg.Task, modelOverride string) bool {
	backend, ok := m.taskSvc.taskMgr.Backends[source.Harness]
	model := modelOverride
	if model == "" {
		model = source.RequestedModel
	}
	return ok && (model == "" || slices.Contains(backend.ModelInventory().IDs(), model))
}

// forkWithoutModelValid reports whether the effective harness can accept the
// source model after an explicit model override is omitted.
func (m *mcpRegistry) forkWithoutModelValid(source *taskpkg.Task, harnessOverride string) bool {
	harnessName := source.Harness
	if harnessOverride != "" {
		apiHarness, err := apiconv.ParseHarness(harnessOverride)
		if err != nil {
			return false
		}
		var conversionErr error
		harnessName, conversionErr = apiconv.AgentHarness(apiHarness)
		if conversionErr != nil {
			return false
		}
	}
	backend, ok := m.taskSvc.taskMgr.Backends[harnessName]
	return ok && (source.RequestedModel == "" || slices.Contains(backend.ModelInventory().IDs(), source.RequestedModel))
}

func (m *mcpRegistry) handleTaskFork(ctx context.Context, args mcpTaskForkArgs) mcp.ToolResult[mcpTaskForkOutput] {
	num, entry, ok := m.taskScopedEntryByNumber(ctx, args.TaskNumber, true)
	if !ok {
		return domainToolError[mcpTaskForkOutput](taskNumberError(args.TaskNumber))
	}
	if args.Prompt == "" {
		return domainToolError[mcpTaskForkOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: prompt"})
	}
	var harness v1.Harness
	if args.Harness != "" {
		var err error
		harness, err = apiconv.ParseHarness(args.Harness)
		if err != nil {
			return m.taskForkToolError(args, entry.Task(), &api.Error{Status: http.StatusBadRequest, Code: api.CodeUnknownHarness, Message: err.Error()})
		}
	}
	req := &v1.ForkTaskReq{Prompt: v1.Prompt{Text: args.Prompt}, Harness: harness, Model: args.Model}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTaskForkOutput](err)
	}
	resp, err := m.taskSvc.forkTask(ctx, entry, req)
	if err != nil {
		return m.taskForkToolError(args, entry.Task(), err)
	}
	return mcp.TypedToolResult(mcpTaskForkOutput{Result: fmt.Sprintf("Forked task #%d. New task ID: %s", num, resp.ID.String()), TaskID: resp.ID.String()})
}

func (m *mcpRegistry) handleGetUsage(ctx context.Context, _ struct{}) mcp.ToolResult[mcp.TextOutput] {
	usage := m.usage.buildResp(ctx)
	var lines []string
	for _, w := range usage.Local.Windows {
		lines = append(lines, fmt.Sprintf("%s cost: %s (%d tokens)", w.Duration, formatUSD(w.CostUSD), w.InputTokens+w.OutputTokens))
	}
	for i := range usage.Providers {
		pq := &usage.Providers[i]
		var parts []string
		if pq.Balance.Currency != "" {
			parts = append(parts, formatBalance(pq.Balance.Currency, pq.Balance.Total))
		}
		for _, rl := range pq.RateLimits {
			parts = append(parts, fmt.Sprintf("%s: %.0f%% remaining", rl.Window, 100-rl.UsedPct))
		}
		if len(parts) > 0 {
			lines = append(lines, pq.Label+": "+strings.Join(parts, ", "))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "No usage data available.")
	}
	return boundedTextToolResult(strings.Join(lines, "\n"))
}

type mcpCloneRepoArgs struct {
	URL  string `json:"url"            jsonschema_description:"The git repository URL to clone"`
	Path string `json:"path,omitempty" jsonschema_description:"Local directory name (optional, derived from URL if omitted)"`
}

func (m *mcpRegistry) handleCloneRepo(ctx context.Context, args mcpCloneRepoArgs) mcp.ToolResult[mcp.TextOutput] {
	if args.URL == "" {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: url"})
	}
	req := &v1.CloneRepoReq{URL: args.URL, Path: args.Path}
	if err := req.Validate(); err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	repo, err := m.serverConfig.cloneRepo(ctx, req)
	if err != nil {
		if apiErr, ok := errors.AsType[*api.Error](err); ok && apiErr.Code == api.CodeRepositoryPathConflict {
			return mcp.ToolErrorWithMeta[mcp.TextOutput](apiErr.Error()+". Choose a different path and retry clone_repo.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
		}
		return domainToolError[mcp.TextOutput](err)
	}
	base := repo.BaseBranch.Name
	if repo.BaseBranch.Remote != "" {
		base = repo.BaseBranch.Remote + "/" + base
	}
	return mcp.TextToolResult(fmt.Sprintf("Cloned **%s** (base: %s).", repo.Path, base))
}

func (m *mcpRegistry) handleAgentLastMessage(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	var message agent.Message
	var historyErr error
	for candidate, err := range m.taskSvc.taskMgr.BackwardMessages(ctx, entry) {
		if err != nil {
			historyErr = err
			break
		}
		switch candidate := candidate.(type) {
		case *agent.ResultMessage:
			if candidate.Result != "" {
				message = candidate
			}
		case *agent.AskMessage:
			if len(candidate.Questions) > 0 {
				message = candidate
			}
		case *agent.TextMessage:
			if candidate.Text != "" {
				message = candidate
			}
		}
		if message != nil {
			break
		}
	}
	if historyErr != nil {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: fmt.Sprintf("Task #%d history is unavailable: %v", num, historyErr)})
	}
	if message == nil {
		return mcp.TextToolResult(fmt.Sprintf("No messages from task #%d yet.", num))
	}
	switch message := message.(type) {
	case *agent.ResultMessage:
		return boundedTextToolResult(fmt.Sprintf("Task #%d result: %s", num, message.Result))
	case *agent.AskMessage:
		q := message.Questions[0]
		options := make([]string, len(q.Options))
		for i, opt := range q.Options {
			options[i] = opt.Label
		}
		suffix := ""
		if len(options) > 0 {
			suffix = " Options: " + strings.Join(options, ", ")
		}
		return boundedTextToolResult(fmt.Sprintf("Task #%d is asking: %s%s", num, q.Question, suffix))
	case *agent.TextMessage:
		return boundedTextToolResult(fmt.Sprintf("Last message from task #%d: %s", num, message.Text))
	default:
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: fmt.Sprintf("Task #%d history returned an unsupported message", num)})
	}
}

type mcpTaskFixPRArgs struct {
	TaskNumber int `json:"task_number" jsonschema_description:"The task number whose PR CI should be fixed"`
}

func (m *mcpRegistry) handleTaskFixPR(ctx context.Context, args mcpTaskFixPRArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	_, err := m.ci.fixPR(ctx, &v1.BotFixPRReq{TaskID: entry.Task().ID.String()})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Injected fix-PR command into task #%d.", num))
}

type mcpBotFixCIArgs struct {
	Repo string `json:"repo" jsonschema_description:"Repository to fix CI for"`
}

func (m *mcpRegistry) handleBotFixCI(ctx context.Context, args mcpBotFixCIArgs) mcp.ToolResult[mcpTaskCreatedOutput] {
	if args.Repo == "" {
		return domainToolError[mcpTaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: repo"})
	}
	resp, err := m.ci.fixCI(ctx, &v1.BotFixCIReq{Repo: args.Repo})
	if err != nil {
		if apiErr, ok := errors.AsType[*api.Error](err); ok && apiErr.Code == api.CodeUnknownRepository {
			message := repositoryRecoveryMessage(ctx, apiErr.Error(), "bot_fix_ci")
			return mcp.ToolErrorWithMeta[mcpTaskCreatedOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
		}
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	taskList := m.taskSvc.taskListSnapshot(ctx)
	num := taskNumberForID(taskList, resp.ID.String())
	if num > 0 {
		return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task #%d for %s.", num, args.Repo), TaskNumber: num, TaskID: resp.ID.String()})
	}
	return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task for %s.", args.Repo), TaskID: resp.ID.String()})
}

type mcpTaskInputArgs struct {
	TaskNumber int
	Message    string
}

func (m *mcpRegistry) sendTaskInput(ctx context.Context, args mcpTaskInputArgs, field, format string) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNumberError(args.TaskNumber))
	}
	if args.Message == "" {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: " + field})
	}
	_, err := m.taskSvc.sendInput(ctx, entry, &v1.InputReq{Prompt: v1.Prompt{Text: args.Message}})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf(format, num))
}

func (m *mcpRegistry) taskByNumber(ctx context.Context, num int) (v1.Task, error) {
	_, entry, ok := m.entryByNumber(ctx, num)
	if !ok {
		return v1.Task{}, taskNumberError(num)
	}
	return m.taskDTO(ctx, entry)
}

func (m *mcpRegistry) entryByNumber(ctx context.Context, num int) (int, *taskmgr.Entry, bool) {
	keys, _ := m.taskKeys(ctx)
	if num < 1 || num > len(keys) {
		return 0, nil, false
	}
	entry, ok := m.visibleTaskEntry(ctx, keys[num-1].ID)
	return num, entry, ok
}

func (m *mcpRegistry) taskByID(ctx context.Context, id ksid.ID) (v1.Task, error) {
	entry, ok := m.inspectableTaskEntry(ctx, id)
	if !ok {
		return v1.Task{}, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task not found"}
	}
	return m.taskDTO(ctx, entry)
}

func (m *mcpRegistry) taskDTO(ctx context.Context, entry *taskmgr.Entry) (v1.Task, error) {
	t, err := taskDTO(ctx, entry, m.taskSvc.taskMgr, m.taskSvc.checkouts, m.taskSvc.authStore)
	if err != nil {
		return v1.Task{}, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: err.Error()}
	}
	return t, nil
}

// taskScopedEntryByNumber resolves task numbers from a task-scoped list and
// may additionally resolve 0 to the calling task.
func (m *mcpRegistry) taskScopedEntryByNumber(ctx context.Context, num int, allowSelf bool) (int, *taskmgr.Entry, bool) {
	delegatingTaskID, taskScoped := taskMCPTaskID(ctx)
	if !taskScoped {
		return m.entryByNumber(ctx, num)
	}
	if num == 0 && allowSelf {
		entry, ok := m.taskSvc.taskMgr.GetEntry(delegatingTaskID.String())
		return num, entry, ok
	}
	if num < 1 {
		return num, nil, false
	}
	keys, _ := m.taskKeys(ctx)
	if num > len(keys) {
		return num, nil, false
	}
	entry, ok := m.visibleTaskEntry(ctx, keys[num-1].ID)
	return num, entry, ok
}

func taskNumberError(num int) *api.Error {
	if num < 1 {
		return &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "task_number must be a positive integer"}
	}
	return &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task not found"}
}

// Static schema builders. Dynamic repos, harnesses, and preferences are read on
// demand by resources and tool handlers instead of embedded in tool schemas.

func buildTaskCreateSchema() *jsonschema.Schema {
	minOne := uint64(1)
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The task description/prompt for the coding agent"})
	props.Set("repos", &jsonschema.Schema{Type: "array", Description: "Repositories to work in (one or more). Call repos_list first if you need the current repository list.", Items: &jsonschema.Schema{Type: "string"}, MinItems: &minOne})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model override (optional). Omit unless the user explicitly chose a model; omitted means the selected harness default."})
	props.Set("effort", &jsonschema.Schema{Type: "string", Description: "Thinking effort override (optional). Omit unless the user explicitly chose an effort; omitted means the selected harness default."})
	props.Set("harness", &jsonschema.Schema{Type: "string", Description: "Agent harness override (optional). Omit to use the saved default harness."})
	props.Set("runtimeName", &jsonschema.Schema{Type: "string", Description: "Runtime backend name (optional). Use docker or podman when multiple runtimes are available."})
	props.Set("display", &jsonschema.Schema{Type: "boolean", Description: "Enable virtual display (VNC) for this task"})
	props.Set("tailscale", &jsonschema.Schema{Type: "boolean", Description: "Enable Tailscale networking for this task"})
	props.Set("usb", &jsonschema.Schema{Type: "boolean", Description: "Enable USB passthrough for this task"})
	props.Set("sudo", &jsonschema.Schema{Type: "boolean", Description: "Enable root access via sudo with a random password"})
	props.Set("gitHubToken", &jsonschema.Schema{Type: "boolean", Description: "Enable GitHub token injection for this task"})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"prompt", "repos"}}
}

func buildDelegatedTaskCreateSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The child task prompt. Its parent, repository, and runtime are derived from this task."})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"prompt"}}
}

func buildTaskForkSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("task_number", &jsonschema.Schema{Type: "integer", Description: "The task number to fork, e.g. 1 for task #1"})
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The initial prompt for the forked task"})
	props.Set("harness", &jsonschema.Schema{Type: "string", Description: "Override harness (optional, inherits from source if omitted)"})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model override (optional, inherits from source if omitted)"})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"task_number", "prompt"}}
	mcp.AddHeaderToProperty(schema, "task_number", "Task-Number")
	return schema
}

func buildDelegatedTaskForkSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("task_number", &jsonschema.Schema{Type: "integer", Description: "Use 0 to fork this task, or a child task number returned by tasks_list"})
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The initial prompt for the forked child task"})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"task_number", "prompt"}}
	mcp.AddHeaderToProperty(schema, "task_number", "Task-Number")
	return schema
}

func buildBotFixCISchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("repo", &jsonschema.Schema{Type: "string", Description: "Repository path to fix CI for. Call repos_list first if you need the current repository list."})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"repo"}}
	mcp.AddHeaderToProperty(schema, "repo", "Repo")
	return schema
}

// Support code

func (m *mcpRegistry) resourceKeys(ctx context.Context) ([]mcpResourceKey, error) {
	staticResources := mcpStaticResources()
	keys := make([]mcpResourceKey, 0, len(staticResources))
	for i := range staticResources {
		resource := &staticResources[i]
		if _, ok := m.authorizeResource(ctx, resource.URI); ok {
			keys = append(keys, mcpResourceKey{URI: resource.URI, Kind: mcpResourceStatic, Value: resource.Name})
		}
	}
	if mcpHasScope(ctx, mcpScopeRead) {
		for checkout := range m.serverConfig.checkouts.Checkouts() {
			if err := validateMCPRepositoryPath(checkout.RelPath); err != nil {
				return nil, err
			}
			keys = append(keys, mcpResourceKey{URI: "caic://repos/" + url.PathEscape(checkout.RelPath), Kind: mcpResourceRepo, Value: checkout.RelPath})
		}
	}
	_, taskScoped := taskMCPTaskID(ctx)
	if mcpHasScope(ctx, mcpScopeTasksRead) || taskScoped {
		taskKeys, _ := m.taskKeys(ctx)
		for _, taskKey := range taskKeys {
			keys = append(keys, mcpResourceKey{URI: "caic://tasks/" + taskKey.ID.String(), Kind: mcpResourceTask, TaskID: taskKey.ID})
		}
	}
	slices.SortFunc(keys, func(a, b mcpResourceKey) int { return strings.Compare(a.URI, b.URI) })
	return keys, nil
}

func (m *mcpRegistry) resourceDescriptors(ctx context.Context, keys []mcpResourceKey) iter.Seq2[mcp.ResourceDescriptor, error] {
	return func(yield func(mcp.ResourceDescriptor, error) bool) {
		staticResources := mcpStaticResources()
		for _, key := range keys {
			var resource mcp.ResourceDescriptor
			switch key.Kind {
			case mcpResourceStatic:
				for i := range staticResources {
					if staticResources[i].Name == key.Value {
						resource = staticResources[i]
						break
					}
				}
			case mcpResourceRepo:
				resource = mcp.ResourceDescriptor{URI: key.URI, Name: "repo " + key.Value, Title: key.Value, MimeType: "application/json"}
			case mcpResourceTask:
				entry, ok := m.visibleTaskEntry(ctx, key.TaskID)
				if !ok {
					yield(mcp.ResourceDescriptor{}, errors.New("resource list changed while paging; restart without a cursor"))
					return
				}
				title, truncated := truncateMCPTaskTitle(entry.Task().Title())
				resource = mcp.ResourceDescriptor{URI: key.URI, Name: "task " + key.Value, Title: title, MimeType: "application/json"}
				if truncated {
					resource.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
				}
			}
			if !yield(resource, nil) {
				return
			}
		}
	}
}

func mcpStaticResources() []mcp.ResourceDescriptor {
	return []mcp.ResourceDescriptor{
		{URI: "caic://usage", Name: "usage", Title: "Usage", Description: "Local and provider usage", MimeType: "application/json"},
		{URI: "gomode://items", Name: "items", Title: "Items", Description: "Generic service item status for native clients", MimeType: "application/json"},
		{URI: "gomode://notifications", Name: "notifications", Title: "Notifications", Description: "Service notifications for native clients", MimeType: "application/json"},
	}
}

func (m *mcpRegistry) repositoryPaths() []string {
	var paths []string
	for checkout := range m.serverConfig.checkouts.Checkouts() {
		paths = append(paths, checkout.RelPath)
	}
	slices.Sort(paths)
	return paths
}

func (m *mcpRegistry) repositorySummaries(paths []string) (summaries []mcpRepoSummary, fieldsTruncated bool, err error) {
	summaries = make([]mcpRepoSummary, len(paths))
	for i, path := range paths {
		checkout, ok := m.serverConfig.checkouts.Checkout(path)
		if !ok {
			return nil, false, errMCPRepositoryListChanged
		}
		summary, truncated, summaryErr := m.repositorySummary(checkout)
		if summaryErr != nil {
			return nil, false, summaryErr
		}
		summaries[i] = summary
		fieldsTruncated = fieldsTruncated || truncated
	}
	return summaries, fieldsTruncated, nil
}

func (m *mcpRegistry) repositorySummary(checkout *repodomain.Checkout) (mcpRepoSummary, bool, error) {
	if err := validateMCPRepositoryPath(checkout.RelPath); err != nil {
		return mcpRepoSummary{}, false, err
	}
	var remote string
	var forgeKind v1.Forge
	if checkout.Repository != nil {
		remote = checkout.Repository.Remote
		var err error
		forgeKind, err = apiconv.RepoForge(checkout.Repository.ForgeKind)
		if err != nil {
			m.serverConfig.log.Error("convert repository forge for MCP", "repo", checkout.RelPath, "err", err)
		}
	}
	var ciStatus v1.CIStatus
	if m.serverConfig.repoStatus != nil {
		if status, ok := m.serverConfig.repoStatus.StatusFor(checkout.RelPath); ok {
			var err error
			ciStatus, err = apiconv.CIStatus(status)
			if err != nil {
				m.serverConfig.log.Error("convert repository CI status for MCP", "repo", checkout.RelPath, "err", err)
			}
		}
	}
	baseBranch, baseBranchTruncated := truncateMCPRepoField(checkout.BaseBranch)
	baseBranchRemote, baseBranchRemoteTruncated := truncateMCPRepoField(checkout.BaseBranchRemote)
	remoteURL, remoteURLTruncated := truncateMCPRepoField(git.RemoteToHTTPS(remote))
	return mcpRepoSummary{
		Path:       checkout.RelPath,
		BaseBranch: v1.BranchInfo{Name: baseBranch, Remote: baseBranchRemote},
		Forge:      forgeKind,
		RemoteURL:  remoteURL,
		CI:         ciStatus,
	}, baseBranchTruncated || baseBranchRemoteTruncated || remoteURLTruncated, nil
}

func validateMCPRepositoryPath(path string) error {
	if len(path) > maxMCPRepoField {
		return errMCPRepositoryPathTooLong
	}
	if !utf8.ValidString(path) {
		return errMCPRepositoryPathInvalidUTF8
	}
	return nil
}

func truncateMCPRepoField(value string) (string, bool) {
	original := value
	value = strings.ToValidUTF8(value, "�")
	truncated := truncateUTF8(value, maxMCPRepoField)
	return truncated, truncated != original
}

func mcpKeyCursor(key string) string {
	digest := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func decodeMCPKeyCursor(cursor string) ([sha256.Size]byte, bool) {
	var digest [sha256.Size]byte
	if len(cursor) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return digest, false
	}
	n, err := base64.RawURLEncoding.Strict().Decode(digest[:], []byte(cursor))
	return digest, err == nil && n == sha256.Size
}

func paginateMCPRepositories(paths []string, cursor string, pageSize int) (page []string, next string, err error) {
	start := 0
	if cursor != "" {
		anchor, ok := decodeMCPKeyCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		found := false
		for i := range paths {
			if sha256.Sum256([]byte(paths[i])) != anchor {
				continue
			}
			start = i + 1
			found = true
			break
		}
		if !found {
			return nil, "", errors.New("invalid cursor")
		}
	}
	if start >= len(paths) {
		return []string{}, "", nil
	}
	end := min(start+pageSize, len(paths))
	if end < len(paths) {
		next = mcpKeyCursor(paths[end-1])
	}
	return paths[start:end], next, nil
}

type mcpTaskCursor struct {
	Revision string `json:"revision"`
	TaskID   string `json:"taskID"`
}

type mcpResourceKind uint8

const (
	mcpResourceStatic mcpResourceKind = iota
	mcpResourceRepo
	mcpResourceTask
)

type mcpResourceKey struct {
	URI    string
	Value  string
	TaskID ksid.ID
	Kind   mcpResourceKind
}

func paginateMCPResourceKeys(keys []mcpResourceKey, cursor string) (page []mcpResourceKey, next string, err error) {
	start := 0
	if cursor != "" {
		anchor, ok := decodeMCPKeyCursor(cursor)
		if !ok {
			return nil, "", errors.New("invalid cursor")
		}
		found := false
		for i := range keys {
			if sha256.Sum256([]byte(keys[i].URI)) != anchor {
				continue
			}
			start = i + 1
			found = true
			break
		}
		if !found {
			return nil, "", errors.New("resource list changed while paging; restart without a cursor")
		}
	}
	if start >= len(keys) {
		return []mcpResourceKey{}, "", nil
	}
	end := min(start+mcpResourcePageSize, len(keys))
	if end < len(keys) {
		next = mcpKeyCursor(keys[end-1].URI)
	}
	return keys[start:end], next, nil
}

type mcpTaskKey struct {
	ID     ksid.ID
	Active bool
}

func paginateMCPTaskKeys(keys []mcpTaskKey, revision, cursor string, pageSize int) (page []mcpTaskKey, next string, start int, err error) {
	if cursor != "" {
		if len(cursor) > mcpTaskCursorMaxBytes {
			return nil, "", 0, errors.New("invalid cursor")
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		if decodeErr != nil {
			return nil, "", 0, errors.New("invalid cursor")
		}
		var anchor mcpTaskCursor
		if json.Unmarshal(data, &anchor) != nil || anchor.Revision != revision {
			return nil, "", 0, errors.New("invalid cursor")
		}
		anchorID, parseErr := ksid.Parse(anchor.TaskID)
		if parseErr != nil {
			return nil, "", 0, errors.New("invalid cursor")
		}
		found := false
		for i := range keys {
			if keys[i].ID != anchorID {
				continue
			}
			start = i + 1
			found = true
			break
		}
		if !found {
			return nil, "", 0, errors.New("invalid cursor")
		}
	}
	if start >= len(keys) {
		return []mcpTaskKey{}, "", start, nil
	}
	end := min(start+pageSize, len(keys))
	if end < len(keys) {
		next, err = encodeMCPTaskCursor(revision, keys[end-1].ID)
		if err != nil {
			return nil, "", 0, err
		}
	}
	return keys[start:end], next, start, nil
}

func encodeMCPTaskCursor(revision string, id ksid.ID) (string, error) {
	data, err := json.Marshal(mcpTaskCursor{Revision: revision, TaskID: id.String()})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func fitMCPJSONPage[T any](
	items []T,
	hasMore bool,
	maxBytes int,
	cursorForCount func(int) (string, error),
	outputForPage func([]T, string) any,
) (page []T, next string, err error) {
	if len(items) == 0 {
		return items, "", nil
	}
	pageFits := func(count int, includeCursor bool) (string, bool, error) {
		var cursor string
		var err error
		if includeCursor {
			cursor, err = cursorForCount(count)
			if err != nil {
				return "", false, err
			}
		}
		data, err := json.Marshal(outputForPage(items[:count], cursor))
		return cursor, len(data) <= maxBytes, err
	}

	if cursor, fits, err := pageFits(len(items), hasMore); err != nil || fits {
		return items, cursor, err
	}
	page, err = fitMCPJSONPrefix(items[:len(items)-1], maxBytes, func(page []T) (any, error) {
		if len(page) == 0 {
			return outputForPage(page, ""), nil
		}
		cursor, cursorErr := cursorForCount(len(page))
		return outputForPage(page, cursor), cursorErr
	})
	if err != nil {
		return nil, "", err
	}
	if len(page) == 0 {
		return nil, "", errors.New("first page item exceeds JSON response limit")
	}
	next, err = cursorForCount(len(page))
	return page, next, err
}

// fitMCPJSONPrefix returns the largest leading slice whose wrapped JSON fits.
// outputForPrefix must produce monotonically nondecreasing encoded sizes as the
// prefix grows so the binary search cannot skip a later, smaller representation.
func fitMCPJSONPrefix[T any](items []T, maxBytes int, outputForPrefix func([]T) (any, error)) ([]T, error) {
	best := -1
	low, high := 0, len(items)
	for low <= high {
		middle := low + (high-low)/2
		output, err := outputForPrefix(items[:middle])
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(output)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxBytes {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if best < 0 {
		return nil, errors.New("empty MCP JSON response exceeds limit")
	}
	return items[:best], nil
}

func boundedTextToolResult(text string) mcp.ToolResult[mcp.TextOutput] {
	text, truncated := truncateMCPText(text)
	if !truncated {
		return mcp.TextToolResult(text)
	}
	result := mcp.TextToolResult(text)
	result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	return result
}

func truncateMCPText(text string) (string, bool) {
	if len(text) <= mcpTextOutputMaxBytes {
		return text, false
	}
	const suffix = "\n\n[Output truncated. Use a more specific tool or resource to inspect the omitted data.]"
	end := mcpTextOutputMaxBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix, true
}

func boundedResourceJSON(uri string, value any) (mcp.ResourcesReadResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return mcp.ResourcesReadResult{}, err
	}
	// TODO(observability): Record MCP resource response bytes and oversize
	// rejections by stable resource family here. Do not label metrics with full
	// resource URIs because task IDs and repository names are unbounded.
	if len(data) > mcpResourceJSONMaxBytes {
		return mcp.ResourcesReadResult{}, errors.New("resource exceeds the 256 KiB response limit; read a more specific resource URI")
	}
	return mcp.ResourcesReadResult{
		ResultType: mcp.ResultTypeComplete,
		Contents:   []mcp.ResourceContent{{URI: uri, MimeType: "application/json", Text: string(data)}},
		TTLMS:      mcp.DefaultTTLMS,
		CacheScope: mcp.CacheScopePrivate,
	}, nil
}

func (m *mcpRegistry) resourceJSON(ctx context.Context, uri string, value any) (mcp.ResourcesReadResult, error) {
	result, err := boundedResourceJSON(uri, value)
	status := "ok"
	if err != nil {
		status = "error"
	}
	m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: status})
	return result, err
}

// visibleTaskEntry resolves a task only when ordinary task access permits it.
func (m *mcpRegistry) visibleTaskEntry(ctx context.Context, id ksid.ID) (*taskmgr.Entry, bool) {
	entry, ok := m.taskSvc.taskMgr.GetEntry(id.String())
	if !ok {
		return nil, false
	}
	if !taskAccessFromContext(ctx).canAccess(entry.Task()) {
		return nil, false
	}
	return entry, true
}

// inspectableTaskEntry resolves a task when its stable ID can be inspected.
func (m *mcpRegistry) inspectableTaskEntry(ctx context.Context, id ksid.ID) (*taskmgr.Entry, bool) {
	entry, ok := m.taskSvc.taskMgr.GetEntry(id.String())
	if !ok {
		return nil, false
	}
	if !taskAccessFromContext(ctx).canInspect(entry.Task()) {
		return nil, false
	}
	return entry, true
}

func (m *mcpRegistry) taskKeys(ctx context.Context) (keys []mcpTaskKey, revision string) {
	access := taskAccessFromContext(ctx)
	keys = make([]mcpTaskKey, 0)
	for _, entry := range m.taskSvc.taskMgr.Entries() {
		task := entry.Task()
		if !access.canAccess(task) {
			continue
		}
		state, err := apiconv.TaskState(task.GetState())
		if err != nil {
			m.taskSvc.log.ErrorContext(ctx, "convert task state for MCP listing", "task", task.ID, "err", err)
			continue
		}
		keys = append(keys, mcpTaskKey{ID: task.ID, Active: taskStateActive(state)})
	}
	slices.SortFunc(keys, func(a, b mcpTaskKey) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return a.ID.Compare(b.ID)
	})
	hash := sha256.New()
	var idBytes [8]byte
	for _, key := range keys {
		if key.Active {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		binary.BigEndian.PutUint64(idBytes[:], uint64(key.ID))
		_, _ = hash.Write(idBytes[:])
	}
	return keys, base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func (m *mcpRegistry) taskSummaries(ctx context.Context, keys []mcpTaskKey, start int, revision string) ([]mcpTaskSummary, error) {
	summaries := make([]mcpTaskSummary, len(keys))
	for i, key := range keys {
		entry, ok := m.visibleTaskEntry(ctx, key.ID)
		if !ok {
			return nil, errors.New("task ordering changed while listing")
		}
		task, err := taskDTO(ctx, entry, m.taskSvc.taskMgr, m.taskSvc.checkouts, m.taskSvc.authStore)
		if err != nil {
			return nil, err
		}
		summaries[i] = taskMCPSummary(start+i+1, &task)
	}
	_, currentRevision := m.taskKeys(ctx)
	if currentRevision != revision {
		return nil, errors.New("task ordering changed while listing")
	}
	return summaries, nil
}

func taskMCPSummary(number int, task *v1.Task) mcpTaskSummary {
	var additions int
	var deletions int
	for _, file := range task.DiffStat {
		additions += file.LinesAdded
		deletions += file.LinesDeleted
	}
	message := ""
	switch task.State {
	case v1.TaskStatePurged:
		message = task.Result
	case v1.TaskStateStopped:
		message = "container stopped"
	case v1.TaskStateCrashed, v1.TaskStateFailed:
		message = task.Error
	default:
	}
	model := task.ReportedModel
	if model == "" {
		model = task.RequestedModel
	}
	effort := task.ReportedEffort
	if effort == "" {
		effort = task.RequestedEffort
	}
	return mcpTaskSummary{
		TaskNumber:      number,
		TaskID:          task.ID.String(),
		Title:           truncateUTF8(taskTitle(task), maxMCPTaskTitle),
		State:           task.State,
		Harness:         task.Harness,
		Model:           truncateUTF8(model, maxTaskSummaryModel),
		Effort:          truncateUTF8(effort, maxTaskSummaryEffort),
		RuntimeName:     truncateUTF8(task.Runtime.RuntimeName, maxTaskSummaryRuntime),
		StatusMessage:   truncateUTF8(message, maxTaskSummaryMessage),
		ChangedFiles:    len(task.DiffStat),
		Additions:       additions,
		Deletions:       deletions,
		ForgePR:         task.ForgePR,
		CIStatus:        task.CIStatus,
		DurationSeconds: task.Duration,
		CostUSD:         task.CostUSD,
	}
}

func truncateMCPTaskTitle(title string) (string, bool) {
	truncated := truncateUTF8(title, maxMCPTaskTitle)
	return truncated, truncated != title
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const suffix = "…"
	end := maxBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix
}

func taskTitle(t *v1.Task) string {
	if t.Title != "" {
		return t.Title
	}
	return t.ID.String()
}

func taskNumberForID(taskList []v1.Task, id string) int {
	for i := range taskList {
		if taskList[i].ID.String() == id {
			return i + 1
		}
	}
	return 0
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func formatCost(cost float64) string {
	if cost == 0 {
		return "$0"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return formatUSD(cost)
}

func formatUSD(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

func formatBalance(currency string, total float64) string {
	switch strings.ToUpper(currency) {
	case "USD":
		return formatUSD(total)
	default:
		return fmt.Sprintf("%.2f %s", total, currency)
	}
}

var mcpToolScopes = map[string]string{
	"repos_list":                 mcpScopeRead,
	"tasks_list":                 mcpScopeTasksRead,
	"task_get_detail":            mcpScopeTasksRead,
	"agent_last_message":         mcpScopeTasksRead,
	"get_usage":                  mcpScopeRead,
	"task_send_message":          mcpScopeTasksWrite,
	"task_answer_question":       mcpScopeTasksWrite,
	"task_create":                mcpScopeTasksCreate,
	"task_fork":                  mcpScopeTasksWrite,
	"task_stop":                  mcpScopeTasksWrite,
	"task_revive":                mcpScopeTasksWrite,
	"task_purge":                 mcpScopeTasksAdmin,
	"clone_repo":                 mcpScopeReposWrite,
	"task_push_branch_to_remote": mcpScopeReposWrite,
	"task_fix_pr":                mcpScopeReposWrite,
	"bot_fix_ci":                 mcpScopeReposWrite,
}

var mcpForgeTools = map[string]struct{}{
	"task_push_branch_to_remote": {},
	"task_fix_pr":                {},
	"bot_fix_ci":                 {},
}

// authorizeTool enforces MCP scope and linked forge authority policy.
//
// Remote forge tools require linked forge authority. A GitHub-linked caic user
// may use user OAuth, server PAT, or GitHub App authority. GitLab-linked remote
// MCP users require a user token until there is an explicit server-side GitLab
// authority policy.
func (m *mcpRegistry) authorizeTool(ctx context.Context, name string) (string, bool) {
	if reason, ok := authorizeToolScope(ctx, name); !ok {
		return reason, false
	}
	if _, needsForge := mcpForgeTools[name]; needsForge && isRemoteMCP(ctx) && !userHasForgeAuthority(ctx) {
		return "linked GitHub identity or GitLab token is required for forge MCP tools", false
	}
	return "allow", true
}

func (m *mcpRegistry) authorizeResource(ctx context.Context, uri string) (string, bool) {
	if _, ok := strings.CutPrefix(uri, "caic://tasks/"); ok {
		id, err := mcpTaskResourceID(uri)
		if err != nil {
			return "task not found", false
		}
		if _, taskScoped := taskMCPTaskID(ctx); taskScoped {
			if _, ok := m.visibleTaskEntry(ctx, id); !ok {
				return "task not found", false
			}
			return "allow", true
		}
		if !mcpHasScope(ctx, mcpScopeTasksRead) {
			return "missing required MCP scope: " + mcpScopeTasksRead, false
		}
		if _, ok := m.visibleTaskEntry(ctx, id); !ok {
			return "task not found", false
		}
		return "allow", true
	}
	required := mcpScopeRead
	if uri == "gomode://items" || uri == "gomode://notifications" {
		required = mcpScopeTasksRead
	}
	if !mcpHasScope(ctx, required) {
		return "missing required MCP scope: " + required, false
	}
	return "allow", true
}

func mcpTaskResourceID(uri string) (ksid.ID, error) {
	rawID, ok := strings.CutPrefix(uri, "caic://tasks/")
	if !ok {
		return 0, errors.New("not a task resource")
	}
	return ksid.Parse(rawID)
}

type mcpTaskNumberArgs struct {
	TaskNumber int `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
}

type mcpTaskGetDetailArgs struct {
	Task taskID `json:"task" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number from tasks_list or stable task ID"`
}

// taskID accepts an MCP task number or a stable task ID.
type taskID string

// UnmarshalJSON decodes taskID from either its numeric task-number or string ID representation.
func (id *taskID) UnmarshalJSON(data []byte) error {
	var number int
	if err := json.Unmarshal(data, &number); err == nil {
		*id = taskID(strconv.Itoa(number))
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("task must be a task number or stable task ID")
	}
	*id = taskID(raw)
	return nil
}

// Decode resolves taskID as a task number up to maxTaskNumber or a stable ID.
func (id taskID) Decode(maxTaskNumber int) (ksid.ID, int, error) {
	raw := string(id)
	if number, err := strconv.Atoi(raw); err == nil {
		if number < 1 || number > maxTaskNumber {
			return 0, 0, fmt.Errorf("task number %d is outside 1 through %d", number, maxTaskNumber)
		}
		return 0, number, nil
	}
	parsed, err := ksid.Parse(raw)
	if err != nil {
		return 0, 0, errors.New("task must be a task number or stable task ID")
	}
	return parsed, 0, nil
}

// scopedMCPRegistry binds a server-owned task principal to an MCP registry.
// Agent session contexts do not carry an MCP principal, and a container must
// not supply its own task identity. This wrapper therefore makes the registry
// instance itself the task capability and scopes every registry method.
type scopedMCPRegistry struct {
	mcp.Registry

	principal *mcpPrincipal
}

func (r scopedMCPRegistry) Instructions(ctx context.Context) (string, error) {
	return r.Registry.Instructions(r.scopedContext(ctx))
}

func (r scopedMCPRegistry) Tools(ctx context.Context) ([]mcp.ToolDescriptor, error) {
	return r.Registry.Tools(r.scopedContext(ctx))
}

func (r scopedMCPRegistry) CallTool(ctx context.Context, name string, args json.RawMessage) (mcp.RawToolResult, error) {
	return r.Registry.CallTool(r.scopedContext(ctx), name, args)
}

func (r scopedMCPRegistry) ListResources(ctx context.Context, cursor string) (mcp.ResourcesListResult, error) {
	return r.Registry.ListResources(r.scopedContext(ctx), cursor)
}

func (r scopedMCPRegistry) ReadResource(ctx context.Context, uri string) (mcp.ResourcesReadResult, error) {
	return r.Registry.ReadResource(r.scopedContext(ctx), uri)
}

func (r scopedMCPRegistry) SubscribeResourceUpdates(ctx context.Context, filter mcp.SubscriptionFilter) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	return r.Registry.SubscribeResourceUpdates(r.scopedContext(ctx), filter)
}

func (r scopedMCPRegistry) scopedContext(ctx context.Context) context.Context {
	return newMCPPrincipalContext(ctx, r.principal)
}

func authorizeToolScope(ctx context.Context, name string) (string, bool) {
	if _, ok := taskMCPTaskID(ctx); ok {
		switch name {
		case "task_create", "tasks_list", "task_get_detail", "task_fork", "task_stop", "task_purge":
			return "allow", true
		}
		return "task-scoped MCP only permits task_create, tasks_list, task_get_detail, task_fork, task_stop, and task_purge", false
	}
	required := requiredScopeForTool(name)
	if required == "" {
		if isRemoteMCP(ctx) {
			return "MCP tool is missing a scope policy", false
		}
		return "allow", true
	}
	if !mcpHasScope(ctx, required) {
		return "missing required MCP scope: " + required, false
	}
	return "allow", true
}

func requiredScopeForTool(name string) string {
	return mcpToolScopes[name]
}

func isRemoteMCP(ctx context.Context) bool {
	p, ok := mcpPrincipalFromContext(ctx)
	return ok && p.Remote
}

func userHasForgeAuthority(ctx context.Context) bool {
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		return false
	}
	if u.Provider == auth.ProviderGitHub {
		return true
	}
	return u.Provider == auth.ProviderGitLab && u.AccessToken != ""
}

func mcpScopeChallenge(scope string) string {
	if scope == "" {
		scope = mcpScopeRead
	}
	return oauth.BearerScopeChallenge(scope)
}

type subscriptionSources struct {
	taskC        <-chan struct{}
	taskVersion  uint64
	repoC        <-chan struct{}
	repoStatusC  <-chan struct{}
	usagePolling bool

	resourcesListChanged bool
	taskResourceIDs      []ksid.ID
	taskUpdateURIs       []string
	repoResourceURIs     []string
	usageResourceURIs    []string
}

func (s *subscriptionSources) taskUpdate(ctx context.Context, registry *mcpRegistry) mcp.ResourceUpdate {
	uris := append([]string(nil), s.taskUpdateURIs...)
	resourcesListChanged := s.resourcesListChanged
	for _, id := range s.taskResourceIDs {
		if _, ok := registry.visibleTaskEntry(ctx, id); ok {
			uris = append(uris, "caic://tasks/"+id.String())
		} else {
			resourcesListChanged = true
		}
	}
	return mcp.ResourceUpdate{ResourcesListChanged: resourcesListChanged, ResourceURIs: uris}
}

func (s *subscriptionSources) repoUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourcesListChanged: s.resourcesListChanged, ResourceURIs: s.repoResourceURIs}
}

func (s *subscriptionSources) usageUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourcesListChanged: s.resourcesListChanged, ResourceURIs: s.usageResourceURIs}
}
