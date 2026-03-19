# OpenCode Package

Implements `agent.Backend` for OpenCode via ACP (Agent Client Protocol):
JSON-RPC 2.0 over stdin/stdout, analogous to the Codex harness.

## Architecture

- `opencode.go` — Backend lifecycle, handshake, `wireFormat` state machine
- `wire.go` — ACP JSON-RPC 2.0 type definitions
- `parse.go` — Stateless parser: `session/update` notifications → `agent.Message`

`wireFormat` wraps the stateless parser to accumulate usage and auto-handle
permission requests. The handshake performs `initialize` → `session/new` (or
`session/load` for resume), extracting session ID and model list.

## ACP Handshake

```
→ initialize (protocolVersion:1, clientInfo:{name:"caic"})
← initialize result (agentCapabilities, agentInfo)
→ session/new (cwd: "/repo", mcpServers: []) or session/load (sessionId, cwd, mcpServers: [])
← session result (sessionId, models, modes)
→ session/prompt (sessionId, prompt:[{type:"text",text:"..."}])
← session/update notifications (streaming)
← session/prompt result (stopReason, usage)
```

## Event → agent.Message Mapping

| ACP session/update type | agent.Message type   |
|-------------------------|----------------------|
| `agent_message_chunk`   | TextDeltaMessage     |
| `agent_thought_chunk`   | ThinkingDeltaMessage |
| `tool_call`             | ToolUseMessage       |
| `tool_call_update` (completed/failed) | ToolResultMessage |
| `tool_call_update` (in_progress) | ToolOutputDeltaMessage |
| `plan`                  | TodoMessage          |
| `usage_update`          | UsageMessage         |
| `user_message_chunk`    | UserInputMessage     |
| `current_mode_update`   | SystemMessage        |

## Tool Name Normalization

`normalizeToolName()` maps OpenCode tool titles to caic canonical names:
`bash` → `Bash`, `edit` → `Edit`, `write` → `Write`, etc. Falls back to
kind-based mapping (`execute` → `Bash`, `edit` → `Edit`), then passthrough.

## Key Design Decisions

- **ACP over run mode**: `opencode run` is single-turn per process (no stdin
  loop). ACP provides long-lived JSON-RPC over stdin/stdout with multi-turn.
- **Dynamic model list**: initial `["anthropic/claude-sonnet-4"]` replaced after
  handshake with the live list from `session/new`.
- **Image support**: detected from `agentCapabilities.promptCapabilities.image`
  in the initialize response.
- **Permission auto-approve**: `session/request_permission` requests are passed
  through as RawMessage; permissions should be set to `"always"` in
  `opencode.json` config.
- **`--no-log-stdin`**: relay flag to avoid logging JSON-RPC requests to
  `output.jsonl` (they contain large prompt content).

## References

Source code:
- https://github.com/anomalyco/opencode

Documentation:
- https://opencode.ai/docs/acp/: ACP documentation
- https://agentclientprotocol.com: ACP specification
- https://opencode.ai/docs/config/: configuration format
- https://opencode.ai/docs/providers/: provider configurations
