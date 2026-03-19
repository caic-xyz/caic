# ACP Wire Protocol

JSON-RPC 2.0 over stdin/stdout with nd-JSON framing.
Spec: https://agentclientprotocol.com

## Lifecycle

```
opencode acp                              # launch long-lived process
→ initialize        (protocolVersion:1, clientInfo)
← initialize result (agentCapabilities, agentInfo)
→ initialized       (notification, no response)
→ session/new       (cwd)
← session result    (sessionId, models, modes)
→ session/prompt    (sessionId, prompt content blocks)
← session/update    (streaming notifications)
← session/prompt result (stopReason, usage)
→ session/prompt    (follow-up prompt)
...
```

## Request Methods

| Method | Description |
|--------|-------------|
| `initialize` | Exchange capabilities (protocolVersion, clientInfo) |
| `initialized` | Notification: handshake complete |
| `session/new` | Create new session (cwd) |
| `session/load` | Resume existing session (sessionId, cwd) |
| `session/prompt` | Send user prompt (long-lived: streams session/update during turn) |
| `session/cancel` | Cancel current operation (notification) |
| `session/set_model` | Change model at runtime |
| `session/set_mode` | Switch agent mode (code/ask) |

## Session Update Notifications

All streaming events arrive as `session/update` notifications with a tagged
union `update` field discriminated by `sessionUpdate`.

### `agent_message_chunk` → TextDeltaMessage
```json
{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"..."}}
```

### `agent_thought_chunk` → ThinkingDeltaMessage
```json
{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"..."}}
```

### `tool_call` → ToolUseMessage
```json
{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"bash","kind":"execute","status":"pending","rawInput":{...}}
```

### `tool_call_update` → ToolResultMessage / ToolOutputDeltaMessage
```json
{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed|failed|in_progress","content":[...]}
```

### `plan` → TodoMessage
```json
{"sessionUpdate":"plan","entries":[{"status":"completed","content":"..."},{"status":"pending","content":"..."}]}
```

### `usage_update` → UsageMessage
```json
{"sessionUpdate":"usage_update","used":45000,"size":200000,"cost":{"amount":0.42,"currency":"USD"}}
```

## Prompt Content Blocks

The `session/prompt` params accept an array of content blocks:

```json
[
  {"type":"text","text":"Fix the bug"},
  {"type":"image","data":"<base64>","mimeType":"image/png"}
]
```

## Permission Handling

The agent may send `session/request_permission` requests during tool execution.
In caic, tool permissions should be pre-configured to `"always"` in
`opencode.json` to avoid blocking.

## Comparison with Codex JSON-RPC

| Aspect | Codex | OpenCode ACP |
|--------|-------|--------------|
| Handshake | `initialize` → `initialized` → `model/list` → `thread/start` | `initialize` → `initialized` → `session/new` |
| Send prompt | `turn/start` | `session/prompt` |
| Session concept | Thread ID | Session ID |
| Streaming events | Per-method notifications (`item/*`, `turn/*`) | Single `session/update` with tagged union |
| Turn completion | `turn/completed` notification | `session/prompt` response |
| Framing | nd-JSON | nd-JSON |
