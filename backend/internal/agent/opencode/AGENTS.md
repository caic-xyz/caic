# OpenCode ACP Agent Backend

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

## Upstream Source

Type names in `github.com/maruel/genai/providers/opencode` follow the upstream ACP SDK definitions:

- `packages/opencode/src/acp/agent.ts` — session update types and request/response handling

When updating wire types, update `github.com/maruel/genai` and diff against
`agent.ts` to find new session update types or fields.

## Key Design Decisions

- **Upstream naming**: Go types mirror ACP SDK naming (e.g. `AgentMessageChunkUpdate`,
  `ToolCallUpdate`) to simplify syncing with the OpenCode source.
- **ACP over run mode**: `opencode run` is single-turn per process (no stdin
  loop). ACP provides long-lived JSON-RPC over stdin/stdout with multi-turn.
- **Permission auto-approve**: `session/request_permission` requests are passed
  through as RawMessage; permissions should be set to `"always"` in
  `opencode.json` config.
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
