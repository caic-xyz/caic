# Codex CLI Backend — Integration Spec

OpenAI Codex CLI as an `agent.Backend` alongside Claude and Gemini.

## Codex CLI Overview

Codex CLI is OpenAI's agentic coding tool (Rust binary, npm-installable).
Interactive mode: `codex app-server` starts a JSON-RPC 2.0 server over stdio,
enabling multi-turn interaction, follow-ups, and session resume.

Repo: https://github.com/openai/codex

## Launch Command

```
codex app-server
```

The `app-server` subcommand starts a JSON-RPC 2.0 server on stdin/stdout.
Model selection, prompts, and approval mode are configured via JSON-RPC
requests rather than CLI flags.

## JSON-RPC 2.0 Protocol (`codex app-server`)

### Handshake

The client performs a three-step handshake before sending user prompts:

1. **`initialize` request** — client sends capabilities, server responds with
   server info.
2. **`initialized` notification** — client confirms initialization.
3. **`thread/start` request** (or `thread/resume` for session resume) — server
   responds with `{thread: {id: "..."}}`.

```jsonl
→ {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"client_info":{"name":"caic","version":"1.0.0"},"capabilities":{}}}
← {"jsonrpc":"2.0","id":1,"result":{"server_info":{"name":"codex","version":"0.1.0"}}}
→ {"jsonrpc":"2.0","method":"initialized"}
→ {"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"model":"o4-mini"}}
← {"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}}}
```

### Turn Lifecycle

After the handshake, send a `turn/start` request to begin a turn:

```jsonl
→ {"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"thread_id":"0199a213-...","input":"fix the bug"}}
← {"jsonrpc":"2.0","id":3,"result":{}}
```

The server then emits notifications for the turn:

### Notification Methods

| Method | When | Params |
|--------|------|--------|
| `thread/started` | Thread created | `{thread: {id}}` |
| `turn/started` | Turn begins | `{}` |
| `turn/completed` | Turn ends | `{turn: {status, usage, error?}}` |
| `item/started` | Tool/action begins | `{item: {...}}` |
| `item/updated` | Incremental update | `{item: {...}}` |
| `item/completed` | Tool/action finishes | `{item: {...}}` |
| `item/agentMessage/delta` | Text streaming delta | `{item_id, delta}` |

### Item Types (inside `item/started` / `item/completed` params)

Each item has `item.type`:

| `item.type` | Description | Key Fields |
|-------------|-------------|------------|
| `agent_message` | Final text response | `text` |
| `reasoning` | Model thinking summary | `text` |
| `command_execution` | Shell command | `command`, `aggregated_output`, `exit_code`, `status` |
| `file_change` | File write/edit/delete | `changes[].path`, `changes[].kind` (add/update/delete) |
| `mcp_tool_call` | MCP tool invocation | `server`, `tool`, `arguments`, `result`, `error` |
| `web_search` | Web search | `query` |
| `todo_list` | Plan tracking | `items[].text`, `items[].completed` |
| `error` | Non-fatal warning | `message` |

### Turn Status (in `turn/completed`)

`turn.status`: `completed` | `failed`. When failed, `turn.error` contains
the error message.

### Follow-up Prompts

After a turn completes, send another `turn/start` request with the same
thread ID to continue the conversation:

```jsonl
→ {"jsonrpc":"2.0","id":4,"method":"turn/start","params":{"thread_id":"0199a213-...","input":"now add tests"}}
```

### Session Resume

To resume a previous session, use `thread/resume` instead of `thread/start`
during the handshake:

```jsonl
→ {"jsonrpc":"2.0","id":2,"method":"thread/resume","params":{"thread_id":"0199a213-..."}}
```

### Example Notification Stream

```jsonl
{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}}}
{"jsonrpc":"2.0","method":"turn/started","params":{}}
{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_0","type":"reasoning","text":"**Scanning...**","status":"completed"}}}
{"jsonrpc":"2.0","method":"item/started","params":{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"docs\nsrc\n","exit_code":0,"status":"completed"}}}
{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"item_id":"item_3","delta":"Done."}}
{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_4","type":"file_change","changes":[{"path":"docs/foo.md","kind":"add"}],"status":"completed"}}}
{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"id":"item_3","type":"agent_message","text":"Done.","status":"completed"}}}
{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}}}
```

### Usage Object (in `turn/completed`)

```json
{
  "input_tokens": 24763,
  "cached_input_tokens": 24448,
  "output_tokens": 122
}
```

Codex does not report `total_cost_usd`. Cost must be computed externally or
left at 0.

## Relay Integration

### `--no-log-stdin` flag

The relay daemon (`relay.py`) accepts a `--no-log-stdin` flag for
`serve-attach` mode. When set, the relay still forwards stdin to the
subprocess but does NOT write it to `output.jsonl`. This keeps the log clean:
only server output (notifications + responses) appears in the log, not our
handshake requests and `turn/start` requests.

```
python3 relay.py serve-attach --dir /path --no-log-stdin -- codex app-server
```

## Mapping to `agent.Message`

### JSON-RPC Methods → Go Types

| Codex Method | Go Message Type |
|-------------|---------------|
| `thread/started` | `agent.SystemInitMessage` (subtype "init") |
| `turn/started` | `agent.SystemMessage` (subtype "turn_started") |
| `turn/completed` (completed) | `agent.ResultMessage` |
| `turn/completed` (failed) | `agent.ResultMessage` (is_error=true) |
| `item/completed` + `agent_message` | `agent.AssistantMessage` (text block) |
| `item/completed` + `reasoning` | `agent.AssistantMessage` (text block) |
| `item/started` + `command_execution` | `agent.AssistantMessage` (tool_use, name="Bash") |
| `item/completed` + `command_execution` | `agent.UserMessage` (tool result) |
| `item/completed` + `file_change` | `agent.AssistantMessage` (tool_use, name="Write"/"Edit") |
| `item/started` + `mcp_tool_call` | `agent.AssistantMessage` (tool_use) |
| `item/completed` + `mcp_tool_call` | `agent.UserMessage` (tool result) |
| `item/updated` | `agent.RawMessage` (pass-through) |
| `item/agentMessage/delta` | `agent.StreamEvent` (text delta) |
| JSON-RPC response | `agent.RawMessage` (handshake/turn ack) |

### Tool Name Mapping

Codex doesn't expose individual tool names for its built-in tools the way
Claude/Gemini do. Instead, actions surface as typed items. Map item types:

```go
// Item type → normalized tool name.
var itemTypeToTool = map[string]string{
    "command_execution": "Bash",
    "file_change":       "Edit",  // or "Write" based on changes[].kind
    "web_search":        "WebSearch",
    "todo_list":         "TodoWrite",
}
```

For `mcp_tool_call` items, use the `tool` field directly (MCP tool names are
already provider-agnostic).

### Key Difference: Two-Phase Items

Unlike Claude/Gemini (which emit separate `tool_use` + `tool_result` records),
Codex emits `item/started` (tool invoked) + `item/completed` (result ready)
for the **same item ID**. The parser must:

1. On `item/started` with `command_execution`: emit `AssistantMessage` with a
   `tool_use` content block (tool ID = item ID, name = "Bash", input =
   `{"command": item.command}`).
2. On `item/completed` with `command_execution`: emit `UserMessage` with the
   tool result (parent_tool_use_id = item ID, content = aggregated_output).

For `file_change`, only `item/completed` is emitted (no started phase). Emit
the tool_use with the changes as input.

## Implementation

### `agent/codex/` package

| File | Contents |
|------|----------|
| `codex.go` | `Backend` struct implementing `agent.Backend`. `Harness() → "codex"`. `buildArgs()` returns `["codex", "app-server"]`. Handshake (initialize → initialized → thread/start). `wireFormat` struct with per-session thread ID and WritePrompt for turn/start requests. |
| `record.go` | JSON-RPC envelope `JSONRPCMessage` + notification params types: `ThreadStartedParams`, `TurnCompletedParams`, `ItemParams`, `ItemDeltaParams`. All embed `Overflow`. Inner types: `ItemData`, `FileChange`, `TodoItem`, `TurnUsage`. |
| `parse.go` | `ParseMessage(line []byte) (agent.Message, error)`. Three-way dispatch: caic-injected (has "type"), JSON-RPC response (has "id"), JSON-RPC notification (has "method"). |
| `unknown.go` | `Overflow` infrastructure (same pattern as `agent/gemini/unknown.go`). |
| `parse_test.go` | Test cases covering all notification methods, responses, deltas, and full stream parse. |
| `record_test.go` | JSON-RPC envelope and params unmarshaling tests with unknown field handling. |

### `task/runner.go` — N/A

Codex backend is registered externally via `Server.SetRunnerOps`, same as
Gemini. `initDefaults` only sets Claude as a fallback and was not changed.

### `task/load.go` — Already done

`case agent.Codex: return agentcodex.ParseMessage` in `parseFnForHarness`.

## Constraints

- **No cost reporting.** `TotalCostUSD` will be 0. Token counts are available.
- **Relay compatibility.** The relay daemon (relay.py) is process-agnostic.
  With `--no-log-stdin`, stdin (handshake/turn requests) is not logged to
  output.jsonl, keeping the log clean for replay.
- **`item/started` + `item/completed` pairing.** Unlike the other backends
  where tool_use and tool_result are independent records, Codex pairs them by
  item ID. The parser handles this correctly to avoid duplicate or orphaned
  tool events in the frontend.
