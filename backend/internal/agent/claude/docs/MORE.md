# Future Enhancements for Agent Communication

This document outlines how caic could leverage the Claude Code wire protocol
(documented in `wire.go`) to enhance user capabilities, and how to design
these features to work across all agent backends.

## Current State

caic currently sends only `inputUser` messages via `WritePrompt`. All control
requests, slash commands, keep-alive, and environment variable updates are
unused. The `--dangerously-skip-permissions` flag means we never receive
`control_request` messages for tool approvals either.

## Opportunities

### 1. Context Management: /compact

**Problem:** Long sessions accumulate context until auto-compact fires. Users
have no visibility into or control over context usage.

**Claude Code:** Send `/compact` as user message content, or `/compact <instructions>`
to guide the summary. Available in `-p` mode.

**caic integration:**
- Add a "Compact" button to the task UI that sends `/compact` as a user message.
- Optionally expose `/compact <instructions>` to let users guide summarization.
- Surface context usage by sending `ControlGetContextUsage` control requests
  and displaying the breakdown in the UI.

**Cross-provider design:** Define an `agent.CompactCommand` interface:
```go
type CompactCommand interface {
    WriteCompact(w io.Writer, instructions string, logW io.Writer) error
}
```
- Claude Code: sends `/compact <instructions>` as user message content.
- Other providers: implement provider-specific compaction or no-op.
- The server checks if the backend implements `CompactCommand` before showing
  the button in the UI.

### 2. Context Usage Display: /context

**Problem:** Users don't know how full the context window is until it auto-compacts
or performance degrades.

**Claude Code:** Send `ControlGetContextUsage` control request. Response includes
token counts by category, percentage used, and auto-compact threshold.

**caic integration:**
- Add a context usage indicator to the task detail view (e.g. progress bar
  showing percentage, tooltip with breakdown).
- Poll periodically or after each assistant turn.

**Cross-provider design:** Define an `agent.ContextUsage` message type in
`agent/types.go` with common fields (total tokens, max tokens, percentage).
Each backend emits this after parsing provider-specific data. The server
forwards it to the frontend via SSE.

### 3. Model Switching: ControlSetModel

**Problem:** Users must create a new task to change models mid-session.

**Claude Code:** Send `ControlSetModel` control request. Takes effect on the
next turn.

**caic integration:**
- Add a model selector dropdown to the task detail view.
- On change, send a `ControlSetModel` control request via a new
  `WriteControlRequest` method on the session.
- Update the task's `reportedModel` field when the agent confirms.

**Cross-provider design:** This requires extending `WireFormat`:
```go
type ModelSwitcher interface {
    WriteSetModel(w io.Writer, model string, logW io.Writer) error
}
```
Not all providers support mid-session model switching. The UI should only
show the selector when the backend implements `ModelSwitcher`.

### 4. Interrupt/Cancel: ControlInterrupt

**Problem:** Users can stop a task, but stopping kills the entire session.
There's no way to interrupt a single turn and continue the conversation.

**Claude Code:** Send `ControlInterrupt` control request. Interrupts the
current turn; the session remains open for follow-up messages.

**caic integration:**
- Add an "Interrupt" button distinct from "Stop". Interrupt cancels the
  current turn; Stop terminates the session.
- Requires extending the session to support writing control requests
  alongside user messages.

**Cross-provider design:**
```go
type Interruptable interface {
    WriteInterrupt(w io.Writer, logW io.Writer) error
}
```

### 5. Session Cost: /cost

**Problem:** Users have no visibility into per-session API cost.

**Claude Code:** Send `/cost` as user message. Returns cost breakdown for
subscription users. For API key users, `outputResult.TotalCostUSD` already
provides this.

**caic integration:**
- Already partially implemented: `outputResult.TotalCostUSD` and
  `outputResult.Usage` are parsed and displayed.
- For subscription users, could periodically send `/cost` and parse the
  `SystemLocalCommandOutput` response.

### 6. Environment Variables: InputUpdateEnvVars

**Problem:** Some agent behaviors depend on environment variables that are
fixed at container creation time.

**Claude Code:** Send `InputUpdateEnvVars` to push env vars at runtime.

**caic integration:**
- Could be used to toggle features mid-session (e.g. `DISABLE_COMPACT=1`).
- Could propagate user preferences or API keys without restarting.

### 7. Keep-Alive: InputKeepAlive

**Problem:** Long SSH connections may be dropped by intermediate proxies.

**Claude Code:** Send `InputKeepAlive` periodically.

**caic integration:**
- The relay already handles reconnection, but keep-alive could prevent
  unnecessary disconnects in the first place.
- Send every 30-60 seconds when the session is idle.

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

type ContextQuerier interface {
    WriteGetContextUsage(w io.Writer, logW io.Writer) error
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

| Feature | Claude Code | Codex | Gemini | Kilo | OpenCode |
|---------|------------|-------|--------|------|----------|
| Compact | `/compact` msg | ? | N/A | ? | ? |
| Context usage | `ControlGetContextUsage` | N/A | N/A | ? | ? |
| Model switch | `ControlSetModel` | N/A | N/A | ? | ? |
| Interrupt | `ControlInterrupt` | ? | N/A | ? | ? |
| Cost | `/cost` msg | N/A | N/A | N/A | N/A |
| Env vars | `InputUpdateEnvVars` | N/A | N/A | N/A | N/A |
| Keep-alive | `InputKeepAlive` | N/A | N/A | N/A | N/A |

`?` = needs investigation. `N/A` = not supported by provider.

## Implementation Priority

1. **Context usage display** — low effort, high value. Just parse the
   existing `outputResult.Usage` more prominently.
2. **Compact button** — low effort, sends `/compact` as user message.
   No wire protocol changes needed.
3. **Interrupt** — medium effort, needs `WriteControlRequest` on Session.
   High value for long-running turns.
4. **Model switching** — medium effort, needs UI + control request plumbing.
5. **Keep-alive** — low effort, prevents spurious disconnects.
6. **Environment variables** — low priority, niche use cases.
