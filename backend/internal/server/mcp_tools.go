// MCP tool catalogs, schemas, execution, bounded task details, and task command adapters.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	"github.com/caic-xyz/caic/backend/internal/server/data/audit"
	taskpkg "github.com/caic-xyz/caic/backend/internal/task"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/metrics"
	"github.com/invopop/jsonschema"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/ksid"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

type mcpTools struct {
	serverConfig *serverHandlers
	taskSvc      *taskService
	ci           *ciHandlers
	usage        *usageHandlers
	audit        *auditStore
	metrics      metrics.Recorder
}

func (m *mcpTools) Tools(ctx context.Context) ([]mcp.ToolDescriptor, error) {
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

func (m *mcpTools) CallTool(ctx context.Context, params *mcp.ToolsCallParams) (mcp.RawToolResult, error) {
	for _, s := range m.specs() {
		if s.Name != params.Name {
			continue
		}
		if authResult, ok := m.authorizeTool(ctx, params.Name); !ok {
			m.audit.record(ctx, &audit.Event{Operation: "tools/call", Name: params.Name, Args: auditArgsSummary(params.Arguments), Decision: authResult})
			return mcp.RawToolResult{Meta: mcp.MetaObject{
				"mcp/www_authenticate":   []string{mcpScopeChallenge(requiredScopeForTool(params.Name))},
				mcp.ToolErrorCodeMetaKey: string(api.CodeUnauthorized),
			}, Structured: mcp.ErrorOutput{Error: authResult}, IsError: true}, nil
		}
		start := time.Now()
		res, err := s.Handler(ctx, params)
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
		m.metrics.Record(ctx, "mcp.tool."+params.Name, outcome, metrics.Duration(time.Since(start)))
		m.audit.record(ctx, &audit.Event{Operation: "tools/call", Name: params.Name, Args: auditArgsSummary(params.Arguments), Decision: "allow", Status: status})
		return res, err
	}
	return mcp.RawToolResult{}, mcp.ErrInvalidParams("unknown tool: %s", params.Name)
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
// github.com/maruel/gomode/voicegateway/voicertc/AGENTS.md.
func (m *mcpTools) specs() []mcp.ToolSpec {
	createSpec := mcp.NewToolSpec("task_create", "Create task", "Create a new coding task with zero or more repositories. Confirm repositories and prompt with the user before calling. Omit harness, model, and effort unless the user explicitly asks for an override; caic resolves an omitted harness from saved preferences and leaves omitted model/effort to harness defaults.", m.handleTaskCreate)
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
		annotateTool(mcp.NewToolSpec("harnesses_list", "List harnesses", "List available coding-agent harnesses and the models each accepts, with the reasoning effort values every model supports. Call this to choose a valid model or effort for task_create.", m.handleHarnessesList), mcp.ToolAnnotations{Title: "List harnesses", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		createSpec,
		annotateTool(mcp.NewToolSpec("task_get_detail", "Get task detail", "Get task status by task number or stable ID. Optionally include committed and uncommitted file diffstats with branch and upstream divergence. Output is limited to 32 KiB with an explicit truncation notice.", m.handleTaskGetDetail), mcp.ToolAnnotations{Title: "Get task detail", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_send_message", "Send task message", "Send a text message to a waiting or asking agent by task number.", m.handleTaskSendMessage), mcp.ToolAnnotations{Title: "Send task message", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_answer_question", "Answer task question", "Answer an agent's question by task number. The agent is in 'asking' state.", m.handleTaskAnswerQuestion), mcp.ToolAnnotations{Title: "Answer task question", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_push_branch_to_remote", "Push task branch", "Sync or push a task's existing commits to GitHub. Push to task branch (default) or directly to main without rewriting commits.", m.handleTaskPushBranchToRemote), mcp.ToolAnnotations{Title: "Push task branch", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("task_stop", "Stop task", "Stop a running or waiting task. The container is preserved and can be revived later.", m.handleTaskStop), mcp.ToolAnnotations{Title: "Stop task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_purge", "Purge task", "Stop a task and schedule its container for deletion after the recovery window. Reviving the task during the window cancels deletion.", m.handleTaskPurge), mcp.ToolAnnotations{Title: "Purge task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_revive", "Revive task", "Revive a stopped or crashed task, restarting its container and agent session.", m.handleTaskRevive), mcp.ToolAnnotations{Title: "Revive task", DestructiveHint: false, OpenWorldHint: false}),
		forkSpec,
		annotateTool(mcp.NewToolSpec("get_system_stats", "Get system stats", "Read whole-machine CPU, memory, network, and working-filesystem utilization for the caic server. Returns up to 60 five-second samples, including timestamps and capacity totals, plus any sampling warning. The cpus array reports each logical CPU ID and 0–100% usage; sum those percentages for total usage (100% per fully busy CPU). Memory excludes available pages; network sums all interfaces. Works without opening the UI.", m.handleGetSystemStats), mcp.ToolAnnotations{Title: "Get system stats", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("get_usage", "Get usage", "Check current API quota utilization, limits, and reset times for each provider.", m.handleGetUsage), mcp.ToolAnnotations{Title: "Get usage", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("clone_repo", "Clone repository", "Clone a git repository by URL. Optionally specify a local path.", m.handleCloneRepo), mcp.ToolAnnotations{Title: "Clone repository", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("agent_last_message", "Get last agent message", "Get latest agent message, question, or result. Call to check what the agent needs or relay to user.", m.handleAgentLastMessage), mcp.ToolAnnotations{Title: "Get last agent message", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_fix_pr", "Fix task PR", "Inject a fix-PR command into an existing task to fix its failing PR CI in auto mode.", m.handleTaskFixPR), mcp.ToolAnnotations{Title: "Fix task PR", DestructiveHint: true, OpenWorldHint: true}),
		botFixCISpec,
	}
}

// validateToolSchemas fails when any advertised tool schema is structurally
// invalid, so a malformed catalog is caught at startup instead of on the
// request path. It covers the static catalog plus the delegated schemas that
// only task-scoped callers receive.
func (m *mcpTools) validateToolSchemas() error {
	specs := m.specs()
	type namedSchema struct {
		name   string
		schema *jsonschema.Schema
	}
	schemas := make([]namedSchema, 0, 2*len(specs)+2)
	for _, spec := range specs {
		schemas = append(schemas,
			namedSchema{spec.Name + " input", spec.InputSchema},
			namedSchema{spec.Name + " output", spec.OutputSchema},
		)
	}
	schemas = append(schemas,
		namedSchema{"task_create (delegated) input", buildDelegatedTaskCreateSchema()},
		namedSchema{"task_fork (delegated) input", buildDelegatedTaskForkSchema()},
	)
	for _, entry := range schemas {
		if err := mcp.ValidateToolSchema(entry.schema); err != nil {
			return fmt.Errorf("MCP tool %s schema: %w", entry.name, err)
		}
	}
	return nil
}

func (m *mcpTools) handleTasksList(ctx context.Context, call *mcp.ToolRequest[mcpTaskListArgs]) mcp.ToolResult[mcpTaskListOutput] {
	pageSize := call.Arguments.Limit
	if pageSize == 0 {
		pageSize = mcpTaskPageSizeDefault
	}
	if pageSize < 1 || pageSize > mcpTaskPageSizeMax {
		return mcp.ToolErrorWithMeta[mcpTaskListOutput]("Invalid limit. Use an integer from 1 through 50.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	keys, revision := taskKeys(ctx, m.taskSvc)
	page, next, start, err := paginateMCPTaskKeys(keys, revision, call.Arguments.Cursor, pageSize)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpTaskListOutput]("Invalid cursor. Omit cursor to restart task listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	tasks, err := taskSummaries(ctx, m.taskSvc, page, start, revision)
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

func (m *mcpTools) handleReposList(_ context.Context, call *mcp.ToolRequest[mcpRepoListArgs]) mcp.ToolResult[mcpRepoListOutput] {
	pageSize := call.Arguments.Limit
	if pageSize == 0 {
		pageSize = mcpRepoPageSizeDefault
	}
	if pageSize < 1 || pageSize > mcpRepoPageSizeMax {
		return mcp.ToolErrorWithMeta[mcpRepoListOutput]("Invalid limit. Use an integer from 1 through 200.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	page, next, err := paginateMCPRepositories(repositoryPaths(m.serverConfig), call.Arguments.Cursor, pageSize)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpRepoListOutput]("Invalid cursor. Omit cursor to restart repository listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	repositories, fieldsTruncated, err := repositorySummaries(m.serverConfig, page)
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

// handleHarnessesList reports every available harness with its model inventory
// so a client can correct an unsupported model or effort override.
func (m *mcpTools) handleHarnessesList(ctx context.Context, _ *mcp.ToolRequest[struct{}]) mcp.ToolResult[mcpHarnessListOutput] {
	harnesses, err := m.serverConfig.listHarnesses(ctx, nil)
	if err != nil {
		return domainToolError[mcpHarnessListOutput](err)
	}
	out := make([]mcpHarnessInfo, 0, len(*harnesses))
	for _, info := range *harnesses {
		models := make([]mcpHarnessModel, 0, len(info.Models))
		for _, model := range info.Models {
			models = append(models, mcpHarnessModel{ID: model.ID, EffortOptions: nonNilSlice(model.EffortOptions)})
		}
		out = append(out, mcpHarnessInfo{Name: info.Name, Models: models})
	}
	return mcp.TypedToolResult(mcpHarnessListOutput{Harnesses: out})
}

// supportedModelHint names the models the harness accepts so a caller that sent
// an unsupported model override can correct it without another discovery call.
// It returns an empty string when the harness has no published inventory; the
// returned sentence has no trailing period so callers can append their own.
func (m *mcpTools) supportedModelHint(apiHarness v1.Harness) string {
	harnessName, err := apiconv.AgentHarness(apiHarness)
	if err != nil {
		return ""
	}
	backend, ok := m.serverConfig.taskMgr.Backends[harnessName]
	if !ok {
		return ""
	}
	ids := backend.ModelInventory().IDs()
	if len(ids) == 0 {
		return ""
	}
	slices.Sort(ids)
	const maxHintedModels = 40
	if len(ids) > maxHintedModels {
		ids = append(ids[:maxHintedModels:maxHintedModels], "...")
	}
	return "Supported models: " + strings.Join(ids, ", ")
}

// taskCreateToolError adds a recovery step only when the rejected task-create
// override can be safely omitted or the caller can list available repositories.
func (m *mcpTools) taskCreateToolError(ctx context.Context, args mcpTaskCreateArgs, err error) mcp.ToolResult[mcpTaskCreatedOutput] { //nolint:gocritic // Recovery depends on request-shaped arguments.
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
		if apiHarness, harnessErr := m.resolveTaskCreateHarness(ctx, args.Harness); harnessErr == nil {
			if hint := m.supportedModelHint(apiHarness); hint != "" {
				message += ". " + hint
			}
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

// taskCreateConfigurationValid reports whether the effective harness, model,
// and runtime selections are valid after a rejected override is removed.
func (m *mcpTools) taskCreateConfigurationValid(ctx context.Context, args mcpTaskCreateArgs) bool { //nolint:gocritic // Validation consumes request-shaped arguments.
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

func (m *mcpTools) handleTaskCreate(ctx context.Context, call *mcp.ToolRequest[mcpTaskCreateArgs]) mcp.ToolResult[mcpTaskCreatedOutput] {
	if call.Arguments.Prompt == "" {
		return domainToolError[mcpTaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: prompt"})
	}
	if _, ok := taskMCPTaskID(ctx); ok {
		if len(call.Arguments.Repos) != 0 || call.Arguments.Harness != "" || call.Arguments.Model != "" || call.Arguments.Effort != "" || call.Arguments.RuntimeName != "" || call.Arguments.Display || call.Arguments.Tailscale || call.Arguments.USB || call.Arguments.Sudo || call.Arguments.GitHubToken {
			return mcp.ToolError[mcpTaskCreatedOutput]("Task-scoped MCP may only set prompt")
		}
		resp, err := m.taskSvc.createTask(ctx, &v1.CreateTaskReq{InitialPrompt: v1.Prompt{Text: call.Arguments.Prompt}})
		if err != nil {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: "Created child task: " + resp.ID.String(), TaskID: resp.ID.String()})
	}
	apiHarness, err := m.resolveTaskCreateHarness(ctx, call.Arguments.Harness)
	if err != nil {
		if call.Arguments.Harness == "" {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		return m.taskCreateToolError(ctx, call.Arguments, &api.Error{Status: http.StatusBadRequest, Code: api.CodeUnknownHarness, Message: err.Error()})
	}
	req := &v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: call.Arguments.Prompt},
		Repos:         make([]v1.RepoSpec, len(call.Arguments.Repos)),
		Harness:       apiHarness,
		Model:         call.Arguments.Model,
		Effort:        call.Arguments.Effort,
		RuntimeName:   call.Arguments.RuntimeName,
		Display:       call.Arguments.Display,
		Tailscale:     call.Arguments.Tailscale,
		USB:           call.Arguments.USB,
		Sudo:          call.Arguments.Sudo,
		GitHubToken:   call.Arguments.GitHubToken,
		CaicMCP:       true,
	}
	for i, repo := range call.Arguments.Repos {
		req.Repos[i] = v1.RepoSpec{Name: repo}
	}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	resp, err := m.taskSvc.createTask(ctx, req)
	if err != nil {
		return m.taskCreateToolError(ctx, call.Arguments, err)
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

func (m *mcpTools) resolveTaskCreateHarness(ctx context.Context, harness string) (v1.Harness, error) {
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

func (m *mcpTools) firstHarness(ctx context.Context) (v1.Harness, error) {
	harnesses, err := m.serverConfig.listHarnesses(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list available harnesses: %w", err)
	}
	if harnesses == nil || len(*harnesses) == 0 {
		return "", &api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "no available harnesses"}
	}
	return (*harnesses)[0].Name, nil
}

func (m *mcpTools) handleTaskGetDetail(ctx context.Context, call *mcp.ToolRequest[mcpTaskGetDetailArgs]) mcp.ToolResult[mcp.TextOutput] {
	keys, _ := taskKeys(ctx, m.taskSvc)
	id, number, err := call.Arguments.Task.Decode(len(keys))
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
	// Repository probes happen only when explicitly requested.
	txt := strings.ToValidUTF8(strings.TrimSpace(strings.Join(lines, "\n")), "\ufffd")
	if !call.Arguments.IncludeDiffstat {
		return boundedTextToolResult(txt)
	}
	ent, ok := inspectableTaskEntry(ctx, m.taskSvc, t.ID)
	if !ok {
		return domainToolError[mcp.TextOutput](taskNotFoundError())
	}
	idx, err := m.taskSvc.taskDiffIndex(ctx, ent)
	if err != nil {
		msg, code := err.Error(), api.CodeInternalError
		if e, ok := errors.AsType[*api.Error](err); ok {
			msg, code = e.Error(), e.Code
		}
		msg, cut := truncateMCPText(strings.ToValidUTF8(msg, "\ufffd"))
		res := mcp.ToolErrorWithMeta[mcp.TextOutput](msg, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(code)})
		if cut {
			res.Meta[mcpTruncatedMetaKey] = true
		}
		return res
	}
	d := mcpTaskDiffstat{}
	d.out.WriteString(truncateUTF8(txt, 4<<10))
	d.truncated = d.out.Len() != len(txt)
	d.out.WriteString("\n\nDiffstat:\n")
	d.append(idx)
	res := mcp.TextToolResult(d.out.String())
	if d.truncated {
		res.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	}
	return res
}

func (m *mcpTools) handleTaskSendMessage(ctx context.Context, call *mcp.ToolRequest[mcpTaskSendMessageArgs]) mcp.ToolResult[mcp.TextOutput] {
	return m.sendTaskInput(ctx, mcpTaskInputArgs(call.Arguments), "message", "Sent message to task #%d.")
}

func (m *mcpTools) handleTaskAnswerQuestion(ctx context.Context, call *mcp.ToolRequest[mcpTaskAnswerQuestionArgs]) mcp.ToolResult[mcp.TextOutput] {
	return m.sendTaskInput(ctx, mcpTaskInputArgs{TaskNumber: call.Arguments.TaskNumber, Message: call.Arguments.Answer}, "answer", "Answered task #%d.")
}

func (m *mcpTools) handleTaskPushBranchToRemote(ctx context.Context, call *mcp.ToolRequest[mcpTaskPushBranchArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	targetRaw := call.Arguments.Target
	if targetRaw == "main" || targetRaw == "master" {
		targetRaw = string(v1.SyncTargetDefault)
	}
	req := &v1.SyncReq{Force: call.Arguments.Force, Target: v1.SyncTarget(targetRaw)}
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

func (m *mcpTools) handleTaskStop(ctx context.Context, call *mcp.ToolRequest[mcpTaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	_, err = m.taskSvc.stopTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Stopping task #%d.", num))
}

func (m *mcpTools) handleTaskPurge(ctx context.Context, call *mcp.ToolRequest[mcpTaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	_, err = m.taskSvc.purgeTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Stopped task #%d and scheduled it for purge.", num))
}

func (m *mcpTools) handleTaskRevive(ctx context.Context, call *mcp.ToolRequest[mcpTaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	_, err = m.taskSvc.reviveTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Reviving task #%d.", num))
}

// taskForkToolError adds a recovery step only when the rejected override can
// be omitted to inherit the source task's configuration.
func (m *mcpTools) taskForkToolError(args mcpTaskForkArgs, source *taskpkg.Task, err error) mcp.ToolResult[mcpTaskForkOutput] {
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
		apiHarness := v1.Harness(args.Harness)
		if apiHarness == "" {
			if converted, convErr := apiconv.Harness(source.Harness); convErr == nil {
				apiHarness = converted
			}
		}
		if hint := m.supportedModelHint(apiHarness); hint != "" {
			message += ". " + hint
		}
		message += ". Omit model to inherit the source task's model, then retry task_fork."
	default:
		return domainToolError[mcpTaskForkOutput](err)
	}
	return mcp.ToolErrorWithMeta[mcpTaskForkOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
}

func (m *mcpTools) forkWithoutHarnessValid(source *taskpkg.Task, modelOverride string) bool {
	backend, ok := m.taskSvc.taskMgr.Backends[source.Harness]
	model := modelOverride
	if model == "" {
		model = source.RequestedModel
	}
	return ok && (model == "" || slices.Contains(backend.ModelInventory().IDs(), model))
}

// forkWithoutModelValid reports whether the effective harness can accept the
// source model after an explicit model override is omitted.
func (m *mcpTools) forkWithoutModelValid(source *taskpkg.Task, harnessOverride string) bool {
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

func (m *mcpTools) handleTaskFork(ctx context.Context, call *mcp.ToolRequest[mcpTaskForkArgs]) mcp.ToolResult[mcpTaskForkOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, true)
	if err != nil {
		return domainToolError[mcpTaskForkOutput](err)
	}
	if call.Arguments.Prompt == "" {
		return domainToolError[mcpTaskForkOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: prompt"})
	}
	var harness v1.Harness
	if call.Arguments.Harness != "" {
		var conversionErr error
		harness, conversionErr = apiconv.ParseHarness(call.Arguments.Harness)
		if conversionErr != nil {
			return m.taskForkToolError(call.Arguments, entry.Task(), &api.Error{Status: http.StatusBadRequest, Code: api.CodeUnknownHarness, Message: conversionErr.Error()})
		}
	}
	req := &v1.ForkTaskReq{Prompt: v1.Prompt{Text: call.Arguments.Prompt}, Harness: harness, Model: call.Arguments.Model}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTaskForkOutput](err)
	}
	resp, err := m.taskSvc.forkTask(ctx, entry, req)
	if err != nil {
		return m.taskForkToolError(call.Arguments, entry.Task(), err)
	}
	return mcp.TypedToolResult(mcpTaskForkOutput{Result: fmt.Sprintf("Forked task #%d. New task ID: %s", num, resp.ID.String()), TaskID: resp.ID.String()})
}

func (m *mcpTools) handleGetSystemStats(_ context.Context, call *mcp.ToolRequest[struct{}]) mcp.ToolResult[v1.SystemStatsResp] {
	return mcp.TypedToolResult(mcpSystemStatsInLocation(m.serverConfig.systemStats.snapshot(), mcpRequestLocation(&call.Params.Meta)))
}

func (m *mcpTools) handleGetUsage(ctx context.Context, call *mcp.ToolRequest[struct{}]) mcp.ToolResult[mcp.TextOutput] {
	// Client metadata is only a display hint. Never use the server's local zone.
	loc := mcpRequestLocation(&call.Params.Meta)
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
			part := fmt.Sprintf("%s: %.0f%% remaining", rl.Window, (1-rl.Utilization)*100)
			if !rl.ResetsAt.IsZero() {
				part += " (resets at " + rl.ResetsAt.In(loc).Format("2006-01-02T15:04:05-07:00") + ")"
			}
			parts = append(parts, part)
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

func (m *mcpTools) handleCloneRepo(ctx context.Context, call *mcp.ToolRequest[mcpCloneRepoArgs]) mcp.ToolResult[mcp.TextOutput] {
	if call.Arguments.URL == "" {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: url"})
	}
	req := &v1.CloneRepoReq{URL: call.Arguments.URL, Path: call.Arguments.Path}
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

func (m *mcpTools) handleAgentLastMessage(ctx context.Context, call *mcp.ToolRequest[mcpTaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	var message agent.Message
	var historyErr error
	for candidate, err := range m.taskSvc.taskMgr.BackwardMessages(ctx, entry) {
		if err != nil {
			historyErr = err
			break
		}
		if candidate.Provenance().IsChild() {
			continue
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

func (m *mcpTools) handleTaskFixPR(ctx context.Context, call *mcp.ToolRequest[mcpTaskFixPRArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	_, err = m.ci.fixPR(ctx, &v1.BotFixPRReq{TaskID: entry.Task().ID.String()})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Injected fix-PR command into task #%d.", num))
}

func (m *mcpTools) handleBotFixCI(ctx context.Context, call *mcp.ToolRequest[mcpBotFixCIArgs]) mcp.ToolResult[mcpTaskCreatedOutput] {
	if call.Arguments.Repo == "" {
		return domainToolError[mcpTaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: repo"})
	}
	resp, err := m.ci.fixCI(ctx, &v1.BotFixCIReq{Repo: call.Arguments.Repo})
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
		return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task #%d for %s.", num, call.Arguments.Repo), TaskNumber: num, TaskID: resp.ID.String()})
	}
	return mcp.TypedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task for %s.", call.Arguments.Repo), TaskID: resp.ID.String()})
}

func (m *mcpTools) sendTaskInput(ctx context.Context, args mcpTaskInputArgs, field, format string) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, args.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	if args.Message == "" {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: " + field})
	}
	_, err = m.taskSvc.sendInput(ctx, entry, &v1.InputReq{Prompt: v1.Prompt{Text: args.Message}})
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf(format, num))
}

func (m *mcpTools) taskByNumber(ctx context.Context, num int) (v1.Task, error) {
	_, entry, err := m.entryByTaskRef(ctx, taskID(strconv.Itoa(num)), false)
	if err != nil {
		return v1.Task{}, err
	}
	return m.taskDTO(ctx, entry)
}

func (m *mcpTools) taskByID(ctx context.Context, id ksid.ID) (v1.Task, error) {
	entry, ok := inspectableTaskEntry(ctx, m.taskSvc, id)
	if !ok {
		return v1.Task{}, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task not found"}
	}
	m.taskSvc.focusTask(ctx, id)
	return m.taskDTO(ctx, entry)
}

func (m *mcpTools) taskDTO(ctx context.Context, entry *taskmgr.Entry) (v1.Task, error) {
	t, err := taskDTO(ctx, entry, m.taskSvc.taskMgr, m.taskSvc.checkouts, m.taskSvc.authStore)
	if err != nil {
		return v1.Task{}, &api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: err.Error()}
	}
	return t, nil
}

// entryByTaskRef resolves a task reference — a number, a "#N" string, or a
// stable task ID — to its current list position and entry. When allowSelf is
// set in a task-scoped session, a zero reference resolves to the calling task.
// Successful user-directed resolution also selects the task in connected frontends.
func (m *mcpTools) entryByTaskRef(ctx context.Context, ref taskID, allowSelf bool) (num int, resolved *taskmgr.Entry, err error) {
	defer func() {
		if err == nil {
			m.taskSvc.focusTask(ctx, resolved.Task().ID)
		}
	}()
	raw := strings.TrimSpace(string(ref))
	keys, _ := taskKeys(ctx, m.taskSvc)
	delegatingTaskID, taskScoped := taskMCPTaskID(ctx)
	if number, err := parseTaskNumber(raw); err == nil {
		if number == 0 && allowSelf && taskScoped {
			entry, ok := m.taskSvc.taskMgr.GetEntry(delegatingTaskID)
			if !ok {
				return 0, nil, taskNotFoundError()
			}
			return 0, entry, nil
		}
		if number < 1 {
			return 0, nil, invalidTaskNumberError()
		}
		if number > len(keys) {
			return number, nil, taskNotFoundError()
		}
		entry, ok := visibleTaskEntry(ctx, m.taskSvc, keys[number-1].ID)
		if !ok {
			return number, nil, taskNotFoundError()
		}
		return number, entry, nil
	}
	id, err := ksid.Parse(raw)
	if err != nil {
		return 0, nil, invalidTaskNumberError()
	}
	entry, ok := visibleTaskEntry(ctx, m.taskSvc, id)
	if !ok {
		return 0, nil, taskNotFoundError()
	}
	for i := range keys {
		if keys[i].ID == id {
			return i + 1, entry, nil
		}
	}
	return 0, entry, nil
}

// authorizeTool enforces MCP scope and linked forge authority policy.
//
// Remote forge tools require linked forge authority. A GitHub-linked caic user
// may use user OAuth, server PAT, or GitHub App authority. GitLab-linked remote
// MCP users require a user token until there is an explicit server-side GitLab
// authority policy.
func (m *mcpTools) authorizeTool(ctx context.Context, name string) (string, bool) {
	if reason, ok := authorizeToolScope(ctx, name); !ok {
		return reason, false
	}
	if _, needsForge := mcpForgeTools[name]; needsForge && isRemoteMCP(ctx) && !userHasForgeAuthority(ctx) {
		return "linked GitHub identity or GitLab token is required for forge MCP tools", false
	}
	return "allow", true
}

func annotateTool(spec mcp.ToolSpec, annotations mcp.ToolAnnotations) mcp.ToolSpec { //nolint:gocritic // Tool specs are immutable catalog entries; value style keeps call sites simple.
	spec.Annotations = &annotations
	return spec
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

type mcpRepoListArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema_description:"Opaque cursor returned by the previous page"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"minimum=1,maximum=200"                                   jsonschema_description:"Maximum repositories to return; defaults to 200, cannot exceed 200, and a response-size limit may shorten the page"`
}

type mcpRepoListOutput struct {
	Repositories    []mcpRepoSummary `json:"repositories"              jsonschema_description:"Repositories in this page"`
	NextCursor      string           `json:"nextCursor,omitempty"      jsonschema_description:"Opaque cursor for the next page"`
	FieldsTruncated bool             `json:"fieldsTruncated,omitempty" jsonschema_description:"Whether presentation-only repository fields were shortened to the MCP response limit"`
}

type mcpHarnessModel struct {
	ID            string   `json:"id"                      jsonschema_description:"Model identifier accepted by the harness"`
	EffortOptions []string `json:"effortOptions,omitempty" jsonschema_description:"Reasoning effort values the model accepts; absent means the harness default only"`
}

type mcpHarnessInfo struct {
	Name   v1.Harness        `json:"name"   jsonschema_description:"Harness identifier accepted by task_create"`
	Models []mcpHarnessModel `json:"models" jsonschema_description:"Models this harness accepts, in harness order"`
}

type mcpHarnessListOutput struct {
	Harnesses []mcpHarnessInfo `json:"harnesses" jsonschema_description:"Available harnesses and their supported models"`
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

// repositoryRecoveryMessage adds a safe retry instruction for a rejected
// repository path. It only refers to repos_list when the caller can use it.
func repositoryRecoveryMessage(ctx context.Context, message, operation string) string {
	if mcpHasScope(ctx, mcpScopeRead) {
		return message + ". Call repos_list, use an exact returned path, then retry " + operation + "."
	}
	return message + ". The path must exactly match a configured repository."
}

type mcpTaskCreatedOutput struct {
	Result     string `json:"result"               jsonschema_description:"Human-readable task creation result"`
	TaskNumber int    `json:"taskNumber,omitempty" jsonschema_description:"Current session task number"`
	TaskID     string `json:"taskID"               jsonschema_description:"Stable task ID"`
}

type mcpTaskCreateArgs struct {
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

type mcpTaskSendMessageArgs struct {
	TaskNumber taskID `json:"task_number" jsonschema:"oneof_type=integer;string"                    jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
	Message    string `json:"message"     jsonschema_description:"The message to send to the agent"`
}

type mcpTaskAnswerQuestionArgs struct {
	TaskNumber taskID `json:"task_number" jsonschema:"oneof_type=integer;string"                      jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
	Answer     string `json:"answer"      jsonschema_description:"The answer to the agent's question"`
}

type mcpTaskPushBranchArgs struct {
	TaskNumber taskID `json:"task_number"      jsonschema:"oneof_type=integer;string"                      jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
	Force      bool   `json:"force,omitempty"  jsonschema_description:"Force sync even with safety issues"`
	Target     string `json:"target,omitempty" jsonschema:"enum=branch,enum=default,enum=main,enum=master" jsonschema_description:"Where to push: branch (default) or main"`
}

type mcpTaskForkOutput struct {
	Result string `json:"result" jsonschema_description:"Human-readable fork result"`
	TaskID string `json:"taskID" jsonschema_description:"Stable task ID for the forked task"`
}

type mcpTaskForkArgs struct {
	TaskNumber taskID `json:"task_number"       jsonschema:"oneof_type=integer;string"                                                jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID to fork"`
	Prompt     string `json:"prompt"            jsonschema_description:"The initial prompt for the forked task"`
	Harness    string `json:"harness,omitempty" jsonschema_description:"Override harness (optional, inherits from source if omitted)"`
	Model      string `json:"model,omitempty"   jsonschema_description:"Model override (optional, inherits from source if omitted)"`
}

type mcpCloneRepoArgs struct {
	URL  string `json:"url"            jsonschema_description:"The git repository URL to clone"`
	Path string `json:"path,omitempty" jsonschema_description:"Local directory name (optional, derived from URL if omitted)"`
}

type mcpTaskFixPRArgs struct {
	TaskNumber taskID `json:"task_number" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number or stable task ID whose PR CI should be fixed"`
}

type mcpBotFixCIArgs struct {
	Repo string `json:"repo" jsonschema_description:"Repository to fix CI for"`
}

type mcpTaskInputArgs struct {
	TaskNumber taskID
	Message    string
}

func taskNotFoundError() *api.Error {
	return &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task not found"}
}

func invalidTaskNumberError() *api.Error {
	return &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: `task_number must be a positive integer, a "#N" reference, or a stable task ID`}
}

func mcpStaticResources() []mcp.ResourceDescriptor {
	return []mcp.ResourceDescriptor{
		{URI: "caic://system/health", Name: "system-health", Title: "System health", Description: "Last-observed machine CPU and memory usage, overload warnings, and sampling errors", MimeType: "application/json"},
		{URI: "caic://usage", Name: "usage", Title: "Usage", Description: "Local and provider usage", MimeType: "application/json"},
		{URI: "gomode://items", Name: "items", Title: "Items", Description: "Generic service item status for native clients", MimeType: "application/json"},
		{URI: "gomode://notifications", Name: "notifications", Title: "Notifications", Description: "Service notifications for native clients", MimeType: "application/json"},
	}
}

func mcpTaskResourceID(uri string) (ksid.ID, error) {
	rawID, ok := strings.CutPrefix(uri, "caic://tasks/")
	if !ok {
		return 0, errors.New("not a task resource")
	}
	return ksid.Parse(rawID)
}

func buildTaskCreateSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The task description/prompt for the coding agent"})
	props.Set("repos", &jsonschema.Schema{Type: "array", Description: "Repositories to work in (zero or more). Omit or pass an empty list for a task without repositories. Call repos_list first if you need the current repository list.", Items: &jsonschema.Schema{Type: "string"}})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model override (optional). Omit unless the user explicitly chose a model; omitted means the selected harness default."})
	props.Set("effort", &jsonschema.Schema{Type: "string", Description: "Thinking effort override (optional). Omit unless the user explicitly chose an effort; omitted means the selected harness default."})
	props.Set("harness", &jsonschema.Schema{Type: "string", Description: "Agent harness override (optional). Omit to use the saved default harness."})
	props.Set("runtimeName", &jsonschema.Schema{Type: "string", Description: "Runtime backend name (optional). Use docker or podman when multiple runtimes are available."})
	props.Set("display", &jsonschema.Schema{Type: "boolean", Description: "Enable virtual display (VNC) for this task"})
	props.Set("tailscale", &jsonschema.Schema{Type: "boolean", Description: "Enable Tailscale networking for this task"})
	props.Set("usb", &jsonschema.Schema{Type: "boolean", Description: "Enable USB passthrough for this task"})
	props.Set("sudo", &jsonschema.Schema{Type: "boolean", Description: "Enable root access via sudo with a random password"})
	props.Set("gitHubToken", &jsonschema.Schema{Type: "boolean", Description: "Enable GitHub token injection for this task"})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"prompt"}}
}

func buildDelegatedTaskCreateSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The child task prompt. Its parent, repository, and runtime are derived from this task."})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"prompt"}}
}

func buildTaskForkSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("task_number", taskReferenceProperty(`Task number or stable task ID to fork, e.g. 1 or "#1" for task #1`))
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The initial prompt for the forked task"})
	props.Set("harness", &jsonschema.Schema{Type: "string", Description: "Override harness (optional, inherits from source if omitted)"})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model override (optional, inherits from source if omitted)"})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"task_number", "prompt"}}
	mcp.AddHeaderToProperty(schema, "task_number", "Task-Number")
	return schema
}

func buildDelegatedTaskForkSchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("task_number", taskReferenceProperty("Use 0 to fork this task, or a child task number returned by tasks_list"))
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The initial prompt for the forked child task"})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"task_number", "prompt"}}
	mcp.AddHeaderToProperty(schema, "task_number", "Task-Number")
	return schema
}

// taskReferenceProperty describes a task reference accepted as a JSON integer
// or as a string such as "#1" or a stable task ID.
func taskReferenceProperty(description string) *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf:       []*jsonschema.Schema{{Type: "integer"}, {Type: "string"}},
		Description: description,
	}
}

func buildBotFixCISchema() *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("repo", &jsonschema.Schema{Type: "string", Description: "Repository path to fix CI for. Call repos_list first if you need the current repository list."})
	schema := &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"repo"}}
	mcp.AddHeaderToProperty(schema, "repo", "Repo")
	return schema
}

type mcpTaskNumberArgs struct {
	TaskNumber taskID `json:"task_number" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number (for example 1 or \"#1\") or stable task ID"`
}

var errNotTaskNumber = errors.New("not a task number")

// parseTaskNumber parses an integer task number, allowing a leading '#'.
func parseTaskNumber(raw string) (int, error) {
	number, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "#")))
	if err != nil {
		return 0, errNotTaskNumber
	}
	return number, nil
}

// taskID accepts an MCP task number (optionally prefixed with '#') or a stable
// task ID.
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
	if number, err := parseTaskNumber(raw); err == nil {
		if number < 1 || number > maxTaskNumber {
			return 0, 0, fmt.Errorf("task number %d is outside 1 through %d", number, maxTaskNumber)
		}
		return 0, number, nil
	}
	parsed, err := ksid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return 0, 0, errors.New("task must be a task number or stable task ID")
	}
	return parsed, 0, nil
}

type mcpTaskGetDetailArgs struct {
	Task            taskID `json:"task" jsonschema:"oneof_type=integer;string" jsonschema_description:"Task number from tasks_list (for example 1 or #1) or stable task ID; use your stable task ID to inspect your own workspace"`
	IncludeDiffstat bool   `json:"include_diffstat,omitempty" jsonschema_description:"Include committed and uncommitted file diffstats, branch, upstream, and ahead/behind counts"`
}

// mcpTaskDiffstat renders repository metadata and file counts within the MCP text budget.
type mcpTaskDiffstat struct {
	out       strings.Builder
	limit     int
	omitLine  bool
	truncated bool
}

func (d *mcpTaskDiffstat) append(idx *v1.TaskDiffIndexResp) {
	// Count the complete comparison before limiting the rendered detail rows.
	var files, added, deleted, binaryFiles int
	for i := range idx.Repositories {
		r := &idx.Repositories[i]
		for j := range r.Commits {
			for _, f := range r.Commits[j].Stat {
				files++
				added += f.LinesAdded
				deleted += f.LinesDeleted
				if f.OldSize >= 0 || f.NewSize >= 0 {
					binaryFiles++
				}
			}
		}
		for _, f := range r.Uncommitted {
			files++
			added += f.LinesAdded
			deleted += f.LinesDeleted
			if f.OldSize >= 0 || f.NewSize >= 0 {
				binaryFiles++
			}
		}
	}
	footer := fmt.Sprintf("\nTotal (all compared commits and working trees): %d file changes, %d insertions(+), %d deletions(-), %d binary file changes.\n", files, added, deleted, binaryFiles)
	const notice = "[Diffstat output truncated]\n"
	d.limit = mcpTextOutputMaxBytes - len(footer) - len(notice)

repositories:
	for i := range idx.Repositories {
		r := &idx.Repositories[i]
		if !d.write("\nRepository %s: branch %s, upstream %s, ahead %d, behind %d\n", d.field(r.Name), d.field(r.Branch), d.field(r.Upstream), r.Ahead, r.Behind) {
			break repositories
		}
		for j := range r.Commits {
			c := &r.Commits[j]
			label := "Local commit"
			if c.Behind {
				label = "Missing upstream commit"
			}
			if !d.write("%s %s: %s\n", label, d.field(c.SHA), d.field(c.Subject)) {
				break repositories
			}
			for _, f := range c.Stat {
				if !d.file(f.Path, "", f.LinesAdded, f.LinesDeleted, f.OldSize, f.NewSize) {
					break repositories
				}
			}
		}
		if !d.write("Uncommitted files: %d\n", len(r.Uncommitted)) {
			break repositories
		}
		for _, f := range r.Uncommitted {
			if !d.file(f.Path, f.OriginalPath, f.LinesAdded, f.LinesDeleted, f.OldSize, f.NewSize) {
				break repositories
			}
		}
	}
	if d.truncated {
		d.out.WriteString(notice)
	}
	d.out.WriteString(footer)
}

func (d *mcpTaskDiffstat) file(p, prev string, add, del int, old, size int64) bool {
	name := d.field(p)
	if prev != "" {
		name = d.field(prev) + " -> " + name
	}
	if old >= 0 || size >= 0 {
		return d.write("  %s: binary (%d -> %d bytes)\n", name, old, size)
	}
	return d.write("  %s: +%d -%d\n", name, add, del)
}

// field quotes metadata and marks rows with oversized fields for omission.
func (d *mcpTaskDiffstat) field(s string) string {
	if len(s) > d.limit-d.out.Len() {
		d.omitLine = true
		return ""
	}
	s = strings.ToValidUTF8(s, "\ufffd")
	return strconv.Quote(s)
}

func (d *mcpTaskDiffstat) write(format string, args ...any) bool {
	s := fmt.Sprintf(format, args...)
	if d.omitLine || d.out.Len()+len(s) > d.limit {
		d.truncated = true
		return false
	}
	d.out.WriteString(s)
	return true
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
