# Future Enhancements for Agent Communication

This document describes the cross-provider architecture for enhancing caic's
agent capabilities. Each harness has its own `docs/MORE.md` with
provider-specific opportunities:

- [`claude/docs/MORE.md`](../claude/docs/MORE.md) — Claude Code enhancements
- [`codex/docs/MORE.md`](../codex/docs/MORE.md) — Codex CLI enhancements
- [`opencode/docs/MORE.md`](../opencode/docs/MORE.md) — OpenCode ACP enhancements

## Cross-Provider Architecture

The key design principle: **capabilities should be interfaces, not
assumptions**. Each enhancement is gated behind an optional interface
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

The server checks which capabilities a backend supports:

```go
func hasCapability[T any](wire WireFormat) bool {
    _, ok := wire.(T)
    return ok
}
```

The frontend queries available capabilities via the existing harness metadata
endpoint and conditionally renders UI controls.

### Provider Mapping

| Feature            | Claude Code              | Codex                       | OpenCode                     | Gemini | Kilo |
|--------------------|--------------------------|-----------------------------|-----------------------------|--------|------|
| Interrupt          | `ControlInterrupt`       | `turn/interrupt`            | `session/cancel`             | N/A    | ?    |
| Steer              | N/A                      | `turn/steer`                | N/A                          | N/A    | ?    |
| Compact            | `/compact` msg           | `thread/compact/start`      | `/compact` prompt            | N/A    | ?    |
| Context usage      | `ControlGetContextUsage` | `tokenUsage/updated` notif  | `usage_update` notif         | N/A    | ?    |
| Model switch       | `ControlSetModel`        | `turn/start` model param    | `session/set_model`          | N/A    | ?    |
| Mode switch        | N/A                      | N/A                         | `session/set_mode`           | N/A    | ?    |
| Approval flow      | `control_request`        | approval request notif      | `session/request_permission` | N/A    | ?    |
| Session fork       | N/A                      | `thread/fork`               | `unstable_forkSession`       | N/A    | N/A  |
| Session resume     | N/A                      | N/A                         | `unstable_resumeSession`     | N/A    | N/A  |
| Session list       | N/A                      | N/A                         | `unstable_listSessions`      | N/A    | N/A  |
| Available commands | N/A                      | N/A                         | `available_commands_update`  | N/A    | N/A  |
| Code review        | N/A                      | `review/start`              | N/A                          | N/A    | N/A  |
| Rollback           | N/A                      | `thread/rollback`           | N/A                          | N/A    | N/A  |
| Image generation   | N/A                      | `imageGeneration` item      | N/A                          | N/A    | N/A  |
| Cost               | `/cost` msg              | N/A                         | N/A                          | N/A    | N/A  |
| Keep-alive         | `InputKeepAlive`         | N/A                         | N/A                          | N/A    | N/A  |
| Env vars           | `InputUpdateEnvVars`     | N/A                         | N/A                          | N/A    | N/A  |

`?` = needs investigation. `N/A` = not supported by provider.

### Implementation Priority

Ordered by effort-to-value ratio across all providers:

1. **Context usage display** — low effort, high value. Most providers
   already emit usage data; just surface it more prominently in the UI.
2. **Interrupt/cancel** — medium effort, high value. Each provider has a
   mechanism; needs `WriteInterrupt` + UI button.
3. **Compact** — low effort. All three providers support it via different
   mechanisms.
4. **Model switching** — low-medium effort. All three providers support it.
5. **Approval flow** — high effort, high value. Requires bidirectional
   response handling and interactive UI cards. All three providers support it.
6. **Turn steering** — medium effort, Codex-specific but novel UX.
7. **Session fork** — medium effort. Codex and OpenCode support it.
8. **Mode switching** — low effort, OpenCode-specific.
