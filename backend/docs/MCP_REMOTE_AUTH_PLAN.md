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

This mode is unsafe for an exposed server, so it is confined to loopback: when
auth is not configured and the listener binds a non-loopback address, the MCP
endpoint is left unregistered and requests get `404`.

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
6. caic requires a web session. If the user is not signed in, the authorize
   endpoint redirects to caic login and resumes the same authorization request
   after provider callback.
7. caic shows consent.
8. Client exchanges the code for a caic MCP access token.
9. Client sends `Authorization: Bearer <token>` to `/api/caic/v1/mcp`.

Access tokens are RS256 JWTs with a one-hour lifetime. Refresh tokens are
caic-scoped opaque tokens with a 30-day lifetime and a durable hashed on-disk
store. The token endpoint rotates refresh tokens on every refresh grant; reused,
expired, revoked, or unknown-user refresh tokens are rejected. The revocation
endpoint lets clients revoke refresh tokens.

## Invariants

- Do not accept query-string credentials.
- Do not support pasted static bearer tokens.
- Do not accept Anthropic, OpenAI, Google, GitHub, or GitLab bearer tokens at the
  MCP endpoint.
- Do not pass inbound MCP bearer tokens to upstream APIs.
- Preserve exact registered redirect URI matching.
- Preserve Authorization Code + PKCE S256.
- Require an authenticated caic session before MCP consent; redirect browser
  authorization requests through caic login when no session exists.
- Keep MCP access tokens scoped to caic.
- Keep remote MCP forge actions bound to linked user authority unless a product
  decision explicitly allows server-side forge authority.

## Work Plan

### 1. Token lifecycle

Add user-owned grant management before relying on long-lived hosted use:

- add a Settings page/API for connected MCP clients.
- persist explicit grant records for the UX: user ID, client ID/name, scopes,
  resource, creation time, last-used time, expiry, and revocation status.
- support user-initiated grant revocation in addition to client-driven token
  revocation.

Validation:

- grant list shows only the authenticated user's MCP clients.
- grant details show client identity, scopes, resource, timestamps, expiry, and
  status.
- user revocation blocks future access-token refresh for the grant.
- user revocation does not affect unrelated users or clients.

### 2. Forge authority policy

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

### 3. Audit and revocation UX

MCP tool/resource calls are audited in memory and logged. Durable audit is
optional unless product policy requires persistent traceability.

If implemented:

- persist allowed and denied MCP tool/resource calls.
- persist auth approvals and token refresh/revocation events.
- redact arguments and credentials before writing.
- fail open with a warning unless product policy requires fail-closed audit.

Add UI/API for users to view and revoke MCP client grants. This is required
because refresh tokens are long-lived and survive server restart.

Validation:

- persistence success test.
- redaction test.
- denied-call audit test.
- write-failure behavior test.
