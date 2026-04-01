# Codex CLI Package

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
- `wire.go` — JSON-RPC 2.0 type definitions, organized as: shared → input → output
- `parse.go` — Stateless parser: JSON-RPC notifications → `agent.Message`
- `docs/MORE.md` — Future enhancement opportunities (interrupt, steer, compact, review, etc.)

`wireFormat` wraps the stateless parser to accumulate per-turn token usage
from `thread/tokenUsage/updated` notifications, emitting a final `ResultMessage`
with totals on `turn/completed`.

## Upstream Source

Type names in `wire.go` match the upstream Rust definitions:

- `codex-rs/app-server-protocol/src/protocol/v2.rs` — notification and item structs
- `codex-rs/app-server-protocol/src/protocol/common.rs` — method string ↔ struct mapping

When updating wire types, clone https://github.com/openai/codex and diff
against these files to find new fields, item types, or notification methods.

## References

Source code:
- https://github.com/openai/codex

Documentation:
- https://developers.openai.com/codex/cli: CLI documentation
- https://developers.openai.com/codex/cli/reference: CLI reference

## Key Design Decisions

- **Upstream naming**: Go types mirror upstream Rust struct names (e.g. `ThreadStartedNotification`,
  not `ThreadStartedParams`) to simplify syncing with the Codex source.
- **Dynamic model list**: initial `["gpt-5.4"]` replaced after handshake with live list from `model/list`.
- **Error suppression**: notifications with `willRetry=true` are silently dropped.
- **Two-phase file changes**: tool name (`Write` vs `Edit`) determined by checking `kind.type=="add"`.
- **Widget plugin disabled**: TODO comment — needs fixing for Codex.
- **Opt-out capabilities**: handshake disables verbose notifications caic doesn't need
  (e.g., `item/fileChange/outputDelta`, `turn/diff/updated`).
