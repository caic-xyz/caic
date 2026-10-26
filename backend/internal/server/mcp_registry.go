// MCP registry composition, voice instructions, task scoping, and scope authorization.

package server

import (
	"context"
	"errors"
	"iter"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/preferences"
	"github.com/maruel/gomode/mcp"
	"github.com/maruel/gomode/oauth"
	"github.com/maruel/ksid"
)

type mcpRegistry struct {
	*mcpTools
	*mcpResources

	prefs *preferences.Store
}

func (m *mcpRegistry) Instructions(ctx context.Context) (string, error) {
	out := caicVoiceSystemInstruction
	if defaults := m.voiceSessionDefaults(ctx); defaults != "" {
		out += "\n\n" + defaults
	}
	return out, nil
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
	prefs := m.prefs.Get(userIDFromCtx(ctx))
	if len(prefs.Repositories) > 0 {
		parts = append(parts, "[Default repo: "+prefs.Repositories[0].Path+"]")
	}
	if prefs.Harness != "" {
		parts = append(parts, "[Default harness: "+prefs.Harness+"]")
	}
	return strings.Join(parts, "\n")
}

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
The client can provide task titles, states, IDs, and task references at session start and in later updates. These updates may not include the agent's message, question, or failure reason. Do not infer missing details from a state alone.

## Mandatory spoken notifications
- Notify the user only when an agent enters the waiting, asking, failed, or crashed state. Do not notify on any other state transition.
- This is a hard requirement with no discretion: your next assistant turn MUST consist only of the announcement, spoken out loud immediately. Do not stay silent, defer the announcement to a later turn, or put any other content or tool call before it. Silence or deferral on that turn is an error.
- Announce every such transition, even when it arrives only as context in a service item update and the user has not asked for a response. This requirement overrides instructions to stay silent, wait for a user request, reply only to the current request, or discuss only the current task.
- Name the task by title and state what happened: waiting means the agent finished a turn and needs input; asking means it has a question; failed means the task failed; crashed means the agent session crashed. If several tasks transition together, announce each in that turn. Do not invent a question, result, or failure reason. Do not call tools to prepare the announcement.
- After announcing a failed or crashed task, use your following turn to call task_get_detail and report the diagnostic under Error or Crashed. A new required announcement takes priority. Read the returned diagnostic faithfully; do not replace it with an invented cause or an unknown-error claim. If the tool returns no diagnostic, say the failure reason is not available.

## Behavior guidelines
- Stick to the current task being discussed; only talk about one task at a time, except for mandatory spoken notifications.
- Summarize task status: state and what the agent is doing. Only mention elapsed time or cost when the user specifically asks.
- For a status update about all tasks, be extremely concise and focus on the waiting tasks. For a status update about one specific task, be detailed and avoid over-summarizing.
- After announcing that an agent is asking, call agent_last_message in a following turn to get its question and options. Read them clearly, wait for the user's answer, then call task_answer_question. Do not guess missing question text.
- When creating a task, omit harness, model, and effort unless the user explicitly asks for an override. caic fills an omitted harness from saved preferences; omitted model and effort use the selected harness defaults. Never pair one harness's model or effort with another harness. Confirm repo and prompt before creating.
- Refer to tasks by title.
- Free tools: agent_last_message, harnesses_list, repos_list, tasks_list, task_get_detail, get_usage. Call them whenever useful without asking.
- When the user asks for a status update, call agent_last_message for each waiting/asking task to get latest output.
- For safety issues during sync, describe each issue and ask whether to force.`

var mcpToolScopes = map[string]string{
	"repos_list":                 mcpScopeRead,
	"harnesses_list":             mcpScopeRead,
	"tasks_list":                 mcpScopeTasksRead,
	"task_get_detail":            mcpScopeTasksRead,
	"agent_last_message":         mcpScopeTasksRead,
	"get_usage":                  mcpScopeRead,
	"get_system_stats":           mcpScopeServerAdmin,
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

func (r scopedMCPRegistry) CallTool(ctx context.Context, params *mcp.ToolsCallParams) (mcp.RawToolResult, error) {
	return r.Registry.CallTool(r.scopedContext(ctx), params)
}

func (r scopedMCPRegistry) ListResources(ctx context.Context, cursor string) (mcp.ResourcesListResult, error) {
	return r.Registry.ListResources(r.scopedContext(ctx), cursor)
}

func (r scopedMCPRegistry) ResourceTemplates(ctx context.Context) ([]mcp.ResourceTemplateDescriptor, error) {
	registry, ok := r.Registry.(mcp.ResourceTemplatesRegistry)
	if !ok {
		return nil, errors.New("MCP resource templates are unavailable")
	}
	return registry.ResourceTemplates(r.scopedContext(ctx))
}

func (r scopedMCPRegistry) ListSkills(ctx context.Context, cursor string) (mcp.SkillsListResult, error) {
	registry, ok := r.Registry.(mcp.SkillsRegistry)
	if !ok {
		return mcp.SkillsListResult{}, errors.New("MCP Skills extension is unavailable")
	}
	return registry.ListSkills(r.scopedContext(ctx), cursor)
}

func (r scopedMCPRegistry) GetSkill(ctx context.Context, uri string) (mcp.SkillsGetResult, error) {
	registry, ok := r.Registry.(mcp.SkillsRegistry)
	if !ok {
		return mcp.SkillsGetResult{}, errors.New("MCP Skills extension is unavailable")
	}
	return registry.GetSkill(r.scopedContext(ctx), uri)
}

func (r scopedMCPRegistry) ReadResource(ctx context.Context, params *mcp.ResourcesReadParams) (mcp.ResourcesReadResult, error) {
	return r.Registry.ReadResource(r.scopedContext(ctx), params)
}

func (r scopedMCPRegistry) SubscribeResourceUpdates(ctx context.Context, filter mcp.SubscriptionFilter) (iter.Seq2[mcp.ResourceUpdate, error], error) {
	registry, ok := r.Registry.(mcp.SubscriptionRegistry)
	if !ok {
		return nil, errors.New("MCP resource subscriptions are unavailable")
	}
	return registry.SubscribeResourceUpdates(r.scopedContext(ctx), filter)
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
