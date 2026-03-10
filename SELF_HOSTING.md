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
| `CAIC_HTTP` | `-http` | Yes | — | HTTP listen address (e.g. `:8080`). Port-only addresses listen on localhost. Use `0.0.0.0:8080` to listen on all interfaces. (required) |
| `CAIC_ROOT` | `-root` | Yes | — | Parent directory containing your git repositories. Each subdirectory is a repo caic can manage. (required) |
| `CAIC_LOG_LEVEL` | `-log-level` | No | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. |
| `CAIC_LLM_PROVIDER` | — | No | — | AI provider for LLM features (title generation, commit descriptions). E.g. `anthropic`, `gemini`, `openaichat`. See [genai providers](https://pkg.go.dev/github.com/maruel/genai/providers). |
| `CAIC_LLM_MODEL` | — | No | — | Model name for LLM features (e.g. `claude-haiku-4-5-20251001`). |
| `GEMINI_API_KEY` | — | No | — | Gemini API key for the Gemini Live voice agent. |
| `TAILSCALE_API_KEY` | — | No | — | Tailscale API key for Tailscale ephemeral node. Get one at [login.tailscale.com/admin/settings/keys](https://login.tailscale.com/admin/settings/keys). |

## Running

```bash
# Via flags:
caic -http :8080 -root ~/src

# Via environment variables:
CAIC_HTTP=:8080 CAIC_ROOT=~/src caic

# Get help:
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

## GitHub integration modes

Without any GitHub configuration, caic works for local repos but PR creation, CI monitoring, and webhook features are unavailable.

caic supports three GitHub integration modes. They cover different use cases
and are not mutually exclusive — PAT or OAuth handles forge operations (PR
creation, CI monitoring), while GitHub App handles webhook delivery and
automatic task creation independently.

| | **GitHub PAT** | **GitHub OAuth** | **GitHub App** |
|---|---|---|---|
| **Use case** | Single-user | Multi-user with login | Org-wide automation |
| **Setup complexity** | Minimal — one token | Medium — OAuth app + HTTPS URL | Medium — app registration + HTTPS URL |
| **User authentication** | ❌ — anyone with access to caic can use it | ✅ Users log in via GitHub; access controlled by allowlist | ❌ — app acts on behalf of itself |
| **Identity used for actions** | ✅ PAT for all PR/CI operations | ✅ Each user's own token for PR/CI | ✅ Installation token for PR/CI |
| **Webhook** | ❌ | ❌ | ✅ Receives events from GitHub automatically for fast response |
| **Polling** | ✅ — slower reaction | ✅ — slower reaction | ❌ |
| **Automatic task creation** | ❌ | ❌ | ✅ Issues, PRs, comments trigger tasks |
| **Post-completion comments** | ✅ Via `GITHUB_TOKEN` (same PAT) | ❌ | ✅ Via installation token |
| **Token scope** | Server-wide single token | Per-user, per-session | Per-installation |
| **Env vars** | `GITHUB_TOKEN` | `CAIC_EXTERNAL_URL` + `GITHUB_OAUTH_CLIENT_ID` + `GITHUB_OAUTH_CLIENT_SECRET` + `GITHUB_OAUTH_ALLOWED_USERS` | `CAIC_EXTERNAL_URL` + `GITHUB_APP_ID` + `GITHUB_APP_PRIVATE_KEY_PEM` + `GITHUB_WEBHOOK_SECRET` |
| **Mutually exclusive with** | GitHub OAuth | GitHub PAT | Neither — combines with PAT or OAuth |
| **Token creation** | [Fine-grained token](https://github.com/settings/personal-access-tokens/new?name=my+caic+instance&description=caic+PR+creation+and+CI+monitoring&pull_requests=write&checks=read&expires_in=365) (`pull_requests: write`, `checks: read`) | [GitHub OAuth app](https://github.com/settings/applications/new) | [GitHub App](https://github.com/settings/apps/new?name=my+caic+instance&webhook_active=true&issues=write&pull_requests=write&checks=read&events[]=issues&events[]=pull_request&events[]=issue_comment) (generate private key from app settings) |

PAT and OAuth are mutually exclusive per provider. GitHub App is independent
and can be layered on top of either. The server refuses to start if both PAT
and OAuth are set for the same provider.

### OAuth setup

When OAuth is enabled, users must sign in before accessing the UI. A session
secret is generated automatically on first startup and stored in
`~/.config/caic/settings.json`. No manual key management is required.

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
   GITHUB_OAUTH_ALLOWED_USERS=alice,bob
   ```

### GitHub App setup

A GitHub App receives webhooks from all repositories in an org in real time,
enabling automatic task creation the moment an issue is opened, a PR is
created, or a comment mentions `@caic`. It uses short-lived installation
tokens instead of a PAT. GitHub must be able to reach caic to deliver
webhooks — see [Exposure options](#exposure-options).

Once configured, caic creates tasks automatically for:

| Event | Condition | Prompt sent to agent |
|---|---|---|
| Issue opened | Issue has the `caic` label | Fix the linked issue |
| PR opened or reopened | Any PR targeting the default branch | Review/fix the PR |
| Comment created | Comment body contains `@caic` | Act on the comment |

When the task completes, caic posts a comment on the originating issue or PR.

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App**
   (or your org's equivalent).
2. Fill in:
   - **GitHub App name**: `caic`
   - **Homepage URL**: your caic URL
   - **Webhook URL**: `https://<your-domain>/api/v1/github/webhook`
   - **Webhook secret**: a random string (same value as `GITHUB_WEBHOOK_SECRET`)
   - **Permissions**: Issues (read/write), Pull requests (read/write), Checks (read)
   - **Subscribe to events**: Issues, Pull requests, Issue comments
3. Click **Create GitHub App**, then **Generate a private key** (downloads a `.pem` file).
4. Note the **App ID** shown on the app's settings page.
5. Install the app on your org or specific repositories.
6. Set environment variables:
   ```
   GITHUB_WEBHOOK_SECRET=<webhook-secret>
   GITHUB_APP_ID=<app-id>
   GITHUB_APP_PRIVATE_KEY_PEM=/path/to/private-key.pem
   ```
   Or set `GITHUB_APP_PRIVATE_KEY_PEM` to the PEM content directly.

## GitLab integration modes

caic supports two GitLab integration modes. GitLab does not have an equivalent
to GitHub App for webhook delivery.

| | **GitLab PAT** | **GitLab OAuth** |
|---|---|---|
| **Use case** | Single-user | Multi-user with login |
| **Setup complexity** | Minimal — one token | Medium — OAuth app + HTTPS URL |
| **User authentication** | ❌ — anyone with access to caic can use it | ✅ — Users log in via GitLab; access controlled by allowlist |
| **Identity used for actions** | ✅ PAT for all MR/CI operations | ✅ Each user's own token for MR/CI |
| **Polling** | ✅ | ✅ |
| **Token scope** | Server-wide single token | Per-user, per-session |
| **Env vars** | `GITLAB_TOKEN` | `CAIC_EXTERNAL_URL` + `GITLAB_OAUTH_CLIENT_ID` + `GITLAB_OAUTH_CLIENT_SECRET` + `GITLAB_OAUTH_ALLOWED_USERS` |
| **Mutually exclusive with** | GitLab OAuth | GitLab PAT |
| **Token creation** | [Personal access token](https://gitlab.com/-/user_settings/personal_access_tokens?name=my+caic+instance&scopes=api) (`api` scope) | [GitLab OAuth app](https://gitlab.com/-/user_settings/applications) |

PAT and OAuth are mutually exclusive. The server refuses to start if both are set.

### OAuth setup

When OAuth is enabled, users must sign in before accessing the UI. A session
secret is generated automatically on first startup and stored in
`~/.config/caic/settings.json`. No manual key management is required.

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
   GITLAB_OAUTH_ALLOWED_USERS=alice,bob
   ```

**Self-hosted GitLab instance:**

Follow the same steps on your instance, then also set:
```
GITLAB_URL=https://<your-gitlab-instance>
```

## Exposure options

OAuth login and webhooks require `CAIC_EXTERNAL_URL` to be set. Webhooks
additionally require GitHub to reach caic from the internet. OAuth login only
requires that users' browsers can reach caic, so a tailnet URL is sufficient.

**Public HTTPS endpoint** — fully internet-accessible. Required for webhooks
if not using Tailscale:

```
CAIC_EXTERNAL_URL=https://<your-domain>
```

**Tailscale Funnel** — exposes caic to the internet via Tailscale without
opening any ports. Suitable for webhooks:

```bash
tailscale funnel 8080
CAIC_EXTERNAL_URL=https://<hostname>.<tailnet>.ts.net  # confirm with: tailscale funnel status
```

**Tailscale serve** — private access over your tailnet only, not reachable
from the internet. Sufficient for OAuth login (users on the tailnet can
complete the OAuth flow), but not for webhooks:

```bash
tailscale serve --bg 8080
CAIC_EXTERNAL_URL=https://<hostname>.<tailnet>.ts.net
```

