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

### 4. Thread Rollback: thread/rollback

**Problem:** Users cannot undo agent actions without stopping the session.

**Codex:** Send a `thread/rollback` JSON-RPC request with `threadId`. The
agent rolls back to a previous state.

**caic integration:**
- Add a "Rollback" button or menu option in the task detail view.
- Requires understanding what state is being rolled back to.

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

### 7. Model Switching at Turn Level

**Problem:** Users must create a new task to change models.

**Codex:** The `turn/start` request accepts an optional `model` field that
overrides the thread's model for that turn only. The `thread/start` request
also accepts `model`.

**caic integration:**
- Add a model selector to the prompt input area.
- Pass the selected model in `turn/start` params.
- Requires extending `turnStartParams` (already has `ThreadID` and `Input`).

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

See [`agent/docs/MORE.md`](../../docs/MORE.md) for the shared cross-provider
architecture: optional capability interfaces, server-side discovery, provider
mapping table, and implementation priority.
