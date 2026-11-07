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

## Verification environment

Run `make fix`, `make verify`, and `make test` from the repository root.
The first run downloads the pinned Go, Node, pnpm, and Python dependencies;
`make test-e2e` also installs Playwright's pinned Chromium. Caches must be
writable by the current user. In a sandbox with a read-only home cache, use
the tools' standard overrides before running the same Make targets:

```bash
export XDG_CACHE_HOME="$(mktemp -d)"
export PNPM_CONFIG_CACHE_DIR="$XDG_CACHE_HOME/pnpm"
export PNPM_CONFIG_STORE_DIR="$XDG_CACHE_HOME/pnpm-store"
export UV_CACHE_DIR="$XDG_CACHE_HOME/uv"
export PLAYWRIGHT_BROWSERS_PATH="$XDG_CACHE_HOME/ms-playwright"
make verify
make test
make test-e2e
```

Keep the same environment for installation and execution so Playwright can
find its downloaded browser. The cache directory is disposable and must not
be committed. The checkout itself must also be writable for `node_modules`,
the Python virtual environment, generated assets, and test results.

The Python relay tests launch real subprocesses. On Linux, the sandbox must
allow those children to increase `/proc/self/oom_score_adj`; making `/proc`
read-only prevents the relay's agent command from starting. The test runner
checks this prerequisite before running the relay suite and fails explicitly
rather than skipping lifecycle coverage.

## Frontend design

[The frontend design direction](frontend/docs/DESIGN.md) defines caic's brand,
visual style, and interaction character. Apply it to new and touched components;
it does not call for a full UX redesign. See [frontend/AGENTS.md](frontend/AGENTS.md)
for implementation and validation rules.

## End-to-end failures

`make test-e2e` prints a summary of every failed Playwright trace before the
fake server logs. Read it in order: a document request marked `no response` is
still in flight, and Playwright holds every locator query until a pending
navigation commits, so such a request explains an assertion that stalls.
Summarize a downloaded artifact with the same tool:

```bash
uv run --locked python scripts/summarize_playwright_trace.py test-results
uv run --locked python scripts/summarize_playwright_trace.py trace.zip --logs 0
```

Repeat one spec when the failure looks timing-dependent. Every round starts its
own fake backend, so in-memory fake state such as device-code limits and
fixtures resets; `--repeat-each` keeps one server and trips those limits instead
of reproducing the failure:

```bash
make test-e2e-repeat SPEC=e2e/tests/device-login.spec.ts ROUNDS=5
```

## Profiling

Enable profiling with the `[debug]` settings documented in
[contrib/config.toml](contrib/config.toml).

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
