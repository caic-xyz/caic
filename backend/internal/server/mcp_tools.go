// MCP tool catalogs, schemas, execution, structured task details, pending prompts, and message delivery modes.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gomodedev/gomode"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/server/mcpschema"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/server/apiconv"
	auditv1 "github.com/caic-xyz/caic/backend/internal/server/data/audit/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	"github.com/caic-xyz/caic/backend/internal/task/taskrun"
	"github.com/caic-xyz/caic/metrics"
	"github.com/gomodedev/gomode/mcp"
	"github.com/invopop/jsonschema"
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
	accounts     *accountService
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
	ctx, err := mcpRequestSession(ctx, params.Meta.Session)
	if err != nil {
		return mcp.RawToolResult{}, err
	}
	for _, s := range m.specs() {
		if s.Name != params.Name {
			continue
		}
		if authResult, ok := m.authorizeTool(ctx, params.Name); !ok {
			m.audit.record(ctx, &auditv1.Event{Operation: "tools/call", Name: params.Name, Args: auditArgsSummary(params.Arguments), Decision: authResult})
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
		m.audit.record(ctx, &auditv1.Event{Operation: "tools/call", Name: params.Name, Args: auditArgsSummary(params.Arguments), Decision: "allow", Status: status})
		if err == nil && mcpClientSessionFromContext(ctx) != "" {
			projection, projectionErr := m.taskSvc.conversationFocus(ctx)
			if projectionErr != nil {
				return res, projectionErr
			}
			if res.Meta == nil {
				res.Meta = mcp.MetaObject{}
			}
			res.Meta[gomode.ConversationContextMetaKey] = projection
		}
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
// github.com/gomodedev/gomode/voicegateway/voicertc/AGENTS.md.
func (m *mcpTools) specs() []mcp.ToolSpec {
	createSpec := mcp.NewToolSpec("task_create", "Create task", "Create a new coding task with zero or more repositories. Confirm repositories and prompt with the user before calling. Omit harness, model, and effort unless the user explicitly asks for an override; caic fills an omitted harness, model, and effort from the user's saved preferences, falling back to harness defaults.", m.handleTaskCreate)
	createSpec.InputSchema = buildTaskCreateSchema()
	createSpec.Annotations = &mcp.ToolAnnotations{Title: "Create task", DestructiveHint: true, OpenWorldHint: false}

	forkSpec := mcp.NewToolSpec("task_fork", "Fork task", "Fork a running or waiting task, creating a snapshot of its container on a new branch. The prompt describes what the forked task should do. Optionally override the harness and model.", m.handleTaskFork)
	forkSpec.InputSchema = buildTaskForkSchema()
	forkSpec.Annotations = &mcp.ToolAnnotations{Title: "Fork task", DestructiveHint: true, OpenWorldHint: false}

	botFixCISpec := mcp.NewToolSpec("bot_fix_ci", "Fix repository CI", "Create a task to investigate and fix a failing CI on a repository's default branch.", m.handleBotFixCI)
	botFixCISpec.InputSchema = buildBotFixCISchema()
	botFixCISpec.Annotations = &mcp.ToolAnnotations{Title: "Fix repository CI", DestructiveHint: true, OpenWorldHint: false}

	specs := []mcp.ToolSpec{
		annotateTool(mcp.NewToolSpec("tasks_list", "List tasks", "List coding tasks in stable active-first order. A task-scoped agent lists only the tasks it created; task_get_detail accepts any stable task ID. A response-size limit may return fewer tasks than limit; pass nextCursor as cursor until it is absent.", m.handleTasksList), mcp.ToolAnnotations{Title: "List tasks", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("tasks_show_on_glasses", "Show tasks on glasses", "Show the user's tasks on their connected glasses: number, state, and title of the first tasks in tasks_list order. Call when the user asks to see their tasks. Returns which tasks the glasses show; do not read them out. Fails when no glasses are connected.", m.handleTasksShowOnGlasses), mcp.ToolAnnotations{Title: "Show tasks on glasses", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("repos_list", "List repositories", "List repositories in stable path order. Defaults to at most 200 repositories; a response-size limit may shorten the page, so pass nextCursor as cursor until it is absent.", m.handleReposList), mcp.ToolAnnotations{Title: "List repositories", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("harnesses_list", "List harnesses", "List available coding-agent harnesses and the models each accepts, with the reasoning effort values every model supports. Call this to choose a valid model or effort for task_create.", m.handleHarnessesList), mcp.ToolAnnotations{Title: "List harnesses", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		createSpec,
		annotateTool(mcp.NewToolSpec("task_get_detail", "Get task detail", "Get structured task status, running background work, the raw task log path, and pending prompt identity, text, and delivery status by task number or stable ID. Optionally include committed and uncommitted file diffstats with branch and upstream divergence. Fields have independent limits and explicit truncation indicators.", m.handleTaskGetDetail), mcp.ToolAnnotations{Title: "Get task detail", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_send_message", "Send task message", "Send a text message to an agent by task number. Choose steer for immediate delivery, append to preserve and extend pending input, or replace to overwrite the entire pending prompt. Queued input is delivered when waiting; append and replace are rejected once delivery starts.", m.handleTaskSendMessage), mcp.ToolAnnotations{Title: "Send task message", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_answer_question", "Answer task question", "Answer an agent's question by task number. The agent is in 'asking' state.", m.handleTaskAnswerQuestion), mcp.ToolAnnotations{Title: "Answer task question", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_push_branch_to_remote", "Push task branch", "Sync or push a task's existing commits to GitHub. Push to task branch (default) or directly to main without rewriting commits.", m.handleTaskPushBranchToRemote), mcp.ToolAnnotations{Title: "Push task branch", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("task_stop", "Stop task", "Stop a running or waiting task. The container is preserved and can be revived later.", m.handleTaskStop), mcp.ToolAnnotations{Title: "Stop task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_purge", "Purge task", "Stop a task and schedule its container for deletion after the recovery window. Reviving the task during the window cancels deletion.", m.handleTaskPurge), mcp.ToolAnnotations{Title: "Purge task", DestructiveHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_cancel_push", "Cancel task push", "Cancel the task's running push started by task_push_branch_to_remote or the web UI. Fails when no push is running.", m.handleTaskCancelPush), mcp.ToolAnnotations{Title: "Cancel task push", DestructiveHint: false, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_revive", "Revive task", "Revive a stopped or crashed task, restarting its container and agent session.", m.handleTaskRevive), mcp.ToolAnnotations{Title: "Revive task", DestructiveHint: false, OpenWorldHint: false}),
		forkSpec,
		annotateTool(mcp.NewToolSpec("get_system_stats", "Get system stats", "Read whole-machine CPU, memory, network, and working-filesystem utilization for the caic server. Returns up to 60 five-second samples, including timestamps and capacity totals, plus any sampling warning. The cpus array reports each logical CPU ID and 0–100% usage; sum those percentages for total usage (100% per fully busy CPU). Memory excludes available pages; network sums all interfaces. Works without opening the UI.", m.handleGetSystemStats), mcp.ToolAnnotations{Title: "Get system stats", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("get_usage", "Get usage", "Check current API quota utilization, limits, and reset times for each provider.", m.handleGetUsage), mcp.ToolAnnotations{Title: "Get usage", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("clone_repo", "Clone repository", "Clone a git repository by URL. Optionally specify a local path.", m.handleCloneRepo), mcp.ToolAnnotations{Title: "Clone repository", DestructiveHint: true, OpenWorldHint: true}),
		annotateTool(mcp.NewToolSpec("agent_last_message", "Get last agent message", "Get latest agent message, question, or result. Call to check what the agent needs or relay to user.", m.handleAgentLastMessage), mcp.ToolAnnotations{Title: "Get last agent message", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
		annotateTool(mcp.NewToolSpec("task_fix_pr", "Fix task PR", "Inject a fix-PR command into an existing task to fix its failing PR CI in auto mode.", m.handleTaskFixPR), mcp.ToolAnnotations{Title: "Fix task PR", DestructiveHint: true, OpenWorldHint: true}),
		botFixCISpec,
	}
	if m.accounts != nil {
		specs = append(specs,
			annotateTool(mcp.NewToolSpec("accounts_list", "List accounts", "Administrator only. List existing account identities, effective roles and enabled state without tokens or session details.", m.handleListAccounts), mcp.ToolAnnotations{Title: "List accounts", ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: false}),
			annotateTool(mcp.NewToolSpec("account_set_role", "Assign account role", "Super administrator only. Assign admin or member to an existing account. The configuration-owned super_admin cannot be changed.", m.handleSetAccountRole), mcp.ToolAnnotations{Title: "Assign account role", DestructiveHint: true, OpenWorldHint: false}),
			annotateTool(mcp.NewToolSpec("account_set_enabled", "Enable or suspend account", "Administrator only. Disable an account, cancel its sessions, and confirm its retained runtimes are stopped without deleting history. Retry on an unconfirmed suspension. Re-enabling requires confirmed suspension and revokes old OAuth grants; it never restarts tasks or restores sessions.", m.handleSetAccountEnabled), mcp.ToolAnnotations{Title: "Enable or suspend account", DestructiveHint: true, OpenWorldHint: false}),
		)
	}
	return specs
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

func (m *mcpTools) handleTasksList(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskListArgs]) mcp.ToolResult[mcpschema.TaskListOutput] {
	pageSize := call.Arguments.Limit
	if pageSize == 0 {
		pageSize = mcpTaskPageSizeDefault
	}
	if pageSize < 1 || pageSize > mcpTaskPageSizeMax {
		return mcp.ToolErrorWithMeta[mcpschema.TaskListOutput]("Invalid limit. Use an integer from 1 through 50.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	keys, revision := taskKeys(ctx, m.taskSvc)
	page, next, start, err := paginateMCPTaskKeys(keys, revision, call.Arguments.Cursor, pageSize)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpschema.TaskListOutput]("Invalid cursor. Omit cursor to restart task listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	tasks, err := taskSummaries(ctx, m.taskSvc, page, start, revision)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpschema.TaskListOutput]("Task ordering changed while listing. Omit cursor to restart task listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
	}
	activeTaskID := m.taskSvc.focusedTaskIDString(ctx)
	for i := range tasks {
		tasks[i].Active = tasks[i].TaskID == activeTaskID
	}
	tasks, next, err = fitMCPJSONPage(tasks, next != "", mcpListJSONMaxBytes,
		func(count int) (string, error) { return encodeMCPTaskCursor(revision, page[count-1].ID) },
		func(page []mcpschema.TaskSummary, cursor string) any {
			return mcpschema.TaskListOutput{Tasks: page, NextCursor: cursor}
		},
	)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpschema.TaskListOutput]("A task summary exceeds the MCP response-size limit.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeInternalError)})
	}
	// TODO(observability): Record requested/effective page size, cursor outcomes,
	// returned task count, and pre-wire logical payload size here. Do not use
	// cursor, task ID, title, or user identity as metric labels.
	return mcp.TypedToolResult(mcpschema.TaskListOutput{Tasks: tasks, NextCursor: next})
}

// handleTasksShowOnGlasses draws the first tasks on the glasses the client
// declares, as gomode.DisplayExtension documents, and tells the model which
// ones it drew. A malformed declaration counts as no glasses, as that contract
// requires.
func (m *mcpTools) handleTasksShowOnGlasses(ctx context.Context, call *mcp.ToolRequest[struct{}]) mcp.ToolResult[mcp.TextOutput] {
	display, ok, err := mcp.ClientDisplay(&call.Params.Meta)
	if err != nil {
		m.taskSvc.log.WarnContext(ctx, "ignore malformed glasses display", "err", err)
	}
	if !ok || err != nil {
		return mcp.ToolErrorWithMeta[mcp.TextOutput]("No glasses are connected.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	keys, revision := taskKeys(ctx, m.taskSvc)
	tasks, err := taskSummaries(ctx, m.taskSvc, keys[:min(len(keys), glassesTaskLimit)], 0, revision)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcp.TextOutput]("Task ordering changed while reading. Try again.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
	}
	card, err := glassesTasksCard(&display, tasks)
	if err != nil {
		m.taskSvc.log.ErrorContext(ctx, "draw glasses task card", "err", err)
		return mcp.ToolErrorWithMeta[mcp.TextOutput]("The task card could not be drawn.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeInternalError)})
	}
	res := mcp.TypedToolResult(mcp.TextOutput{Result: glassesTasksSummary(len(tasks), len(keys))})
	res.Content = []mcp.ContentBlock{card}
	return res
}

func (m *mcpTools) handleReposList(_ context.Context, call *mcp.ToolRequest[mcpschema.RepoListArgs]) mcp.ToolResult[mcpschema.RepoListOutput] {
	pageSize := call.Arguments.Limit
	if pageSize == 0 {
		pageSize = mcpRepoPageSizeDefault
	}
	if pageSize < 1 || pageSize > mcpRepoPageSizeMax {
		return mcp.ToolErrorWithMeta[mcpschema.RepoListOutput]("Invalid limit. Use an integer from 1 through 200.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	page, next, err := paginateMCPRepositories(repositoryPaths(m.serverConfig), call.Arguments.Cursor, pageSize)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpschema.RepoListOutput]("Invalid cursor. Omit cursor to restart repository listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
	}
	repositories, fieldsTruncated, err := repositorySummaries(m.serverConfig, page)
	if err != nil {
		if errors.Is(err, errMCPRepositoryPathTooLong) {
			return mcp.ToolErrorWithMeta[mcpschema.RepoListOutput](err.Error(), mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeBadRequest)})
		}
		return mcp.ToolErrorWithMeta[mcpschema.RepoListOutput]("Repository list changed while paging. Omit cursor to restart repository listing.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
	}
	repositories, next, err = fitMCPJSONPage(repositories, next != "", mcpListJSONMaxBytes,
		func(count int) (string, error) { return mcpKeyCursor(page[count-1]), nil },
		func(page []mcpschema.RepoSummary, cursor string) any {
			return mcpschema.RepoListOutput{Repositories: page, NextCursor: cursor, FieldsTruncated: fieldsTruncated}
		},
	)
	if err != nil {
		return mcp.ToolErrorWithMeta[mcpschema.RepoListOutput]("A repository summary exceeds the MCP response-size limit.", mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeInternalError)})
	}
	result := mcp.TypedToolResult(mcpschema.RepoListOutput{Repositories: repositories, NextCursor: next, FieldsTruncated: fieldsTruncated})
	if fieldsTruncated {
		result.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	}
	return result
}

// handleHarnessesList reports every available harness with its model inventory
// so a client can correct an unsupported model or effort override.
func (m *mcpTools) handleHarnessesList(ctx context.Context, _ *mcp.ToolRequest[struct{}]) mcp.ToolResult[mcpschema.HarnessListOutput] {
	harnesses, err := m.serverConfig.listHarnesses(ctx, nil)
	if err != nil {
		return domainToolError[mcpschema.HarnessListOutput](err)
	}
	out := make([]mcpschema.HarnessInfo, 0, len(*harnesses))
	for _, info := range *harnesses {
		models := make([]mcpschema.HarnessModel, 0, len(info.Models))
		for _, model := range info.Models {
			models = append(models, mcpschema.HarnessModel{ID: model.ID, EffortOptions: nonNilSlice(model.EffortOptions)})
		}
		out = append(out, mcpschema.HarnessInfo{Name: info.Name, Models: models})
	}
	return mcp.TypedToolResult(mcpschema.HarnessListOutput{Harnesses: out})
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
func (m *mcpTools) taskCreateToolError(ctx context.Context, args mcpschema.TaskCreateArgs, err error) mcp.ToolResult[mcpschema.TaskCreatedOutput] { //nolint:gocritic // Recovery depends on request-shaped arguments.
	apiErr, ok := errors.AsType[*api.Error](err)
	if !ok {
		return domainToolError[mcpschema.TaskCreatedOutput](err)
	}
	message := apiErr.Error()
	switch apiErr.Code {
	case api.CodeUnknownRepository:
		if !m.taskCreateConfigurationValid(ctx, args) {
			return domainToolError[mcpschema.TaskCreatedOutput](err)
		}
		message = repositoryRecoveryMessage(ctx, message, "task_create")
	case api.CodeUnknownHarness:
		if args.Harness == "" {
			return mcp.ToolErrorWithMeta[mcpschema.TaskCreatedOutput](apiErr.Error(), mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(api.CodeConflict)})
		}
		args.Harness = ""
		message += ". Omit harness to use caic's default harness, then retry task_create."
	case api.CodeUnsupportedModel:
		if args.Model == "" {
			return domainToolError[mcpschema.TaskCreatedOutput](err)
		}
		if apiHarness, harnessErr := m.resolveTaskCreateHarness(ctx, args.Harness); harnessErr == nil {
			if hint := m.supportedModelHint(apiHarness); hint != "" {
				message += ". " + hint
			}
		}
		args.Model = ""
		message += ". Omit model to use the saved or default model of the selected harness, then retry task_create."
	case api.CodeUnknownRuntime:
		if args.RuntimeName == "" {
			return domainToolError[mcpschema.TaskCreatedOutput](err)
		}
		args.RuntimeName = ""
		message += ". Omit runtimeName to use caic's default runtime, then retry task_create."
	default:
		return domainToolError[mcpschema.TaskCreatedOutput](err)
	}
	if !m.taskCreateConfigurationValid(ctx, args) {
		return domainToolError[mcpschema.TaskCreatedOutput](err)
	}
	return mcp.ToolErrorWithMeta[mcpschema.TaskCreatedOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
}

// taskCreateConfigurationValid reports whether the effective harness, model,
// and runtime selections are valid after a rejected override is removed.
func (m *mcpTools) taskCreateConfigurationValid(ctx context.Context, args mcpschema.TaskCreateArgs) bool { //nolint:gocritic // Validation consumes request-shaped arguments.
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

func (m *mcpTools) handleTaskCreate(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskCreateArgs]) mcp.ToolResult[mcpschema.TaskCreatedOutput] {
	if call.Arguments.Prompt == "" {
		return domainToolError[mcpschema.TaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: prompt"})
	}
	if _, ok := taskMCPTaskID(ctx); ok {
		if len(call.Arguments.Repos) != 0 || call.Arguments.Harness != "" || call.Arguments.Model != "" || call.Arguments.Effort != "" || call.Arguments.RuntimeName != "" || call.Arguments.Display || call.Arguments.Tailscale || call.Arguments.USB || call.Arguments.Sudo || call.Arguments.GitHubToken {
			return mcp.ToolError[mcpschema.TaskCreatedOutput]("Task-scoped MCP may only set prompt")
		}
		resp, err := m.taskSvc.createTask(ctx, &v1.CreateTaskReq{InitialPrompt: v1.Prompt{Text: call.Arguments.Prompt}}, nil)
		if err != nil {
			return domainToolError[mcpschema.TaskCreatedOutput](err)
		}
		return mcp.TypedToolResult(mcpschema.TaskCreatedOutput{Result: "Created child task: " + resp.ID.String(), TaskID: resp.ID.String()})
	}
	apiHarness, err := m.resolveTaskCreateHarness(ctx, call.Arguments.Harness)
	if err != nil {
		if call.Arguments.Harness == "" {
			return domainToolError[mcpschema.TaskCreatedOutput](err)
		}
		return m.taskCreateToolError(ctx, call.Arguments, &api.Error{Status: http.StatusBadRequest, Code: api.CodeUnknownHarness, Message: err.Error()})
	}
	model, effort := m.resolveTaskCreateModel(ctx, apiHarness, call.Arguments.Model, call.Arguments.Effort)
	req := &v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: call.Arguments.Prompt},
		Repos:         make([]v1.RepoSpec, len(call.Arguments.Repos)),
		Harness:       apiHarness,
		Model:         model,
		Effort:        effort,
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
		return domainToolError[mcpschema.TaskCreatedOutput](err)
	}
	resp, err := m.taskSvc.createTask(ctx, req, nil)
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
	var output mcpschema.TaskCreatedOutput
	if num > 0 {
		output = mcpschema.TaskCreatedOutput{Result: fmt.Sprintf("Created task #%d: %s", num, title), TaskNumber: num, TaskID: resp.ID.String()}
	} else {
		output = mcpschema.TaskCreatedOutput{Result: "Created task: " + title, TaskID: resp.ID.String()}
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

// resolveTaskCreateModel fills an omitted model and effort from the user's
// saved preferences for harness h, as the web UI's task form does. A saved
// value the harness no longer offers resolves to the harness default.
func (m *mcpTools) resolveTaskCreateModel(ctx context.Context, h v1.Harness, model, effort string) (resolvedModel, resolvedEffort string) {
	harnessName, err := apiconv.AgentHarness(h)
	if err != nil {
		return model, effort
	}
	backend, ok := m.serverConfig.taskMgr.Backends[harnessName]
	if !ok {
		return model, effort
	}
	models := backend.ModelInventory().Models
	prefs := m.serverConfig.prefs.Get(userIDFromCtx(ctx))
	if model == "" {
		saved := prefs.Models[string(h)]
		if slices.ContainsFunc(models, func(c agent.Model) bool { return c.ID == saved }) {
			model = saved
		}
	}
	if effort == "" {
		saved, ok := prefs.Efforts[string(h)][model]
		if !ok {
			saved = prefs.Efforts[string(h)][""]
		}
		if i := slices.IndexFunc(models, func(c agent.Model) bool { return c.ID == model }); i >= 0 && slices.Contains(models[i].EffortOptions, saved) {
			effort = saved
		}
	}
	return model, effort
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

func (m *mcpTools) handleTaskGetDetail(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskGetDetailArgs]) mcp.ToolResult[v1.MCPTaskDetail] {
	keys, _ := taskKeys(ctx, m.taskSvc)
	id, number, err := decodeTaskReference(call.Arguments.Task, len(keys))
	if err != nil {
		return domainToolError[v1.MCPTaskDetail](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: err.Error()})
	}
	var ent *taskmgr.Entry
	if number != 0 {
		ent, err = m.entryByNumber(ctx, number)
	} else {
		ent, err = m.entryByID(ctx, id)
	}
	if err != nil {
		return domainToolError[v1.MCPTaskDetail](err)
	}
	t, err := m.taskDTO(ctx, ent)
	if err != nil {
		return domainToolError[v1.MCPTaskDetail](err)
	}
	rt := mcpTaskRuntime{background: ent.Task().BackgroundActivity(), logPath: ent.LogPath.Get()}
	out, err := taskMCPDetail(number, &t, &rt)
	if err != nil {
		return domainToolError[v1.MCPTaskDetail](err)
	}
	// Repository probes happen only when explicitly requested.
	if call.Arguments.IncludeDiffstat {
		idx, err := m.taskSvc.taskDiffIndex(ctx, ent)
		if err != nil {
			msg, code := err.Error(), api.CodeInternalError
			if e, ok := errors.AsType[*api.Error](err); ok {
				msg, code = e.Error(), e.Code
			}
			msg, cut := truncateMCPText(strings.ToValidUTF8(msg, "\ufffd"))
			res := mcp.ToolErrorWithMeta[v1.MCPTaskDetail](msg, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(code)})
			if cut {
				res.Meta[mcpTruncatedMetaKey] = true
			}
			return res
		}
		out.Diffstat, err = taskMCPDiffstat(idx)
		if err != nil {
			return domainToolError[v1.MCPTaskDetail](err)
		}
	}
	res := mcp.TypedToolResult(out)
	if len(out.TruncatedFields) > 0 || out.ChangedPathsTruncated || out.BackgroundItemsTruncated ||
		(out.PendingInput != nil && (out.PendingInput.TextTruncated || out.PendingInput.ErrorTruncated)) ||
		(out.Diffstat != nil && out.Diffstat.Truncated) {
		res.Meta = mcp.MetaObject{mcpTruncatedMetaKey: true}
	}
	return res
}

func (m *mcpTools) handleTaskSendMessage(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskSendMessageArgs]) mcp.ToolResult[mcp.TextOutput] {
	return m.sendTaskInput(ctx, mcpTaskInputArgs{TaskNumber: call.Arguments.TaskNumber, Message: call.Arguments.Message, DeliveryMode: call.Arguments.DeliveryMode}, "message", "Sent message to task #%d.")
}

func (m *mcpTools) handleTaskAnswerQuestion(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskAnswerQuestionArgs]) mcp.ToolResult[mcp.TextOutput] {
	return m.sendTaskInput(ctx, mcpTaskInputArgs{TaskNumber: call.Arguments.TaskNumber, Message: call.Arguments.Answer}, "answer", "Answered task #%d.")
}

func (m *mcpTools) handleTaskPushBranchToRemote(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskPushBranchArgs]) mcp.ToolResult[mcp.TextOutput] {
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
	return pushToolResult(num, req.Target, resp)
}

// pushToolResult reports a finished push. A blocked push pushed nothing; only
// a push to the task branch can be retried with force.
func pushToolResult(num int, target v1.SyncTarget, resp *v1.SyncResp) mcp.ToolResult[mcp.TextOutput] {
	dest := "the task branch"
	if target == v1.SyncTargetDefault {
		dest = "main"
	}
	var head, tail string
	switch {
	case resp.Status == "blocked" && target == v1.SyncTargetDefault:
		head = fmt.Sprintf("Did not push task #%d to main: blocked by safety issues:", num)
		tail = "A push to main cannot be forced. Remove the flagged content from the task's commits, or push to the task branch with force."
	case resp.Status == "blocked":
		head = fmt.Sprintf("Did not push task #%d to the task branch: blocked by safety issues:", num)
		tail = "Retry with force to push anyway."
	case len(resp.SafetyIssues) == 0:
		return mcp.TextToolResult(fmt.Sprintf("Pushed task #%d to %s.", num, dest))
	default:
		head = fmt.Sprintf("Pushed task #%d to %s despite safety issues:", num, dest)
	}
	issueCount := min(len(resp.SafetyIssues), maxMCPSafetyIssues)
	lines := make([]string, 0, issueCount+3)
	lines = append(lines, head)
	for _, issue := range resp.SafetyIssues[:issueCount] {
		lines = append(lines, fmt.Sprintf("- **%s** %s: %s", issue.Kind, issue.File, issue.Detail))
	}
	if omitted := len(resp.SafetyIssues) - issueCount; omitted > 0 {
		lines = append(lines, fmt.Sprintf("- … %d more safety issues omitted", omitted))
	}
	if tail != "" {
		lines = append(lines, tail)
	}
	return boundedTextToolResult(strings.Join(lines, "\n"))
}

func (m *mcpTools) handleTaskStop(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
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

func (m *mcpTools) handleTaskCancelPush(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	if _, err = m.taskSvc.cancelTaskSync(ctx, entry, &api.EmptyReq{}); err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	return mcp.TextToolResult(fmt.Sprintf("Cancelling the push of task #%d.", num))
}

func (m *mcpTools) handleTaskPurge(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
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

func (m *mcpTools) handleTaskRevive(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
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
func (m *mcpTools) taskForkToolError(args mcpschema.TaskForkArgs, source *taskrun.Task, err error) mcp.ToolResult[mcpschema.TaskForkOutput] {
	apiErr, ok := errors.AsType[*api.Error](err)
	if !ok {
		return domainToolError[mcpschema.TaskForkOutput](err)
	}
	message := apiErr.Error()
	switch apiErr.Code {
	case api.CodeUnknownHarness:
		if args.Harness == "" || !m.forkWithoutHarnessValid(source, args.Model) {
			return domainToolError[mcpschema.TaskForkOutput](err)
		}
		message += ". Omit harness to inherit the source task's harness, then retry task_fork."
	case api.CodeUnsupportedModel:
		if args.Model == "" || !m.forkWithoutModelValid(source, args.Harness) {
			return domainToolError[mcpschema.TaskForkOutput](err)
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
		return domainToolError[mcpschema.TaskForkOutput](err)
	}
	return mcp.ToolErrorWithMeta[mcpschema.TaskForkOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
}

func (m *mcpTools) forkWithoutHarnessValid(source *taskrun.Task, modelOverride string) bool {
	backend, ok := m.taskSvc.taskMgr.Backends[source.Harness]
	model := modelOverride
	if model == "" {
		model = source.RequestedModel
	}
	return ok && (model == "" || slices.Contains(backend.ModelInventory().IDs(), model))
}

// forkWithoutModelValid reports whether the effective harness can accept the
// source model after an explicit model override is omitted.
func (m *mcpTools) forkWithoutModelValid(source *taskrun.Task, harnessOverride string) bool {
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

func (m *mcpTools) handleTaskFork(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskForkArgs]) mcp.ToolResult[mcpschema.TaskForkOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, true)
	if err != nil {
		return domainToolError[mcpschema.TaskForkOutput](err)
	}
	if call.Arguments.Prompt == "" {
		return domainToolError[mcpschema.TaskForkOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: prompt"})
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
		return domainToolError[mcpschema.TaskForkOutput](err)
	}
	resp, err := m.taskSvc.forkTask(ctx, entry, req, nil)
	if err != nil {
		return m.taskForkToolError(call.Arguments, entry.Task(), err)
	}
	return mcp.TypedToolResult(mcpschema.TaskForkOutput{Result: fmt.Sprintf("Forked task #%d. New task ID: %s", num, resp.ID.String()), TaskID: resp.ID.String()})
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
		cost := "unknown"
		if w.CostKnown {
			cost = formatUSD(w.CostUSD)
		} else if w.CostUSD != 0 {
			cost = formatUSD(w.CostUSD) + " known subtotal; total unknown"
		}
		lines = append(lines, fmt.Sprintf("%s cost: %s (%d tokens)", w.Duration, cost, w.InputTokens+w.OutputTokens))
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

func (m *mcpTools) handleCloneRepo(ctx context.Context, call *mcp.ToolRequest[mcpschema.CloneRepoArgs]) mcp.ToolResult[mcp.TextOutput] {
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

func (m *mcpTools) handleAgentLastMessage(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskNumberArgs]) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, call.Arguments.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	// A completed task cannot produce further output, so its recorded result is
	// the latest message. Replaying a multi-gigabyte log to confirm that costs
	// seconds per call, and callers poll every waiting task. A task stopped while
	// it waits for an answer reports its earlier recorded result rather than
	// repeating the question, which a stopped task can no longer act on.
	if result := entry.Result(); result != nil && result.AgentResult != "" {
		return boundedTextToolResult(fmt.Sprintf("Task #%d result: %s", num, result.AgentResult))
	}
	message, err := m.taskSvc.taskMgr.LastAgentMessage(ctx, entry)
	if err != nil {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusInternalServerError, Code: api.CodeInternalError, Message: fmt.Sprintf("Task #%d history is unavailable: %v", num, err)})
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

func (m *mcpTools) handleTaskFixPR(ctx context.Context, call *mcp.ToolRequest[mcpschema.TaskFixPRArgs]) mcp.ToolResult[mcp.TextOutput] {
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

func (m *mcpTools) handleBotFixCI(ctx context.Context, call *mcp.ToolRequest[mcpschema.BotFixCIArgs]) mcp.ToolResult[mcpschema.TaskCreatedOutput] {
	if call.Arguments.Repo == "" {
		return domainToolError[mcpschema.TaskCreatedOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: repo"})
	}
	resp, err := m.ci.fixCI(ctx, &v1.BotFixCIReq{Repo: call.Arguments.Repo})
	if err != nil {
		if apiErr, ok := errors.AsType[*api.Error](err); ok && apiErr.Code == api.CodeUnknownRepository {
			message := repositoryRecoveryMessage(ctx, apiErr.Error(), "bot_fix_ci")
			return mcp.ToolErrorWithMeta[mcpschema.TaskCreatedOutput](message, mcp.MetaObject{mcp.ToolErrorCodeMetaKey: string(apiErr.Code)})
		}
		return domainToolError[mcpschema.TaskCreatedOutput](err)
	}
	taskList := m.taskSvc.taskListSnapshot(ctx)
	num := taskNumberForID(taskList, resp.ID.String())
	if num > 0 {
		return mcp.TypedToolResult(mcpschema.TaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task #%d for %s.", num, call.Arguments.Repo), TaskNumber: num, TaskID: resp.ID.String()})
	}
	return mcp.TypedToolResult(mcpschema.TaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task for %s.", call.Arguments.Repo), TaskID: resp.ID.String()})
}

func (m *mcpTools) sendTaskInput(ctx context.Context, args mcpTaskInputArgs, field, format string) mcp.ToolResult[mcp.TextOutput] {
	num, entry, err := m.entryByTaskRef(ctx, args.TaskNumber, false)
	if err != nil {
		return domainToolError[mcp.TextOutput](err)
	}
	if args.Message == "" {
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "Missing required parameter: " + field})
	}
	canceled := false
	switch args.DeliveryMode {
	case "", mcpschema.InputSteer, mcpschema.InputAppend:
	case mcpschema.InputReplace:
		// Match the client edit flow: withdraw the pending prompt by identity,
		// then submit the replacement through the normal queued-input API.
		if p := entry.PendingInput(); p != nil {
			if p.Delivered {
				return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusConflict, Code: api.CodeConflict, Message: "pending prompt was already sent"})
			}
			if _, err := m.taskSvc.cancelQueuedInput(ctx, entry, &v1.CancelQueuedInputReq{ID: p.ID}); err != nil {
				return domainToolError[mcp.TextOutput](err)
			}
			canceled = true
		}
	default:
		return domainToolError[mcp.TextOutput](&api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "delivery_mode must be append, replace, or steer"})
	}
	resp, err := m.taskSvc.sendInput(ctx, entry, &v1.InputReq{Prompt: v1.Prompt{Text: args.Message}, Queue: args.DeliveryMode == mcpschema.InputAppend || args.DeliveryMode == mcpschema.InputReplace}, nil)
	if err != nil {
		if canceled {
			return mcp.ToolErrorWithMeta[mcp.TextOutput]("Pending prompt canceled, but sending its replacement failed: "+err.Error(), domainToolError[mcp.TextOutput](err).Meta)
		}
		return domainToolError[mcp.TextOutput](err)
	}
	if resp.Status == "queued" {
		if args.DeliveryMode == mcpschema.InputReplace {
			return mcp.TextToolResult(fmt.Sprintf("Queued message for task #%d in replace mode. Any prior pending prompt was canceled before submitting this message. Delivery occurs when the agent is waiting.", num))
		}
		return mcp.TextToolResult(fmt.Sprintf("Queued message for task #%d in append mode. Existing pending text and attachments are preserved. Delivery occurs when the agent is waiting.", num))
	}
	if entry.PendingInput() != nil {
		return mcp.TextToolResult(fmt.Sprintf(format, num) + " A pending prompt still exists; direct delivery does not clear it. Use task_get_detail to inspect it.")
	}
	return mcp.TextToolResult(fmt.Sprintf(format, num))
}

func (m *mcpTools) entryByNumber(ctx context.Context, num int) (*taskmgr.Entry, error) {
	_, entry, err := m.entryByTaskRef(ctx, mcpschema.TaskReference("#"+strconv.Itoa(num)), false)
	return entry, err
}

func (m *mcpTools) entryByID(ctx context.Context, id ksid.ID) (*taskmgr.Entry, error) {
	entry, ok := inspectableTaskEntry(ctx, m.taskSvc, id)
	if !ok {
		return nil, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "task not found"}
	}
	m.taskSvc.focusTask(ctx, id)
	return entry, nil
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
func (m *mcpTools) entryByTaskRef(ctx context.Context, ref mcpschema.TaskReference, allowSelf bool) (num int, resolved *taskmgr.Entry, err error) {
	defer func() {
		if err == nil {
			m.taskSvc.focusTask(ctx, resolved.Task().ID)
		}
	}()
	raw := strings.TrimSpace(string(ref))
	keys, _ := taskKeys(ctx, m.taskSvc)
	delegatingTaskID, taskScoped := taskMCPTaskID(ctx)
	if number, err := parseTaskNumber(raw, len(keys)); err == nil {
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
	if ctx.Err() != nil {
		return "request authority ended", false
	}
	if m.accounts != nil {
		if u, ok := auth.UserFromContext(ctx); ok {
			current, active := m.accounts.store.ActiveUser(u.ID)
			if !active || current.AccountEpoch != u.AccountEpoch {
				return "account authority ended", false
			}
		}
		if id, ok := taskMCPTaskID(ctx); ok {
			e, found := m.taskSvc.taskMgr.GetEntry(id)
			if !found {
				return "task authority unavailable", false
			}
			if _, release, err := m.taskSvc.taskMgr.OwnerContext(ctx, e.Task().OwnerID); err != nil {
				return "task owner authority ended", false
			} else {
				release()
			}
		}
	}
	if reason, ok := authorizeToolScope(ctx, name); !ok {
		return reason, false
	}
	if _, needsForge := mcpForgeTools[name]; needsForge && isRemoteMCP(ctx) && !userHasForgeAuthority(ctx) {
		return "linked GitHub identity or GitLab token is required for forge MCP tools", false
	}
	return "allow", true
}

func (m *mcpTools) handleSetAccountEnabled(ctx context.Context, call *mcp.ToolRequest[mcpschema.SetAccountEnabledArgs]) mcp.ToolResult[mcpschema.AccountEnabledOutput] {
	if call.Arguments.UserID == "" {
		return mcp.ToolError[mcpschema.AccountEnabledOutput]("user_id is required")
	}
	if call.Arguments.Enabled == nil {
		return mcp.ToolError[mcpschema.AccountEnabledOutput]("enabled is required")
	}
	out, err := m.accounts.setEnabled(ctx, call.Arguments.UserID, *call.Arguments.Enabled)
	if err != nil {
		return domainToolError[mcpschema.AccountEnabledOutput](err)
	}
	return mcp.TypedToolResult(out)
}

func annotateTool(spec mcp.ToolSpec, annotations mcp.ToolAnnotations) mcp.ToolSpec { //nolint:gocritic // Tool specs are immutable catalog entries; value style keeps call sites simple.
	spec.Annotations = &annotations
	return spec
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

type mcpTaskInputArgs struct {
	TaskNumber   mcpschema.TaskReference
	Message      string
	DeliveryMode mcpschema.InputDeliveryMode
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
		{Meta: mcp.MetaObject{gomode.ModelContextMetaKey: true}, URI: "caic://conversation/context", Name: "context", Title: "Conversation context", Description: "Authorized caic conversation state", MimeType: "application/json"},
		{Meta: mcp.MetaObject{gomode.ModelContextMetaKey: true}, URI: "caic://catalog", Name: "items", Title: "Items", Description: "Generic service item status for native clients", MimeType: "application/json"},
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
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model override (optional). Omit unless the user explicitly chose a model; omitted means the user's saved model for the selected harness, else its default."})
	props.Set("effort", &jsonschema.Schema{Type: "string", Description: "Thinking effort override (optional). Omit unless the user explicitly chose an effort; omitted means the user's saved effort for the selected model, else its default."})
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

var errNotTaskNumber = errors.New("not a task number")

// parseTaskNumber accepts an explicit "#N" or a bare integer. A bare
// integer above the list range can also be a canonical all-digit stable ID.
func parseTaskNumber(raw string, maxTaskNumber int) (int, error) {
	raw = strings.TrimSpace(raw)
	digits, explicit := strings.CutPrefix(raw, "#")
	number, err := strconv.Atoi(strings.TrimSpace(digits))
	if err != nil {
		return 0, errNotTaskNumber
	}
	if !explicit && number > maxTaskNumber {
		if id, err := ksid.Parse(raw); err == nil && id.String() == raw {
			return 0, errNotTaskNumber
		}
	}
	return number, nil
}

// decodeTaskReference resolves a task number up to maxTaskNumber or a stable ID.
func decodeTaskReference(id mcpschema.TaskReference, maxTaskNumber int) (ksid.ID, int, error) {
	raw := string(id)
	if number, err := parseTaskNumber(raw, maxTaskNumber); err == nil {
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

func (m *mcpTools) handleListAccounts(ctx context.Context, _ *mcp.ToolRequest[struct{}]) mcp.ToolResult[mcpschema.AccountsOutput] {
	out, err := m.accounts.list(ctx)
	if err != nil {
		return domainToolError[mcpschema.AccountsOutput](err)
	}
	return mcp.TypedToolResult(out)
}

func (m *mcpTools) handleSetAccountRole(ctx context.Context, call *mcp.ToolRequest[mcpschema.SetAccountRoleArgs]) mcp.ToolResult[mcpschema.AccountRoleOutput] {
	if call.Arguments.UserID == "" {
		return mcp.ToolError[mcpschema.AccountRoleOutput]("user_id is required")
	}
	out, err := m.accounts.setRole(ctx, call.Arguments.UserID, call.Arguments.Role)
	if err != nil {
		return domainToolError[mcpschema.AccountRoleOutput](err)
	}
	return mcp.TypedToolResult(out)
}
