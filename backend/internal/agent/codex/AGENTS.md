# Codex CLI Agent Backend

Implements `agent.Backend` for OpenAI Codex CLI in app-server mode.
Translates Codex's JSON-RPC 2.0 wire protocol into normalized `agent.Message` types.

## Protocol

Codex CLI runs in **app-server mode** — a JSON-RPC 2.0 NDJSON protocol over stdin/stdout.

**Handshake sequence** (30s timeout):
1. `initialize` request → response
2. `initialized` notification
3. `model/list` request → response (populates model list dynamically)
4. `thread/start` or `thread/resume` → response with thread ID

**Prompt delivery**: `turn/start` JSON-RPC request with text + optional images as data URLs.

**Streaming events**: `item/agentMessage/delta`, `item/reasoning/summaryTextDelta`,
`item/commandExecution/outputDelta`, `item/mcpToolCall/progress`.

## Architecture

- `codex.go` — Backend lifecycle, handshake, `wireFormat` state machine
- `parse.go` — Stateless parser: JSON-RPC notifications → `agent.Message`
- `docs/MORE.md` — Future enhancement opportunities (interrupt, steer, compact, review, etc.)

Wire types are provided by `github.com/maruel/genai/providers/codex` (imported as `cx`).

`wireFormat` wraps the stateless parser to accumulate per-turn token usage
from `thread/tokenUsage/updated` notifications, emitting a final `ResultMessage`
with totals on `turn/completed`.

## Upstream Source

Wire type names in the genai package match the upstream Rust definitions:

- `codex-rs/app-server-protocol/src/protocol/v2.rs` — notification and item structs
- `codex-rs/app-server-protocol/src/protocol/common.rs` — method string ↔ struct mapping

When updating wire types, update `github.com/maruel/genai` and diff against
the upstream Rust definitions to find new fields, item types, or notification methods.

## References

Source code:
- https://github.com/openai/codex
- https://github.com/openai/codex/blob/main/codex-rs/core/src/client.rs: prompt-cache key construction and Responses request assembly
- https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/common.rs: serialized Responses request fields
- https://github.com/openai/codex/tree/50fffd5ed367aa99491d9ec58575626fce4e9dd4: source revision inspected for the prompt-cache controls below

Documentation:
- https://developers.openai.com/codex/cli: CLI documentation
- https://developers.openai.com/codex/cli/reference: CLI reference
- https://developers.openai.com/api/docs/guides/prompt-caching: prompt-cache behavior and retention
- https://developers.openai.com/api/reference/cli/resources/responses/methods/create: prompt-cache options, retention, and usage fields

## Prompt Cache Controls

Current Codex sends `prompt_cache_key`, but it does not send
`prompt_cache_options` or `prompt_cache_retention`. Its current source has no
environment variable that directly controls the model prompt-cache TTL. The
OpenAI service therefore selects the default from the model, organization, and
data-retention policy. Authentication and endpoint environment variables may
change which account or provider handles a request, but they are not TTL
controls and must not be interpreted as an applied cache duration.

Do not confuse the Codex model-catalog cache with model prompt caching.
`DEFAULT_MODEL_CACHE_TTL = 300` and the behavior changed by
https://github.com/openai/codex/commit/7cde2323f3712999e9ab98b16287e08b7735d52f
apply to Codex's local `/models` metadata cache, not to prompt tokens sent to a
model.

Codex's `thread/tokenUsage/updated` event reports cached and cache-write token
counts but not the service's applied retention policy or TTL. Leave
`agent.Usage.CacheTTLSeconds` unknown unless that wire protocol gains an
explicit applied-policy or duration field.

## Key Design Decisions

- **Upstream naming**: Go types mirror upstream Rust struct names (e.g. `ThreadStartedNotification`,
  not `ThreadStartedParams`) to simplify syncing with the Codex source.
- **Dynamic model list**: initial `["gpt-5.4"]` replaced after handshake with live list from `model/list`.
- **Error suppression**: notifications with `willRetry=true` are silently dropped.
- **Two-phase file changes**: tool name (`Write` vs `Edit`) determined by checking `kind.type=="add"`.
- **Widget plugin disabled**: TODO comment — needs fixing for Codex.
- **Opt-out capabilities**: handshake disables verbose notifications caic doesn't need
  (e.g., `turn/diff/updated`, `turn/plan/updated`).
