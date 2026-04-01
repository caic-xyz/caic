# Future Enhancements for OpenCode Agent Communication

This document outlines how caic could leverage the OpenCode ACP protocol
to enhance user capabilities, and how to design these features to work across
all agent backends.

## Current State

caic currently sends only `session/prompt` messages after the initial handshake
(initialize → session/new). Model switching uses the unstable
`unstable_setSessionModel` method. Permission requests are passed through as
RawMessage — the OpenCode config is expected to pre-approve all permissions.

## Opportunities

### 1. Turn Cancel: session/cancel

**Problem:** Users can stop a task, but stopping kills the entire session.
There's no way to cancel a single turn and continue the conversation.

**OpenCode ACP:** Send a `session/cancel` notification with `sessionId`. The
agent aborts the current operation; the session remains open for follow-up
prompts.

**caic integration:**
- Add an "Interrupt" button distinct from "Stop". Interrupt cancels the
  current turn; Stop terminates the session.
- Already have the `MethodSessionCancel` constant in wire.go.

### 2. Session Fork: unstable_forkSession

**Problem:** Users cannot branch a conversation to explore alternatives
without losing the original thread.

**OpenCode ACP:** Send an `unstable_forkSession` request with `sessionId`.
Returns a new session with the forked conversation history. Declared in
`sessionCapabilities: { fork: {} }`.

**caic integration:**
- Add a "Fork" button that creates a new task with the forked session.
- Requires creating a new task linked to the forked session ID.
- The forked session replays history via `session/update` notifications.

### 3. Session Resume: unstable_resumeSession

**Problem:** Reconnecting to a session after server restart requires
replaying the full session via `session/load`.

**OpenCode ACP:** Send an `unstable_resumeSession` request with `sessionId`.
Resumes a paused or interrupted session without full replay. Declared in
`sessionCapabilities: { resume: {} }`.

**caic integration:**
- Use `unstable_resumeSession` instead of `session/load` for reconnection
  when the agent supports it.
- Faster reconnection, less bandwidth.

### 4. Session List: unstable_listSessions

**Problem:** Users have no way to browse previous sessions from the agent's
perspective.

**OpenCode ACP:** Send an `unstable_listSessions` request. Returns paginated
`SessionInfo[]` with `sessionId`, `cwd`, `title`, `updatedAt`. Declared in
`sessionCapabilities: { list: {} }`.

**caic integration:**
- Could power a "Resume previous session" picker in the UI.
- Supports cursor-based pagination.

### 5. Context Compaction: /compact command

**Problem:** Long sessions accumulate context until performance degrades.

**OpenCode ACP:** Send `/compact` as a prompt via `session/prompt`. OpenCode
recognizes it as a slash command and triggers context summarization. The
`available_commands_update` notification lists it as an available command.

**caic integration:**
- Add a "Compact" button to the task UI that sends `/compact` as a prompt.
- The `usage_update` notification will reflect the reduced context size.

### 6. Model Switching: session/set_model

**Problem:** Users must create a new task to change models mid-session.

**OpenCode ACP:** Send a `session/set_model` request or
`unstable_setSessionModel` (already implemented for handshake). Takes effect
on the next turn.

**caic integration:**
- Add a model selector dropdown to the task detail view.
- On change, send `session/set_model` via a new method on wireFormat.
- Update the task's reported model.

### 7. Mode Switching: session/set_mode

**Problem:** Users cannot switch between agent modes (e.g. code/ask) within
an active session.

**OpenCode ACP:** Send a `session/set_mode` request with `sessionId` and
`modeId`. The agent switches mode; a `current_mode_update` notification
confirms the change (already parsed as `SystemMessage`).

**caic integration:**
- Add a mode selector to the task UI.
- The parser already handles `current_mode_update` → `SystemMessage`.

### 8. Permission Approval Flow

**Problem:** caic pre-approves all permissions via OpenCode config. Users
have no control over dangerous operations.

**OpenCode ACP:** The agent sends `session/request_permission` with a
`ToolCall` and `PermissionOption[]` (allow_once, allow_always, reject_once).
Requires a JSON-RPC response with the selected option.

**caic integration:**
- Surface permission requests in the task UI as interactive cards.
- Send approval/denial responses via JSON-RPC.
- This would allow running without blanket auto-approval, giving users
  control over dangerous operations.

### 9. Context Usage Display

**Problem:** Users don't know how full the context window is until
performance degrades.

**OpenCode ACP:** Already emits `usage_update` with `used` (tokens used)
and `size` (context limit). caic already intercepts this and emits
`UsageMessage` with `ContextWindow`.

**caic integration:**
- Already partially implemented — `UsageMessage` is emitted.
- Display a context usage indicator (progress bar) in the task detail view
  using `used / size`.

### 10. Available Commands

**Problem:** Users don't know what slash commands the agent supports.

**OpenCode ACP:** Emits `available_commands_update` with a list of
`{name, description}` entries. Currently skipped in the parser.

**caic integration:**
- Parse `available_commands_update` into a new message type.
- Display available commands in the prompt input area (autocomplete/hints).
- Could enable sending commands like `/compact` via a UI menu.

## Cross-Provider Architecture

See [`agent/docs/MORE.md`](../../docs/MORE.md) for the shared cross-provider
architecture: optional capability interfaces, server-side discovery, provider
mapping table, and implementation priority.
