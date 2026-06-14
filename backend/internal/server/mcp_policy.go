// MCP tool and resource authorization policy.

package server

import (
	"context"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/forge"
	"github.com/caic-xyz/caic/backend/internal/mcp"
)

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

func (c *caicToolRegistry) authorizeTool(ctx context.Context, name string) (string, bool) {
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
	if _, needsForge := mcpForgeTools[name]; needsForge && isRemoteMCP(ctx) && !userHasForgeIdentity(ctx) {
		return "linked GitHub or GitLab identity is required for forge MCP tools", false
	}
	return "allow", true
}

func authorizeResource(ctx context.Context, uri string) (string, bool) {
	required := mcpScopeRead
	if uri == "caic://tasks" || strings.HasPrefix(uri, "caic://tasks/") {
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

func userHasForgeIdentity(ctx context.Context) bool {
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		return false
	}
	return (u.Provider == forge.KindGitHub || u.Provider == forge.KindGitLab) && u.AccessToken != ""
}

func mcpScopeChallenge(scope string) string {
	if scope == "" {
		scope = mcpScopeRead
	}
	return mcp.BearerScopeChallenge(scope)
}

func redactedResourceJSON(uri string, value any) (mcp.ResourcesReadResult, error) {
	return mcp.ResourceJSON(uri, redactForJSON(value))
}
