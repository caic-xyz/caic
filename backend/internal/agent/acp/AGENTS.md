# Shared Agent Client Protocol

Keep this package independent of individual harness packages. Native model
routes, effort IDs, permissions policy, and tool evidence are
supplied by the owning adapter through `Config`. Do not branch on harness
identity or import a harness provider here. Wire DTOs belong to
`github.com/maruel/genai/acp`.

Each harness builds its own `Launch{Args, Env}` in its `Start` method and passes
that concrete value to shared ACP startup. Keep native launch configuration in
the owning harness.

Preserve directional request correlation, ordered journal recording, bounded
replay, handshake cancellation, task-scoped MCP startup progress, and detached
relay cleanup. Each wire owns fresh tool state; the metadata projection must
report the same session/settings/usage without retaining transcript content.

## Event → agent.Message Mapping

A `session/prompt` request (UserInputMessage) starts a turn and its response
(ResultMessage) ends it. ACP reports no other turn boundary.

ACP reports no compaction boundary either. When a successful response settles a
prompt whose only content is a text block equal to `Config.CompactPrompt`, both
the full and metadata parsers emit a root `compact_boundary` SystemMessage
right before the ResultMessage, as Claude, Codex, and Pi report compaction
inside its turn. The decision comes from the recorded `session/prompt` input,
so live parsing and log replay agree. The boundary carries no token counts:
the turn's `usage_update` measured the summarization request, so the context
meter stays unknown until the next usage report. A cancelled, refused, or
failed response emits none.

| ACP session/update type | agent.Message type   |
|-------------------------|----------------------|
| `agent_message_chunk`   | TextDeltaMessage     |
| `agent_thought_chunk`   | ThinkingDeltaMessage |
| `tool_call`             | ToolUseMessage / WidgetMessage; also ToolResultMessage when announced completed/failed |
| `tool_call_update` (completed/failed) | ToolResultMessage |
| `tool_call_update` (in_progress) | ToolOutputDeltaMessage |
| `plan`                  | TodoMessage          |
| `usage_update`          | UsageMessage         |
| `user_message_chunk`    | UserInputMessage outside a running prompt; the `session/prompt` input owns a live prompt |
| `current_mode_update`   | SystemMessage (with mode detail) |
| `session_info_update`   | (skipped)            |
| `available_commands_update` | (skipped)        |
| `config_option_update`  | SessionMetadataMessage (reported model and effort) |
