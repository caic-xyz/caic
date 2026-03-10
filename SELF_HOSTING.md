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
| `GITHUB_TOKEN` | — | No | — | GitHub PAT for PR creation and CI monitoring. Required for github.com repos when GitHub OAuth login is **not** configured. Mutually exclusive with `GITHUB_OAUTH_CLIENT_ID`. [Create a fine-grained token](https://github.com/settings/personal-access-tokens/new?name=caic&description=caic+PR+creation+and+CI+monitoring&pull_requests=write&checks=read&expires_in=365) with `pull_requests: write` and `checks: read`. |
| `GITLAB_TOKEN` | — | No | — | GitLab PAT for MR creation and CI monitoring. Required for gitlab.com repos when GitLab OAuth login is **not** configured. Mutually exclusive with `GITLAB_OAUTH_CLIENT_ID`. [Create a token](https://gitlab.com/-/user_settings/personal_access_tokens?name=caic&scopes=api) with `api` scope. |
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

## Forge integration: PAT vs OAuth

caic can create PRs/MRs and monitor CI on github.com and gitlab.com. There are
two ways to provide credentials — **PAT mode** and **OAuth mode**. They are
mutually exclusive per provider. The server refuses to start if both are set
for the same provider.

### PAT mode (headless / single-user)

Set a personal access token. No external URL or HTTPS is required.

- **GitHub**: set `GITHUB_TOKEN`. [Create a fine-grained token](https://github.com/settings/personal-access-tokens/new?name=caic&description=caic+PR+creation+and+CI+monitoring&pull_requests=write&checks=read&expires_in=365) with `pull_requests: write` and `checks: read`.
- **GitLab**: set `GITLAB_TOKEN`. [Create a token](https://gitlab.com/-/user_settings/personal_access_tokens?name=caic&scopes=api) with `api` scope.

### OAuth mode (multi-user, requires external HTTPS URL)

Configure an OAuth app for the provider. Users log in via the browser and their
own token is used for all forge operations — no PAT is needed or allowed.

**Requires**: `CAIC_EXTERNAL_URL` set to a stable HTTPS URL that the OAuth
provider can redirect back to. See [Serving over Tailscale](#serving-over-tailscale)
for an easy way to get one without opening public ports.

Do **not** set `GITHUB_TOKEN` when GitHub OAuth is configured, and do **not**
set `GITLAB_TOKEN` when GitLab OAuth is configured.

## OAuth Login

caic supports optional OAuth 2.0 login via GitHub and/or GitLab. When OAuth is
enabled, users must sign in before accessing the UI. Auth is disabled by
default.

A session secret is generated automatically on first startup and stored in
`~/.config/caic/settings.json`. No manual key management is required.

### GitHub OAuth app

1. Go to **Settings → Developer settings → OAuth Apps → New OAuth App**
   (or [click here](https://github.com/settings/applications/new)).
2. Fill in:
   - **Application name**: `caic`
   - **Homepage URL**: `https://<your-domain>`
   - **Authorization callback URL**: `https://<your-domain>/api/v1/auth/github/callback`
3. Click **Register application**, then **Generate a new client secret**.
4. Set environment variables:
   ```
   CAIC_EXTERNAL_URL=https://<your-domain>
   GITHUB_OAUTH_CLIENT_ID=<client-id>
   GITHUB_OAUTH_CLIENT_SECRET=<client-secret>
   ```

Do **not** set `GITHUB_TOKEN` alongside these — the server will refuse to start.

### GitLab OAuth app

**gitlab.com:**

1. Go to **User Settings → Applications** (or [click here](https://gitlab.com/-/user_settings/applications)).
2. Fill in:
   - **Name**: `caic`
   - **Redirect URI**: `https://<your-domain>/api/v1/auth/gitlab/callback`
   - **Scopes**: `api`, `read_user`
3. Click **Save application** and copy the Application ID and Secret.
4. Set environment variables:
   ```
   CAIC_EXTERNAL_URL=https://<your-domain>
   GITLAB_OAUTH_CLIENT_ID=<application-id>
   GITLAB_OAUTH_CLIENT_SECRET=<secret>
   ```

Do **not** set `GITLAB_TOKEN` alongside these — the server will refuse to start.

**Self-hosted GitLab instance:**

Follow the same steps on your instance, then also set:
```
GITLAB_URL=https://<your-gitlab-instance>
```

### Using both providers

Set variables for both GitHub and GitLab — caic will show a login button for
each configured provider.

### Environment variable reference

| Variable | Description |
|---|---|
| `CAIC_EXTERNAL_URL` | Public HTTPS base URL (e.g. `https://caic.example.com`). Required to enable OAuth. |
| `GITHUB_OAUTH_CLIENT_ID` | GitHub OAuth app client ID. Mutually exclusive with `GITHUB_TOKEN`. |
| `GITHUB_OAUTH_CLIENT_SECRET` | GitHub OAuth app client secret. |
| `GITLAB_OAUTH_CLIENT_ID` | GitLab OAuth app client ID. Mutually exclusive with `GITLAB_TOKEN`. |
| `GITLAB_OAUTH_CLIENT_SECRET` | GitLab OAuth app client secret. |
| `GITLAB_URL` | GitLab instance base URL. Default: `https://gitlab.com`. |

## Serving over Tailscale

Safely expose caic on your [Tailscale](https://tailscale.com/) network using
`tailscale serve` when using single user mode, not exposed on the internet. This provides HTTPS (via Let's
Encrypt), with the server exclusively accessible over your tailnet.

```bash
tailscale serve --bg 8080
```

caic is then reachable at `https://<hostname>.<tailnet>.ts.net` from any device
on your tailnet.
