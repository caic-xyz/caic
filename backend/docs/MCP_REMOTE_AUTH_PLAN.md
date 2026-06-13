# MCP Remote Authentication Plan

Remaining work to make hosted Claude and ChatGPT use caic's MCP endpoint with
caic-owned OAuth. caic itself is the only supported OAuth authorization server;
`authorization_servers` must remain `server.external_url`, and the MCP resource
must remain `server.external_url + /api/caic/v1/mcp`.

## Constraints

- Re-read `backend/internal/mcp/AGENTS.md` and the upstream MCP draft auth and
  streamable HTTP docs before changing MCP protocol behavior.
- Do not add MCP auth config knobs for `resource_url`, `authorization_server`,
  or global scopes.
- Do not make Google login imply GitHub/GitLab authority.
- Do not accept Google, OpenAI, Anthropic, GitHub, or GitLab tokens as MCP
  bearer tokens.
- Do not pass inbound MCP bearer tokens to upstream APIs.
- Do not support query-string credentials or pasted static bearer tokens.

## 1. Hosted-Client Compatibility Testing

Test with real clients and document the operator setup:

- MCP Inspector auth flow.
- Claude custom connector.
- ChatGPT custom app/connector.

Verify:

- unauthenticated MCP call receives 401 with `WWW-Authenticate`.
- protected resource metadata discovery works.
- Claude hosted redirect URI works: `https://claude.ai/api/mcp/auth_callback`.
- Claude Code loopback redirect works if supported.
- ChatGPT redirect URI from app management works.
- DCR public-client registration is accepted by both hosted clients.
- token endpoint form parsing works.
- one-hour access-token expiry behavior is acceptable, or refresh tokens are
  required.
- hosted clients either omit `Origin` or send an origin compatible with the
  current same-origin check.

Do not add hosted-client origin allowlists, refresh tokens, or CIMD until this
compatibility pass proves they are needed.

Validation: document manual test evidence and keep automated tests passing.

## 2. Production OAuth UX

The local OAuth server is functional but uses a minimal HTML consent page.
Replace it with production caic UI when hosted-client testing confirms the auth
flow shape.

Requirements:

- preserve Authorization Code + PKCE S256 semantics.
- preserve exact registered redirect URI matching.
- show client name, requested resource, and requested scopes.
- require an authenticated caic session before consent.
- keep issued MCP access tokens as caic-scoped tokens.

Validation: `make lint-go` and focused OAuth browser-flow tests.

## 3. Optional OAuth Extensions After Compatibility Testing

Implement only if real hosted-client testing requires them:

- Refresh-token grant with rotation and revocation.
- Client ID Metadata Document support if DCR is too noisy or incompatible.
- Google login as a caic login provider. Google remains upstream identity only;
  issued MCP bearer tokens must remain caic tokens.
- Key rotation with overlapping JWKS keys. Signing key persistence already avoids
  invalidating every access token on normal restart.

Validation: add focused protocol tests for each extension before enabling it.

## 4. Persisted Audit Store

MCP tool/resource calls are audited in memory and logged. If durable audit is
required, add an append-only JSONL audit store under the caic cache or config
directory.

Requirements:

- redact argument summaries before persistence.
- record allowed and denied tool/resource calls.
- record rate-limit denials if the limiter moves to a lower layer.
- fail open with `slog.WarnContext` unless a separate product decision requires
  fail-closed audit.

Validation: persistence tests for success, denied calls, redaction, and write
failure behavior.

## 5. Forge Authority Policy Hardening

Remote MCP forge tools currently require the remote caic user to have linked
GitHub/GitLab identity. Before allowing server-side PAT or GitHub App authority
for remote MCP users, decide the product policy.

Open decision:

- whether any authenticated caic user may use configured server-side forge
  credentials, or whether this requires repo allowlists/admin scopes.

Until decided, do not weaken the linked-forge requirement for remote MCP bearer
tokens.

Validation: add tests for any approved server-side forge credential policy.
