# Kilo Code Package

Implements `agent.Backend` for Kilo Code via an embedded Python bridge
that translates between relay stdin/stdout NDJSON and Kilo's HTTP+SSE API.

## Architecture

```
Go Backend (kilo.Backend)
  → SSH → relay.py (persistent daemon)
    → stdin/stdout NDJSON → bridge.py (embedded Python)
      → HTTP + SSE → kilo serve (local HTTP server)
```

- `kilo.go` — Backend lifecycle, `kiloWireFormat` state machine
- `parse.go` — Stateless parser: Kilo SSE events → `agent.Message`
- `wire.go` — SSE event type definitions and part types
- `models.go` — Model list sorting into three tiers (recent/old/other)
- `bridge.py` — Embedded Python bridge (HTTP+SSE ↔ NDJSON translator)
- `embed.go` — Embeds bridge.py for deployment to containers

## Protocol

Kilo emits SSE events: `message.part.updated`, `message.part.delta`,
`session.turn.close`, `session.error`. Part types: `text`, `tool`,
`reasoning`, `step-start`, `step-finish`.

## State Machine (`kiloWireFormat`)

Wraps the stateless parser to handle:
- **Part type tracking**: records `partID → partType` to route deltas correctly
  (reasoning parts emit `ThinkingDeltaMessage` instead of `TextDeltaMessage`)
- **Per-turn usage accumulation**: step-finish events contribute to running totals;
  `session.turn.close` emits final `ResultMessage` and resets accumulators
- **Error dedup**: `session.error` sets `errorSeen` flag to suppress redundant
  error on subsequent `turn.close`

## Model Sorting (`SortModels`)

Organizes discovered models into three tiers:
1. **Recent** — latest version per family from top providers (anthropic, openai, google, etc.)
2. **Old** — superseded versions with provider-specific staleness rules
3. **Other** — everything else

## References

Source code:
- https://github.com/Kilo-Org/kilocode

Documentation:
- https://kilo.ai/docs/
