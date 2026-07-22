// MCP tool registry, schemas, resource catalog, and subscription invalidation.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	providerusage "github.com/caic-xyz/caic/backend/internal/usage"
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
At session start this prompt includes a snapshot of all current tasks. Use it to answer questions about task status without calling tasks_list first. Call task_get_detail when the user asks for specifics (recent events, diffs).

## Behavior guidelines
- Always speak fast.
- Do not ask follow-up questions like "would you like me to…" or "should I also…". Answer the user's request and stop. Only ask a question if the user's request is genuinely ambiguous or you misunderstood something critical — then ask the single clarifying question needed and nothing else.
- Be concise. The user is often away from the screen.
- Summarize task status: state and what the agent is doing. Only mention elapsed time or cost when the user specifically asks.
- When an agent is asking, read the question and options clearly, wait for the verbal answer, then call task_answer_question.
- When creating a task, omit harness, model, and effort unless the user explicitly asks for an override. caic fills an omitted harness from saved preferences; omitted model and effort use the selected harness defaults. Never pair one harness's model or effort with another harness. Confirm repo and prompt before creating.
- Refer to tasks by title.
- Proactively notify the user when tasks finish or need input.
- Free tools: agent_last_message, tasks_list, task_get_detail, get_usage. Call them whenever useful without asking.
- When the user asks for a status update, call agent_last_message for each waiting/asking task to get latest output.
- For safety issues during sync, describe each issue and ask whether to force.`

type mcpRegistry struct {
	serverConfig  *serverHandlers // required
	taskSvc       *taskService
	ci            *ciHandlers
	usage         *usageHandlers
	notifications *notificationFeed
	audit         *auditStore
}

func (m *mcpRegistry) Instructions(ctx context.Context) (string, error) {
	parts := make([]string, 0, 2)
	parts = append(parts, caicVoiceSystemInstruction, m.voiceSessionContext(ctx))
	return strings.Join(parts, "\n\n"), nil
}

func (m *mcpRegistry) Tools(context.Context) ([]mcp.ToolDescriptor, error) {
	specs := m.specs()
	tools := make([]mcp.ToolDescriptor, len(specs))
	for i, s := range specs {
		tools[i] = mcp.ToolDescriptor{Name: s.Name, Title: s.Title, Description: s.Description, InputSchema: s.InputSchema, OutputSchema: s.OutputSchema, Annotations: s.Annotations}
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
			return mcp.RawToolResult{Meta: mcp.MetaObject{"mcp/www_authenticate": []string{mcpScopeChallenge(requiredScopeForTool(name))}}, Structured: mcp.ErrorOutput{Error: authResult}, IsError: true}, nil
		}
		res, err := s.Handler(ctx, argsJSON)
		status := "ok"
		if err != nil {
			status = "error"
		} else if res.IsError {
			status = "tool_error"
		}
		res.Structured = redactForJSON(res.Structured)
		m.audit.record(ctx, &auditEvent{Operation: "tools/call", Name: name, Args: auditArgsSummary(argsJSON), Decision: "allow", Status: status})
		return res, err
	}
	return mcp.RawToolResult{}, mcp.ErrInvalidParams("unknown tool: %s", name)
}

func (m *mcpRegistry) ListResources(ctx context.Context) mcp.ResourcesListResult {
	taskList, repoList := m.currentTasksAndRepos(ctx)
	resources := make([]mcp.ResourceDescriptor, 0, 3+len(repoList)+len(taskList))
	resources = append(resources,
		mcp.ResourceDescriptor{URI: "caic://repos", Name: "repos", Title: "Repositories", Description: "Managed repository summary", MimeType: "application/json"},
		mcp.ResourceDescriptor{URI: "caic://tasks", Name: "tasks", Title: "Tasks", Description: "Coding task summary", MimeType: "application/json"},
		mcp.ResourceDescriptor{URI: "caic://usage", Name: "usage", Title: "Usage", Description: "Local and provider usage", MimeType: "application/json"},
		mcp.ResourceDescriptor{URI: "gomode://items", Name: "items", Title: "Items", Description: "Generic service item status for native clients", MimeType: "application/json"},
		mcp.ResourceDescriptor{URI: "gomode://notifications", Name: "notifications", Title: "Notifications", Description: "Service notifications for native clients", MimeType: "application/json"},
	)
	for i := range repoList {
		repo := &repoList[i]
		resources = append(resources, mcp.ResourceDescriptor{URI: "caic://repos/" + url.PathEscape(repo.Path), Name: "repo " + repo.Path, Title: repo.Path, MimeType: "application/json"})
	}
	for i := range taskList {
		task := &taskList[i]
		id := task.ID.String()
		resources = append(resources, mcp.ResourceDescriptor{URI: "caic://tasks/" + id, Name: "task " + id, Title: task.Title, MimeType: "application/json"})
	}
	return mcp.ResourcesListResult{ResultType: mcp.ResultTypeComplete, Resources: resources, TTLMS: mcp.DefaultTTLMS, CacheScope: mcp.CacheScopePrivate}
}

func (m *mcpRegistry) ReadResource(ctx context.Context, uri string) (mcp.ResourcesReadResult, error) {
	if authResult, ok := authorizeResource(ctx, uri); !ok {
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: authResult})
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("%s", authResult)
	}
	taskList, repoList := m.currentTasksAndRepos(ctx)
	switch {
	case uri == "caic://repos":
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
		return redactedResourceJSON(uri, repoList)
	case uri == "caic://tasks":
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
		return redactedResourceJSON(uri, taskList)
	case uri == "caic://usage":
		usage := m.usage.buildResp(ctx)
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
		return redactedResourceJSON(uri, usage)
	case uri == "gomode://items":
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
		return redactedResourceJSON(uri, serviceItems(taskList))
	case uri == "gomode://notifications":
		notifications := m.notifications.notifications(ctx, taskList, m.usage.buildResp(ctx))
		m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
		return redactedResourceJSON(uri, notifications)
	case strings.HasPrefix(uri, "caic://repos/"):
		name, err := url.PathUnescape(strings.TrimPrefix(uri, "caic://repos/"))
		if err != nil {
			return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("invalid repo uri: %w", err)
		}
		for i := range repoList {
			if repoList[i].Path == name {
				m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
				return redactedResourceJSON(uri, repoList[i])
			}
		}
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("repo not found: %s", name)
	case strings.HasPrefix(uri, "caic://tasks/"):
		id := strings.TrimPrefix(uri, "caic://tasks/")
		for i := range taskList {
			if taskList[i].ID.String() == id {
				m.audit.record(ctx, &auditEvent{Operation: "resources/read", Name: uri, Decision: "allow", Status: "ok"})
				return redactedResourceJSON(uri, taskList[i])
			}
		}
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("task not found: %s", id)
	default:
		return mcp.ResourcesReadResult{}, mcp.ErrInvalidParams("unknown resource: %s", uri)
	}
}

func (m *mcpRegistry) SubscribeResourceUpdates(ctx context.Context, filter mcp.SubscriptionFilter) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	sources, err := m.subscriptionSources(filter)
	if err != nil {
		return nil, err
	}
	taskC := sources.taskC
	repoC := sources.repoC
	repoStatusC := sources.repoStatusC
	return func(yield func(mcp.ResourceUpdate, error) bool) {
		var usageC <-chan time.Time
		if sources.usagePolling {
			usageTicker := time.NewTicker(providerusage.CacheTTL)
			defer usageTicker.Stop()
			usageC = usageTicker.C
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-taskC:
				if !yield(sources.taskUpdate(), nil) {
					return
				}
				if m.taskSvc != nil && m.taskSvc.taskMgr != nil {
					taskC = m.taskSvc.taskMgr.Changed()
				} else {
					taskC = nil
				}
			case <-repoC:
				if !yield(sources.repoUpdate(), nil) {
					return
				}
				repoC = m.serverConfig.repoSvc.Changed()
			case <-repoStatusC:
				if !yield(sources.repoUpdate(), nil) {
					return
				}
				repoStatusC = m.serverConfig.repoStatus.Changed()
			case <-usageC:
				if !yield(sources.usageUpdate(), nil) {
					return
				}
			}
			if taskC == nil && repoC == nil && repoStatusC == nil && usageC == nil {
				return
			}
		}
	}, nil
}

func (m *mcpRegistry) voiceSessionContext(ctx context.Context) string {
	parts := make([]string, 0, 3)
	prefs := m.serverConfig.prefs.Get(userIDFromCtx(ctx))
	if len(prefs.Repositories) > 0 {
		parts = append(parts, "[Default repo: "+prefs.Repositories[0].Path+"]")
	}
	if prefs.Harness != "" {
		parts = append(parts, "[Default harness: "+prefs.Harness+"]")
	}
	taskList := m.taskSvc.taskListSnapshot(ctx)
	if len(taskList) == 0 {
		if len(parts) == 0 {
			return "[No active tasks]"
		}
		return strings.Join(parts, "\n")
	}
	lines := make([]string, len(taskList))
	for i := range taskList {
		t := &taskList[i]
		lines[i] = fmt.Sprintf(
			"- Task #%d: %s (%s, %s, %s, %s)",
			i+1,
			taskTitle(t),
			t.State,
			formatElapsed(time.Duration(t.Duration*float64(time.Second))),
			formatCost(t.CostUSD),
			t.Harness,
		)
	}
	parts = append(parts, "[Current tasks at session start]\n"+strings.Join(lines, "\n"))
	return strings.Join(parts, "\n")
}

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
		annotateTool(mcp.NewToolSpec("tasks_list", "List tasks", "List all current coding tasks with their status, cost, and duration.", m.handleTasksList), mcp.ToolAnnotations{Title: "List tasks", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		createSpec,
		annotateTool(mcp.NewToolSpec("task_get_detail", "Get task detail", "Get recent activity and status details for a task by its number.", m.handleTaskGetDetail), mcp.ToolAnnotations{Title: "Get task detail", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_send_message", "Send task message", "Send a text message to a waiting or asking agent by task number.", m.handleTaskSendMessage), mcp.ToolAnnotations{Title: "Send task message", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_answer_question", "Answer task question", "Answer an agent's question by task number. The agent is in 'asking' state.", m.handleTaskAnswerQuestion), mcp.ToolAnnotations{Title: "Answer task question", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_push_branch_to_remote", "Push task branch", "Sync or push a task's changes to GitHub. Push to task branch (default) or squash-push to main.", m.handleTaskPushBranchToRemote), mcp.ToolAnnotations{Title: "Push task branch", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("task_stop", "Stop task", "Stop a running or waiting task. The container is preserved and can be revived later.", m.handleTaskStop), mcp.ToolAnnotations{Title: "Stop task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_purge", "Purge task", "Permanently delete a stopped or crashed task's container. Cannot be undone.", m.handleTaskPurge), mcp.ToolAnnotations{Title: "Purge task", DestructiveHint: true, OpenWorldHint: false}),
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

func (m *mcpRegistry) currentTasksAndRepos(ctx context.Context) ([]v1.Task, []v1.Repo) {
	taskList := m.taskSvc.taskListSnapshot(ctx)
	repoList := repoListFromSnapshot(m.serverConfig.repoSvc.Snapshot(), m.serverConfig.repoStatus)
	return taskList, *repoList
}

func (m *mcpRegistry) subscriptionSources(filter mcp.SubscriptionFilter) (subscriptionSources, error) {
	var sources subscriptionSources
	hasFilter := false
	for _, uri := range filter.ResourceSubscriptions {
		hasFilter = true
		switch {
		case uri == "caic://tasks" || strings.HasPrefix(uri, "caic://tasks/"):
			if m.taskSvc != nil && m.taskSvc.taskMgr != nil {
				sources.taskC = m.taskSvc.taskMgr.Changed()
				sources.taskResourceURIs = append(sources.taskResourceURIs, uri)
			} else {
				return subscriptionSources{}, errors.New("task subscription notifier unavailable")
			}
		case uri == "caic://repos" || strings.HasPrefix(uri, "caic://repos/"):
			sources.repoC = m.serverConfig.repoSvc.Changed()
			sources.repoStatusC = m.serverConfig.repoStatus.Changed()
			sources.repoResourceURIs = append(sources.repoResourceURIs, uri)
		case uri == "gomode://items":
			if m.taskSvc != nil && m.taskSvc.taskMgr != nil {
				sources.taskC = m.taskSvc.taskMgr.Changed()
				sources.taskResourceURIs = append(sources.taskResourceURIs, uri)
			} else {
				return subscriptionSources{}, errors.New("item subscription notifier unavailable")
			}
		case uri == "gomode://notifications":
			sources.usagePolling = true
			sources.usageResourceURIs = append(sources.usageResourceURIs, uri)
		default:
			return subscriptionSources{}, mcp.ErrInvalidParams("unsupported resource subscription: %s", uri)
		}
	}
	if filter.ResourcesListChanged {
		hasFilter = true
		if m.taskSvc != nil && m.taskSvc.taskMgr != nil {
			sources.taskC = m.taskSvc.taskMgr.Changed()
		} else {
			return subscriptionSources{}, errors.New("task subscription notifier unavailable")
		}
		sources.repoC = m.serverConfig.repoSvc.Changed()
		sources.repoStatusC = m.serverConfig.repoStatus.Changed()
		sources.resourcesListChanged = true
	}
	if !hasFilter {
		return subscriptionSources{}, mcp.ErrInvalidParams("subscription filter is empty")
	}
	return sources, nil
}

func (m *mcpRegistry) handleTasksList(ctx context.Context, _ struct{}) mcp.ToolResult[mcp.TextOutput] {
	taskList := m.taskSvc.taskListSnapshot(ctx)
	if len(taskList) == 0 {
		return mcp.TextToolResult("No tasks running.")
	}
	lines := make([]string, len(taskList))
	for i := range taskList {
		lines[i] = taskSummaryLine(i+1, &taskList[i])
	}
	return mcp.TextToolResult("## Tasks\n\n" + strings.Join(lines, "\n"))
}

func domainToolError[T any](err error) mcp.ToolResult[T] {
	if err == nil {
		return mcp.ToolResult[T]{}
	}
	if ews, ok := errors.AsType[api.ErrorWithStatus](err); ok {
		return mcp.ToolError[T](ews.Error())
	}
	return mcp.ToolError[T](err.Error())
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
		return mcp.ToolError[mcpTaskCreatedOutput]("Missing required parameter: prompt")
	}
	if len(args.Repos) == 0 {
		return mcp.ToolError[mcpTaskCreatedOutput]("Missing required parameter: repos")
	}
	harness := m.resolveTaskCreateHarness(ctx, args.Harness)
	req := &v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: args.Prompt},
		Repos:         make([]v1.RepoSpec, len(args.Repos)),
		Harness:       v1.Harness(harness),
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
		return domainToolError[mcpTaskCreatedOutput](err)
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
	if num > 0 {
		return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created task #%d: %s", num, title), TaskNumber: num, TaskID: resp.ID.String()})
	}
	return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: "Created task: " + title, TaskID: resp.ID.String()})
}

func (m *mcpRegistry) resolveTaskCreateHarness(ctx context.Context, harness string) string {
	if harness != "" {
		return harness
	}
	prefs := m.serverConfig.prefs.Get(userIDFromCtx(ctx))
	if prefs.Harness != "" {
		return prefs.Harness
	}
	return m.firstHarness(ctx)
}

func (m *mcpRegistry) firstHarness(ctx context.Context) string {
	harnesses, err := m.serverConfig.listHarnesses(ctx, nil)
	if err != nil || harnesses == nil || len(*harnesses) == 0 {
		return ""
	}
	return (*harnesses)[0].Name
}

type mcpTaskNumberArgs struct {
	TaskNumber int `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
}

func (m *mcpRegistry) handleTaskGetDetail(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	if args.TaskNumber == 0 {
		return mcp.ToolError[mcp.TextOutput]("Missing required integer: task_number")
	}
	t, ok := m.taskByNumber(ctx, args.TaskNumber)
	if !ok {
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
	}
	lines := []string{
		fmt.Sprintf("## Task #%d: %s", args.TaskNumber, taskTitle(&t)),
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
		paths := make([]string, len(t.DiffStat))
		for i, d := range t.DiffStat {
			paths[i] = d.Path
		}
		lines = append(lines, "**Changed:** "+strings.Join(paths, ", "))
	}
	return mcp.TextToolResult(strings.TrimSpace(strings.Join(lines, "\n")))
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
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
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
	issueLines := make([]string, len(resp.SafetyIssues))
	for i, issue := range resp.SafetyIssues {
		issueLines[i] = fmt.Sprintf("- **%s** %s: %s", issue.Kind, issue.File, issue.Detail)
	}
	return mcp.TextToolResult(verb + " with safety issues:\n" + strings.Join(issueLines, "\n"))
}

func (m *mcpRegistry) handleTaskStop(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
	}
	_, err := m.taskSvc.stopTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Stopping task #%d.", num))
}

func (m *mcpRegistry) handleTaskPurge(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
	}
	_, err := m.taskSvc.purgeTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Purged task #%d.", num))
}

func (m *mcpRegistry) handleTaskRevive(ctx context.Context, args mcpTaskNumberArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
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
	Model      string `json:"model,omitempty"   jsonschema_description:"Override model (optional, inherits from source if omitted)"`
}

func (m *mcpRegistry) handleTaskFork(ctx context.Context, args mcpTaskForkArgs) mcp.ToolResult[mcpTaskForkOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return mcp.ToolError[mcpTaskForkOutput]("Unknown task number")
	}
	if args.Prompt == "" {
		return mcp.ToolError[mcpTaskForkOutput]("Missing required parameter: prompt")
	}
	req := &v1.ForkTaskReq{Prompt: v1.Prompt{Text: args.Prompt}, Harness: v1.Harness(args.Harness), Model: args.Model}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTaskForkOutput](err)
	}
	resp, err := m.taskSvc.forkTask(ctx, entry, req)
	if err != nil {
		return domainToolError[mcpTaskForkOutput](err)
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
			parts = append(parts, fmt.Sprintf("%s: %.0f%%", rl.Window, rl.UsedPct))
		}
		if len(parts) > 0 {
			lines = append(lines, pq.Label+": "+strings.Join(parts, ", "))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "No usage data available.")
	}
	return mcp.TextToolResult(strings.Join(lines, "\n"))
}

type mcpCloneRepoArgs struct {
	URL  string `json:"url"            jsonschema_description:"The git repository URL to clone"`
	Path string `json:"path,omitempty" jsonschema_description:"Local directory name (optional, derived from URL if omitted)"`
}

func (m *mcpRegistry) handleCloneRepo(ctx context.Context, args mcpCloneRepoArgs) mcp.ToolResult[mcp.TextOutput] {
	if args.URL == "" {
		return mcp.ToolError[mcp.TextOutput]("Missing required parameter: url")
	}
	req := &v1.CloneRepoReq{URL: args.URL, Path: args.Path}
	if err := req.Validate(); err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	repo, err := m.serverConfig.cloneRepo(ctx, req)
	if err != nil {
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
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
	}
	m.taskSvc.taskMgr.LoadMessagesOnDemand(entry)
	history, _, unsub := entry.Task().Subscribe(ctx)
	unsub()
	for _, msg := range slices.Backward(history) {
		switch msg := msg.(type) {
		case *agent.ResultMessage:
			if msg.Result != "" {
				return mcp.TextToolResult(fmt.Sprintf("Task #%d result: %s", num, msg.Result))
			}
		case *agent.AskMessage:
			if len(msg.Questions) > 0 {
				q := msg.Questions[0]
				options := make([]string, len(q.Options))
				for i, opt := range q.Options {
					options[i] = opt.Label
				}
				suffix := ""
				if len(options) > 0 {
					suffix = " Options: " + strings.Join(options, ", ")
				}
				return mcp.TextToolResult(fmt.Sprintf("Task #%d is asking: %s%s", num, q.Question, suffix))
			}
		case *agent.TextMessage:
			if msg.Text != "" {
				return mcp.TextToolResult(fmt.Sprintf("Last message from task #%d: %s", num, msg.Text))
			}
		}
	}
	return mcp.TextToolResult(fmt.Sprintf("No messages from task #%d yet.", num))
}

type mcpTaskFixPRArgs struct {
	TaskNumber int `json:"task_number" jsonschema_description:"The task number whose PR CI should be fixed"`
}

func (m *mcpRegistry) handleTaskFixPR(ctx context.Context, args mcpTaskFixPRArgs) mcp.ToolResult[mcp.TextOutput] {
	num, entry, ok := m.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
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
		return mcp.ToolError[mcpTaskCreatedOutput]("Missing required parameter: repo")
	}
	resp, err := m.ci.fixCI(ctx, &v1.BotFixCIReq{Repo: args.Repo})
	if err != nil {
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
		return mcp.ToolError[mcp.TextOutput]("Unknown task number")
	}
	if args.Message == "" {
		return mcp.ToolError[mcp.TextOutput]("Missing required parameter: " + field)
	}
	_, err := m.taskSvc.sendInput(ctx, entry, &v1.InputReq{Prompt: v1.Prompt{Text: args.Message}})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf(format, num))
}

func (m *mcpRegistry) taskByNumber(ctx context.Context, num int) (v1.Task, bool) {
	taskList := m.taskSvc.taskListSnapshot(ctx)
	if num < 1 || num > len(taskList) {
		return v1.Task{}, false
	}
	return taskList[num-1], true
}

func (m *mcpRegistry) entryByNumber(ctx context.Context, num int) (int, *taskmgr.Entry, bool) {
	if num == 0 {
		return 0, nil, false
	}
	t, ok := m.taskByNumber(ctx, num)
	if !ok {
		return num, nil, false
	}
	entry, ok := m.taskSvc.taskMgr.GetEntry(t.ID.String())
	return num, entry, ok
}

// Static schema builders. Dynamic repos, harnesses, and preferences are read on
// demand by resources and tool handlers instead of embedded in tool schemas.

func buildTaskCreateSchema() *jsonschema.Schema {
	minOne := uint64(1)
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The task description/prompt for the coding agent"})
	props.Set("repos", &jsonschema.Schema{Type: "array", Description: "Repositories to work in (one or more). Read caic://repos first if you need the current repository list.", Items: &jsonschema.Schema{Type: "string"}, MinItems: &minOne})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model override (optional). Omit unless the user explicitly requested a model; omitted means the selected harness default."})
	props.Set("effort", &jsonschema.Schema{Type: "string", Description: "Thinking effort override (optional). Omit unless the user explicitly requested effort; omitted means the selected harness default."})
	props.Set("harness", &jsonschema.Schema{Type: "string", Description: "Agent harness override (optional). Omit to use the saved default harness."})
	props.Set("runtimeName", &jsonschema.Schema{Type: "string", Description: "Runtime backend name (optional). Use docker or podman when multiple runtimes are available."})
	props.Set("display", &jsonschema.Schema{Type: "boolean", Description: "Enable virtual display (VNC) for this task"})
	props.Set("tailscale", &jsonschema.Schema{Type: "boolean", Description: "Enable Tailscale networking for this task"})
	props.Set("usb", &jsonschema.Schema{Type: "boolean", Description: "Enable USB passthrough for this task"})
	props.Set("sudo", &jsonschema.Schema{Type: "boolean", Description: "Enable root access via sudo with a random password"})
	props.Set("gitHubToken", &jsonschema.Schema{Type: "boolean", Description: "Enable GitHub token injection for this task"})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"prompt", "repos"}}
}

func buildTaskForkSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("task_number", &jsonschema.Schema{Type: "integer", Description: "The task number to fork, e.g. 1 for task #1"})
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The initial prompt for the forked task"})
	props.Set("harness", &jsonschema.Schema{Type: "string", Description: "Override harness (optional, inherits from source if omitted)"})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Override model (optional, inherits from source if omitted)"})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"task_number", "prompt"}}
	mcp.AddHeaderToProperty(schema, "task_number", "Task-Number")
	return schema
}

func buildBotFixCISchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("repo", &jsonschema.Schema{Type: "string", Description: "Repository path to fix CI for. Read caic://repos first if you need the current repository list."})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"repo"}}
	mcp.AddHeaderToProperty(schema, "repo", "Repo")
	return schema
}

// Support code

func taskSummaryLine(num int, t *v1.Task) string {
	var extras []string
	if t.ForgePR != 0 && t.ForgePRState != v1.ForgePRStateClosed && t.ForgePRState != v1.ForgePRStateMerged {
		extras = append(extras, fmt.Sprintf("PR #%d", t.ForgePR))
	}
	if t.CIStatus != "" {
		extras = append(extras, "CI: "+string(t.CIStatus))
	}
	if t.Runtime.RuntimeName != "" {
		extras = append(extras, "runtime: "+t.Runtime.RuntimeName)
	}
	extrasStr := ""
	if len(extras) > 0 {
		extrasStr = ", " + strings.Join(extras, ", ")
	}
	base := fmt.Sprintf("%d. **%s** — %s, %s, %s, %s%s%s", num, taskTitle(t), t.State, formatElapsed(time.Duration(t.Duration*float64(time.Second))), formatCost(t.CostUSD), t.Harness, diffStatSummary(t), extrasStr)
	if t.State == v1.TaskStatePurged && t.Result != "" {
		return base + " — " + truncate(t.Result, 120)
	}
	if t.State == v1.TaskStateStopped {
		return base + " — container stopped"
	}
	if t.State == v1.TaskStateCrashed && t.Error != "" {
		return base + " — " + t.Error
	}
	if t.State == v1.TaskStateFailed && t.Error != "" {
		return base + " — " + t.Error
	}
	return base
}

func diffStatSummary(t *v1.Task) string {
	if len(t.DiffStat) == 0 {
		return ""
	}
	var added int
	var deleted int
	for _, f := range t.DiffStat {
		added += f.Added
		deleted += f.Deleted
	}
	fileWord := "files"
	if len(t.DiffStat) == 1 {
		fileWord = "file"
	}
	return fmt.Sprintf(", +%d -%d in %d %s", added, deleted, len(t.DiffStat), fileWord)
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

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

var mcpToolScopes = map[string]string{
	"tasks_list":                 mcpScopeTasksRead,
	"task_get_detail":            mcpScopeTasksRead,
	"agent_last_message":         mcpScopeTasksRead,
	"get_usage":                  mcpScopeRead,
	"task_send_message":          mcpScopeTasksWrite,
	"task_answer_question":       mcpScopeTasksWrite,
	"task_create":                mcpScopeTasksWrite,
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
	if _, needsForge := mcpForgeTools[name]; needsForge && isRemoteMCP(ctx) && !userHasForgeAuthority(ctx) {
		return "linked GitHub identity or GitLab token is required for forge MCP tools", false
	}
	return "allow", true
}

func authorizeResource(ctx context.Context, uri string) (string, bool) {
	required := mcpScopeRead
	if uri == "caic://tasks" || strings.HasPrefix(uri, "caic://tasks/") || uri == "gomode://items" {
		required = mcpScopeTasksRead
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

func redactedResourceJSON(uri string, value any) (mcp.ResourcesReadResult, error) {
	return mcp.ResourceJSON(uri, redactForJSON(value))
}

type subscriptionSources struct {
	taskC        <-chan struct{}
	repoC        <-chan struct{}
	repoStatusC  <-chan struct{}
	usagePolling bool

	resourcesListChanged bool
	taskResourceURIs     []string
	repoResourceURIs     []string
	usageResourceURIs    []string
}

func (s *subscriptionSources) taskUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourcesListChanged: s.resourcesListChanged, ResourceURIs: s.taskResourceURIs}
}

func (s *subscriptionSources) repoUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourcesListChanged: s.resourcesListChanged, ResourceURIs: s.repoResourceURIs}
}

func (s *subscriptionSources) usageUpdate() mcp.ResourceUpdate {
	return mcp.ResourceUpdate{ResourcesListChanged: s.resourcesListChanged, ResourceURIs: s.usageResourceURIs}
}
