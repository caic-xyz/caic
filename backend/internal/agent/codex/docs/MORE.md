# Future Enhancements for Codex Agent Communication

This document outlines how caic could leverage the Codex CLI app-server protocol
to enhance user capabilities, and how to design these features to work across all
agent backends.

## Current State

caic currently sends only `turn/start` messages after the initial handshake
(initialize → initialized → model/list → thread/start). The handshake opts out
of verbose notifications caic doesn't need. Permission approval is not
implemented — Codex runs in its default approval mode.

## Opportunities

### 1. Turn Interrupt: turn/interrupt

**Problem:** Users can stop a task, but stopping kills the entire session.
There's no way to interrupt a single turn and continue the conversation.

**Codex:** Send a `turn/interrupt` JSON-RPC request with `threadId` and `turnId`.
The agent stops the current turn; the session remains open for follow-up messages.

**caic integration:**
- Add an "Interrupt" button distinct from "Stop". Interrupt cancels the
  current turn; Stop terminates the session.
- Track the current turn ID from `turn/started` notifications (currently
  suppressed — would need to stop suppressing or track from `turn/completed`).

**Cross-provider design:** Define an `agent.Interruptable` interface:
```go
type Interruptable interface {
    WriteInterrupt(w io.Writer, logW io.Writer) error
}
```
- Codex: sends `turn/interrupt` JSON-RPC with thread/turn IDs.
- Claude Code: sends `ControlInterrupt` control request.
- The server checks if the backend implements `Interruptable` before showing
  the button in the UI.

### 2. Turn Steering: turn/steer

**Problem:** Users cannot redirect a running turn without interrupting and
restarting. This is wasteful when the agent is headed in roughly the right
direction but needs a course correction.

**Codex:** Send a `turn/steer` JSON-RPC request with `threadId`, a new `input`
array, and `expectedTurnId`. The agent incorporates the new input into the
current turn without restarting from scratch.

**caic integration:**
- Add a "Steer" action to the task UI that lets users send additional context
  while the agent is working.
- Requires tracking the current turn ID.

**Cross-provider design:** This is Codex-specific — Claude Code has no
equivalent. Use an optional interface:
```go
type Steerable interface {
    WriteSteer(w io.Writer, p agent.Prompt, logW io.Writer) error
}
```

### 3. Context Compaction: thread/compact/start

**Problem:** Long sessions accumulate context until performance degrades.
Users have no visibility into or control over context usage.

**Codex:** Send a `thread/compact/start` JSON-RPC request with `threadId`.
The agent compacts the context window. A `contextCompaction` item is emitted
on completion (already parsed by caic).

**caic integration:**
- Add a "Compact" button to the task UI.
- Already handles `contextCompaction` items in the parser — just needs the
  outbound request.

**Cross-provider design:** Define an `agent.CompactCommand` interface:
```go
type CompactCommand interface {
    WriteCompact(w io.Writer, instructions string, logW io.Writer) error
}
```
- Codex: sends `thread/compact/start` JSON-RPC.
- Claude Code: sends `/compact` as user message content.
- Other providers: no-op or provider-specific.

### 4. Thread Rollback: thread/rollback

**Problem:** Users cannot undo agent actions without stopping the session.

**Codex:** Send a `thread/rollback` JSON-RPC request with `threadId`. The
agent rolls back to a previous state.

**caic integration:**
- Add a "Rollback" button or menu option in the task detail view.
- Requires understanding what state is being rolled back to.

**Cross-provider design:** Codex-specific. No equivalent in Claude Code.

### 5. Permission Approval Flow

**Problem:** caic runs Codex in default approval mode. The agent may block
waiting for approval on commands or file changes, with no way for the user
to respond.

**Codex:** The server sends approval request notifications:
- `item/commandExecution/requestApproval` — approve shell command execution
- `item/fileChange/requestApproval` — approve file modifications
- `item/tool/requestUserInput` — request user input for a tool
- `mcpServer/elicitation/request` — MCP server requests user input

Each requires a JSON-RPC response with the approval decision.

**caic integration:**
- Surface approval requests in the task UI as interactive cards.
- Send approval/denial responses via JSON-RPC.
- This would allow running Codex without blanket auto-approval, giving
  users control over dangerous operations.

**Cross-provider design:** Define an `agent.ApprovalHandler` interface:
```go
type ApprovalHandler interface {
    WriteApproval(w io.Writer, requestID string, approved bool, logW io.Writer) error
}
```
- Codex: sends JSON-RPC response to the approval request.
- Claude Code: sends `control_response` with `can_use_tool` decision.

### 6. Code Review: review/start

**Problem:** Users have no way to request an automated code review within
an active session.

**Codex:** Send a `review/start` JSON-RPC request with `threadId` and a
`target`:
- `uncommittedChanges` — review uncommitted changes
- `baseBranch { branch }` — review changes against a base branch
- `commit { sha }` — review a specific commit
- `custom { instructions }` — review with custom instructions

Review can be delivered `inline` (in the conversation) or `detached`
(separate review thread).

**caic integration:**
- Add a "Review" button to the task detail view.
- Could auto-trigger on task completion before creating a PR.

**Cross-provider design:** Codex-specific. Could be abstracted if other
providers gain similar capabilities.

### 7. Model Switching at Turn Level

**Problem:** Users must create a new task to change models.

**Codex:** The `turn/start` request accepts an optional `model` field that
overrides the thread's model for that turn only. The `thread/start` request
also accepts `model`.

**caic integration:**
- Add a model selector to the prompt input area.
- Pass the selected model in `turn/start` params.
- Requires extending `turnStartParams` (already has `ThreadID` and `Input`).

**Cross-provider design:**
```go
type ModelSwitcher interface {
    WriteSetModel(w io.Writer, model string, logW io.Writer) error
}
```
- Codex: passes `model` in `turn/start` params.
- Claude Code: sends `ControlSetModel` control request.

### 8. Token Usage Display

**Problem:** Users don't know how full the context window is.

**Codex:** Already emits `thread/tokenUsage/updated` with cumulative and
per-turn breakdowns plus `modelContextWindow`. caic already intercepts this
in `wireFormat.ParseMessage` and emits `UsageMessage`.

**caic integration:**
- Already partially implemented — `UsageMessage` is emitted with context
  window size from `modelContextWindow`.
- Display a context usage indicator (progress bar) in the task detail view.

### 9. Thread Forking

**Problem:** Users cannot branch a conversation to explore alternatives
without losing the original thread.

**Codex:** Send a `thread/fork` JSON-RPC request to create a new thread
forked from an existing one.

**caic integration:**
- Add a "Fork" button that creates a new task with the forked thread.
- Requires creating a new task linked to the forked thread ID.

### 10. Image Generation

**Problem:** Image generation items are new and not yet surfaced in the UI.

**Codex:** Emits `imageGeneration` items with `status`, `revisedPrompt`,
`result` (image data), and optional `savedPath`.

**caic integration:**
- Already added parsing support (item/started → ToolUseMessage,
  item/completed → ToolResultMessage).
- The frontend/Android need to render image results inline.

## Cross-Provider Architecture

The key design principle: **capabilities should be interfaces, not
assumptions**. Each enhancement above is gated behind an optional interface
that backends can implement.

### Extending WireFormat

The current `WireFormat` interface has two methods: `WritePrompt` and
`ParseMessage`. Rather than adding methods to `WireFormat` (which would
break all backends), use optional interfaces:

```go
// In agent/agent.go:

type CompactCommand interface {
    WriteCompact(w io.Writer, instructions string, logW io.Writer) error
}

type ModelSwitcher interface {
    WriteSetModel(w io.Writer, model string, logW io.Writer) error
}

type Interruptable interface {
    WriteInterrupt(w io.Writer, logW io.Writer) error
}

type Steerable interface {
    WriteSteer(w io.Writer, p Prompt, logW io.Writer) error
}

type ApprovalHandler interface {
    WriteApproval(w io.Writer, requestID string, approved bool, logW io.Writer) error
}
```

### Server-Side Capability Discovery

The server can check which capabilities a backend supports:

```go
func hasCapability[T any](wire WireFormat) bool {
    _, ok := wire.(T)
    return ok
}
```

The frontend queries available capabilities via the existing harness metadata
endpoint and conditionally renders UI controls.

### Provider Mapping

| Feature            | Claude Code              | Codex                       | Gemini | Kilo | OpenCode |
|--------------------|--------------------------|-----------------------------| -------|------|----------|
| Interrupt          | `ControlInterrupt`       | `turn/interrupt`            | N/A    | ?    | ?        |
| Steer              | N/A                      | `turn/steer`                | N/A    | ?    | ?        |
| Compact            | `/compact` msg           | `thread/compact/start`      | N/A    | ?    | ?        |
| Context usage      | `ControlGetContextUsage` | `tokenUsage/updated` notif  | N/A    | ?    | ?        |
| Model switch       | `ControlSetModel`        | `turn/start` model param    | N/A    | ?    | ?        |
| Approval flow      | `control_request`        | approval request notif      | N/A    | ?    | ?        |
| Code review        | N/A                      | `review/start`              | N/A    | N/A  | N/A      |
| Thread fork        | N/A                      | `thread/fork`               | N/A    | N/A  | N/A      |
| Rollback           | N/A                      | `thread/rollback`           | N/A    | N/A  | N/A      |
| Image generation   | N/A                      | `imageGeneration` item      | N/A    | N/A  | N/A      |
| Keep-alive         | `InputKeepAlive`         | N/A                         | N/A    | N/A  | N/A      |
| Env vars           | `InputUpdateEnvVars`     | N/A                         | N/A    | N/A  | N/A      |

`?` = needs investigation. `N/A` = not supported by provider.

## Implementation Priority

1. **Token usage display** — low effort, high value. Already parsed;
   just surface `UsageMessage.ContextWindow` more prominently in the UI.
2. **Turn interrupt** — medium effort, high value. Needs turn ID tracking +
   outbound JSON-RPC + UI button. Enables graceful mid-turn cancellation.
3. **Compact button** — low effort, sends `thread/compact/start` JSON-RPC.
   Parser already handles the `contextCompaction` item.
4. **Model switching** — low effort, add optional `model` to `turn/start`.
5. **Turn steering** — medium effort, novel UX concept. Very useful for
   long-running tasks.
6. **Approval flow** — high effort, high value. Requires bidirectional
   JSON-RPC response handling and interactive UI cards.
7. **Code review** — medium effort, Codex-specific but high value for
   PR workflows.
8. **Thread fork/rollback** — medium effort, niche use cases.
