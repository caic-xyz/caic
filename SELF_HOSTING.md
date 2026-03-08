# Self-Hosting caic

## Prerequisites

- **Docker** — required to run agent containers via [md](https://github.com/caic-xyz/md).
- **Go** — to build from source, or use the prebuilt binary via `go install`.
- **At least one agent** — Claude Code, Codex CLI, or Kilo Code authenticated and working in your terminal.
  You can do it via a [md container](https://github.com/caic-xyz/md).

## Install

```bash
go install github.com/caic-xyz/caic/backend/cmd/caic@latest
```

## Configuration

All configuration is via environment variables. Flags take precedence when set.

### Required

| Variable | Flag | Description |
|---|---|---|
| `CAIC_HTTP` | `-http` | HTTP listen address (e.g. `:8080`). Port-only addresses listen on localhost. Use `0.0.0.0:8080` to listen on all interfaces. |
| `CAIC_ROOT` | `-root` | Parent directory containing your git repositories. Each subdirectory is a repo caic can manage. |

### Optional

| Variable | Flag | Default | Description |
|---|---|---|---|
| `CAIC_MAX_TURNS` | `-max-turns` | `0` (unlimited) | Maximum agentic turns per task before the agent stops. |
| `CAIC_LOG_LEVEL` | `-log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |

### Agent Backends

caic supports multiple agent backends. Install and authenticate at least one:

| Backend | CLI tool | Notes |
|---|---|---|
| **Claude Code** | `claude` | Authenticate via `claude login`. No extra env var needed. |
| **Codex CLI** | `codex` | Authenticate via `codex login` (browser OAuth) or pipe an API key with `codex login --with-api-key`. |
| **Kilo Code** | `kilo` | Authenticate via `kilo login` or set the relevant API key. |

| Variable | Description |
|---|---|
| `GEMINI_API_KEY` | Gemini API key for the Gemini agent backend. Get one at [aistudio.google.com](https://aistudio.google.com/app/apikey). |

### LLM Features (title generation, commit descriptions)

These power caic's built-in LLM features (task title generation, commit message descriptions). They are independent of the agent backend.

| Variable | Description |
|---|---|
| `CAIC_LLM_PROVIDER` | AI provider name (e.g. `anthropic`, `gemini`, `openaichat`). See [genai providers](https://pkg.go.dev/github.com/maruel/genai/providers). Set the corresponding `FOO_API_KEY` for the chosen provider. |
| `CAIC_LLM_MODEL` | Model name (e.g. `claude-haiku-4-5-20251001`). |

### Integrations

| Variable | Description |
|---|---|
| `TAILSCALE_API_KEY` | Tailscale API key for Tailscale integration. Get one at [login.tailscale.com/admin/settings/keys](https://login.tailscale.com/admin/settings/keys). |
| `GITHUB_TOKEN` | GitHub personal access token for automatic PR creation and CI check-run monitoring. [Create a fine-grained token](https://github.com/settings/personal-access-tokens/new?name=caic&description=caic+PR+creation+and+CI+monitoring&pull_requests=write&checks=read&expires_in=365) with `pull_requests: write` and `checks: read` permissions. |

## Running

```bash
CAIC_HTTP=:8080 CAIC_ROOT=~/src caic
```

Or with flags:

```bash
caic -http :8080 -root ~/src
```

## systemd User Service

Install the included unit file and env template, then enable:

```bash
mkdir -p ~/.config/systemd/user ~/.config/caic
cp contrib/caic.service ~/.config/systemd/user/
cp contrib/caic.env ~/.config/caic/caic.env
# Edit ~/.config/caic/caic.env to set CAIC_HTTP, CAIC_ROOT, and any API keys.
systemctl --user daemon-reload
systemctl --user enable --now caic
```

View logs:

```bash
journalctl --user -u caic -f
```

When caic is reinstalled (binary replaced), the service detects the change and
restarts automatically.

## Serving over Tailscale

Safely expose caic on your [Tailscale](https://tailscale.com/) network using
`tailscale serve`. This provides HTTPS (via Let's Encrypt) with no open ports
and no firewall configuration.

```bash
tailscale serve --bg 8080
```

caic is then reachable at `https://<hostname>.<tailnet>.ts.net` from any device
on your tailnet. Do **not** use `tailscale funnel` (public internet exposure).
