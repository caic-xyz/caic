# Agent Harness Protocols

## Wire Schema Drift

Runtime harness parsers are deliberately forward-compatible: they ignore wire
fields they do not consume. Do not add live unknown-field warnings or
`UnmarshalJSON` overflow tracking.

Use `make check-agent-logs` to detect schema drift. It strictly validates recent
v2 and v3 task-log records against the corresponding genai DTOs and reports the DTO and
provider file to update. When a wire protocol changes, update both the genai DTO
and the command's type-dispatch registry.

## Native Subagent Lifecycle Evidence

The canonical native-subagent card is parent-turn activity: task state keeps a
task `running` while a card is active even after the parent's trailing
ResultMessage. Only harness-reported lifecycle evidence may create or settle a
card, because a wrong or never-settling card pins the task state. An audit of
the retained task-log cache (`~/.cache/caic/tasks`, 956 logs) found two harnesses
whose cards do not mean "a child is still working":

- **Codex** reports its own root thread through `subAgentActivity` items whose
  `agentPath` is `/root` and whose `kind` is `interacted`; the targeted
  `agentThreadId` is the session's root thread, not a child. `parseActivity`
  folds every `agentThreadId` into a card and maps `interacted` to running, and
  Codex never emits a terminal activity for the root, so that card runs for the
  rest of the session. 16 of the 17 codex logs with native cards ended with the
  root card still running (755 `interacted /root` items corpus-wide). A root
  card must not be created: require a real spawn, or skip the `/root` path.
- **Pi** dispatches an asynchronous `subagent` run with an `asyncId`, and reports
  its completion only in `details.completions` on a `subagent_wait` or `bg_wait`
  tool end. When the parent never waits, the only completion record is the
  pi-subagents async-status widget (`extension_ui_request`/`setWidget`,
  `widgetKey: "subagent-async"`, payload `PI_SUBAGENT_ASYNC_JSON`), which the
  parser does not consume. Several logs show a card still running while that
  widget reports the run `complete` with zero `completions`. Fold the
  async-status snapshot, or treat a Pi running card as weak evidence.

Claude and OpenCode show no such drift in the same corpus: Claude settles a
synchronous delegation (background `run_in_background` agents were not
exercised), and OpenCode's ACP task call settles on completion. A `paused`,
`unknown`, or terminal card is never active, so an unclear status falls back to
`waiting`.

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
