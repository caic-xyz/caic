// MCP tool registry, schemas, and resource catalog.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/tasks"
	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

type caicToolCatalogState struct {
	Harnesses      []string
	Repos          []string
	DefaultHarness string
	DefaultModel   string
	Caps           v1.Config
}

type caicToolRegistry struct {
	serverConfig *serverConfigHandlers
	tasks        *taskAPIService
	ci           *ciHandlers
	usage        *usageHandlers
	webFetch     *webFetchHandlers
}

func (c *caicToolRegistry) specs(ctx context.Context) ([]toolSpec, error) {
	// TODO: This is inefficient.
	state, err := c.catalogState(ctx)
	if err != nil {
		return nil, err
	}
	return c.specsForState(&state), nil
}

func (c *caicToolRegistry) tools(ctx context.Context) ([]mcpToolDescriptor, error) {
	specs, err := c.specs(ctx)
	if err != nil {
		return nil, err
	}
	tools := make([]mcpToolDescriptor, len(specs))
	for i, s := range specs {
		tools[i] = mcpToolDescriptor{Name: s.Name, Title: s.Title, Description: s.Description, InputSchema: s.InputSchema, OutputSchema: s.OutputSchema}
	}
	return tools, nil
}

func (c *caicToolRegistry) callTool(ctx context.Context, name string, argsJSON json.RawMessage) (rawToolResult, error) {
	specs, err := c.specs(ctx)
	if err != nil {
		return rawToolResult{}, err
	}
	for _, s := range specs {
		if s.Name == name {
			return s.Handler(ctx, argsJSON)
		}
	}
	return rawToolResult{}, errInvalidParams("unknown tool: %s", name)
}

func (c *caicToolRegistry) specsForState(s *caicToolCatalogState) []toolSpec {
	createSpec := newToolSpec("task_create", "Create task", "Create a new coding task. Confirm repo and prompt with the user before calling.", c.handleTaskCreate(s.DefaultHarness, s.DefaultModel))
	createSpec.InputSchema = buildTaskCreateSchema(s)

	forkSpec := newToolSpec("task_fork", "Fork task", "Fork a running or waiting task, creating a snapshot of its container on a new branch. The prompt describes what the forked task should do. Optionally override the harness and model.", c.handleTaskFork)
	forkSpec.InputSchema = buildTaskForkSchema(s)

	botFixCISpec := newToolSpec("bot_fix_ci", "Fix repository CI", "Create a task to investigate and fix a failing CI on a repository's default branch.", c.handleBotFixCI)
	botFixCISpec.InputSchema = buildBotFixCISchema(s)

	return []toolSpec{
		newToolSpec("tasks_list", "List tasks", "List all current coding tasks with their status, cost, and duration.", c.handleTasksList),
		createSpec,
		newToolSpec("task_get_detail", "Get task detail", "Get recent activity and status details for a task by its number.", c.handleTaskGetDetail),
		newToolSpec("task_send_message", "Send task message", "Send a text message to a waiting or asking agent by task number.", c.handleTaskSendMessage),
		newToolSpec("task_answer_question", "Answer task question", "Answer an agent's question by task number. The agent is in 'asking' state.", c.handleTaskAnswerQuestion),
		newToolSpec("task_push_branch_to_remote", "Push task branch", "Sync or push a task's changes to GitHub. Push to task branch (default) or squash-push to main.", c.handleTaskPushBranchToRemote),
		newToolSpec("task_stop", "Stop task", "Stop a running or waiting task. The container is preserved and can be revived later.", c.handleTaskStop),
		newToolSpec("task_purge", "Purge task", "Permanently delete a stopped task's container. Cannot be undone.", c.handleTaskPurge),
		newToolSpec("task_revive", "Revive task", "Revive a stopped task, restarting its container and agent session.", c.handleTaskRevive),
		forkSpec,
		newToolSpec("get_usage", "Get usage", "Check current API quota utilization and limits.", c.handleGetUsage),
		newToolSpec("clone_repo", "Clone repository", "Clone a git repository by URL. Optionally specify a local path.", c.handleCloneRepo),
		newToolSpec("agent_last_message", "Get last agent message", "Get latest agent message, question, or result. Call to check what the agent needs or relay to user.", c.handleAgentLastMessage),
		newToolSpec("web_search", "Web search", "Search the web for a query and display the results in an embedded browser.", c.handleWebSearch),
		newToolSpec("web_fetch", "Web fetch", "Open a URL in the embedded browser.", c.handleWebFetch),
		newToolSpec("task_fix_pr", "Fix task PR", "Inject a fix-PR command into an existing task to fix its failing PR CI in auto mode.", c.handleTaskFixPR),
		botFixCISpec,
	}
}

func (c *caicToolRegistry) catalogState(ctx context.Context) (caicToolCatalogState, error) {
	repos, err := c.serverConfig.listRepos(ctx, nil)
	if err != nil {
		return caicToolCatalogState{}, err
	}
	harnesses, err := c.serverConfig.listHarnesses(ctx, nil)
	if err != nil {
		return caicToolCatalogState{}, err
	}
	cfg, err := c.serverConfig.getConfig(ctx, nil)
	if err != nil {
		return caicToolCatalogState{}, err
	}
	state := caicToolCatalogState{Caps: *cfg}
	state.Repos = make([]string, len(*repos))
	for i := range *repos {
		state.Repos[i] = (*repos)[i].Path
	}
	state.Harnesses = make([]string, len(*harnesses))
	for i := range *harnesses {
		state.Harnesses[i] = (*harnesses)[i].Name
	}
	if c.serverConfig.prefs != nil {
		prefs := c.serverConfig.prefs.Get(userIDFromCtx(ctx))
		state.DefaultHarness = prefs.Harness
		if state.DefaultHarness != "" {
			state.DefaultModel = prefs.Models[state.DefaultHarness]
		}
	}
	if state.DefaultHarness == "" && len(state.Harnesses) > 0 {
		state.DefaultHarness = state.Harnesses[0]
	}
	return state, nil
}

func (c *caicToolRegistry) listResources(ctx context.Context) resourcesListResult {
	taskList, repos := c.currentTasksAndRepos(ctx)
	resources := make([]mcpResourceDescriptor, 0, 3+len(repos)+len(taskList))
	resources = append(resources,
		mcpResourceDescriptor{URI: "caic://repos", Name: "repos", Title: "Repositories", Description: "Managed repository summary", MimeType: "application/json"},
		mcpResourceDescriptor{URI: "caic://tasks", Name: "tasks", Title: "Tasks", Description: "Coding task summary", MimeType: "application/json"},
		mcpResourceDescriptor{URI: "caic://usage", Name: "usage", Title: "Usage", Description: "Local and provider usage", MimeType: "application/json"},
	)
	for i := range repos {
		repo := &repos[i]
		resources = append(resources, mcpResourceDescriptor{URI: "caic://repos/" + url.PathEscape(repo.Path), Name: "repo " + repo.Path, Title: repo.Path, MimeType: "application/json"})
	}
	for i := range taskList {
		task := &taskList[i]
		id := task.ID.String()
		resources = append(resources, mcpResourceDescriptor{URI: "caic://tasks/" + id, Name: "task " + id, Title: task.Title, MimeType: "application/json"})
	}
	return resourcesListResult{ResultType: mcpResultTypeComplete, Resources: resources, TTLMS: mcpDefaultTTLMS, CacheScope: mcpCacheScopePrivate}
}

func (c *caicToolRegistry) readResource(ctx context.Context, uri string) (resourcesReadResult, error) {
	taskList, repos := c.currentTasksAndRepos(ctx)
	switch {
	case uri == "caic://repos":
		return resourceJSON(uri, repos)
	case uri == "caic://tasks":
		return resourceJSON(uri, taskList)
	case uri == "caic://usage":
		usage := c.usage.buildResp(ctx)
		return resourceJSON(uri, usage)
	case strings.HasPrefix(uri, "caic://repos/"):
		name, err := url.PathUnescape(strings.TrimPrefix(uri, "caic://repos/"))
		if err != nil {
			return resourcesReadResult{}, errInvalidParams("invalid repo uri: %w", err)
		}
		for i := range repos {
			if repos[i].Path == name {
				return resourceJSON(uri, repos[i])
			}
		}
		return resourcesReadResult{}, errInvalidParams("repo not found: %s", name)
	case strings.HasPrefix(uri, "caic://tasks/"):
		id := strings.TrimPrefix(uri, "caic://tasks/")
		for i := range taskList {
			if taskList[i].ID.String() == id {
				return resourceJSON(uri, taskList[i])
			}
		}
		return resourcesReadResult{}, errInvalidParams("task not found: %s", id)
	default:
		return resourcesReadResult{}, errInvalidParams("unknown resource: %s", uri)
	}
}

func (c *caicToolRegistry) currentTasksAndRepos(ctx context.Context) ([]v1.Task, []v1.Repo) {
	taskList := c.tasks.taskListSnapshot(ctx)
	repos := repoListFromSnapshot(c.serverConfig.repos.SnapshotWithCI())
	return taskList, *repos
}

func (c *caicToolRegistry) handleTasksList(ctx context.Context, _ struct{}) toolResult[mcpTextOutput] {
	taskList := c.tasks.taskListSnapshot(ctx)
	if len(taskList) == 0 {
		return textToolResult("No tasks running.")
	}
	lines := make([]string, len(taskList))
	for i := range taskList {
		lines[i] = taskSummaryLine(i+1, &taskList[i])
	}
	return textToolResult("## Tasks\n\n" + strings.Join(lines, "\n"))
}

type mcpTaskCreatedOutput struct {
	Result     string `json:"result"               jsonschema_description:"Human-readable task creation result"`
	TaskNumber int    `json:"taskNumber,omitempty" jsonschema_description:"Current session task number"`
	TaskID     string `json:"taskID"               jsonschema_description:"Stable task ID"`
}

type mcpTaskCreateArgs struct {
	Prompt      string   `json:"prompt"                jsonschema_description:"The task description/prompt for the coding agent"`
	Repos       []string `json:"repos"                 jsonschema:"minItems=1"                                                     jsonschema_description:"Repositories to work in (one or more)"`
	Model       string   `json:"model,omitempty"       jsonschema_description:"Model to use (optional)"`
	Harness     string   `json:"harness,omitempty"     jsonschema_description:"Agent harness to use (optional)"`
	Display     bool     `json:"display,omitempty"     jsonschema_description:"Enable virtual display (VNC) for this task"`
	Tailscale   bool     `json:"tailscale,omitempty"   jsonschema_description:"Enable Tailscale networking for this task"`
	USB         bool     `json:"usb,omitempty"         jsonschema_description:"Enable USB passthrough for this task"`
	Sudo        bool     `json:"sudo,omitempty"        jsonschema_description:"Enable root access via sudo with a random password"`
	GitHubToken bool     `json:"gitHubToken,omitempty" jsonschema_description:"Enable GitHub token injection for this task"`
}

func (c *caicToolRegistry) handleTaskCreate(defaultHarness, defaultModel string) func(context.Context, mcpTaskCreateArgs) toolResult[mcpTaskCreatedOutput] {
	return func(ctx context.Context, args mcpTaskCreateArgs) toolResult[mcpTaskCreatedOutput] {
		if args.Prompt == "" {
			return toolError[mcpTaskCreatedOutput]("Missing required parameter: prompt")
		}
		if len(args.Repos) == 0 {
			return toolError[mcpTaskCreatedOutput]("Missing required parameter: repos")
		}
		harness := args.Harness
		if harness == "" {
			harness = defaultHarness
		}
		model := args.Model
		if model == "" {
			model = defaultModel
		}
		req := &v1.CreateTaskReq{
			InitialPrompt: v1.Prompt{Text: args.Prompt},
			Repos:         make([]v1.RepoSpec, len(args.Repos)),
			Harness:       v1.Harness(harness),
			Model:         model,
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
		resp, err := c.tasks.createTask(ctx, req)
		if err != nil {
			return domainToolError[mcpTaskCreatedOutput](err)
		}
		taskList := c.tasks.taskListSnapshot(ctx)
		num := taskNumberForID(taskList, resp.ID.String())
		title := resp.ID.String()
		for i := range taskList {
			if taskList[i].ID == resp.ID {
				title = taskTitle(&taskList[i])
				break
			}
		}
		if num > 0 {
			return typedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created task #%d: %s", num, title), TaskNumber: num, TaskID: resp.ID.String()})
		}
		return typedToolResult(mcpTaskCreatedOutput{Result: "Created task: " + title, TaskID: resp.ID.String()})
	}
}

type mcpTaskNumberArgs struct {
	TaskNumber int `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
}

func (c *caicToolRegistry) handleTaskGetDetail(ctx context.Context, args mcpTaskNumberArgs) toolResult[mcpTextOutput] {
	if args.TaskNumber == 0 {
		return toolError[mcpTextOutput]("Missing required integer: task_number")
	}
	t, ok := c.taskByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	lines := []string{
		fmt.Sprintf("## Task #%d: %s", args.TaskNumber, taskTitle(&t)),
		"",
		fmt.Sprintf("State: %s  Elapsed: %s  Cost: %s", t.State, formatElapsed(time.Duration(t.Duration*float64(time.Second))), formatCost(t.CostUSD)),
	}
	if t.State == v1.TaskStatePurged && t.Result != "" {
		lines = append(lines, "**Result:** "+t.Result)
	}
	if t.State == v1.TaskStateStopped {
		lines = append(lines, "**Stopped:** container died")
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
	return textToolResult(strings.TrimSpace(strings.Join(lines, "\n")))
}

type mcpTaskSendMessageArgs struct {
	TaskNumber int    `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
	Message    string `json:"message"     jsonschema_description:"The message to send to the agent"`
}

func (c *caicToolRegistry) handleTaskSendMessage(ctx context.Context, args mcpTaskSendMessageArgs) toolResult[mcpTextOutput] {
	return c.sendTaskInput(ctx, mcpTaskInputArgs(args), "message", "Sent message to task #%d.")
}

type mcpTaskAnswerQuestionArgs struct {
	TaskNumber int    `json:"task_number" jsonschema_description:"The task number, e.g. 1 for task #1"`
	Answer     string `json:"answer"      jsonschema_description:"The answer to the agent's question"`
}

func (c *caicToolRegistry) handleTaskAnswerQuestion(ctx context.Context, args mcpTaskAnswerQuestionArgs) toolResult[mcpTextOutput] {
	return c.sendTaskInput(ctx, mcpTaskInputArgs{TaskNumber: args.TaskNumber, Message: args.Answer}, "answer", "Answered task #%d.")
}

type mcpTaskPushBranchArgs struct {
	TaskNumber int    `json:"task_number"      jsonschema_description:"The task number, e.g. 1 for task #1"`
	Force      bool   `json:"force,omitempty"  jsonschema_description:"Force sync even with safety issues"`
	Target     string `json:"target,omitempty" jsonschema:"enum=branch,enum=default,enum=main,enum=master"  jsonschema_description:"Where to push: branch (default) or main"`
}

func (c *caicToolRegistry) handleTaskPushBranchToRemote(ctx context.Context, args mcpTaskPushBranchArgs) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	targetRaw := args.Target
	if targetRaw == "main" || targetRaw == "master" {
		targetRaw = string(v1.SyncTargetDefault)
	}
	req := &v1.SyncReq{Force: args.Force, Target: v1.SyncTarget(targetRaw)}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	resp, err := c.tasks.syncTask(ctx, entry, req)
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	verb := fmt.Sprintf("Synced task #%d", num)
	if req.Target == v1.SyncTargetDefault {
		verb = fmt.Sprintf("Pushed task #%d to main", num)
	}
	if len(resp.SafetyIssues) == 0 {
		return textToolResult(verb + ".")
	}
	issueLines := make([]string, len(resp.SafetyIssues))
	for i, issue := range resp.SafetyIssues {
		issueLines[i] = fmt.Sprintf("- **%s** %s: %s", issue.Kind, issue.File, issue.Detail)
	}
	return textToolResult(verb + " with safety issues:\n" + strings.Join(issueLines, "\n"))
}

func (c *caicToolRegistry) handleTaskStop(ctx context.Context, args mcpTaskNumberArgs) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	_, err := c.tasks.stopTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	return textToolResult(fmt.Sprintf("Stopping task #%d.", num))
}

func (c *caicToolRegistry) handleTaskPurge(ctx context.Context, args mcpTaskNumberArgs) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	_, err := c.tasks.purgeTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	return textToolResult(fmt.Sprintf("Purged task #%d.", num))
}

func (c *caicToolRegistry) handleTaskRevive(ctx context.Context, args mcpTaskNumberArgs) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	_, err := c.tasks.reviveTask(ctx, entry, &api.EmptyReq{})
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	return textToolResult(fmt.Sprintf("Reviving task #%d.", num))
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

func (c *caicToolRegistry) handleTaskFork(ctx context.Context, args mcpTaskForkArgs) toolResult[mcpTaskForkOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTaskForkOutput]("Unknown task number")
	}
	if args.Prompt == "" {
		return toolError[mcpTaskForkOutput]("Missing required parameter: prompt")
	}
	req := &v1.ForkTaskReq{Prompt: v1.Prompt{Text: args.Prompt}, Harness: v1.Harness(args.Harness), Model: args.Model}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTaskForkOutput](err)
	}
	resp, err := c.tasks.forkTask(ctx, entry, req)
	if err != nil {
		return domainToolError[mcpTaskForkOutput](err)
	}
	return typedToolResult(mcpTaskForkOutput{Result: fmt.Sprintf("Forked task #%d. New task ID: %s", num, resp.ID.String()), TaskID: resp.ID.String()})
}

func (c *caicToolRegistry) handleGetUsage(ctx context.Context, _ struct{}) toolResult[mcpTextOutput] {
	usage := c.usage.buildResp(ctx)
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
	return textToolResult(strings.Join(lines, "\n"))
}

type mcpCloneRepoArgs struct {
	URL  string `json:"url"            jsonschema_description:"The git repository URL to clone"`
	Path string `json:"path,omitempty" jsonschema_description:"Local directory name (optional, derived from URL if omitted)"`
}

func (c *caicToolRegistry) handleCloneRepo(ctx context.Context, args mcpCloneRepoArgs) toolResult[mcpTextOutput] {
	if args.URL == "" {
		return toolError[mcpTextOutput]("Missing required parameter: url")
	}
	req := &v1.CloneRepoReq{URL: args.URL, Path: args.Path}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	repo, err := c.serverConfig.cloneRepo(ctx, req)
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	base := repo.BaseBranch.Name
	if repo.BaseBranch.Remote != "" {
		base = repo.BaseBranch.Remote + "/" + base
	}
	return textToolResult(fmt.Sprintf("Cloned **%s** (base: %s).", repo.Path, base))
}

func (c *caicToolRegistry) handleAgentLastMessage(ctx context.Context, args mcpTaskNumberArgs) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	c.tasks.taskMgr.LoadMessagesOnDemand(entry)
	history, _, unsub := entry.Task().Subscribe(ctx)
	unsub()
	for _, msg := range slices.Backward(history) {
		switch msg := msg.(type) {
		case *agent.ResultMessage:
			if msg.Result != "" {
				return textToolResult(fmt.Sprintf("Task #%d result: %s", num, msg.Result))
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
				return textToolResult(fmt.Sprintf("Task #%d is asking: %s%s", num, q.Question, suffix))
			}
		case *agent.TextMessage:
			if msg.Text != "" {
				return textToolResult(fmt.Sprintf("Last message from task #%d: %s", num, msg.Text))
			}
		}
	}
	return textToolResult(fmt.Sprintf("No messages from task #%d yet.", num))
}

type mcpWebFetchOutput struct {
	Title   string `json:"title"   jsonschema_description:"Fetched page title"`
	Content string `json:"content" jsonschema_description:"Fetched page content"`
}

type mcpWebSearchArgs struct {
	Query string `json:"query" jsonschema_description:"The search query"`
}

func (c *caicToolRegistry) handleWebSearch(ctx context.Context, args mcpWebSearchArgs) toolResult[mcpWebFetchOutput] {
	if args.Query == "" {
		return toolError[mcpWebFetchOutput]("Missing required parameter: query")
	}
	return c.fetchURL(ctx, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(args.Query))
}

type mcpWebFetchArgs struct {
	URL string `json:"url" jsonschema_description:"The URL to open"`
}

func (c *caicToolRegistry) handleWebFetch(ctx context.Context, args mcpWebFetchArgs) toolResult[mcpWebFetchOutput] {
	if args.URL == "" {
		return toolError[mcpWebFetchOutput]("Missing required parameter: url")
	}
	return c.fetchURL(ctx, args.URL)
}

type mcpTaskFixPRArgs struct {
	TaskNumber int `json:"task_number" jsonschema_description:"The task number whose PR CI should be fixed"`
}

func (c *caicToolRegistry) handleTaskFixPR(ctx context.Context, args mcpTaskFixPRArgs) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	_, err := c.ci.fixPR(ctx, &v1.BotFixPRReq{TaskID: entry.Task().ID.String()})
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	return textToolResult(fmt.Sprintf("Injected fix-PR command into task #%d.", num))
}

type mcpBotFixCIArgs struct {
	Repo string `json:"repo" jsonschema_description:"Repository to fix CI for"`
}

func (c *caicToolRegistry) handleBotFixCI(ctx context.Context, args mcpBotFixCIArgs) toolResult[mcpTaskCreatedOutput] {
	if args.Repo == "" {
		return toolError[mcpTaskCreatedOutput]("Missing required parameter: repo")
	}
	resp, err := c.ci.fixCI(ctx, &v1.BotFixCIReq{Repo: args.Repo})
	if err != nil {
		return domainToolError[mcpTaskCreatedOutput](err)
	}
	taskList := c.tasks.taskListSnapshot(ctx)
	num := taskNumberForID(taskList, resp.ID.String())
	if num > 0 {
		return typedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task #%d for %s.", num, args.Repo), TaskNumber: num, TaskID: resp.ID.String()})
	}
	return typedToolResult(mcpTaskCreatedOutput{Result: fmt.Sprintf("Created fix-CI task for %s.", args.Repo), TaskID: resp.ID.String()})
}

type mcpTaskInputArgs struct {
	TaskNumber int
	Message    string
}

func (c *caicToolRegistry) sendTaskInput(ctx context.Context, args mcpTaskInputArgs, field, format string) toolResult[mcpTextOutput] {
	num, entry, ok := c.entryByNumber(ctx, args.TaskNumber)
	if !ok {
		return toolError[mcpTextOutput]("Unknown task number")
	}
	if args.Message == "" {
		return toolError[mcpTextOutput]("Missing required parameter: " + field)
	}
	_, err := c.tasks.sendInput(ctx, entry, &v1.InputReq{Prompt: v1.Prompt{Text: args.Message}})
	if err != nil {
		return domainToolError[mcpTextOutput](err)
	}
	return textToolResult(fmt.Sprintf(format, num))
}

func (c *caicToolRegistry) fetchURL(ctx context.Context, targetURL string) toolResult[mcpWebFetchOutput] {
	req := &v1.WebFetchReq{URL: targetURL}
	if err := req.Validate(); err != nil {
		return domainToolError[mcpWebFetchOutput](err)
	}
	resp, err := c.webFetch.webFetch(ctx, req)
	if err != nil {
		return domainToolError[mcpWebFetchOutput](err)
	}
	return typedToolResult(mcpWebFetchOutput{Title: resp.Title, Content: resp.Content})
}

func (c *caicToolRegistry) taskByNumber(ctx context.Context, num int) (v1.Task, bool) {
	taskList := c.tasks.taskListSnapshot(ctx)
	if num < 1 || num > len(taskList) {
		return v1.Task{}, false
	}
	return taskList[num-1], true
}

func (c *caicToolRegistry) entryByNumber(ctx context.Context, num int) (int, *tasks.Entry, bool) {
	if num == 0 {
		return 0, nil, false
	}
	t, ok := c.taskByNumber(ctx, num)
	if !ok {
		return num, nil, false
	}
	entry, ok := c.tasks.taskMgr.GetEntry(t.ID.String())
	return num, entry, ok
}

// Dynamic schema builders

func buildTaskCreateSchema(s *caicToolCatalogState) *jsonschema.Schema {
	harnessDesc := "Agent harness to use (optional)"
	if s.DefaultHarness != "" {
		harnessDesc = "Agent harness (default: " + s.DefaultHarness + ")"
	}
	minOne := uint64(1)
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The task description/prompt for the coding agent"})
	props.Set("repos", &jsonschema.Schema{Type: "array", Description: "Repositories to work in (one or more)", Items: stringSchemaWithEnum(s.Repos), MinItems: &minOne})
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Model to use (optional)"})
	props.Set("harness", stringSchemaWithEnumDesc(s.Harnesses, harnessDesc))
	props.Set("display", &jsonschema.Schema{Type: "boolean", Description: capDesc(s.Caps.DisplayAvailable, "Enable virtual display (VNC) for this task")})
	props.Set("tailscale", &jsonschema.Schema{Type: "boolean", Description: capDesc(s.Caps.TailscaleAvailable, "Enable Tailscale networking for this task")})
	props.Set("usb", &jsonschema.Schema{Type: "boolean", Description: capDesc(s.Caps.USBAvailable, "Enable USB passthrough for this task")})
	props.Set("sudo", &jsonschema.Schema{Type: "boolean", Description: capDesc(s.Caps.SudoAvailable, "Enable root access via sudo with a random password")})
	props.Set("gitHubToken", &jsonschema.Schema{Type: "boolean", Description: capDesc(s.Caps.GitHubTokenAvailable, "Enable GitHub token injection for this task")})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"prompt", "repos"}}
}

func buildTaskForkSchema(s *caicToolCatalogState) *jsonschema.Schema {
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("task_number", &jsonschema.Schema{Type: "integer", Description: "The task number to fork, e.g. 1 for task #1"})
	props.Set("prompt", &jsonschema.Schema{Type: "string", Description: "The initial prompt for the forked task"})
	props.Set("harness", stringSchemaWithEnumDesc(s.Harnesses, "Override harness (optional, inherits from source if omitted)"))
	props.Set("model", &jsonschema.Schema{Type: "string", Description: "Override model (optional, inherits from source if omitted)"})
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"task_number", "prompt"}}
}

func buildBotFixCISchema(s *caicToolCatalogState) *jsonschema.Schema {
	desc := "Repository to fix CI for"
	if len(s.Repos) == 0 {
		desc = "Repository path to fix CI for"
	}
	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("repo", stringSchemaWithEnumDesc(s.Repos, desc))
	return &jsonschema.Schema{Type: "object", Properties: props, Required: []string{"repo"}}
}

func stringSchemaWithEnum(values []string) *jsonschema.Schema {
	if len(values) == 0 {
		return &jsonschema.Schema{Type: "string"}
	}
	return &jsonschema.Schema{Type: "string", Enum: stringsToAny(values)}
}

func stringSchemaWithEnumDesc(values []string, desc string) *jsonschema.Schema {
	s := stringSchemaWithEnum(values)
	s.Description = desc
	return s
}

func capDesc(available bool, desc string) string {
	if available {
		return desc
	}
	return desc + " (not available on this server)"
}

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
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
	extrasStr := ""
	if len(extras) > 0 {
		extrasStr = ", " + strings.Join(extras, ", ")
	}
	base := fmt.Sprintf("%d. **%s** — %s, %s, %s, %s%s%s", num, taskTitle(t), t.State, formatElapsed(time.Duration(t.Duration*float64(time.Second))), formatCost(t.CostUSD), t.Harness, diffStatSummary(t), extrasStr)
	if t.State == v1.TaskStatePurged && t.Result != "" {
		return base + " — " + truncate(t.Result, 120)
	}
	if t.State == v1.TaskStateStopped {
		return base + " — container died"
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
