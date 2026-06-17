# Go Mode Server Library — Design & Migration

Design and staged-extraction plan for the reusable Go Mode **server** library:
the backend counterpart to the Go Mode Android shell. It lets any backend become
a Go Mode host (discovery manifest + voice transport + scoped tokens). caic is
the first consumer; mddb is the planned second.

This supersedes `gomode/docs/GOMODE_SERVICE_DISCOVERY.md` for architecture and
ownership (see [Supersession](#supersession)).

## Ownership Principle

Go Mode owns the **contract**; hosts are **consumers**.

The Android shell (`com.fghbuild.gomode`) and this server library are two halves
of one product. The voice gateway, the discovery manifest, and the scoped-token
format belong to Go Mode, not to caic. A host (caic, mddb) configures and embeds
the library and writes a thin adapter; it does not own the wire contract.

This is the same trajectory as `oauth/`: a generic, stdlib-first package that
serves caic and "at least one additional client", staged toward a standalone
module. We follow that precedent deliberately.

## Library Boundary

In the library (Go Mode owns it):

- The `/.well-known/gomode.json` discovery manifest: types, builder, handler.
- The web-shell contract: bridge version, tool-group catalog shape, voice
  gateway metadata.
- The voice gateway transport: WebRTC signaling/media, Gemini Live and
  local-stack adapters, standalone + embedded handlers, gateway config.
- The scoped-token format: claim struct, issue (host-side), verify
  (gateway-side), key encode/parse. Pure stdlib ed25519.

A host adapter (each consumer owns it):

- Which tool groups to advertise and their endpoints (caic: `tasks` at
  `/api/caic/v1/mcp`; mddb: its own).
- `service`, `serviceVersion`, voice-gateway mode/URL — passed in as values.
- Token **issuance policy**: who gets a token, under what auth. The host signs
  with the library helper; the library does not decide policy.
- Mounting the handlers on the host's router; product auth.

## Package Layout

One library, single namespace. The heavy transport is a **subpackage**, so the
dependency cost is paid per-import, not per-module: a host that only serves the
manifest imports `gomode` and never compiles pion/opus/genai.

```text
gomode/                      module: github.com/caic-xyz/caic/gomode (now)
                                     github.com/caic-xyz/gomode      (later)
  docs/DESIGN.md
  http.go                  manifest types + builder + handler
                           (to split into discovery.go + handler.go)
  sdk.go                   discovery-manifest SDK spec
  voicegateway/            transport subpackage (heavy deps: pion, opus, genai)
    config.go  http.go  scoped_token.go  ...
    api/v1/               data-channel protocol DTOs + SDK spec
    voicertc/             WebRTC bridge, gemini, localstack
```

`gomode/` and its `voicegateway/` subpackage now live at the repo root (out of
`backend/internal/`), alongside `oauth/`. The library has no `backend/internal/*`
dependency.

Dependency direction: `gomode` must never import the transport (the manifest
carries only a URL + auth flags), keeping the contract package stdlib-only. Once
`scoped_token` moves to the root, `gomode/voicegateway` will import `gomode` for
token verification — child-imports-parent, no cycle.

### Why scoped_token should move to the root

`Issue` (host) and `Verify` (gateway) both live in
`gomode/voicegateway/scoped_token.go`. Signing is host-side; verifying is
gateway-side; both share the claim struct. Hosting it in the `gomode` root would
let a host sign without importing the WebRTC transport, and keep the token a
first-class part of the Go Mode contract rather than a voice-gateway internal.
Deferred from the initial move to keep that step a pure path relocation.

## The Three Contracts

1. **Discovery manifest** — static JSON at `/.well-known/gomode.json` (RFC 8615),
   public, cached with ETag. Advertises `service`, `apiVersion`,
   `webShell.bridgeVersion`, `webShell.toolGroups`, `webShell.voiceGateway`.
   Tool groups are a catalog of MCP skills the shell activates on-device.

2. **Voice transport** — WebRTC offer/answer + provider-neutral data-channel
   protocol (`session.setup`, `tool.call`, `transcript.delta`, …). Brokers to
   Gemini Live or a local stack. No host domain concepts. Runs standalone
   (`cmd/voice-gateway`) or embedded.

3. **Scoped token** — short-lived ed25519-signed claim (`serviceKind`,
   `serviceInstanceID`, `backendOrigin`, `sub`, `capabilities`, `aud`, `exp`).
   Host signs; gateway verifies against trusted issuers. Already generic — config
   already names `"caic" or "mddb"` as issuer kinds. See [Auth Model](#auth-model)
   for the planned migration to oauth-issued JWTs.

## Auth Model

Three roles, deliberately separated:

- **Host = authorization server**: owns user identity, login, sessions. It
  authenticates the user and *mints* the voice token. caic/mddb use `oauth/`.
- **gomode = token contract**: the claim shape and audience (`voice-gateway`).
  It does not authenticate users and is not an auth server.
- **Gateway = resource server**: holds no credentials, runs no login. It only
  *verifies* tokens and brokers media. This verify-only posture is a security
  feature — a compromised gateway leaks no long-lived secrets.

### A shared gateway needs federation, not SSO

The goal "people use a shared voice gateway" (one gateway serving caic, mddb,
others) is an **issuer-federation** problem: the gateway must verify tokens from
many hosts. It is *not* SSO. The gateway has no login screen and never sees a
user authenticate, so single-sign-on (one user identity across products) is an
orthogonal, host-level concern. You can run a fully shared gateway with zero SSO.

"SSO between the host and the gateway", in the useful sense, just means the
gateway is another resource server protected by the host's AS: one login at the
host yields a token whose `aud` includes `voice-gateway`. That is the RFC 8707
audience pattern, which `oauth/` already supports.

Pursue user-SSO (a central IdP, hosts as OIDC relying parties) only if "one
identity across caic + mddb" is a product goal. If so it is an `oauth/`
initiative, not a gateway or gomode one.

### Two deployment modes

- **Embedded** (gateway in the host process, e.g. caic today): the RTC routes
  ride the host's own session auth (`AuthRequired:false`, no token). De-facto
  single-auth already; nothing to build.
- **External shared** (one gateway, many hosts): the only place federation
  applies. Host mints a JWT; gateway verifies it (below).

### Token format: bespoke ed25519 → oauth JWT

Today the scoped token is a custom ed25519 envelope verified against a static
per-host key list (`TrustedIssuers`). That does not scale to an open set of
hosts. Target:

- Host mints an **oauth-issued JWT** (`aud=voice-gateway`, short `exp`,
  capabilities as `scope`) using its `oauth/` `AccessTokenService`.
- Gateway resolves the token's `iss` → fetches the issuer's
  `/.well-known/oauth-authorization-server` → `jwks_uri` → caches keys →
  verifies locally. Trust is gated by an **allowlist of issuer origins** (the
  operator's knob), replacing the static *key* list with a static *origin* list;
  keys then auto-resolve and rotate via JWKS.

### `oauth/` readiness

The issuer side is ~80% done in `oauth/` (stdlib-only): JWT access tokens
(RS256/ES256, RFC 9068), JWKS at `/oauth/jwks` with rotation, AS metadata at
`/.well-known/oauth-authorization-server`, and Bearer/local JWT verification a
resource server can reuse. Two gaps, neither blocking:

- **RFC 8693 token exchange is not implemented** (the package docs over-claimed
  it; corrected). Not needed — the host backend already knows the user from the
  session and mints the voice JWT *at source* with the right `aud`.
- **No multi-issuer verifier** in `oauth/`: resolve-issuer-by-`iss` + JWKS cache
  + origin allowlist is the one genuinely new component, and it lives on the
  gateway. Modest and self-contained.

The bespoke `scoped_token` is then retired (or kept only for a fully-offline
embedded case). Until that migration lands, moving `scoped_token` to the gomode
root (migration step 3) still holds: it keeps host signing decoupled from the
WebRTC transport regardless of the eventual format.

## Staging (oauth precedent)

The library lives at top-level `gomode/` in the caic module
(`github.com/caic-xyz/caic/gomode`) — cheap and atomic, lets the contract keep
moving without cross-repo cost.

**Later**: split to `github.com/caic-xyz/gomode` once the contract settles and
mddb is ready. With the dependency direction above and no `backend/internal/*`
deps, the split is a `go.mod` + import-path rename, no restructuring.

Mark the staging intent in `gomode/AGENTS.md`, mirroring `oauth/AGENTS.md`'s
"Planned: extracted to a standalone Go module" note.

## SDK Generation

`backend/internal/cmd/gen-api-sdk` discovers `SDKAPI()` specs and emits TS /
Kotlin / Swift. Two specs now live with the library: the discovery manifest
(`gomode.SDKAPI`) and the voice protocol (`gomode/voicegateway/api/v1.SDKAPI`).
Generated output is byte-identical after the move.

- Keep two generated SDKs (`sdk/gomode`, `sdk/voicegateway`): they are distinct
  wire surfaces (a JSON manifest vs a WebRTC data-channel protocol) and map to
  the per-package boundary. Android keeps consuming `:gomode-sdk`,
  `:voicegateway-sdk` unchanged.
- Output dirs and Kotlin package names (`com.fghbuild.gomode.sdk.v1`,
  `com.caic.voicegateway.sdk.v1`) stay, to avoid churn now. The voicegateway
  Kotlin package rename to a `fghbuild`/gomode namespace is a later, separable
  step.

## What Stays In caic (Adapter)

- `backend/internal/server/gomode.go` — builds `gomode.Settings` (tasks group,
  caic MCP endpoint, embedded `/` URL), calls the library handler.
- `backend/internal/server/voice_handlers.go` — wires the embedded transport +
  bridge into caic's router; computes voice metadata mode.
- Voice-token endpoint in `server_handlers.go` — calls `gomode.Issue…` under caic
  auth.
- `backend/cmd/voice-gateway` — standalone binary; re-points imports to
  `gomode/voicegateway`. May move into the library later.

## Migration Plan

The path relocation is done: `gomode/` and `gomode/voicegateway/` live at the
repo root, imports are rewritten, `gen-api-sdk` source paths updated, generated
SDKs unchanged, lint and tests green. Remaining, low-risk-first:

1. Add a short `gomode/README.md` (the contract `AGENTS.md` and file index exist).
2. Split `gomode/http.go` into `discovery.go` (types + builder) and `handler.go`.
3. Move `scoped_token.go` (+ test) to the `gomode` root; qualify the references
   in `voicegateway` (`http.go`, `config.go`, tests) and have the transport
   import `gomode` for verification. See [Why scoped_token…](#why-scoped_token-should-move-to-the-root).
4. Reframe `gomode/docs/GOMODE_SERVICE_DISCOVERY.md` to Android-shell items only
   (a supersession header already points here).

Later / separable: voicegateway Kotlin package rename; fold `cmd/voice-gateway`
into the library at the repo-split.

## Supersession

`gomode/docs/GOMODE_SERVICE_DISCOVERY.md` predates the library framing and mixes
three owners. Reassign its open items:

- **Library**: manifest fields, compatibility states, tool-groups-as-skills
  catalog shape, `server/discover` negotiation, event-driven subscription
  invalidation in the embedded gateway.
- **caic adapter**: the `caic://tasks` resource, caic monitoring/notification
  mapping, product route knowledge.
- **Android shell**: MCP resource client, auth-gated bootstrap states, voice
  session tool merge, native monitoring.

Trim that doc to the Android-shell items and link here for the contract (a
supersession header already points here; full reframe is migration step 4).

## Observations / Later

- **No `backend/internal/*` deps**: the move surfaced one such dependency —
  `voicertc/gemini.go` embedded `backend/internal/jsonutil`'s `Overflow` in two
  Gemini wire structs. That embed was inert (its `Extra` field is `json:"-"` and
  was never populated; the audio-stripping round-trip already drops unknown
  fields), so it was removed. `jsonutil` stays in `backend/internal/`. If real
  forward-preservation of unknown Gemini fields is ever wanted, wire it locally
  rather than reaching back into backend.
- Consider folding `cmd/voice-gateway` into the library (`gomode/cmd/...`) at the
  repo-split step so the standalone binary ships with the contract it serves.
- The voicegateway Kotlin SDK package (`com.caic.voicegateway`) should move to a
  Go Mode namespace when the repo split happens; deferring avoids churn now.
- Evaluate whether `service`/`serviceVersion` belong in a small host-info struct
  passed to the builder, vs loose fields, once mddb's adapter exists to compare.
