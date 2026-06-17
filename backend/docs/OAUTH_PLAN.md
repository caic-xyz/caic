# OAuth Package — Standalone Library Plan

The oauth package will become a standalone Go module reusable across projects.
This plan covers the path from internal package to public library with SDK.

## Working Rules

- Keep each phase independently shippable. Prefer one capability at a time.
- Backward compatibility is not required during development. Once published,
  follow semver.
- The public API surface is the importable package. `ServerConfig`, DTOs,
  interfaces, `Server`, and `AccessTokenService` are the contract.
- `Store` and internal handler details are private implementation.
- Tests for every public symbol.

## Phases

### Phase 1 — Consolidate Seam Functions into Interfaces ✓

`ServerConfig` had 8 seam fields. Consolidated to 5: two interfaces plus
three standalone seams.

- [x] Merge `CurrentUserFunc`, `AttachUserFunc`, `UserLookupFunc`,
      `EndSessionFunc` into `SessionManager`.
- [x] Merge `LoginAdapter` and `ConsentRenderer` into `AuthorizationUI`.
- [x] `BaseURLFunc`, `AuditRecorder`, `RateLimiter` stay as-is.
- [x] Update `Server` struct, `NewServer`, all internal call sites.
- [x] Update `internal/server/oauth_handlers.go` to implement the new
      interfaces (`caicSessionManager`, `caicAuthorizationUI`).
- [x] Tests: no behavior change; all existing tests pass with updated mocks.

### Phase 2 — Pushed Authorization Requests (RFC 9126) ✓

- [x] Add `PARRequest` and `PARResponse` DTOs. `PARResponse` contains
      `request_uri` (short-lived, single-use) and `expires_in`.
- [x] Add `POST /oauth/par` to `Routes()`. Validate the pushed parameters
      identically to authorize. Store the parameter set keyed by `request_uri`.
- [x] In `handleOAuthAuthorizeGET`, accept `request_uri` as an alternative to
      inline parameters. Redirect the browser with only `client_id` and
      `request_uri`.
- [x] Add `pushed_authorization_request_endpoint` and
      `require_pushed_authorization_requests` to AS metadata (RFC 8414 + 9126).
- [x] Tests: successful PAR flow, reuse of request_uri, expired request_uri.

### Phase 3 — Device Authorization Grant (RFC 8628) ✓

- [x] Add `DeviceAuthorizationRequest`, `DeviceAuthorizationResponse`,
      `DeviceTokenPollError` DTOs.
- [x] Add `POST /oauth/device_authorization` returning `device_code`,
      `user_code` (8-char uppercase alphanumeric), `verification_uri`,
      `interval`, `expires_in`.
- [x] Add a verification page at `GET /oauth/device` accepting `user_code`.
- [x] Extend `POST /oauth/token` to accept `grant_type=urn:ietf:params:oauth:grant-type:device_code`
      with polling semantics (authorization_pending, slow_down,
      expired_token, access_denied).
- [x] Add `urn:ietf:params:oauth:grant-type:device_code` to
      `grant_types_supported` metadata.
- [x] Tests: full device flow, polling pending, expired code, page
      rendering, unauthenticated approval, invalid user_code, restart
      resilience.

### Phase 4 — Extract to Standalone Module ✓

- [x] Move `backend/internal/oauth/` to `oauth/` at repo root as standalone module
      (`github.com/caic-xyz/oauth`).
- [x] `go.mod`: module path `github.com/caic-xyz/oauth`, Go 1.25, stdlib-only.
- [x] Audit imports: confirmed zero caic-specific imports.
- [x] Package-level godoc in `dto.go` covering RFCs and interfaces.
- [x] `README.md` with quick-start example.
- [x] Update caic's `go.mod`: add `require` and `replace` directives.
- [x] Update caic's 8 import paths from `backend/internal/oauth` to `github.com/caic-xyz/oauth`.
- [x] Allowed `replace-local` in `.golangci.yml` for unpublished module.
- [x] `make check` passes (lint, build, test, frontend, e2e helpers).

### Phase 5 — Generate OAuth SDK via apisdkgen

Use the existing SDK generation pipeline to produce typed TypeScript and
Kotlin DTOs from the oauth package's types. The oauth endpoints follow
RFC-defined contracts (form-encoded, redirects), so the SDK focuses on
the DTO and discovery types, not a hand-rolled HTTP client.

- [ ] Add an SDK generation specification in the oauth package
      (like `backend/internal/server/api/v1/sdk.go`) that declares which
      types to export and route definitions for endpoints that use
      structured JSON (register, jwks, discovery metadata, introspection).
- [ ] Generate TypeScript types and Kotlin data classes from the DTOs.
- [ ] Generated types include: `AuthorizationServerMetadata`,
      `ProtectedResourceMetadata`, `RegisterRequest`/`Response`,
      `TokenResponse`, `IntrospectionRequest`/`Response`, `JWK`/`JWKSet`,
      `ErrorResponse`, `ConsentPageData`.
- [ ] Tests: generated TypeScript types compile; generated Kotlin types
      match the Go struct JSON tags.

### Phase 6 — Frontend and Android SDK Consumers ✓

Update caic's frontend and Android to use the oauth SDK types.

- [x] SDK generation pipeline produces TypeScript (`sdk/oauth/ts/v1/types.gen.ts`),
      Kotlin (`sdk/oauth/kotlin/.../Types.kt`), and Swift types from the oauth
      DTOs. 15 interfaces/data classes covering discovery metadata, client
      registration, token responses, introspection, and device authorization.
- [x] Frontend and Android already consume OAuth types through the caic API
      SDK (`OAuthGrantResp`), not raw oauth protocol DTOs. No hand-rolled
      OAuth types to replace.
- [x] No end-session flows exist in frontend/Android — login/logout is
      server-side via redirects.
- [x] Generated TypeScript types compile without errors.
