# Shared Agent Client Protocol

Keep this package independent of individual harness packages. Native model
routes, effort IDs, launch settings, permissions policy, and tool evidence are
supplied by the owning adapter through `Config`. Do not branch on harness
identity or import a harness provider here. Wire DTOs belong to
`github.com/maruel/genai/acp`.

Preserve directional request correlation, ordered journal recording, bounded
replay, handshake cancellation, task-scoped MCP startup progress, and detached
relay cleanup. Each wire owns fresh tool state; the metadata projection must
report the same session/settings/usage without retaining transcript content.

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
| `config_option_update`  | SessionMetadataMessage (reported model and effort) |
