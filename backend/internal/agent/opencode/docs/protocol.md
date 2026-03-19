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
{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"bash","kind":"execute","status":"pending","rawInput":{...},"locations":[{"path":"/repo/file.go","line":10}]}
```

### `tool_call_update` → ToolResultMessage / ToolOutputDeltaMessage
```json
{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed|failed|in_progress","content":[...],"rawOutput":{"output":"...","error":"...","metadata":{...}}}
```

Error extraction priority: `rawOutput.error` → `content[].content.text` → "tool call failed".
Output delta extraction: `rawOutput.output` → `content[].content.text`.

### `plan` → TodoMessage
```json
{"sessionUpdate":"plan","entries":[{"priority":"medium","status":"completed","content":"..."},{"status":"pending","content":"..."}]}
```

Plan entry statuses: `pending`, `in_progress`, `completed`, `cancelled`.

### `usage_update` → UsageMessage
```json
{"sessionUpdate":"usage_update","used":45000,"size":200000,"cost":{"amount":0.42,"currency":"USD"}}
```

### `current_mode_update` → SystemMessage
```json
{"sessionUpdate":"current_mode_update","modeId":"code","modeName":"Code Mode"}
```

### `available_commands_update` (skipped)
```json
{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"fix","description":"Fix issues"}]}
```

## Content Block Types

Content blocks appear in message chunks and tool call results. Flat union
discriminated by `type`:

| Type | Fields |
|------|--------|
| `text` | `text`, `annotations` (optional audience) |
| `image` | `data` (base64), `mimeType`, `uri` |
| `resource` | `resource` (embedded: `{uri, mimeType, text?, blob?}`) |
| `resource_link` | `uri`, `name`, `mimeType` |

## Tool Call Content Types

Content entries in `tool_call_update` results:

| Type | Fields |
|------|--------|
| `content` | `content` (nested ContentBlock) |
| `diff` | `path`, `oldText`, `newText` |
| `image` | `content` (image ContentBlock) |
| `resource` | `content` (resource ContentBlock) |
| `resource_link` | `content` (resource_link ContentBlock) |

## Prompt Content Blocks

The `session/prompt` params accept an array of content blocks:

```json
[
  {"type":"text","text":"Fix the bug"},
  {"type":"image","data":"<base64>","mimeType":"image/png"},
  {"type":"resource","resource":{"uri":"file:///repo/file.go","mimeType":"text/x-go","text":"..."}},
  {"type":"resource_link","uri":"file:///repo/file.go","name":"file.go","mimeType":"text/x-go"}
]
```

## Permission Handling

The agent may send `session/request_permission` requests during tool execution.
In caic, tool permissions should be pre-configured to `"always"` in
`opencode.json` to avoid blocking.

## Token Usage

The `session/prompt` response includes token usage:

```json
{"stopReason":"end_turn","usage":{"totalTokens":5000,"inputTokens":3000,"outputTokens":500,"thoughtTokens":200,"cachedReadTokens":100,"cachedWriteTokens":50}}
```

## Comparison with Codex JSON-RPC

| Aspect | Codex | OpenCode ACP |
|--------|-------|--------------|
| Handshake | `initialize` → `initialized` → `model/list` → `thread/start` | `initialize` → `initialized` → `session/new` |
| Send prompt | `turn/start` | `session/prompt` |
| Session concept | Thread ID | Session ID |
| Streaming events | Per-method notifications (`item/*`, `turn/*`) | Single `session/update` with tagged union |
| Turn completion | `turn/completed` notification | `session/prompt` response |
| Framing | nd-JSON | nd-JSON |
