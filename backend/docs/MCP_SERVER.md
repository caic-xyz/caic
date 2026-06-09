# caic MCP Server

caic exposes task, repository, CI, usage, and web-fetch operations through an authenticated MCP JSON-RPC endpoint.

## Endpoint

```text
POST /api/caic/v1/mcp
```

The endpoint uses the same auth middleware as protected caic API routes.

Implemented request/response methods:

- `server/discover`
- `tools/list`
- `tools/call`
- `resources/list`
- `resources/read`

Target protocol version: `2026-07-28`.

Every POST must carry matching MCP metadata in the JSON body `_meta` and HTTP headers:

- `Mcp-Protocol-Version`
- `Mcp-Method`
- `Mcp-Name` for `tools/call` and `resources/read`

## Architecture

`mcp.go` is protocol-only. It depends on a small generic registry interface, not caic tool implementation details.

The caic registry owns:

- tool catalog construction
- tool handlers
- resource listing and reads
- service integration with tasks, repos, CI, usage, and web fetch

Tool specs are built from typed handler functions. The tool initializer derives:

- `inputSchema` from the handler input struct
- `outputSchema` from the handler `toolResult[T]` output marker
- a typed JSON argument decoding shim for the generic handler

JSON Schema generation uses `github.com/invopop/jsonschema` with anonymous, non-referenced schemas.

## Tools

Current tools:

- `tasks_list`
- `task_create`
- `task_get_detail`
- `task_send_message`
- `task_answer_question`
- `task_push_branch_to_remote`
- `task_stop`
- `task_purge`
- `task_revive`
- `task_fork`
- `get_usage`
- `clone_repo`
- `agent_last_message`
- `web_search`
- `web_fetch`
- `task_fix_pr`
- `bot_fix_ci`

Domain failures that the model can act on are returned as tool results with `isError: true`. Protocol failures remain JSON-RPC errors.

## Resources

Current resources:

- `caic://repos`
- `caic://repos/{path}`
- `caic://tasks`
- `caic://tasks/{taskID}`
- `caic://usage`

Resource payloads are JSON text content blocks.

## Deferred

Not implemented yet:

- Streamable HTTP subscriptions/progress
- MCP prompts
- external-client personal access tokens or OAuth
- paginated large resources such as full diffs or CI logs
