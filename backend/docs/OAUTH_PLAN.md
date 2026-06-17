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

### Phase 1 — Consolidate Seam Functions into Interfaces

`ServerConfig` has 8 seam fields: 4 functions (`CurrentUserFunc`,
`AttachUserFunc`, `UserLookupFunc`, `EndSessionFunc`) and 4 interfaces
(`LoginAdapter`, `AuditRecorder`, `RateLimiter`, `ConsentRenderer`).

The user/session functions are tightly coupled and the UI seams are too.
Consolidating now avoids publishing a fragmented public API.

- [ ] Merge `CurrentUserFunc`, `AttachUserFunc`, `UserLookupFunc`,
      `EndSessionFunc` into `SessionManager`:
      ```go
      type SessionManager interface {
          CurrentUser(ctx context.Context) (User, bool)
          AttachUser(ctx context.Context, u User) context.Context
          FindUser(id string) (User, bool)
          EndSession(ctx context.Context, r *http.Request, u User) (redirectURL string)
      }
      ```
- [ ] Merge `LoginAdapter` and `ConsentRenderer` into `AuthorizationUI`:
      ```go
      type AuthorizationUI interface {
          LoginStartURL(r *http.Request) string
          ProviderLabel(provider string) string
          RenderOAuthConsent(w http.ResponseWriter, data *ConsentPageData) error
      }
      ```
- [ ] `BaseURLFunc` becomes `BaseURLResolver` interface with one method.
- [ ] Update `ServerConfig` (6 fields → 4: `SessionManager`, `AuthorizationUI`,
      `BaseURLResolver`, `AuditRecorder`, `RateLimiter`).
- [ ] Update `Server` struct, `NewServer`, all internal call sites.
- [ ] Update `internal/server/oauth_server.go` to implement the new interfaces.
- [ ] Tests: no behavior change; all existing tests pass with updated mocks.

### Phase 2 — Pushed Authorization Requests (RFC 9126) — nice to have

- [ ] Add `PARRequest` and `PARResponse` DTOs. `PARResponse` contains
      `request_uri` (short-lived, single-use) and `expires_in`.
- [ ] Add `POST /oauth/par` to `Routes()`. Validate the pushed parameters
      identically to authorize. Store the parameter set keyed by `request_uri`.
- [ ] In `handleOAuthAuthorizeGET`, accept `request_uri` as an alternative to
      inline parameters. Redirect the browser with only `client_id` and
      `request_uri`.
- [ ] Add `pushed_authorization_request_endpoint` and
      `require_pushed_authorization_requests` to AS metadata (RFC 8414 + 9126).
- [ ] Tests: successful PAR flow, reuse of request_uri, expired request_uri.

### Phase 3 — Device Authorization Grant (RFC 8628) — nice to have

- [ ] Add `DeviceAuthorizationRequest`, `DeviceAuthorizationResponse`,
      `DeviceTokenPollError` DTOs.
- [ ] Add `POST /oauth/device_authorization` returning `device_code`,
      `user_code`, `verification_uri`, `interval`, `expires_in`.
- [ ] Add a verification page at `GET /oauth/device` accepting `user_code`.
- [ ] Extend `POST /oauth/token` to accept `grant_type=urn:ietf:params:oauth:grant-type:device_code`
      with polling semantics (slow-down, authorization_pending).
- [ ] Add `urn:ietf:params:oauth:grant-type:device_code` to
      `grant_types_supported` metadata.
- [ ] Tests: full device flow from request through polling to token issuance.

### Phase 4 — Extract to Standalone Module

- [ ] Move `backend/internal/oauth` to a top-level `backend/oauth/` directory
      (or a separate repo, e.g., `github.com/maruel/oauth`).
- [ ] `go.mod`: module path, Go version (1.25+), minimal dependencies
      (stdlib-only is the goal; `golang.org/x/crypto` if needed for Ed25519).
- [ ] Audit imports: zero caic-specific imports allowed. Any remaining
      references to caic packages must be extracted into caller-provided seams.
- [ ] Public API doc: package-level godoc describing the server, client
      helpers, DTOs, and interfaces. Add a `doc.go`.
- [ ] `README.md` with quick-start: create a server, register a client,
      run the authorization flow.
- [ ] Update caic's `go.mod` to depend on the standalone module
      (or `go.work` during co-development).
- [ ] Update caic's import paths: `"github.com/caic-xyz/caic/backend/internal/oauth"`
      → `"github.com/maruel/oauth"`.

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

### Phase 6 — Frontend and Android SDK Consumers

Update caic's frontend and Android to use the standalone oauth module's
types and, where applicable, the SDK client.

- [ ] Frontend: import oauth DTOs for TypeScript type generation via the
      existing SDK generation pipeline. Replace hand-rolled OAuth type
      definitions with generated ones from the oauth module's JSON types.
- [ ] Frontend: if the oauth SDK has browser-applicable helpers (PKCE
      generation, authorization URL building), use them.
- [ ] Android: Kotlin DTOs generated from oauth module JSON types.
      Replace hand-rolled OAuth types in `gomode/` with generated ones.
- [ ] Both: update end-session flows to use `end_session_endpoint` from
      discovery metadata instead of hardcoded paths.
- [ ] Tests: e2e tests pass with no regression.
