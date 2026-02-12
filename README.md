# caic

Coding Agents in Containers. Manage multiple coding agents.

- Backend is in Go, frontend in SolidJS.
- Expects [md](https://github.com/maruel/md) to be in `$PATH`.
- Requires docker to be installed.

## Architecture

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

## Serving over Tailscale

Safely expose caic on your [Tailscale](https://tailscale.com/) network using `tailscale serve`. This provides
secure access from any device on your tailnet without opening ports or configuring firewalls.

```bash
# Expose caic on your tailnet at https://<hostname>.<tailnet>.ts.net
tailscale serve --bg 8080
```

**HTTPS**: Tailscale serve provides HTTPS automatically via Let's Encrypt TLS certificates.

Do not use tailscale funnel.
