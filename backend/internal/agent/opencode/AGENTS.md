# OpenCode Package

Implements `agent.Backend` for OpenCode via ACP (Agent Client Protocol):
JSON-RPC 2.0 over stdin/stdout, analogous to the Codex harness.

## Architecture

- `opencode.go` — Backend lifecycle, handshake, `wireFormat` state machine
- `wire_test.go` — Wire type unmarshaling tests (types from `github.com/maruel/genai/providers/opencode`)
- `parse.go` — Stateless parser: `session/update` notifications → `agent.Message`
- `parse_test.go` — Parser tests including wireFormat prompt response handling
- `docs/MORE.md` — Future enhancement opportunities (cancel, fork, resume, compact, modes, etc.)

Wire types are provided by `github.com/maruel/genai/providers/opencode` (imported as `oc`).

Unknown field detection is centralized in `unmarshalNotification` (parse.go)
using `sync.Map` caching, matching the pattern used by the Codex harness.

## ACP Handshake

```
→ initialize (protocolVersion:1, clientInfo:{name:"caic"})
← initialize result (agentCapabilities, agentInfo)
→ session/new (cwd: "/repo", mcpServers: []) or session/load (sessionId, cwd, mcpServers: [])
← session result (sessionId, models, modes)
→ unstable_setSessionModel (sessionId, modelId)   [if model requested]
← set model result
→ session/prompt (sessionId, prompt:[{type:"text",text:"..."}])
← session/update notifications (streaming)
← session/prompt result (stopReason, usage)
```

## Event → agent.Message Mapping

| ACP session/update type | agent.Message type   |
|-------------------------|----------------------|
| `agent_message_chunk`   | TextDeltaMessage     |
| `agent_thought_chunk`   | ThinkingDeltaMessage |
| `tool_call`             | ToolUseMessage / WidgetMessage |
| `tool_call_update` (completed/failed) | ToolResultMessage |
| `tool_call_update` (in_progress) | ToolOutputDeltaMessage |
| `plan`                  | TodoMessage          |
| `usage_update`          | UsageMessage         |
| `user_message_chunk`    | UserInputMessage     |
| `current_mode_update`   | SystemMessage (with mode detail) |
| `session_info_update`   | (skipped)            |
| `available_commands_update` | (skipped)        |
| `config_option_update`  | (skipped)            |

## Content Block Types

Content blocks (`ContentBlock`) support all ACP content types:

| Type | Fields |
|------|--------|
| `text` | Text, Annotations |
| `image` | Data (base64), MimeType, URI |
| `resource` | Resource (embedded resource JSON) |
| `resource_link` | URI, Name, MimeType |

## Tool Call Output

`ToolCallUpdateUpdate` supports two error/output paths (checked in order):
1. `rawOutput` — Structured output (`ToolCallRawOutput.Error` / `.Output`)
2. `content` — Array of `ToolCallContent` blocks (text, diff, image, resource)

## Tool Name Normalization

`normalizeToolName()` maps OpenCode tool titles to caic canonical names:
`bash` → `Bash`, `edit`/`patch` → `Edit`, `write` → `Write`, etc. Falls back to
kind-based mapping (`execute` → `Bash`, `edit` → `Edit`, `fetch` → `WebFetch`),
then passthrough.

## Upstream Source

Type names in `github.com/maruel/genai/providers/opencode` follow the upstream ACP SDK definitions:

- `packages/opencode/src/acp/agent.ts` — session update types and request/response handling

When updating wire types, update `github.com/maruel/genai` and diff against
`agent.ts` to find new session update types or fields.

## Key Design Decisions

- **Upstream naming**: Go types mirror ACP SDK naming (e.g. `AgentMessageChunkUpdate`,
  `ToolCallUpdate`) to simplify syncing with the OpenCode source.
- **Typed enums**: `Method`, `UpdateType`, `ToolStatus`, `ToolKind`, `PlanStatus`
  are typed string enums for compile-time safety.
- **ACP over run mode**: `opencode run` is single-turn per process (no stdin
  loop). ACP provides long-lived JSON-RPC over stdin/stdout with multi-turn.
- **Dynamic model list**: initial `["anthropic/claude-sonnet-4"]` replaced after
  handshake with the live list from `session/new`. Current model listed first.
- **Model selection**: If `opts.Model` is set, sends `unstable_setSessionModel`
  after session creation. Best-effort — logs a warning if the method fails.
- **Image support**: detected from `agentCapabilities.promptCapabilities.image`
  in the initialize response.
- **Permission auto-approve**: `session/request_permission` requests are passed
  through as RawMessage; permissions should be set to `"always"` in
  `opencode.json` config.
- **`--no-log-stdin`**: relay flag to avoid logging JSON-RPC requests to
  `output.jsonl` (they contain large prompt content).
- **Forward compatibility**: Unknown fields are detected via centralized
  `unmarshalNotification` (logs warnings, no per-struct `UnmarshalJSON`).

## References

Source code:
- https://github.com/anomalyco/opencode

Documentation:
- https://opencode.ai/docs/acp/: ACP documentation
- https://agentclientprotocol.com: ACP specification
- https://opencode.ai/docs/config/: configuration format
- https://opencode.ai/docs/providers/: provider configurations
