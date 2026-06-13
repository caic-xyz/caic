# MCP Remote Authentication Plan

caic is the OAuth authorization server and MCP resource server for hosted and
local MCP clients. Do not delegate MCP authentication to OpenAI, Anthropic,
Google, GitHub, or GitLab tokens. Those providers may authenticate a caic user,
but issued MCP access tokens remain caic-scoped tokens.

The MCP resource URL is always:

```text
server.external_url + /api/caic/v1/mcp
```

The OAuth authorization server URL is always:

```text
server.external_url
```

Do not add configuration knobs for MCP `resource_url`, authorization-server URL,
or global MCP scopes.

## Current Authentication Modes

### Local no-auth mode

When caic OAuth login is not configured, `authStore == nil` and the MCP endpoint
accepts local clients without credentials. This is the intended localhost
workflow for development and personal use on `127.0.0.1`.

Validated local clients:

- Claude Code HTTP MCP connects and lists tools.
- Codex Streamable HTTP MCP connects and lists tools.

This mode is unsafe for an exposed server. Documentation and startup behavior
should make that clear.

### caic web session mode

When OAuth login is configured, browser users authenticate to caic with GitHub or
GitLab. caic stores the linked forge identity and issues a caic session cookie.
The web UI, Android WebView, and same-origin MCP callers can use this cookie.

The provider token is forge authority only. It is not an MCP bearer token.

### MCP OAuth bearer mode

When OAuth login is configured, remote MCP clients authenticate through caic's
OAuth endpoints:

1. MCP request without credentials receives `401` and `WWW-Authenticate`.
2. Client discovers protected-resource metadata.
3. Client discovers caic OAuth authorization-server metadata.
4. Client dynamically registers as a public client.
5. Client starts Authorization Code + PKCE S256.
6. User must already have a caic session.
7. caic shows consent.
8. Client exchanges the code for a caic MCP access token.
9. Client sends `Authorization: Bearer <token>` to `/api/caic/v1/mcp`.

Access tokens are RS256 JWTs with a one-hour lifetime. Refresh tokens are not
implemented.

## Invariants

- Do not accept query-string credentials.
- Do not support pasted static bearer tokens.
- Do not accept Anthropic, OpenAI, Google, GitHub, or GitLab bearer tokens at the
  MCP endpoint.
- Do not pass inbound MCP bearer tokens to upstream APIs.
- Preserve exact registered redirect URI matching.
- Preserve Authorization Code + PKCE S256.
- Require an authenticated caic session before MCP consent.
- Keep MCP access tokens scoped to caic.
- Keep remote MCP forge actions bound to linked user authority unless a product
  decision explicitly allows server-side forge authority.

## Work Plan

### 1. Real-client compatibility pass

Test the complete authenticated flow with real clients before adding protocol
extensions.

Clients:

- MCP Inspector.
- Claude Code local HTTP MCP.
- Codex local Streamable HTTP MCP.
- Claude hosted custom connector.
- ChatGPT custom app/connector.

Verify:

- unauthenticated MCP call returns `401` with `WWW-Authenticate`.
- protected-resource metadata discovery works.
- authorization-server metadata discovery works.
- Dynamic Client Registration works with each client.
- token endpoint form parsing works with each client.
- Claude hosted redirect URI works:
  `https://claude.ai/api/mcp/auth_callback`.
- Claude Code loopback redirect works, if Claude Code supports MCP OAuth login.
- Codex `codex mcp login caic` works.
- ChatGPT redirect URI from app management works.
- hosted clients either omit `Origin` or send an origin compatible with caic's
  same-origin check.
- one-hour access-token expiry is acceptable, or refresh tokens are required.

Do not add hosted-client origin allowlists, refresh tokens, or Client ID Metadata
Document support until this pass proves they are needed.

Validation:

```bash
go test ./backend/internal/mcp ./backend/internal/server
go run ./backend/internal/cmd/mcp-auth-smoke --client codex
```

Codex local OAuth evidence is covered by `mcp-auth-smoke`: it starts a local
auth-enabled caic server, drives Codex Dynamic Client Registration and PKCE via a
browser helper, verifies `codex mcp list` reports OAuth, then uses Codex's stored
access token to call `tools/list`.

Claude Code local CLI evidence: `go run ./backend/internal/cmd/mcp-auth-smoke
--client claude` reaches caic and receives the auth challenge, but Claude Code
2.1.168 reports `Status: ! Needs authentication` from `claude mcp get` and does
not start the OAuth flow from that command. This matches Claude Code's MCP docs:
remote MCP servers that return `401` or `403` are flagged in `/mcp`, and the user
completes OAuth from the interactive `/mcp` panel.

Next Claude Code local check:

1. add a server-only/manual mode to `mcp-auth-smoke` that starts the auth-enabled
   caic server, prints the endpoint plus isolated `HOME`, `BROWSER`, and
   `CAIC_SESSION_COOKIE` exports, then waits.
2. run Claude Code in a real terminal with those exports.
3. open `/mcp`.
4. authenticate the `caic` server.
5. verify `claude mcp get caic` reports `Status: ✓ Connected`.

`tmux` is not available in the current container. `script` from util-linux is
available and can allocate a PTY, but do not add brittle TUI automation until the
manual `/mcp` path is understood.

Also keep manual evidence for each hosted client: client version, configured
endpoint, observed redirect URI, requested scopes, and final MCP tool-list
result.

### 2. Operator setup and safety

Document the supported deployment modes:

- localhost development with MCP auth disabled.
- remote caic with GitHub OAuth login.
- remote caic with GitLab OAuth login.
- Claude Code local setup.
- Codex local setup.
- hosted Claude/ChatGPT setup.

Add a clear warning: if caic is reachable by other machines and OAuth login is
not configured, MCP is unauthenticated.

Decide whether startup should reject or warn on externally reachable MCP without
auth. A conservative rule would warn when auth is disabled and the server binds
to a non-loopback address or has a non-local `external_url`.

Validation:

- docs show exact commands for Claude Code and Codex.
- server tests cover any new startup warning or rejection rule.

### 3. Production MCP consent UX

Replace the minimal HTML consent page with production caic UI.

Requirements:

- show client name.
- show signed-in caic user.
- show requested resource.
- show requested scopes in human-readable form.
- explain that the client receives caic MCP access, not GitHub/GitLab/OpenAI or
  Anthropic credentials.
- preserve existing GET authorize and POST consent semantics.
- preserve exact redirect URI validation.

Validation:

- browser-flow tests for authorize page, approve, redirect, and error states.
- `make check`.

### 4. Token lifecycle

The current one-hour MCP access token may be enough for local clients and short
hosted sessions. Real-client testing should decide this.

If expiry is not acceptable, add refresh tokens:

- authorization-code response returns a refresh token.
- refresh grant rotates refresh tokens.
- reused refresh tokens are rejected.
- refresh tokens are revocable by user/client.
- logout does not silently revoke unrelated MCP clients unless product policy says
  it should.

Validation:

- expiry test.
- refresh success test.
- refresh rotation test.
- reuse rejection test.
- revoked-token rejection test.
- unknown-user rejection test.

### 5. Client metadata compatibility

Dynamic Client Registration is implemented for public clients. If hosted clients
cannot use DCR reliably, add only the minimum extension needed.

Possible extension:

- Client ID Metadata Document support.

Constraints:

- no static shared client secrets for public hosted clients.
- no broad redirect URI wildcards.
- no bypass of exact redirect URI matching.

Validation:

- compatibility test for the client that needs the extension.
- regression tests for invalid metadata and invalid redirects.

### 6. Origin policy

Current MCP origin validation permits absent `Origin` and requires same-origin
when `Origin` is present. Keep this until real hosted-client tests show it is too
strict.

If hosted clients send fixed third-party origins, add explicit allowlist behavior
only for proven clients and only for the MCP/OAuth endpoints that require it.

Validation:

- absent `Origin` accepted.
- same-origin accepted.
- mismatched origin rejected.
- any new allowed hosted origin is covered by tests.

### 7. Forge authority policy

Remote MCP forge tools currently require the remote caic user to have linked
GitHub/GitLab identity. Keep that rule until product policy changes.

Open decision:

- may an authenticated caic user use server-side PAT or GitHub App authority?
- if yes, is that limited by repo allowlists, admin scopes, or server owner
  approval?

Do not weaken linked-user authority before this decision.

Validation:

- remote MCP user without linked forge token cannot perform forge writes.
- linked user can perform allowed forge actions.
- any server-side authority policy has explicit tests.

### 8. Audit and revocation UX

MCP tool/resource calls are audited in memory and logged. Durable audit is
optional but useful once hosted clients are supported.

If implemented:

- persist allowed and denied MCP tool/resource calls.
- persist auth approvals and token refresh/revocation events.
- redact arguments and credentials before writing.
- fail open with a warning unless product policy requires fail-closed audit.

Add UI/API for users to view and revoke MCP client grants if refresh tokens or
long-lived grants are added.

Validation:

- persistence success test.
- redaction test.
- denied-call audit test.
- write-failure behavior test.
