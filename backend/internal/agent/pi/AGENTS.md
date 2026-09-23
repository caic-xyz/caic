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
| `tool_execution_start` | ToolUseMessage |

`toolcall_start` must stay skipped: it precedes `message_end` and would split
the message's streaming deltas from its consolidated content in the frontend
(duplicated assistant text) and duplicate the tool card.
| `tool_execution_end` | ToolResultMessage (+ result output for subagents) |
| `agent_end` | ResultMessage (with usage, duration, numTurns) |
| `turn_end` | UsageMessage (also increments turn counter) |
| `extension_ui_request` | (auto-respond on stdin) |

## Tool Name Normalization

Pi tool names need normalization to caic canonical names (similar to OpenCode).

## Core protocol and subagent extension

Pi core 0.85.1 owns the RPC JSONL envelope and generic tool lifecycle only.
Its versioned event union is
https://github.com/earendil-works/pi/blob/v0.85.1/packages/agent/src/types.ts;
`tool_execution_start` and `tool_execution_end` carry a tool call ID, tool
name, arguments or result, and error flag. Core Pi does not define a
`subagent` or `subagent_wait` tool.

The installed `subagent` and `subagent_wait` behavior is extension-owned, not
a core Pi protocol. The `nicobailon/pi-subagents` source inspected at
https://github.com/nicobailon/pi-subagents/blob/07bd09e0f93a19caee3c39e3cf4069c70ee8dbcd/src/extension/index.ts
registers `subagent`; its
https://github.com/nicobailon/pi-subagents/blob/07bd09e0f93a19caee3c39e3cf4069c70ee8dbcd/src/runs/background/wait-tool.ts
registers its wait tool (named `bg_wait` at that revision). Therefore names
and result fields such as run IDs, modes, completion lists, output state, and
orchestration vocabulary are extension-defined. The standardized recording's
`subagent_wait` name is from the installed extension version, and is not a
core Pi RPC event type.

The Pi 0.85.1 standardized recording proves one extension-owned `subagent`
invocation after a non-spawning `action:list`: the generic core start event
carries the extension's invocation arguments; its end event carries the
extension's run ID and mode; `subagent_wait` later reports the extension's
terminal state, success, agent, model, and output state.

`native_subagent.go` is the stateful adapter from those events to the canonical
`agent.NativeSubagent` lifecycle. A spawn requires a recognized invocation
shape, never merely a tool name that resembles delegation:

- `{ agent, task }` is one agent-scope card. The card is created by the tool end
  that reports the run ID (the start record only stores the delegation metadata);
  an async tool end (a reported `asyncId` or `background`) only acknowledges
  dispatch, marks it running, and marks it `Background`; the run then settles
  from a `subagent_wait`/`bg_wait` completion. When the parent never waits, the
  installed extension's async-status widget is the completion record: the
  `setWidget` request with `widgetKey: subagent-async` and a
  `PI_SUBAGENT_ASYNC_JSON` payload re-publishes the full detached-run set, which
  the adapter folds so a run the extension reports `complete` cannot stay
  running.
- `{ workflowScript }` is one explicit batch-scope card. The installed 0.56.0
  extension removed the legacy top-level `tasks` and `chain` inputs, which stay
  parseable for historical logs only. A workflow reports no per-agent lifecycle,
  so its card is never presented as per-agent progress.
- `{ action }` management calls (`list`, `status`, steering) never spawn, and a
  `bg_wait` completion whose mode is not a subagent orchestration is ignored.

One card is keyed by the extension's run ID whenever a record reports one, and
by the tool call ID only for a synchronous or legacy run without one; the start
record carries no run ID, so it stores the delegation metadata and the end record
that reports the run creates the card. A restart or relay adoption also replays
the relay history that precedes the attach offset through the same wire
(`agent.Options.WarmHistory`), which keeps that metadata available for the end
record and lets the other adapters resolve records that continue after the
restart.

A completion can report the structured `error` and the artifact paths where the
extension wrote the run's output; it never carries the output text, because the
extension documents that the text stays in its artifact files and its wait result
content only summarises the run. The card therefore shows the error, or references
the output artifact, instead of claiming a result the harness never reported.

Completion states map to the canonical statuses: `complete`/`completed` settle
to completed or failed from the reported success and exit codes,
`failed`/`error` to failed, `interrupted`/`stopped`/`cancelled` to interrupted,
and `paused` to the non-terminal paused state, because the extension can resume
a paused run. An unrecognized state stays unknown instead of being invented.

## Upstream Source

Type definitions in `github.com/maruel/genai/providers/pi` follow the upstream TypeScript:

- `packages/ai/src/types.ts` — Model, UserMessage, AssistantMessage
- `packages/agent/src/types.ts` — AgentMessage, AgentEvent
- `packages/coding-agent/src/modes/rpc/rpc-types.ts` — RPC command/response types

When updating wire types, update `github.com/maruel/genai` and diff against
https://github.com/earendil-works/pi to find new commands, event types, or fields.

## References

Source code:
- https://github.com/earendil-works/pi
- https://github.com/earendil-works/pi/tree/d981de1229ef899957bbe968bc8dcda02a21f477: Pi 0.85.1 source revision inspected for the prompt-cache behavior below
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/ai/src/types.ts: cache-retention and usage types
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/ai/src/api/anthropic-messages.ts: Anthropic cache controls and duration buckets
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/ai/src/api/openai-responses.ts: OpenAI Responses cache-key and retention mapping
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/ai/src/api/bedrock-converse-stream.ts: Bedrock cache-point and retention mapping

npm package:
- https://www.npmjs.com/package/@mariozechner/pi-coding-agent

Documentation:
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/coding-agent/docs/environment-variables.md: Pi and provider environment variables
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/coding-agent/docs/providers.md: provider credentials and configuration
- https://github.com/earendil-works/pi/blob/d981de1229ef899957bbe968bc8dcda02a21f477/packages/coding-agent/docs/models.md: custom-provider cache compatibility controls

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
  `compaction_start` becomes a `compact_start` system message, and
  `compaction_end` becomes a `compact_boundary` carrying Pi's
  `tokensBefore`/`estimatedTokensAfter`; a retried attempt emits nothing until it
  settles, and an aborted or failed attempt becomes `compact_error`.
- **Steering**: `steer` and `follow_up` exist in the protocol but caic does not
  send them (see Not Yet Exposed).
- **Duration tracking**: `piWireFormat` records `startTime` when `WritePrompt`
  is called; `handleAgentEnd` computes duration from `startTime` and emits it in
  the final `ResultMessage`. Pi does not emit `message_update:done` — the stream
  ends with `message_end → turn_end → agent_end`.
- **Turn counting**: `handleTurnEnd` increments `numTurns`; `handleAgentEnd`
  reads and resets it for each `ResultMessage`.

## Skill Reads

Pi has no Skill tool. `parse.go` infers a read from the `read` tool's `path`
and the `bash` tool's `command`.

`CommandSourceSkill` marks a slash command's origin in the command catalog. It
advertises what is available, not what was loaded.

## Not Yet Exposed

RPC commands caic does not send. caic already uses `set_model`,
`set_thinking_level`, `get_available_models`, `compact`, and `get_state`; the
provider package defines the full command set.

- Abort the running turn — `abort`, plus `abort_retry` and `abort_bash` for a
  pending retry or shell command
- Steer a run — `steer`, `follow_up`, with `set_steering_mode` and
  `set_follow_up_mode`
- Fork or clone the session — `fork`, `clone`, `get_fork_messages`
- Cycle the model or thinking level — `cycle_model`, `cycle_thinking_level`
- Move between sessions — `new_session`, `switch_session`, `get_tree`,
  `get_entries`
- Toggle automation — `set_auto_compaction`, `set_auto_retry`
