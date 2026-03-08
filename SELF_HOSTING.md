# Self-Hosting caic

## Prerequisites

- **Docker** or **Podman** — required to run agent containers via [md](https://github.com/caic-xyz/md).
- **Go** — to build from source.
- **At least one agent** — install and authenticate at least one of the following.

### Agents

| Backend | CLI tool | Setup |
|---|---|---|
| **Claude Code** | `claude` | Authenticate via `claude login`. |
| **Codex CLI** | `codex` | Authenticate via `codex login` (browser OAuth) or `codex login --with-api-key`. |
| **Kilo Code** | `kilo` | Authenticate via `kilo login` or set the relevant API key. |
| **Gemini** | `gemini` | Set `GEMINI_API_KEY`. Get one at [aistudio.google.com](https://aistudio.google.com/app/apikey). |

## Install

```bash
go install github.com/caic-xyz/caic/backend/cmd/caic@latest
```

## Configuration

All configuration is via environment variables. Flags take precedence when set. See [`contrib/caic.env`](contrib/caic.env) for a template with all variables.

| Variable | Flag | Required | Default | Description |
|---|---|---|---|---|
| `CAIC_HTTP` | `-http` | Yes | — | HTTP listen address (e.g. `:8080`). Port-only addresses listen on localhost. Use `0.0.0.0:8080` to listen on all interfaces. |
| `CAIC_ROOT` | `-root` | Yes | — | Parent directory containing your git repositories. Each subdirectory is a repo caic can manage. |
| `CAIC_MAX_TURNS` | `-max-turns` | No | `0` (unlimited) | Maximum agentic turns per task before the agent stops. |
| `CAIC_LOG_LEVEL` | `-log-level` | No | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |
| `CAIC_LLM_PROVIDER` | — | No | — | AI provider for LLM features (title generation, commit descriptions). E.g. `anthropic`, `gemini`, `openaichat`. See [genai providers](https://pkg.go.dev/github.com/maruel/genai/providers). |
| `CAIC_LLM_MODEL` | — | No | — | Model name for LLM features (e.g. `claude-haiku-4-5-20251001`). |
| `GEMINI_API_KEY` | — | No | — | Gemini API key for the Gemini agent backend. |
| `GITHUB_TOKEN` | — | No | — | GitHub token for automatic PR creation and CI monitoring. [Create a fine-grained token](https://github.com/settings/personal-access-tokens/new?name=caic&description=caic+PR+creation+and+CI+monitoring&pull_requests=write&checks=read&expires_in=365) with `pull_requests: write` and `checks: read`. |
| `TAILSCALE_API_KEY` | — | No | — | Tailscale API key for Tailscale integration. Get one at [login.tailscale.com/admin/settings/keys](https://login.tailscale.com/admin/settings/keys). |

## Running

```bash
caic -http :8080 -root ~/src

caic -help
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
