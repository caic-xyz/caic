# Future Enhancements for Agent Communication

This document tracks cross-provider agent capabilities that caic does not yet
expose, and the wire mechanisms each backend offers to implement them. It is a
roadmap, not a description of current behaviour.

## Capability Pattern (established)

Capabilities are added without breaking the `WireFormat` interface (`WritePrompt`
+ `ParseMessage`). Two pieces work together, using **compact** as the worked
example:

1. An **optional interface** on `WireFormat` carries the wire write:
   ```go
   // agent/agent.go
   type CompactCommand interface {
       WriteCompact(w io.Writer, instructions string, logW io.Writer) error
   }
   ```
   A backend opts in by implementing it; backends that don't are unaffected.

2. The **`Backend` advertises** the capability to clients with a plain bool
   method (`SupportsCompact() bool`, backed by `Base.Compact`). The frontend
   reads it from harness metadata and conditionally renders the control.

New capabilities should follow the same shape: an optional `Write*` interface
plus a `Supports*` bool the UI can gate on.

## Provider Mapping

Wire mechanism per provider. `✅` = implemented in caic. `?` = needs
investigation. `N/A` = not supported by provider.

| Feature            | Claude Code              | Codex                       | OpenCode                     | Pi  |
|--------------------|--------------------------|-----------------------------|------------------------------|-----|
| Compact            | ✅ `/compact` msg        | ✅ `thread/compact/start`   | ✅ `/compact` prompt         | ✅  |
| Context usage      | ✅ per-turn usage        | ✅ `tokenUsage/updated`     | ✅ `usage_update`            | ✅  |
| Interrupt          | `ControlInterrupt`       | `turn/interrupt`            | `session/cancel`             | ?   |
| Model switch       | `ControlSetModel`        | `turn/start` model param    | `session/set_model`          | ?   |
| Steer              | N/A                      | `turn/steer`                | N/A                          | ?   |
| Session fork       | N/A                      | `thread/fork`               | `unstable_forkSession`       | ?   |
| Session resume     | `--resume`               | N/A                         | `unstable_resumeSession`     | ?   |
| Mode switch        | N/A                      | N/A                         | `session/set_mode`           | ?   |
| Code review        | N/A                      | `review/start`              | N/A                          | ?   |
| Rollback           | N/A                      | `thread/rollback`           | N/A                          | ?   |

Context usage is surfaced today via per-turn token counts plus the model's
context-window limit (`ContextWindowLimit`), not the provider-specific
"get context usage" requests; that was sufficient for the UI fill indicator.

## Open Tasks

Ordered by effort-to-value:

1. **Interrupt/cancel** — medium effort, high value. Today `Stop` (`SendStop`)
   kills the whole session; there is no way to abort a single runaway turn and
   keep the conversation. Add a `WriteInterrupt` optional interface + a
   `Supports*` bool + a distinct UI button. Each provider has a mechanism (see
   table).
2. **Model switching** — low-medium effort, niche. Switch model mid-session
   instead of starting a new task. Useful for cost escalation (start cheap,
   escalate). Marginal while "new task" is an acceptable workaround.
3. **Turn steering** (Codex `turn/steer`) — medium effort, Codex-specific but
   novel UX: inject guidance mid-turn without interrupting.
4. **Session fork / resume / list** — Codex and OpenCode only; low priority.

Per-provider wire details live with each provider in
`github.com/maruel/genai/providers/*` and that package's `CLAUDE.md`.
