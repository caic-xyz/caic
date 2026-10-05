# Antigravity CLI Backend

Use `github.com/maruel/genai/providers/antigravity` for wire DTOs. The DTOs and
`~/src/genai/providers/antigravity/AGENTS.md` describe agy 1.2.14.

## Evidence

`testdata/turn.ndjson` comes from genai's `GenSync_session_turn1.ndjson`.
The session ID and temporary workspace path are sanitized. It pins init,
streamed text, DONE-step usage, and the terminal result.

Use `testdata/cached-tools.ndjson` when changing usage or tool-output parsing.
It is a minimized recording with sanitized session IDs and workspace paths.
Its init event omits the model. Keep that absence intact.

Keep native subagent mapping disabled until recorded lifecycle evidence pins
its semantics. agy does not expose thinking text in this protocol.

## Integration boundary

The backend is selectable through `backends.Default`.
Use the existing relay and preserve v3 input provenance. Do not alter the frozen
v1 relay.

Result usage, duration, and turn count include the entire conversation,
including resumed processes. Sum DONE-step usage for per-turn totals. Keep
conversation-wide measurements out of neutral per-turn fields.

Run `make check-agent-logs` for offline DTO drift checks. Live parsers accept
unknown fields.
