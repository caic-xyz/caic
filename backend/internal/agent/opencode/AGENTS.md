# OpenCode Package

Implements `agent.Backend` for OpenCode via ACP (Agent Client Protocol):
JSON-RPC 2.0 over stdin/stdout, analogous to the Codex harness.

## Architecture

- `opencode.go` — Backend lifecycle, handshake, `wireFormat` state machine
- `wire.go` — ACP JSON-RPC 2.0 type definitions with forward-compatible overflow tracking
- `wire_test.go` — Wire type unmarshaling and overflow detection tests
- `parse.go` — Stateless parser: `session/update` notifications → `agent.Message`
- `parse_test.go` — Parser tests including wireFormat prompt response handling

All inbound wire types embed `jsonutil.Overflow` + custom `UnmarshalJSON()`
to track unknown fields (matching the forward-compatibility pattern used by
the Claude and Codex harnesses).

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

## Key Design Decisions

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
- **Forward compatibility**: All inbound types use `jsonutil.Overflow` to
  capture and warn about unknown fields from future ACP versions.

## References

Source code:
- https://github.com/anomalyco/opencode

Documentation:
- https://opencode.ai/docs/acp/: ACP documentation
- https://agentclientprotocol.com: ACP specification
- https://opencode.ai/docs/config/: configuration format
- https://opencode.ai/docs/providers/: provider configurations
