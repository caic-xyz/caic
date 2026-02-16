# caic

Coding Agents in Containers. Manage multiple coding agents.

Some people use IDEs. Some people use git-worktrees. Some people use Claude Code Web or Jules, or Cursor
Cloud Agents. What if you want to develop safely in YOLO mode but you need a local high performance machine to
run the tests? Enters ciac: manages local docker containers to run your agents locally. Access them from your
phone with Tailscale. All private.

## Architecture

- Backend is in Go, frontend in SolidJS.
- Requires docker to be installed.

Each task runs Claude Code inside an isolated
[md](https://github.com/maruel/md) container. A Python relay process inside
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

## Installation

```bash
go install github.com/maruel/caic/backend/cmd/caic@latest
```

### systemd user service

Install the unit file and enable it:

```bash
mkdir -p ~/.config/systemd/user
cp contrib/caic.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now caic
```

Edit `~/.config/systemd/user/caic.service` to adjust `-root` and `-http`

View logs:

```bash
journalctl --user -u caic -f
```

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

## Serving over Tailscale

Safely expose caic on your [Tailscale](https://tailscale.com/) network using `tailscale serve`. This provides
secure access from any device on your tailnet without opening ports or configuring firewalls.

```bash
# Expose caic on your tailnet at https://<hostname>.<tailnet>.ts.net
tailscale serve --bg 8080
```

**HTTPS**: Tailscale serve provides HTTPS automatically via Let's Encrypt TLS certificates.

Do not use tailscale funnel.
