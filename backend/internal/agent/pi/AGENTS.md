# Pi Coding Agent Backend

Implements `agent.Backend` for Pi coding agent CLI in RPC mode.
Translates Pi's custom JSONL protocol over stdin/stdout into normalized `agent.Message` types.

## Protocol

Pi CLI runs with `--mode rpc --no-session`. No handshake — subprocess is
immediately ready to accept commands. Type-dispatched JSONL (not JSON-RPC 2.0).

Prompts sent as `PromptCmd` with text + optional base64 images.
Events stream on stdout: `message_update` for text/thinking/tool deltas,
`tool_execution_*` for tool lifecycle, `agent_end` with final usage.

Wire types and protocol documentation live in `github.com/maruel/genai/providers/pi`.

## Architecture

- `pi.go` — Backend lifecycle, `piWireFormat` state machine
- `parse.go` — Stateless parser: Pi events → `agent.Message`

## Event → agent.Message Mapping

| Pi event / delta type | agent.Message type |
|-----------------------|--------------------|
| `message_update` (`text_delta`) | TextDeltaMessage |
| `message_update` (`thinking_delta`) | ThinkingDeltaMessage |
| `message_update` (`toolcall_start`) | ToolUseMessage |
| `tool_execution_start` | ToolUseMessage |
| `tool_execution_end` | ToolResultMessage |
| `message_update` (`done`) | ResultMessage |
| `agent_end` | ResultMessage (with usage) |
| `turn_end` | UsageMessage |
| `extension_ui_request` | (auto-respond on stdin) |

## Tool Name Normalization

Pi tool names need normalization to caic canonical names (similar to Gemini/OpenCode).

## Upstream Source

Type definitions in `github.com/maruel/genai/providers/pi` follow the upstream TypeScript:

- `packages/ai/src/types.ts` — Model, UserMessage, AssistantMessage
- `packages/agent/src/types.ts` — AgentMessage, AgentEvent
- `packages/coding-agent/src/modes/rpc/rpc-types.ts` — RPC command/response types

When updating wire types, update `github.com/maruel/genai` and diff against
https://github.com/badlogic/pi-mono to find new commands, event types, or fields.

## References

Source code:
- https://github.com/badlogic/pi-mono

npm package:
- https://www.npmjs.com/package/@mariozechner/pi-coding-agent

## Key Design Decisions

- **No handshake**: unlike Codex/OpenCode, Pi's subprocess is immediately ready.
- **Type-dispatched JSONL**: not JSON-RPC 2.0 — uses a `type` field discriminator.
- **Extension UI auto-response**: confirms all permission prompts and picks first
  option for selects (matching caic's auto-approve policy).
- **Model format**: `provider/modelId` (e.g. `cerebras/gpt-oss-120b`); split on `/`
  for the `set_model` command.
- **Image support**: base64-encoded images sent inline in `PromptCmd.Images`.
- **Thinking support**: reasoning via `thinking_delta` events; configurable via
  `set_thinking_level` command.
- **Compaction**: `compact` command available for context management.
- **Steering**: `steer` and `follow_up` commands for mid-run and post-run messages.
