# Codex Agent Communication

The Codex backend runs `codex app-server` through the in-container relay and
uses Codex's JSON-RPC 2.0 NDJSON protocol over stdin/stdout.

## Runtime

caic starts Codex with:

```text
codex app-server -c approval_policy="never" -c sandbox_mode="danger-full-access"
```

The md container is the security boundary. caic does not implement Codex
approval responses.

## Handshake

Startup performs this sequence:

1. `initialize`
2. `initialized`
3. `model/list`
4. `thread/start` or `thread/resume`

The live `model/list` response replaces the initial fallback model list.

## Requests

After handshake, caic sends:

- `turn/start` for prompts, including attached images as data URLs.
- `thread/compact/start` for context compaction.

## Notifications

The parser converts Codex app-server notifications into backend-neutral
`agent.Message` values. The handshake opts out of verbose notifications that
caic does not surface, including incremental file-change output, raw reasoning
tokens, incremental plan deltas, repeated diff snapshots, and thread name
updates.

`thread/tokenUsage/updated` is handled by `wireFormat.ParseMessage`: caic emits
incremental `UsageMessage` values and attaches accumulated per-turn usage to the
final `ResultMessage`.
