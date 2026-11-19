# Antigravity CLI Backend

Use `github.com/maruel/genai/providers/antigravity` for wire DTOs. The DTOs and
`~/src/genai/providers/antigravity/AGENTS.md` describe agy 1.5.9. A failed tool
step ends in state `ERROR` with `tool_info.error` set and never reaches `DONE`.

## Evidence

`testdata/turn.ndjson` comes from genai's `GenSync_session_turn1.ndjson`.
The session ID and temporary workspace path are sanitized. It pins init,
streamed text, DONE-step usage, and the terminal result.

Use `testdata/cached-tools.ndjson` when changing usage or tool-output parsing.
It is a minimized recording with sanitized session IDs and workspace paths.
Its init event omits the model. Keep that absence intact.

Keep native subagent mapping disabled until recorded lifecycle evidence pins
its semantics. agy does not expose thinking text in this protocol.

Step activity retains its actual conversation ID and step index. A differing
conversation ID alone does not prove a child. The DTO's explicit
`subagent_info.subagents[].conversation_id` can establish a relationship to
the initialized root, but does not establish native lifecycle statuses or a
canonical card ID. Tests using that DTO shape are synthetic protocol cases,
not recorded lifecycle evidence.

## Integration boundary

The backend is selectable through `backends.Default`.
Use the maintained relay and preserve v3 input provenance.

Result usage, duration, and turn count include the entire conversation,
including resumed processes. Sum DONE-step usage for per-turn totals. Keep
conversation-wide measurements out of neutral per-turn fields. Mark each
DONE-step usage `PerCall`: agy sets usage once per model call, so caic prices
it at the vendor's API rates while the result's conversation-wide usage stays
out of the estimate.

agy 1.5.9 stream-json input rejects every block type but `text` and ignores
unknown fields. Prompt images are written to `agent.AttachmentDir` and sent as
a text block naming the path; its `caic_image` field (`data/v1/prompt.go`) keeps
the image for history. Older records carry images as text data URLs.

Run `go run ./backend/internal/cmd/check-agent-logs` for offline DTO drift checks. Live parsers accept
unknown fields.
