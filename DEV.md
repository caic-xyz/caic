# Development

Dependencies to build locally from scratch including the frontend:

- Go
- brotli
- make
- Node.js 26
- pnpm 12.4.2

From the repository root:

```bash
make build
```

## Profiling

Enable profiling by adding a `[debug]` section to `~/.config/caic/config.toml`:

```toml
[debug]
# Expose /debug/pprof/* endpoints (CPU, heap, goroutines, trace).
pprof = true

# Write a CPU profile to file on shutdown.
cpuprofile = "~/.cache/caic/cpu.prof"

# Write an execution trace to file on shutdown.
trace = "~/.cache/caic/trace.out"

# Write a heap profile to file on shutdown.
memprofile = "~/.cache/caic/mem.prof"
```

Analyze the output:

```
go tool pprof -http=:7010 ~/.cache/caic/cpu.prof
go tool trace -http=:7010 ~/.cache/caic/trace.out
```

The execution trace captures named tasks and regions for each server startup
phase: repo discovery, log loading, container listing, runner initialization,
and per-container adoption (with sub-regions for relay status checks, relay
output reads, and message loading). Start the server, let it initialize, stop
it with Ctrl-C, then open the trace viewer to see where time is spent.

## Relay Failure and Recovery

The [relay](backend/internal/agent/relay/AGENTS.md) is a persistent Python daemon
(`relay.py`) running inside each container. It keeps the coding agent process
alive across SSH disconnections and backend restarts, logging all I/O to
`/tmp/caic-relay/output.jsonl`.

On server startup, caic automatically recovers retained containers whose relay
or runtime died, including OOM failures, when validated task history contains
accepted input and a saved conversation for Claude, Codex, OpenCode, or Pi. It
preserves archived task history and resumes the exact session idle, without
replaying the initial prompt. Pi uses `--session <id>` for this.

Stop and purge intent is persisted before shutdown, so deliberate stops remain
stopped even if the server restarts midway through the operation. Revival clears
that intent before restarting the runtime. Incomplete startup failures and
unresumable harnesses require manual recovery; automatic recovery runs once per
startup and leaves failures retryable.

### Diagnostics

The relay writes diagnostics to `/tmp/caic-relay/relay.log` inside the
container. SSH in to inspect:

```bash
ssh <container-name>
cat /tmp/caic-relay/relay.log
```

The backend also tests relay health via socket + PID liveness. When a relay is
dead during server startup, the full `relay.log` tail is logged at the `WARN`
level. Check the server logs for lines containing `"msg":"log from dead relay"`.

### Manual Restart (Development Only)

If the relay dies during development, you can restart it inside the container
without going through the full revive cycle:

```bash
ssh <container-name>

# Inspect what went wrong.
cat /tmp/caic-relay/relay.log

# Kill any stale agent process that may still be running.
pkill -f "<agent>" || true

# Clean stale socket so serve-attach can start.
rm -f /tmp/caic-relay/relay.sock

# Start a new relay with --resume to continue the session.
python3 /tmp/caic-relay/relay.py serve-attach \
  --dir /home/user/src/<repo> \
  -- <agent> --resume <session-id>
```

The agent restores context from its own saved session. The relay replays
`output.jsonl` to the backend when it attaches. Restart the server to let
container import reconnect to the live relay.

### Common Failure Modes

| Symptom                      | Likely Cause                                 | Recovery                                                                      |
| ---------------------------- | -------------------------------------------- | ----------------------------------------------------------------------------- |
| Socket exists but PID stale  | Agent subprocess crashed, daemon still alive | Kill stale pid, `rm -f /tmp/caic-relay/relay.sock`, restart relay             |
| No socket, container running | Relay daemon died (OOM, crash)               | Check `relay.log`; restart caic for automatic recovery, or revive manually    |
| Relay alive but attach fails | Race between check and attach                | Retry attachment; if the relay died, stop and revive the task                 |
| Graceful stop times out      | Agent subprocess ignores SIGINT/SIGTERM      | Relay escalates to SIGKILL; check `relay.log` for the shutdown watchdog trace |
