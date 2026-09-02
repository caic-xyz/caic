# Pi Coding Agent Backend

Implements `agent.Backend` for Pi coding agent CLI in RPC mode.
Translates Pi's custom JSONL protocol over stdin/stdout into normalized `agent.Message` types.

## Protocol

Pi CLI runs with `--mode rpc --approve`. No handshake — subprocess is
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
| `message_update` (`toolcall_start`) | (skipped — no arguments yet; ToolUse comes from `tool_execution_start`) |
| `message_end` | TextMessage / ThinkingMessage consolidated from final assistant content |
| `tool_execution_start` | ToolUseMessage (+ SubagentStartMessage for subagent spawns) |

`toolcall_start` must stay skipped: it precedes `message_end` and would split
the message's streaming deltas from its consolidated content in the frontend
(duplicated assistant text) and duplicate the tool card.
| `tool_execution_end` | ToolResultMessage (+ SubagentEndMessage and result output for subagents) |
| `agent_end` | ResultMessage (with usage, duration, numTurns) |
| `turn_end` | UsageMessage (also increments turn counter) |
| `extension_ui_request` | (auto-respond on stdin) |

## Tool Name Normalization

Pi tool names need normalization to caic canonical names (similar to OpenCode).

## Subagents

Pi's `subagent` tool (normalized to `Agent`) spawns subagents singly, as a
parallel batch (`tasks[]`), or as a phased chain (`chain[]`); introspection
calls (`action: list`/`status`) spawn none. `subagent.go` parses these shapes.
A spawning call emits a `SubagentStartMessage` (driving the frontend progress
panel, like Claude Code) alongside the tool-use, and `tool_execution_end` emits
a `SubagentEndMessage` plus the aggregated result text as tool output. The
`(running...)` progress placeholder is suppressed so the success result is
surfaced whole.

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
- https://github.com/badlogic/pi-mono/tree/e266507b606b9552fa277252644054afd4384b11: source revision inspected for the prompt-cache behavior below
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/ai/src/types.ts: cache-retention and usage types
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/ai/src/api/anthropic-messages.ts: Anthropic cache controls and duration buckets
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/ai/src/api/openai-responses.ts: OpenAI Responses cache-key and retention mapping
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/ai/src/api/bedrock-converse-stream.ts: Bedrock cache-point and retention mapping

npm package:
- https://www.npmjs.com/package/@mariozechner/pi-coding-agent

Documentation:
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/coding-agent/docs/environment-variables.md: Pi and provider environment variables
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/coding-agent/docs/providers.md: provider credentials and configuration
- https://github.com/badlogic/pi-mono/blob/e266507b606b9552fa277252644054afd4384b11/packages/coding-agent/docs/models.md: custom-provider cache compatibility controls

## Prompt Cache Controls

Pi's `CacheRetention` values are `none`, `short`, and `long`, with `short` as
the normal direct-provider default. The coding-agent CLI exposes one direct
cache environment control: `PI_CACHE_RETENTION=long`. It maps long retention
to one hour for Anthropic-style and Bedrock Claude cache markers, and to 24
hours for supported OpenAI endpoints. Any other environment value follows the
short default; `short` is a provider preference, not a universal 300-second
duration. There is no CLI environment value that selects `none`.
Extensions and Pi's internal calls can pass `cacheRetention` directly, including
`none`; for example, compaction summary requests disable prompt caching.

The mapping remains provider-specific:

- Anthropic Messages uses `cache_control: {type: "ephemeral"}` for short
  retention and adds `ttl: "1h"` for long retention when the model's
  compatibility metadata allows it.
- Bedrock Claude emits cache points with the default short TTL or an explicit
  one-hour TTL for long retention when prompt caching is supported.
- OpenAI Responses and direct OpenAI-compatible Chat Completions use the Pi
  session ID for cache affinity when caching is enabled. Long retention maps to
  `prompt_cache_retention: "24h"` only when the endpoint is marked compatible;
  short retention leaves the service default in effect.
- Mistral, OpenAI Codex, OpenRouter, Google, custom endpoints, and Pi-hosted
  providers have adapter- or service-specific behavior. Do not apply an
  Anthropic or OpenAI duration to them based only on token field names.

`PI_CODING_AGENT_DIR` can select different credentials, model definitions, and
compatibility flags. Provider credentials, base URLs, and
`HTTP_PROXY`/`HTTPS_PROXY` can also change which endpoint handles a request and
whether it honors Pi's cache controls. They are configuration or routing
inputs, not proof of the applied TTL. Provider-scoped values stored with Pi
credentials take precedence over the process environment when Pi resolves
these settings, including `PI_CACHE_RETENTION`.

Pi reports normalized `cacheRead`, `cacheWrite`, and provider-computed cost.
Only Anthropic additionally reports `cacheWrite1h`, defined as the one-hour
subset of `cacheWrite`. caic may set `agent.Usage.CacheTTLSeconds` from that
explicit bucket: all-one-hour writes yield 3600 seconds, and mixed one-hour and
short writes yield the first expiry of 300 seconds. A cache write without
`cacheWrite1h` remains unknown because it may come from any provider.

## Key Design Decisions

- **No handshake**: unlike Codex/OpenCode, Pi's subprocess is immediately ready.
- **Type-dispatched JSONL**: not JSON-RPC 2.0 — uses a `type` field discriminator.
- **Project trust**: non-interactive RPC launches pass `--approve` so Pi loads
  trusted project-local settings, instructions, resources, and packages.
- **Extension UI auto-response**: confirms all permission prompts and picks first
  option for selects (matching caic's auto-approve policy).
- **Model format**: `provider/modelId` (e.g. `cerebras/gpt-oss-120b`); split on `/`
  for the `set_model` command.
- **Image support**: base64-encoded images sent inline in `PromptCmd.Images`.
- **Thinking support**: reasoning via `thinking_delta` events; configurable via
  `set_thinking_level` command.
- **Compaction**: `compact` command available for context management.
- **Steering**: `steer` and `follow_up` commands for mid-run and post-run messages.
- **Duration tracking**: `piWireFormat` records `startTime` when `WritePrompt`
  is called; `handleAgentEnd` computes duration from `startTime` and emits it in
  the final `ResultMessage`. Pi does not emit `message_update:done` — the stream
  ends with `message_end → turn_end → agent_end`.
- **Turn counting**: `handleTurnEnd` increments `numTurns`; `handleAgentEnd`
  reads and resets it for each `ResultMessage`.
