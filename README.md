# caic

Coding Agents in Containers. Manage multiple coding agents.

Some people use IDEs. Some people use git-worktrees. Some people use Claude Code Web or Jules, or Cursor
Cloud Agents. What if you want to develop safely in YOLO mode but you need a local high performance machine to
run the tests?

Enters **ciac**: manages local docker containers to run your agents locally. Access them from your
phone with Tailscale. All private.

## Installation

```bash
go install github.com/caic-xyz/caic/backend/cmd/caic@latest
```

See [SELF_HOSTING.md](SELF_HOSTING.md) for configuration, environment variables, systemd setup, and Tailscale.

## Architecture

- Backend is in Go, frontend in SolidJS.
- Requires docker to be installed.

Each task runs Claude Code inside an isolated
[md](https://github.com/caic-xyz/md) container. A Python relay process inside
the container owns Claude's stdin/stdout and persists across SSH disconnects,
so the server can restart without killing the agent or losing messages.

```
HOST (caic server)              CONTAINER (md)
──────────────────              ───────────────────────────────
                                relay.py (setsid, survives SSH)
┌─────────┐   SSH stdin/stdout  ┌────────┐     ┌──────────────┐
│ Session │◄═══════════════════►│ attach │◄═══►│ Unix socket  │
│ (Go)    │     NDJSON bidir    └────────┘     │              │
└─────────┘                                    │ relay server │
                                output.jsonl ◄─┤ ┌────────┐   │
                                (append-only)  │ │ claude │   │
                                               │ │ code   │   │
                                               │ └────────┘   │
                                               └──────────────┘
```

**Normal operation:** The server connects via SSH to the relay's `attach`
command. NDJSON messages flow bidirectionally through a Unix socket to the
Claude process. All output is appended to `output.jsonl` inside the container.

**Server restart:** The relay keeps Claude alive (it is `setsid`'d and
independent of the SSH session). On restart the server:

1. Discovers running containers via `md list`
2. Checks if the relay is still alive (Unix socket exists)
3. Reads `output.jsonl` from the container to restore full conversation history
4. Re-attaches to the relay from the last byte offset — no messages are lost

**Relay dead (Claude crashed):** Falls back to host-side JSONL logs and
`claude --resume` to start a new session continuing the conversation.

## GitHub Integration

### PR creation

After a successful sync to the task's branch, caic automatically opens a pull request against the base branch. The PR title comes from the task title (or initial prompt), and the body is the agent's result summary. Once created, the PR number appears as a link in the task detail header on both the web UI and Android app.

### CI monitoring

After the PR is created, caic polls GitHub check-runs every 30 seconds. While checks are running, the task header shows **CI: pending**. When all checks finish:

- **CI: passed** — all checks succeeded; the agent is notified with a summary.
- **CI: failed** — one or more checks failed; the agent is notified with the names and URLs of the failing checks so it can act on them.

The agent resumes automatically based on the CI outcome — no manual intervention required.

### Configuration

`GITHUB_TOKEN` is required. See [SELF_HOSTING.md](SELF_HOSTING.md) for full environment variable reference.

## Android App

Voice-first companion app to manage coding agents from your phone.

### Prerequisites

1. [Android SDK](https://developer.android.com/studio) with build-tools and platform for API 36.
2. USB debugging enabled on your phone: **Settings > Developer options > USB debugging**.
3. Phone connected via USB and authorized (`adb devices` shows your device).

### Build and deploy

```bash
make android-push
```

This builds the debug APK and installs it on the connected device.

To build without installing:

```bash
make android-build
```
