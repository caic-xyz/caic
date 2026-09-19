# Container Process Relay

Persistent process relay that keeps coding agents alive inside containers
across SSH disconnections and backend restarts. Python-based, embedded into
the Go binary.

## Relay versions

`relay_v2.py` is the maintained relay. It implements canonical v2 framing and is
deployed for every log version except v1. Fix relay behavior here and cover it in
`test_relay_v2.py`.

`relay.py` is a frozen historical artifact: the original v1 relay. It stays in
the tree only so v1 log versions remain deployable and replayable. Do not edit it
and do not port fixes into it. `test_relay.py` exists solely to keep the frozen
v1 relay working and must not be extended with new behavior.

## Deployment

Deployment and management from Go is in the parent `agent.go` (`RelayScript`,
`DeployRelay`, `StartRelay`, `AttachRelaySession`, `ReadRelayOutput`, etc.).
`RelayScript` selects `Script` for `LogVersionV1` and `ScriptV2` otherwise; both
are deployed to the same container path.

## Operational Modes

| Command | Purpose |
|---------|---------|
| `serve-attach --dir <path> -- <cmd>` | Start daemon + attach as first client |
| `attach [--offset N]` | Reconnect to running daemon |
| `read-plan [path]` | Read plan file from container |

## Shutdown Protocol

**Null-byte sentinel line** (`\x00\n`) distinguishes graceful shutdown from SSH drops:
- `\x00\n` on stdin → `_client_reader` sets `shutdown_event`
- `_shutdown_watchdog` thread waits on event, closes proc.stdin, sends SIGINT,
  escalates to SIGTERM/SIGKILL after `--shutdown-grace` seconds
- `reader_thread` is the authoritative "done" signal: blocks on stdout EOF,
  flushes output, closes client socket. `serve()` joins it then cleans up.
- Plain EOF (SSH drop) → daemon + agent keep running → backend reconnects later

## Daemon Threads

1. **reader_thread** (non-daemon) — subprocess stdout → `output.jsonl` + connected
   client; `serve()` joins this thread to determine daemon lifetime
2. **accept_thread** — Unix socket listener, replays from byte offset, one client at a time
3. **client_reader** — client stdin → subprocess stdin (line-buffered);
   detects `\x00\n` sentinel and sets `shutdown_event`
4. **_shutdown_watchdog** (non-daemon) — waits on `shutdown_event`, closes stdin,
   sends SIGINT, escalates to SIGTERM/SIGKILL
5. **diff_watcher** — polls `git diff --numstat --stat` on activity, emits
   `caic_diff_stat` events (throttled 10s, debounced 2s, uses temporary git index
   for untracked files). Only `relay_v2.py` reports binary pre-image and post-image
   sizes.

## Container Layout

```
/tmp/caic-relay/
  relay.py          # Deployed script (v1 or v2 selected by log version)
  relay.sock        # Unix socket
  output.jsonl      # Append-only conversation log (survives restarts)
  relay.log         # Daemon diagnostics
  pid               # PID file
  widget-plugin/    # MCP server + skills (deployed separately)
```

## Key Design Decisions

- **Append-only `output.jsonl`**: enables conversation recovery via byte-offset replay.
- **One client at a time**: simplifies state, prevents concurrent stdin corruption.
- **Temporary git index**: diff watcher avoids mutating the agent's working index.
- **`--no-log-stdin`**: for JSON-RPC protocols (Codex) where stdin contains handshake noise.
