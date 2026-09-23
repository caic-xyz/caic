# OpenCode ACP Agent Backend

Implements `agent.Backend` for OpenCode via ACP (Agent Client Protocol):
JSON-RPC 2.0 over stdin/stdout, analogous to the Codex harness.

## Architecture

- `opencode.go` — Backend lifecycle, handshake, `wireFormat` state machine
- `wire_test.go` — Wire type unmarshaling tests (types from `github.com/maruel/genai/providers/opencode`)
- `parse.go` — Stateless parser: `session/update` notifications → `agent.Message`
- `parse_test.go` — Parser tests including wireFormat prompt response handling

Wire types are provided by `github.com/maruel/genai/providers/opencode` (imported as `oc`).

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

## Native subagent evidence

The OpenCode 1.18.31 standardized recording drove caic's own ACP transport and
used the native `task` tool: a `tool_call` session update with `title: "task"`
and `kind: "think"` (empty `rawInput`), the in-progress `tool_call_update` that
carries the structured `description`/`subagent_type`/`prompt` input, and the
completed update with the child `<task>` output, whose `<task_result>` body
becomes the card result. Those three records are the
retained v3 fixture, so the adapter is exercised on the transport production
actually uses. Earlier evidence came from `opencode run --format json`, whose
event envelope differs from ACP; do not treat those outer event names as ACP
fields.

`native_subagent.go` recognizes delegation only from that ACP task identity plus
structured input: a pending update proves nothing, a normalized display name
such as `Agent` is not proof, and an unrelated tool call never creates a
lifecycle. The matching `v1.18.31` source is the authority for ACP: retain only
the tool-call ID, title/kind, input, lifecycle status, and completion output
that its `session/update` tool events expose. The child session ID and parent
session ID from the run trace are useful correlation evidence but are not yet a
promised ACP contract. The ACP task call blocks the parent until it completes, so
its card is never `Background`: it always settles before the parent result.

The authoritative ACP projection is
https://github.com/anomalyco/opencode/blob/v1.18.31/packages/opencode/src/acp/event.ts,
which emits `tool_call` and `tool_call_update` session updates from tool parts.

## Key Design Decisions

- **Upstream naming**: Go types mirror ACP SDK naming (e.g. `AgentMessageChunkUpdate`,
  `ToolCallUpdate`) to simplify syncing with the OpenCode source.
- **ACP over run mode**: `opencode run` is single-turn per process (no stdin
  loop). ACP provides long-lived JSON-RPC over stdin/stdout with multi-turn.
- **Permission auto-approve**: `session/request_permission` requests are passed
  through as RawMessage; permissions should be set to `"always"` in
  `opencode.json` config.

## References

Source code:
- https://github.com/anomalyco/opencode
- https://github.com/anomalyco/opencode/tree/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5: source revision inspected for the prompt-cache behavior below
- https://github.com/anomalyco/opencode/blob/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5/packages/opencode/src/provider/transform.ts: AI SDK cache-marker and cache-key construction
- https://github.com/anomalyco/opencode/blob/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5/packages/opencode/src/acp/usage.ts: ACP usage projection
- https://github.com/anomalyco/opencode/blob/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5/packages/opencode/src/session/session.ts: provider usage normalization
- https://github.com/anomalyco/opencode/blob/69c172e8a7c0086887b1f93ed5a162f14b6aa0c5/packages/llm/src/cache-policy.ts: experimental native-runtime cache policy

Documentation:
- https://opencode.ai/docs/acp/: ACP documentation
- https://agentclientprotocol.com: ACP specification
- https://opencode.ai/docs/config/: configuration format
- https://opencode.ai/docs/providers/: provider configurations

## Prompt Cache Controls

OpenCode has no dedicated environment variable or provider option for prompt
cache TTL in the inspected source. Its default AI SDK runtime adds ephemeral
cache markers for supported Claude-style providers, including Anthropic,
Bedrock, Anthropic through Vertex, OpenRouter, and compatible endpoints. It
also supplies the session ID as a prompt-cache key for known OpenAI, Azure,
xAI, Mistral, DeepInfra, Cerebras, Venice, and OpenCode-managed paths. The
provider option `setCacheKey: true` forces a key for another provider;
`setCacheKey: false` suppresses OpenCode's automatic key for the recognized
paths. OpenCode's AI Gateway integration requests `caching: "auto"`.

`OPENCODE_EXPERIMENTAL_NATIVE_LLM=true` can change cache placement for the
providers supported by the opt-in native runtime. That runtime defaults to its
`auto` policy: for a supported Anthropic request, the last tool, last system
part, and latest user message are marked. The underlying library also
implements this policy for Bedrock, but OpenCode's current native-runtime gate
does not admit Bedrock. Its internal API supports a `ttlSeconds` cache hint,
but the OpenCode session adapter does not expose or set one, so this environment
variable is not a TTL control. Unsupported native requests fall back to the AI
SDK runtime.

`OPENCODE_CONFIG`, `OPENCODE_CONFIG_CONTENT`, `OPENCODE_CONFIG_DIR`, and
`OPENCODE_DISABLE_PROJECT_CONFIG` can indirectly change caching by selecting
configuration containing `provider.options.setCacheKey`, provider base URLs,
headers, or models. Provider credential and endpoint environment variables can
likewise select a different service or gateway. These are configuration and
routing controls, not evidence of an applied cache duration.

ACP's prompt result exposes `cachedReadTokens` and `cachedWriteTokens`, while
OpenCode normalizes provider usage across many incompatible APIs. It exposes no
cache-duration bucket, applied retention policy, or TTL. Therefore caic must
leave `agent.Usage.CacheTTLSeconds` unknown for OpenCode, even when the selected
adapter normally emits a five-minute marker; a gateway or compatible endpoint
may alter or reject that request.

## Skill Reads

OpenCode has no Skill tool. `parse.go` infers a read from the `read` tool's
`filePath` and the `shell` tool's `command`. The first `tool_call` carries an
empty `rawInput`, and the arguments arrive on an `in_progress` update. OpenCode
can repeat that update with the same input; the wire parser emits the read once
per tool-call ID and clears its state when the call completes.

OpenCode scans more roots than the other harnesses. It reads `~/.claude` and
`~/.agents` like Claude Code, then its own config directories:
`~/.config/opencode`, `~/.opencode`, and any `.opencode` between the working
directory and the worktree root. It globs `{skill,skills}/**/SKILL.md`, so the
directory name is singular or plural and a skill nests; the directory holding
`SKILL.md` names it. The authoritative list is
`packages/opencode/src/skill/index.ts` with
`packages/opencode/src/config/paths.ts`.

Two roots stay out of reach. The `skills.paths` config names arbitrary
directories and `skills.urls` downloads into OpenCode's cache; a path rule
cannot know either.

`available_commands_update` lists skills as commands. It advertises what is
available, not what was loaded.

## Not Yet Exposed

ACP methods caic does not send. caic loads a session with `session/load` for
reconnect, and sets the model only during the handshake.

- Cancel the running turn — `session/cancel`
- Fork a session — `session/fork`
- Resume a session — `session/resume`
- Switch mode — `session/set_mode`
- Switch model mid-session — `session/set_model`, which caic sends only in the
  handshake
- List or close sessions — `session/list`, `session/close`
