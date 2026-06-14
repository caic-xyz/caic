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

### 1. Hosted-client compatibility pass

Local clients are verified. The remaining gap is hosted connectors and the
inspector, which must be exercised manually because they run off-host.

Clients:

- MCP Inspector.
- Claude hosted custom connector.
- ChatGPT custom app/connector.

The local flow already proves the generic protocol path (unauthenticated `401`
with `WWW-Authenticate`, protected-resource and authorization-server metadata
discovery, DCR, token endpoint form parsing). Per hosted client, additionally
verify:

- Dynamic Client Registration works with each client.
- token endpoint form parsing works with each client.
- Claude hosted redirect URI works:
  `https://claude.ai/api/mcp/auth_callback`.
- ChatGPT redirect URI from app management works.
- hosted clients either omit `Origin` or send an origin compatible with caic's
  same-origin check.
- one-hour access-token expiry is acceptable, or refresh tokens are required.

Do not add hosted-client origin allowlists, refresh tokens, or Client ID Metadata
Document support until this pass proves they are needed.

Keep manual evidence for each hosted client: client version, configured endpoint,
observed redirect URI, requested scopes, and final MCP tool-list result.

### 2. Token lifecycle

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

### 3. Client metadata compatibility

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

### 4. Origin policy

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

### 5. Forge authority policy

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

### 6. Audit and revocation UX

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
