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

The canonical card is parent-turn activity. Task state keeps a task `running`
after its trailing ResultMessage only while a card reports `running` **and**
`background`: a harness can end the parent turn when it delegates detached work,
but a foreground delegation always settles before the parent result, so a
foreground card still running then is stale or unread evidence and the task
falls back to `waiting`. Only harness-reported evidence may create or settle a
card; `NativeSubagentTimeline` folds it, and a terminal card is never reopened.

An audit of the retained task-log cache (`~/.cache/caic/tasks`, 956 logs) drove
the harness rules below:

- **Claude Code** reports a detached agent through the Agent tool's
  `run_in_background` input and the task record's `is_backgrounded`
  (`patch.is_backgrounded` repeats it). Both set `Background`. Cards come from
  `task_*` records, never the tool use alone, so a resumed session cannot split
  one agent in two.
- **Codex** runs collaborative agents concurrently. A `spawnAgent` call or a
  `started` `subAgentActivity` proves a detached agent; the root thread is not
  one. Do not create a card for `agentPath == "/root"`, and do not create one
  from `interacted`/`completed`/`interrupted` alone: children interact with the
  root, which previously produced a root card that never settled (755
  `interacted /root` items; 16 of 17 codex logs with cards ended root-active). A
  child thread answers for itself: `thread/status/changed` maps `active` to
  running and `idle` to `paused` (resumable), `systemError` to failed, and a
  `turn/completed` with `failed`/`interrupted` is terminal. The wire knows the
  root thread, so a child's `turn/completed` must not end the parent turn.
- **Pi** marks a run detached at the `subagent` tool end (`details.asyncId` or
  `details.background`). Completion normally arrives in `details.completions` on
  `subagent_wait`/`bg_wait`; when the parent never waits, the installed
  pi-subagents extension publishes the full detached-run set in its `setWidget`
  widget (`widgetKey: "subagent-async"`, payload `PI_SUBAGENT_ASYNC_JSON`). Fold
  that snapshot too, or a run the extension already reports `complete` would stay
  running.
- **OpenCode** ACP task calls are synchronous; they never set `Background`.

`paused`, `unknown`, and terminal cards are never active, so any status the
harness does not prove falls back to `waiting`.

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
