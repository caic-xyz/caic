# Relay Package

Persistent process relay that keeps coding agents alive inside containers
across SSH disconnections and backend restarts. Python-based, embedded into
the Go binary.

## Architecture

- `embed.go` — Embeds `relay.py` as `Script []byte`
- `relay.py` — Daemon + client relay (~650 lines Python, stdlib only)
- `test_relay.py` — Tests for shutdown semantics and diff parsing

Deployment and management from Go is in the parent `agent.go`
(`DeployRelay`, `StartRelay`, `AttachRelaySession`, `ReadRelayOutput`, etc.).

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
5. **diff_watcher** — polls `git diff` on activity, emits `caic_diff_stat` events
   (throttled 10s, debounced 2s, uses temporary git index for untracked files)

## Container Layout

```
/tmp/caic-relay/
  relay.py          # Deployed script
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
