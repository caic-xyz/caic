# Agent Harness Protocols

## Wire Schema Drift

Runtime harness parsers are deliberately forward-compatible: they ignore wire
fields they do not consume. Do not add live unknown-field warnings or
`UnmarshalJSON` overflow tracking.

Use `make check-agent-logs` to detect schema drift. It strictly validates recent
v2 and v3 task-log records against the corresponding genai DTOs and reports the DTO and
provider file to update. When a wire protocol changes, update both the genai DTO
and the command's type-dispatch registry.

## Capability Additions

Wire capabilities are added without changing the `WireFormat` interface
(`WritePrompt` + `ParseMessage`). Two pieces work together, using compact as the
worked example:

1. An optional interface on `WireFormat` carries the wire write
   (`agent.CompactCommand`). A backend that does not implement it is unaffected.
2. `Backend` advertises the capability with a plain bool method
   (`SupportsCompact()`, backed by `Base.Compact`). The frontend reads it from
   harness metadata and conditionally renders the control.

A new capability follows the same shape: an optional `Write*` interface plus a
`Supports*` bool.

## Not Yet Exposed

Each harness lists the wire mechanisms caic does not send in its own `AGENTS.md`,
next to the provider DTOs that define them: `claudecode/AGENTS.md`,
`codex/AGENTS.md`, `opencode/AGENTS.md`, and `pi/AGENTS.md`. Keep that detail
there; a shared copy drifts from the pinned `genai` DTOs.

Interrupt/cancel is the highest-value gap, and every harness has a mechanism for
it. Today `Stop` kills the whole session, so a single runaway turn cannot be
aborted while keeping the conversation. The remaining gaps are switching model
mid-session, turn steering, and session fork/resume/list; each harness's
`AGENTS.md` names the mechanism it provides.
