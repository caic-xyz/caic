# Android Web Shell Plan

This is the canonical plan for replacing duplicated Android screen-mode UI with
a thin Android shell around the existing SolidJS mobile web frontend.

It also records one architectural fork: whether this work should remain inside
the caic Android app or become a second Android app that can host caic, mddb,
and future server-backed tools behind a shared native voice endpoint layer.

The target is not a pure web app. Android stays authoritative for platform
capabilities that require native APIs:

- Local voice endpoint behavior: WebRTC client setup, audio routing,
  microphone permission, and the voice foreground service.
- Android task notifications and notification permission.
- Server bootstrap settings needed before a WebView can load anything.
- OAuth callback handling and native bearer-token state needed by Android API
  clients.
- MediaProjection screenshot capture, if the web image attach flow cannot
  provide the same Android experience.
- Halo/BLE integration. Halo is in scope even if the first implementation is
  still incomplete.

The migration should proceed only if the first implementation proves that
WebView loading, auth, native notifications, and voice-gateway-mediated voice
can work without a broad bridge or fragile request interception.

## Current Facts

- `frontend/` is already the richer screen-mode implementation for task list,
  task detail, grouping, widgets, VNC, diff, process views, formatting, and
  mobile behavior.
- Android duplicates much of that screen UI in Compose and currently relies on
  convention for parity with the frontend.
- `TaskListViewModel` starts `TaskRepository.start(viewModelScope)` and
  `TaskNotifier.start(viewModelScope)`. Shell mode must not depend on that
  ViewModel being created.
- `TaskRepository.start(scope)` is intended to be called once, but it currently
  launches collectors on every call. App-scoped startup must be explicitly
  idempotent.
- `TaskNotifier` creates notification intents for `MainActivity`, but currently
  does not include task routing information. Notification deep links must add
  task ID or route extras.
- `MainActivity.onNewIntent()` currently handles OAuth callback intents. It must
  also handle pending task navigation before WebView routing is complete.
- The generated TypeScript SDK and SSE calls use relative `/api/...` URLs.
  Backend-hosted frontend loading is therefore the correct first spike.
  Bundled APK assets would force API-origin, EventSource, service-worker, and
  auth work too early.
- The frontend currently renders `VoiceOverlay` and requests browser
  notifications. Android host mode must suppress both unless they are explicitly
  backed by native Android behavior.
- The Android app must keep multiplexing multiple caic backend servers. Native
  Android settings and server-specific web settings are separate ownership
  domains.
- mddb is another backend-hosted frontend service with OAuth, generated SDK/API
  docs, e2e coverage, and mobile UI tests. This makes a multi-service Android
  host plausible, but it should be decided explicitly before the caic shell work
  hardens caic-specific abstractions.
- The caic backend currently exposes `/api/v1/voice/rtc/offer` and terminates
  the WebRTC side of a voice session through `internal/server/voicertc`.
- The current caic API reports voice availability as `Config.webrtcAvailable`.
  That boolean is too coarse for the shell architecture because Android must
  distinguish disabled voice, caic-embedded gateway, and external preferred
  gateway modes.
- `backend/cmd/webrtc-relay` is partially implemented, but is currently a caic
  sidecar: it reads caic `settings.json`, caic `users.json`, and validates caic
  JWTs. That coupling must be removed before it can bridge caic, mddb, and future
  services.
- The checked-in `voicertc` bridge dials Gemini Live WebSocket and forwards
  Gemini protocol messages over the WebRTC data channel while converting audio
  between RTP and Gemini PCM messages.
- The checked-in Android and web voice clients still contain local function
  declaration and tool-call dispatch code. That is a transitional split. The
  target shell architecture should move Gemini setup, function declarations,
  tool calls, and tool responses into a standalone voice gateway so Android is
  not the tool executor and the caic backend is not the only possible service.

## Non-Goals

- Do not start with a Trusted Web Activity. The shell needs direct native state,
  foreground service, notification, permission, and voice integration.
- Do not delete Compose screen mode until the WebView shell passes the
  acceptance checks below.
- Do not proxy normal caic API calls through JavaScript bridge methods.
- Do not bundle frontend assets into the APK. There is no offline use case; the
  app cannot do useful work without network access to a backend service.
- Do not move Android voice endpoint behavior into browser JavaScript.
- Do not make Android execute caic voice tools. Tool execution belongs on the
  voice gateway side of the Gemini Live bridge, using service-owned tool binding
  APIs.
- Do not keep `webrtc-relay` as a caic-auth-coupled sidecar if it becomes part
  of the Android shell architecture.
- Do not weaken notification, microphone, audio routing, foreground service, or
  MediaProjection behavior to fit a pure web model.
- Do not add broad refactors before the WebView, auth, notification, and voice
  risks are proven.

## Target Architecture

```text
MainActivity
  -> native bootstrap shell
      -> Android app settings
          -> configured caic backend servers
          -> configured voice gateway
          -> notification policy
          -> voice endpoint settings
          -> Halo/BLE settings
          -> shell protocol compatibility state
      -> app-scoped TaskMonitor
          -> TaskRepository
          -> TaskNotifier
      -> VoiceEndpoint / VoiceService / VoiceViewModel
          -> WebRTC client connection to configured voice gateway
      -> WebShellScreen
          -> WebView loading the backend-hosted SolidJS frontend
          -> narrow JavaScript bridge for native-only capabilities

Voice gateway
  -> WebRTC signaling and media endpoint
      -> Gemini Live WebSocket
      -> voice system instruction and service-specific tool declarations
      -> service adapter dispatch
          -> caic tool binding API
          -> mddb tool binding API
          -> future service binding APIs

Active service backend
  -> hosted web frontend
  -> normal product API
  -> auth/session owner
  -> preferred voice gateway metadata
  -> scoped voice-gateway token issuer
  -> service-specific tool binding API
```

Ownership rules:

- Web owns screen-mode task UI once the shell is accepted.
- Android owns native settings needed to configure the first server or recover
  from a broken WebView.
- Android owns multiplexing between configured backend servers.
- Each backend frontend owns instance-specific server settings such as cache,
  server preferences, workspace settings, and product-specific configuration.
- The backend web UI should remain the primary owner of authentication unless a
  native capability has a proven need for bearer-token access.
- Android owns local voice endpoint lifecycle and platform permissions.
- The voice gateway owns Gemini Live session orchestration, Gemini setup,
  function declarations, tool-call dispatch, and tool responses.
- Each service backend owns its web UI, auth, product API, and service-specific
  tool binding implementation. The gateway calls these bindings through scoped
  credentials issued by the service backend.
- A service backend may either host the voice-gateway protocol itself or
  advertise a preferred external gateway. Android always talks to a
  voice-gateway protocol endpoint, not to caic-specific voice routes.
- Android owns task notifications.
- Android owns MediaProjection screenshot capture until an equivalent web path
  is proven.
- Android owns Halo/BLE integration.
- The JavaScript bridge is capability-oriented, not a second API client.
- The native shell must negotiate protocol compatibility with the loaded server
  before enabling native bindings.

## App Boundary Decision

There are two viable product boundaries.

Option A: evolve the existing caic Android app.

- Best when caic is the only near-term target.
- Lowest migration cost because existing Android voice endpoint code,
  notifications, Halo, screenshot capture, settings, and e2e infrastructure
  already live here.
- Risk: caic-specific naming, repositories, and bridge contracts can become hard
  to generalize later.

Option B: create a second Android app as a generic native service host.

- The app would provide microphone/audio routing, foreground services,
  notifications, WebView hosting, protocol negotiation, and native bindings as
  shared infrastructure. The voice gateway would provide the shared Gemini Live
  bridge and dispatch to service-specific tool binding APIs.
- caic, mddb, and future services would be configured as service instances with
  their own backend URL, web frontend, capabilities, and bridge schema.
- This aligns with a future where servers expose agent skills through
  https://agentskills.io/ style capability metadata.
- Risk: it creates a product and packaging split before the caic WebView shell
  has proven its basic lifecycle, auth, and voice assumptions.

Decision rule:

- Keep the first spike in the existing caic Android app unless the generic host
  boundary can be defined without delaying the caic migration.
- Before implementing the full bridge, decide whether bridge package names,
  settings models, and protocol names should be caic-specific or generic.
- If the generic host is chosen, extract only after the caic spike proves
  WebView loading, auth, notifications, and voice-gateway-mediated voice. Do not start
  by moving incomplete behavior into a new app.

## Settings Model

Native Android settings and backend settings must stay separate.

Android app settings:

- Configured service instances, initially multiple caic backend servers.
- Configured voice gateway URL and last-known compatibility state.
- Active service instance.
- Notification policy and Android notification permission state.
- Voice endpoint settings such as voice enablement, preferred voice name if the
  backend supports it, audio device behavior, and microphone permission recovery.
- Halo/BLE settings and device binding.
- Shell protocol compatibility state and last-known server capability metadata.
- Native recovery and diagnostics settings.

Backend-owned settings:

- caic server preferences such as cache and server-side behavior.
- Product-specific workspace or instance settings.
- Authentication session state exposed through the backend web UI.
- Service-specific notification preferences if the service supports them.
- Voice-gateway token issuance policy for that service.

Voice gateway settings:

- HTTP signaling listen address.
- UDP WebRTC port.
- Gemini API key source and model defaults.
- Gateway-owned authentication settings.
- OAuth provider settings for Google, GitHub, and GitLab.
- Tailscale/private-network access policy.
- Trusted service issuers and token verification material.
- Service adapter registry for caic, mddb, and future services.
- Gateway logging, diagnostics, and compatibility metadata.

Rules:

- Do not mirror backend settings into Android unless a native service needs them.
- Do not let the voice gateway scrape service private config directories such as
  caic `settings.json` or `users.json`.
- If Android caches backend capability or version metadata, treat it as a cache
  that can be refreshed or invalidated.
- Server switching must switch WebView origin, native monitoring, notification
  routing, and native capability state as one coherent operation.
- In a future generic host, settings should be keyed by service type and service
  instance ID, not only by URL.

Voice gateway config ownership:

- Standalone `~/.config/voice-gateway/config.toml` is canonical.
- The gateway implementation should expose a reusable static config type and
  validation function.
- caic may embed the same static gateway config under `[voice-gateway.config]`
  when caic hosts the gateway protocol in-process.
- caic should normally store only gateway connection settings: mode, URL,
  compatibility state, and service-side token issuance policy.
- mddb and future services must be able to use the same standalone gateway
  without depending on caic config.
- Embedding gateway config in caic must not make the gateway read caic
  `settings.json`, `users.json`, or service-private files.
- If caic embeds gateway config, unknown fields and gateway version skew must be
  handled deliberately so a newer gateway config does not make an older caic
  binary fail to start.
- Editing gateway-only config in an embedded caic deployment should not force
  unrelated caic restarts unless caic is actually supervising the gateway.
- caic should advertise its preferred gateway through API metadata instead of
  relying on HTTP redirects for WebRTC signaling.

Recommended standalone voice gateway config:

```toml
[server]
http = ":3479"
webrtc_udp_port = 3478
external_url = "https://voice.example.com"

[gemini]
api_key_env = "GEMINI_API_KEY"
model = "gemini-live-2.5-flash-preview"

[auth]
session_secret_file = "session_secret"
allowed_users = ["marc@example.com"]
allow_tailscale = true
allow_localhost = true

[auth.google]
client_id = "..."
client_secret_env = "GOOGLE_OAUTH_CLIENT_SECRET"
allowed_domains = []

[auth.github]
client_id = "..."
client_secret_env = "GITHUB_OAUTH_CLIENT_SECRET"
allowed_users = ["maruel"]

[auth.gitlab]
client_id = "..."
client_secret_env = "GITLAB_OAUTH_CLIENT_SECRET"
base_url = "https://gitlab.com"
allowed_users = ["maruel"]

[[trusted_issuers]]
service = "caic"
issuer = "https://caic.example.com"
jwks_url = "https://caic.example.com/api/v1/voice/jwks"

[[services]]
id = "home-caic"
kind = "caic"
base_url = "https://caic.example.com"
capabilities = ["tasks", "notifications", "halo"]

[[services]]
id = "home-mddb"
kind = "mddb"
base_url = "https://mddb.example.com"
capabilities = ["documents", "tables"]
```

Use a dedicated XDG config directory such as
`~/.config/voice-gateway/config.toml`. Do not reuse `~/.config/caic`.

Recommended caic reference to an external gateway:

```toml
[voice-gateway]
mode = "external"
url = "https://voice.example.com"
```

Optional caic-embedded gateway config:

```toml
[voice-gateway]
mode = "embedded"

[voice-gateway.config.server]
http = ":3479"
webrtc_udp_port = 3478
external_url = "https://voice.example.com"

[voice-gateway.config.gemini]
api_key_env = "GEMINI_API_KEY"
model = "gemini-live-2.5-flash-preview"

[voice-gateway.config.auth]
session_secret_file = "voice-gateway-session-secret"
allowed_users = ["marc@example.com"]
allow_tailscale = true

[[voice-gateway.config.services]]
id = "home-caic"
kind = "caic"
base_url = "https://caic.example.com"
capabilities = ["tasks", "notifications", "halo"]
```

The embedded shape should stay isomorphic to the standalone gateway config, but
standalone remains the source of truth for the gateway product boundary.

Mode meanings:

- `external`: caic advertises a preferred gateway URL. Android connects to that
  gateway directly and uses caic only for service metadata, web UI, scoped token
  issuance, and caic tool bindings.
- `embedded`: caic registers the reusable voice-gateway protocol handlers
  in-process and advertises itself as the gateway URL. The implementation must
  still use the same gateway config, auth, compatibility, and signaling protocol
  as the standalone binary.
- `disabled`: caic advertises no voice gateway support.

## Voice Gateway Authentication

The voice gateway has its own user authentication. It must not rely on caic,
mddb, or any other service backend user store.

Gateway auth owns:

- user login session
- gateway session secret
- OAuth provider configuration
- allowed-user policy
- Tailscale/private-network access policy
- scoped service-token exchange with selected service backends

OAuth providers:

- Google
- GitHub
- GitLab, including self-hosted GitLab through configurable `base_url`

OAuth rules:

- OAuth redirects terminate at the voice gateway, not at caic or mddb.
- Provider identity is normalized into a gateway user identity.
- Allowed-user checks happen in the gateway before issuing a gateway session.
- A gateway session is not a caic or mddb session.
- Gateway OAuth tokens are not forwarded to service backends.
- Service access still uses backend-issued scoped service tokens.

Tailscale/private-network mode:

- The gateway must be usable when it is reachable only from a tailnet and is not
  internet accessible.
- OAuth can still work if the user's browser and Android device can reach the
  gateway's callback URL over Tailscale, because OAuth redirects are browser
  navigations. The provider does not need inbound network access to the gateway.
- For this mode, configure `external_url` to a stable HTTPS tailnet URL when
  possible.
- If a stable HTTPS callback URL is unavailable, support a local pairing mode:
  the gateway displays a short-lived one-time code or local approval URL, and
  Android exchanges it for a gateway session over the tailnet.
- Pairing mode is only for private-network deployments and must be explicitly
  enabled.
- Do not require Tailscale Funnel or public internet exposure for normal private
  tailnet usage.

Auth implementation notes:

- Reuse generic OAuth/session helpers where practical, but do not reuse caic's
  config directory, `users.json`, or `settings.json`.
- Add Google OAuth support to the shared auth package or gateway auth package.
- Keep GitHub and GitLab provider behavior compatible with caic where possible.
- Treat provider device-code flows as optional future work; verify provider
  support before depending on them.
- Store only the minimum needed gateway identity/session data.
- Gateway logout revokes the gateway session and invalidates active voice
  sessions owned by that user.

## Protocol And Versioning

The Android shell must not assume that every configured server supports the same
frontend, API, or native binding version.

Add a small shell compatibility endpoint to caic before relying on native
bindings. The existing `Config.webrtcAvailable` boolean should be replaced or
augmented with structured voice gateway metadata. The exact path can change, but
the shape should be stable:

```json
{
  "service": "caic",
  "serviceVersion": "0.0.0",
  "apiVersion": 1,
  "webShell": {
    "minAndroidShell": 1,
    "maxAndroidShell": 1,
    "bridgeVersion": 1,
    "voiceGateway": {
      "required": true,
      "minGatewayProtocol": 1,
      "mode": "external",
      "url": "https://voice.example.com",
      "serviceToken": {
        "required": true,
        "endpoint": "/api/v1/voice/token",
        "audience": "voice-gateway"
      },
      "capabilities": [
        "voice.gatewayGeminiLive",
        "voice.gatewayTools",
        "voice.scopedTokens"
      ]
    },
    "capabilities": ["notifications.native", "screenshot.native", "halo.native"]
  }
}
```

Versioning rules:

- `Config.webrtcAvailable` must not remain the only voice signal exposed to
  Android or the web frontend. At minimum, the API must expose gateway mode,
  gateway URL, minimum gateway protocol, auth requirements, token exchange
  endpoint, and capabilities.
- Gateway mode is one of `embedded`, `external`, or `disabled`.
- For `embedded`, `url` points at the caic server origin and caic serves the
  voice-gateway protocol handlers in-process.
- For `external`, `url` points at the preferred standalone gateway. Android
  connects to that gateway directly. caic does not depend on HTTP redirects for
  signaling.
- For `disabled`, Android disables voice without probing legacy WebRTC routes.
- Android reads compatibility metadata before enabling bridge commands.
- The web frontend also receives shell version and capabilities through
  `window.caicAndroid`.
- Unsupported versions must fail visibly with an upgrade message instead of
  partial behavior.
- Server upgrade prompts should be driven by server metadata, not hardcoded app
  guesses.
- Bridge messages carry a version and must be backward-compatible within a
  declared range.
- Feature checks use capabilities, not only version numbers.
- Voice capabilities distinguish local Android audio endpoint support, service
  backend binding support, and voice gateway Gemini/tool support.
- Legacy clients may continue to read `webrtcAvailable` during migration, but
  new clients must use structured gateway metadata.
- For a generic host, the same mechanism should support `service: "mddb"` or
  other service identifiers.

The voice gateway also needs its own compatibility endpoint:

```json
{
  "service": "voice-gateway",
  "gatewayProtocol": 1,
  "serviceKinds": ["caic", "mddb"],
  "capabilities": [
    "auth.oauth.google",
    "auth.oauth.github",
    "auth.oauth.gitlab",
    "auth.pairing.privateNetwork",
    "voice.gatewayGeminiLive",
    "voice.gatewayTools",
    "voice.scopedTokens"
  ]
}
```

## Confirm Before Coding

Before implementation, a coding agent must confirm these hypotheses against the
current code and report any mismatch.

- Hypothesis: `TaskListViewModel` still starts `TaskRepository` and
  `TaskNotifier`; app-scoped monitoring is still required.
- Hypothesis: `TaskRepository.start()` is not idempotent; repeated starts can
  create duplicate SSE collectors.
- Hypothesis: notification intents still lack task routing extras.
- Hypothesis: frontend API and EventSource calls still use relative `/api/...`
  paths.
- Hypothesis: browser voice and browser notifications are still active in normal
  frontend mode and need Android host-mode suppression.
- Hypothesis: authentication is primarily backend-owned through the web UI, and
  Android only needs native auth state for capabilities that cannot use the
  WebView session.
- Hypothesis: voice gateway can own Gemini setup, function declarations,
  tool-call dispatch, and tool responses, leaving Android as a local voice
  endpoint and service backends as binding owners.
- Hypothesis: `backend/cmd/webrtc-relay` can be renamed to
  `backend/cmd/voice-gateway` before it grows, and `internal/server/voicertc`
  can remain the lower-level transport package until it needs a broader name.
- Hypothesis: the voice gateway can use its own config directory and config
  format without reading caic private config files.
- Hypothesis: the voice gateway static config can live in a shared Go package
  without importing caic server packages.
- Hypothesis: caic can embed the same static gateway config under
  `[voice-gateway.config]` for embedded colocated deployments while keeping
  `~/.config/voice-gateway/config.toml` canonical for standalone deployments.
- Hypothesis: caic can tolerate gateway config version skew, unknown fields, or
  unsupported gateway features without making unrelated caic startup fail.
- Hypothesis: `Config.webrtcAvailable` is still a boolean in the generated API
  and can be migrated to structured gateway metadata without breaking existing
  web and Android clients during a compatibility window.
- Hypothesis: caic can advertise `embedded`, `external`, and `disabled` gateway
  modes through `GET /api/v1/server/config` or a new compatibility endpoint
  before Android depends on voice-gateway routing.
- Hypothesis: gateway-owned OAuth can support Google, GitHub, and GitLab without
  coupling to caic's existing user store.
- Hypothesis: Tailscale-only gateway deployments can support OAuth when the
  callback URL is reachable by the user's browser and Android device over the
  tailnet.
- Hypothesis: local pairing mode is sufficient for private-network deployments
  that do not have a stable HTTPS callback URL.
- Hypothesis: caic and mddb can issue scoped voice-gateway tokens or expose a
  token exchange so the gateway does not need direct access to service session
  stores.
- Hypothesis: service-specific voice tools can be represented as backend-owned
  binding APIs that the gateway can call with scoped credentials.
- Hypothesis: Android voice `FunctionHandlers`, local voice function
  declarations, and local Gemini setup can be removed or bypassed after backend
  voice tools are available.
- Hypothesis: Android can still expose enough state to the gateway for voice
  context: active service instance, current task route, selected voice name,
  audio state, and Halo context.
- Hypothesis: the fake backend and Android e2e harness can run a shell-mode
  smoke test with `adb reverse`.
- Hypothesis: caic can expose shell compatibility metadata before Android needs
  to enable bridge commands.
- Hypothesis: the settings model can distinguish Android app settings from
  backend-owned server settings without duplicating configuration.
- Hypothesis: caic only needs gateway connection and service-token policy for
  external gateway deployments; full gateway config is needed only when caic
  explicitly hosts the gateway protocol in-process.
- Hypothesis: the first caic spike can be implemented without preventing a
  future generic service host.

If any hypothesis is false, update this plan before coding.

## Baseline

Read before editing:

- `/home/user/src/AGENTS.md`
- `AGENTS.md`
- `android/AGENTS.md`
- `frontend/AGENTS.md`
- `sdk/halo/AGENTS.md` if Halo code is touched

Load skills before coding:

- `code-quality`
- `android-code-quality` for Kotlin or Android changes
- `typescript-code-quality` for frontend TypeScript changes
- `frontend-design` only if changing visible frontend UI

Run baseline checks before functional changes:

```bash
make lint-frontend
make lint-android
make android-test
make android-build
```

If a baseline check already fails, report it before changing behavior. Do not
hide pre-existing failures behind Web shell work.

## Execution Order

Execute in this order unless a phase explicitly proves the direction is wrong:

1. Establish the voice gateway boundary.
2. Prove backend-hosted WebView shell loading.
3. Prove auth, scoped voice-gateway token exchange, and version negotiation.
4. Move task monitoring and notifications out of screen ViewModels.
5. Add Android host mode to the frontend.
6. Add the narrow native-web bridge.
7. Move voice tool execution into the voice gateway and service bindings.
8. Wire Android voice endpoint to the voice gateway.
9. Add notification deep links, screenshot bridge, and Halo context.
10. Retire duplicated Compose screen-mode UI only after all gates pass.

## Phase 1: Voice Gateway Foundation

Rename and reshape the partial relay before building more Android voice
behavior around it.

Requirements:

- Rename `backend/cmd/webrtc-relay` to `backend/cmd/voice-gateway`.
- Keep `internal/server/voicertc` as the lower-level WebRTC transport package
  until it grows beyond transport concerns.
- Change default config from `~/.config/caic` to
  `~/.config/voice-gateway/config.toml`.
- Remove caic `settings.json` and `users.json` reads from the gateway.
- Add gateway config parsing and validation in a package that can be reused by
  both the standalone gateway binary and caic's optional embedded gateway mode.
- Add caic `[voice-gateway]` config with `external`, `embedded`, and `disabled`
  modes. `external` stores only the gateway URL and service-side policy;
  `embedded` embeds the same static gateway config shape as standalone.
- Replace or augment `Config.webrtcAvailable` with structured gateway metadata
  that reports mode, gateway URL, minimum gateway protocol, auth requirements,
  token endpoint, and capabilities.
- Add gateway-owned session storage and session middleware.
- Add OAuth provider configuration for Google, GitHub, and GitLab.
- Add Tailscale/private-network mode settings.
- Add `GET /health` and `GET /compat` endpoints.
- Keep `POST /offer` and `POST /sessions/{sessionID}` as transport endpoints,
  but version their request and response payloads before Android depends on
  them.
- Add service identity to session creation: service kind, service instance ID,
  service base URL, and scoped service token.
- Add a caic service adapter interface, even if its first implementation only
  calls existing caic APIs.
- Design the mddb adapter interface but do not implement product behavior until
  mddb exposes the required voice binding surface.

Auth requirements:

- Gateway has its own user auth and session cookies.
- Gateway supports Google, GitHub, and GitLab OAuth.
- Gateway supports self-hosted GitLab through configurable base URL.
- Gateway supports Tailscale/private-network deployments where the gateway is
  not internet accessible.
- Gateway supports explicit local pairing mode for private-network deployments
  without a stable OAuth callback URL.
- Gateway accepts only scoped tokens issued by trusted service backends or a
  configured development token.
- Gateway does not validate service UI cookies directly.
- Gateway does not open service private files.
- Tokens include service kind, service instance ID, backend origin, user
  identity or subject, allowed capabilities, expiry, and audience.
- Token verification supports key rotation through JWKS or equivalent
  issuer-published metadata.

Acceptance checks:

- The binary name and logs say `voice-gateway`.
- Running the gateway does not require a caic config directory.
- The gateway can start with no configured caic server and still report health
  and compatibility.
- Standalone gateway config and embedded caic gateway config validate
  through the same rules.
- caic can reference an external gateway without embedding gateway runtime
  settings.
- caic API clients can distinguish `embedded`, `external`, and `disabled` voice
  gateway modes without relying only on `webrtcAvailable`.
- Gateway login works with configured Google, GitHub, and GitLab providers.
- Gateway login works over a Tailscale-only URL when the client can reach that
  URL.
- Pairing mode can create a gateway session without public inbound access.
- Invalid or expired scoped tokens are rejected.
- A configured caic service can create a voice session without Android executing
  tool calls locally.
- Tests cover config loading, compatibility response, token validation, and
  service selection.

## Phase 2: Reversible Remote WebView Spike

Goal: prove that Android can host the existing backend-served mobile frontend
without deleting Compose screen mode.

Add a reversible shell mode:

- Prefer a local developer setting or build flag that defaults to existing
  Compose screen mode.
- Route shell mode through `CaicNavGraph` or a new top-level host.
- Keep existing Compose screens, ViewModels, and tests compiling.
- Keep the native voice panel available as a Compose overlay in the first spike.

Create:

```text
android/app/src/main/java/com/fghbuild/caic/ui/web/WebShellScreen.kt
```

Initial WebView behavior:

- Read the active server URL from `SettingsRepository`.
- If no server is configured, show native settings.
- Load the configured backend URL, not bundled APK assets.
- Enable JavaScript and DOM storage.
- Use a `WebViewClient` that keeps app navigation inside the WebView.
- Use a `WebChromeClient` for permission prompts and file chooser support where
  needed by microphone, camera, or image attachment workflows.
- Handle Android back by navigating WebView history first, then falling back to
  normal activity back handling.
- Show native recovery UI for unconfigured server, page load failure, and auth
  redirect failure.
- Provide a route for opening native settings even when the web page is broken.

Acceptance checks:

- A configured no-auth fake backend loads the web UI inside Android.
- Task list SSE updates appear.
- Task detail pages load.
- Creating a task works.
- Sending task input works.
- Android back navigates predictably.
- The user can still reach native settings if WebView cannot load.
- Default production behavior remains the existing Compose path.

## Phase 3: Auth Spike and Decision

Auth is the highest-risk migration point. Test it before building a broad bridge.
There are two independent auth domains: the active service backend web session
and the voice gateway user session.

Strategy A: backend-owned WebView session

- WebView uses normal web OAuth and session cookies.
- The backend web UI owns login, logout, account selection, and auth refresh.
- Native Android avoids owning credentials unless a native capability proves it
  needs direct API access.
- Prefer this strategy if WebView login, fetch, and EventSource work without
  request interception.

Strategy B: native token handoff for specific capabilities

- The backend web session grants Android a scoped native token only for native
  capabilities that cannot operate through browser session state.
- Examples may include WebRTC voice signaling, native task monitoring, and
  native notifications.
- This is higher risk because browser `EventSource` does not support custom
  headers and broad request interception can become fragile.

Strategy C: gateway-owned user session

- The voice gateway owns its own user login and session cookie.
- Supported providers are Google, GitHub, and GitLab.
- Self-hosted GitLab is supported through configurable provider base URL.
- Android authenticates to the gateway before creating or resuming a voice
  session.
- The gateway session authorizes access to gateway resources only. It does not
  grant caic or mddb product access.
- Tailscale-only deployments can use normal OAuth when the browser and Android
  can reach the gateway callback URL over the tailnet.
- Tailscale-only deployments without a stable HTTPS callback URL use explicit
  local pairing mode to create the gateway session.

Strategy D: service-issued voice gateway token

- The backend web session grants Android a short-lived token for the configured
  voice gateway.
- Android sends that token only to the voice gateway when starting or updating a
  voice session.
- The token lets the gateway call the selected service backend's voice binding
  API for the authenticated user.
- Prefer this strategy for voice. It keeps web login backend-owned while avoiding
  Android-owned service credentials and avoiding gateway access to service
  private files.

Decision rule:

- Proceed with Strategy A if OAuth, logout, server switch, fetch, and SSE are
  coherent enough.
- Add Strategy B only for narrow native capabilities that cannot use the web
  session.
- Use Strategy C for gateway login and gateway session state.
- Use Strategy D for service access inside gateway-dispatched voice tools.
- Stop the shell migration if normal web API calls require broad WebView request
  interception, a custom API proxy layer, or Android-specific forks.

Acceptance checks:

- No-auth server works.
- Auth-enabled server works.
- OAuth login works in WebView.
- Native capabilities either work through backend-owned session state or receive
  scoped token handoff through an explicit backend flow.
- Gateway login works with Google, GitHub, and GitLab.
- Gateway login works in Tailscale-only mode through either OAuth callback or
  explicit pairing.
- Gateway logout invalidates active voice sessions for that gateway user.
- Voice sessions use a service-issued scoped token accepted by the voice gateway.
- A valid gateway session without a valid service token cannot execute service
  tool calls.
- Logout revokes or invalidates native capability tokens.
- Server switch changes WebView origin, native monitoring credentials, and
  native capability state coherently.

## Phase 3.5: Compatibility Handshake

Add protocol negotiation before implementing broad native bindings.

Requirements:

- caic exposes a public or minimally-authenticated compatibility endpoint.
- voice-gateway exposes a public compatibility endpoint.
- Android checks service ID, API version, bridge version range, and capabilities.
- Android checks voice-gateway protocol version and capabilities before enabling
  voice.
- Android stores the last-known compatibility result per configured server.
- Android stores the last-known compatibility result for the configured gateway.
- WebView host mode exposes Android shell version and native capabilities to the
  frontend.
- Incompatible servers show a clear native recovery screen with upgrade guidance.
- Incompatible voice gateways show a clear native recovery screen with gateway
  upgrade guidance.

Acceptance checks:

- Supported server enables shell mode.
- Supported voice gateway enables voice.
- Missing compatibility endpoint degrades visibly.
- Too-old server asks for server upgrade.
- Too-new server asks for Android app upgrade.
- Too-old voice gateway asks for gateway upgrade.
- Too-new voice gateway asks for Android app upgrade.
- Capability absence disables only the affected native feature.

## Phase 4: App-Scoped Task Monitoring

Move task monitoring out of screen-mode ViewModels.

Implement a dedicated app-scoped starter, for example:

```text
android/app/src/main/java/com/fghbuild/caic/data/TaskMonitor.kt
```

Requirements:

- Start `TaskRepository` from an application or activity-retained scope.
- Start `TaskNotifier` from the same long-lived scope.
- Make startup idempotent. Repeated calls must not create duplicate SSE
  collectors, duplicate notification collectors, or duplicated Halo observers.
- Restart monitoring when active server URL or auth token changes.
- Clear task and usage state when no server is configured.
- Keep notification suppression tied to `VoiceSession.state.connected`.
- Do not depend on `TaskListViewModel`.

Implementation note:

- Prefer making `TaskRepository.start()` and `TaskNotifier.start()` idempotent at
  their own boundary. A separate starter cannot be the only protection against
  duplicate starts.

Acceptance checks:

- Android task notifications work with only WebView screen mode mounted.
- Notifications auto-dismiss when a task leaves an attention state.
- Server switch and re-auth restart monitoring.
- Closing and reopening shell mode does not create duplicate SSE collectors.
- Existing Compose task list still works while the migration is reversible.

## Phase 5: Android Host Mode in the Frontend

Add a narrow frontend host-mode abstraction.

Recommended files:

```text
frontend/src/androidHost.ts
frontend/src/androidHost.test.ts
frontend/src/global.d.ts
```

Native should expose one marker before the app code runs:

```ts
window.caicAndroid = {
  version: 1,
  hostMode: true,
};
```

In Android host mode:

- Do not render or auto-start browser `VoiceOverlay`.
- Do not instantiate browser `VoiceSession`.
- Do not request browser notification permission.
- Do not show browser notifications.
- Prefer native screenshot integration when it is available.
- Keep normal browser and PWA behavior unchanged outside Android host mode.

Frontend contract rules:

- Add typed global declarations. Avoid `any`.
- Keep Android host behavior behind a small module.
- Do not scatter `window.caicAndroid` checks throughout feature code.
- Unit-test browser behavior and Android host behavior separately.

Acceptance checks:

- Browser/PWA voice behavior is unchanged.
- Android WebView does not start duplicate browser voice.
- Android WebView does not emit browser notifications in addition to Android
  notifications.
- Frontend tests cover host-mode detection and suppression.

## Phase 6: Native-Web Bridge

Keep the bridge small, versioned, and capability-oriented. Normal caic API calls
continue through the generated TypeScript SDK.

Use one JavaScript object:

```ts
interface CaicAndroidBridge {
  version: 1;
  hostMode: true;
  service: "caic";
  capabilities: string[];
  postMessage(messageJson: string): void;
}
```

All messages should be JSON with explicit version and type:

```json
{
  "version": 1,
  "type": "voice.connect",
  "payload": {}
}
```

Native-to-web events:

- `voice.state`: native voice state needed by web UI.
- `voice.taskNumbers`: task ID to voice task number mapping.
- `native.notificationPermission`: Android notification permission state.
- `native.navigate`: route navigation requested by Android.
- `screenshot.captured`: captured image data, if screenshot bridge is enabled.
- `native.error`: visible native capability failure.

Web-to-native commands:

- `voice.connect`
- `voice.disconnect`
- `voice.toggleMute`
- `voice.selectAudioDevice`
- `voice.clearTranscript`
- `native.openSettings`
- `native.requestNotificationPermission`
- `native.requestScreenshot`
- `native.setCurrentTask`
- `native.setVoiceContext`

Bridge rules:

- Treat incoming JSON as untrusted input.
- Validate message version, type, and payload shape on the native side.
- Fail visibly in logs and, where appropriate, through `native.error`.
- Keep bridge methods main-thread safe.
- Do not expose arbitrary URL fetch, file access, shell access, or unrestricted
  navigation.
- Do not use the bridge as a broad state synchronization layer.
- Emit current native state after WebView page load so reloads reconstruct UI
  state without reconnecting voice.
- Keep compatibility versioned so future web builds can detect unsupported
  Android shells.
- Keep service identity explicit. A future generic host must not let a caic
  frontend call bindings intended for another service.

Acceptance checks:

- Web can request native settings, notification permission, voice commands, and
  screenshot capture where implemented.
- Native can send voice state and task number state to web.
- Native can navigate the WebView to a task route.
- Invalid bridge calls are rejected and observable.
- Page reload reconstructs native state without starting duplicate sessions.

## Phase 7: Voice Gateway Integration

Android voice remains native at the platform boundary, but Gemini Live
orchestration is owned by the voice gateway.

Android continues to own:

- the local WebRTC client endpoint
- `VoiceService`
- microphone permission flow
- audio focus
- input and output device selection
- WebRTC signaling
- foreground service lifetime
- mute and disconnect controls
- transcript and status presentation
- Halo/BLE context capture when Halo-backed awareness is enabled

The voice gateway owns:

- Gemini Live WebSocket connection
- Gemini setup message
- system instruction
- voice function declarations
- tool-call dispatch
- tool responses
- service adapter dispatch
- task or workspace state notifications injected into Gemini

The active service backend owns:

- user auth and session state
- scoped voice-gateway token issuance
- service-specific tool binding API
- product-side authorization for every voice action

Use Option A first: keep the native Compose voice panel above or below the
WebView.

Reason:

- It is the lowest-risk proof.
- Audio device controls and permission flow remain unchanged.
- The first bridge only needs state, navigation, and context updates, not every
  voice control.

Move to Option B later only if the shell is accepted:

- Web renders voice controls.
- Commands and state flow through the bridge.
- Android still owns local audio endpoint controls.
- Voice gateway still owns Gemini Live orchestration.
- Service backends still own binding authorization and product-side effects.

Migration requirements:

- Extend the voice RTC offer/session protocol so Android sends local session
  configuration to the gateway, such as selected voice name, active service
  instance, current task route, and supported native context sources.
- Move or duplicate Android `FunctionHandlers` behavior into caic voice binding
  handlers callable by the gateway, then remove local Android voice tool
  execution.
- Move Gemini setup construction and function declarations from Android/web
  clients to the gateway.
- Keep task number mapping either backend-owned or explicitly synchronized to
  Android/web as presentation state. Do not let Android-only numbering become
  required for service binding correctness.
- Treat Halo/BLE observations as native context sent to the gateway, not as
  Android-executed Gemini tools.

Navigation behavior:

- When WebView route changes, call `native.setCurrentTask` or
  `native.setVoiceContext` so Android can send context to the gateway voice
  session.
- When gateway voice chooses a task, the gateway sends a navigation event through
  the Android voice/session channel or an app bridge event, then Android
  dispatches `native.navigate` to the WebView.

Acceptance checks:

- Native connect, disconnect, mute, audio-device selection, and transcript
  display keep working.
- Voice task numbers are visible or otherwise clearly communicated in web task
  cards.
- Voice-created tasks appear in the WebView task list.
- Voice answer, purge, status lookup, CI notification, and task creation flows
  work through gateway-dispatched service bindings.
- Android does not execute Gemini function calls locally.
- Android task notifications remain suppressed while voice is connected.
- Browser voice code is inactive in Android host mode.

## Phase 8: Notification Deep Links

Update notifications to carry enough routing information for WebView navigation.

Requirements:

- Notification intents include the task ID or canonical web route.
- `MainActivity.onNewIntent()` extracts pending task navigation in addition to
  OAuth callbacks.
- If WebView is ready, navigate immediately.
- If WebView is loading, store pending navigation and replay it after page load.
- If no server is configured, open native settings instead of dropping the
  intent.
- Use `FLAG_ACTIVITY_SINGLE_TOP` or equivalent launch behavior to avoid
  duplicate app instances.

Acceptance checks:

- Tapping a task notification opens the matching task detail.
- Tapping a notification from cold start opens the matching task detail after
  WebView load.
- Tapping while the app is already open navigates the existing WebView.
- Tapping a stale notification for a missing task fails gracefully.
- Notification dismissal behavior remains unchanged.

## Phase 9: Screenshot Bridge

Keep native MediaProjection screenshot capture unless the web attach path proves
equivalent on Android.

Requirements:

- `native.requestScreenshot` starts the existing native screenshot flow.
- Countdown and foreground service behavior remain intact.
- Captured images are converted to the frontend image payload shape.
- The result is delivered through a native-to-web event.
- The frontend inserts the image into the current prompt or task input draft.
- Cancellation and MediaProjection failure are visible to the user.

Acceptance checks:

- Screenshot capture works from WebView UI.
- Captured image attaches to a task prompt or input.
- Cancellation and failure do not silently disappear.

## Phase 10: Decide Remaining Native Features

Before deleting duplicated Compose screens, explicitly decide what happens to
Android-only features:

- Native server settings
- Native login screen
- Native image attachment flows
- Native screenshot capture
- Halo BLE support
- Multi-server switcher and per-server diagnostics
- Generic service host extraction

Recommended defaults:

- Keep native server settings permanently as bootstrap and recovery UI.
- Keep multi-server configuration native.
- Keep native voice endpoint panel through the first accepted shell release.
- Keep screenshot capture if web image attach is worse on Android.
- Keep Halo native.
- Keep backend-owned settings in the backend web UI.
- Remove native task list/detail/diff/process/widget UI only after shell
  acceptance.
- Defer a second app until the caic shell proves the shared host boundaries.

## Phase 11: Retire Duplicated Compose Screen Mode

Only start after all earlier acceptance checks pass.

Remove or quarantine native implementations replaced by the web frontend:

- task list
- task detail
- diff view
- process view
- widget rendering
- task input UI
- duplicated grouping and formatting code, if Android voice endpoint code no
  longer needs it

Keep:

- native bootstrap/settings
- generated Kotlin SDK
- `TaskRepository`
- `TaskNotifier`
- Android voice endpoint package
- screenshot service if retained
- Halo service if retained
- Android tests for native integrations

Requirements:

- Convert coverage instead of deleting it blindly.
- Android e2e should exercise WebView-hosted task creation, task detail,
  notifications, voice startup, and screenshot capture where practical.
- Update `android/AGENTS.md` and generated file indexes if the architecture
  description changes.
- Do not remove Compose entirely if native settings or native voice endpoint
  overlay still use it.

Acceptance checks:

- Removed Compose screens are no longer reachable.
- Native services still start and stop correctly.
- Android e2e covers the accepted shell workflows.
- Lint, unit tests, build, and e2e pass.

## No Bundled Frontend

Do not pursue bundled frontend assets for this app.

Rationale:

- The app has no useful offline mode without a backend server.
- Backend-hosted frontend keeps web UI, generated API bindings, auth behavior,
  and server version aligned.
- Bundling creates extra API-origin, EventSource, service-worker, cache
  invalidation, and auth problems without solving a product requirement.

If startup latency becomes a problem, optimize server startup, WebView cache
policy, and network diagnostics rather than packaging stale frontend assets.

## Testing Plan

Voice gateway unit tests:

- Config loading and validation for `~/.config/voice-gateway/config.toml`.
- Gateway session creation, validation, logout, and active-session invalidation.
- OAuth provider config validation for Google, GitHub, GitLab, and self-hosted
  GitLab.
- OAuth callback state validation and allowed-user enforcement.
- Tailscale/private-network mode does not require a public `external_url` when
  pairing mode is enabled.
- Pairing code creation, expiry, one-time use, and exchange.
- Compatibility endpoint reports gateway protocol, supported service kinds, and
  gateway capabilities.
- Scoped token validation rejects wrong issuer, wrong audience, expired tokens,
  unsupported service kind, and unsupported service instance.
- Voice RTC session setup builds the Gemini setup message in the gateway.
- Service adapter selection uses explicit service kind and service instance ID.
- Tool errors are returned to Gemini as tool responses and surfaced to the
  client transcript/status channel.
- Voice session context updates from Android are applied without trusting
  arbitrary client-supplied task IDs or URLs.

Service backend unit tests:

- caic config API exposes structured gateway metadata for `embedded`,
  `external`, and `disabled` modes.
- caic keeps `Config.webrtcAvailable` only as a legacy compatibility field if it
  remains during migration.
- caic compatibility endpoint reports voice gateway requirements and binding
  capabilities.
- caic scoped voice-gateway token issuance is tied to the current authenticated
  web session and can be revoked by logout.
- caic voice binding handlers cover task create, answer, send message, stop,
  purge, revive, fork, status/detail, usage, web fetch/search, and CI helpers.
- Binding handlers authorize each action server-side and do not trust gateway
  claims beyond the scoped token.

Android unit tests:

- Bridge message parsing and validation.
- Idempotent task monitor startup.
- Notification route construction.
- Pending navigation before WebView load.
- Server switch and auth-token change restart behavior.
- Voice endpoint state machine for connect, mute, disconnect, route context
  updates, and backend session failure.

Android instrumented tests:

- Shell mode loads the task list from fake backend.
- Creating or selecting a task works.
- Notification tap opens the expected task route.
- Back button navigates WebView history.
- Android voice endpoint can connect or bridge commands can be invoked without
  crashing, depending on the implemented phase.
- Gateway-dispatched service bindings execute voice commands without Android
  `FunctionHandlers`.

Frontend tests:

- Android host detection.
- Browser voice path remains unchanged without Android bridge.
- Android host mode does not instantiate browser `VoiceSession`.
- Browser notifications are suppressed in Android host mode.
- Android voice endpoint state updates task number badges if web badges are
  implemented.
- Browser/web voice clients stop executing caic voice tools locally once gateway
  voice tools and service bindings are available.

Manual matrix before Compose retirement:

- Fresh install, no server configured.
- Server configured, no auth.
- Server configured, OAuth enabled.
- Voice gateway configured with Google OAuth.
- Voice gateway configured with GitHub OAuth.
- Voice gateway configured with GitLab OAuth.
- Voice gateway reachable only over Tailscale with OAuth callback URL.
- Voice gateway reachable only over Tailscale with pairing mode.
- caic configured with embedded voice gateway mode.
- caic configured with external preferred voice gateway mode.
- caic configured with disabled voice gateway mode.
- Server switch.
- Logout.
- Server upgrade with older Android app.
- Android app upgrade with older server.
- App restart after voice endpoint connected.
- WebView reload while voice endpoint connected.
- Notification tap from background.
- Notification tap while foregrounded.
- Android back navigation from task detail.
- Camera/image attach.
- Screenshot attach.
- Widget rendering.
- VNC route.
- Halo build or behavior if Halo was touched.
- Multi-server notification routing.
- Gateway-dispatched voice tool execution for task create, answer, purge, status
  lookup, and CI notification flows.
- mddb or another service host proof if the generic app option is active.

## Validation Commands

After Android changes:

```bash
make lint-android
make android-test
make android-build
```

After voice gateway or service backend changes:

```bash
make lint-go
make test
```

After frontend changes:

```bash
make lint-frontend
```

After shell navigation or integration changes:

```bash
make frontend-e2e
make android-e2e
```

After documentation or file-index changes:

```bash
python3 scripts/update_agents_file_index.py
make lint-docs
```

## Go/No-Go Criteria

Proceed with the migration only if all are true:

- WebView loads the mobile web UI reliably from the configured server.
- Auth works without broad request interception or a custom API proxy.
- Compatibility negotiation prevents unsupported bridge/API combinations.
- caic config metadata replaces the `webrtcAvailable` boolean decision with
  structured embedded, external, and disabled gateway modes.
- Android can multiplex multiple configured caic backend servers.
- Native task notifications work independently of WebView lifecycle.
- Android voice endpoint works and can expose enough state for the shell.
- Voice gateway auth works with configured Google, GitHub, and GitLab providers.
- Voice gateway auth works for Tailscale-only deployments without requiring
  public inbound access.
- Voice gateway owns Gemini setup and tool dispatch.
- Service backends own scoped token issuance and tool binding implementation.
- Halo remains compatible with the shell architecture.
- Browser and Android voice endpoints do not duplicate each other.
- Browser and Android notifications do not duplicate each other.
- The shell removes more duplicated screen code than it adds in bridge and
  lifecycle complexity.

Stop and keep native Android screen mode if any are true:

- Auth requires fragile WebView request interception.
- EventSource cannot authenticate cleanly.
- Version compatibility cannot be represented cleanly between app and server.
- `webrtcAvailable` remains the only voice capability signal available to
  Android.
- Voice gateway compatibility cannot be represented cleanly between Android,
  gateway, and service backends.
- Voice gateway auth requires public inbound internet access for Tailscale-only
  deployments.
- Gateway user auth and service backend auth cannot be separated cleanly.
- Multi-server state makes native monitoring or notifications ambiguous.
- Voice state synchronization requires a broad, hard-to-test bridge.
- Android must keep executing caic voice tools locally after gateway tools and
  service bindings are available.
- WebView routing or reload behavior is unreliable.
- Android-only features become worse than the current Compose implementation.

If the migration stops, narrow native Android to voice-endpoint workflows and
stop chasing full web parity for secondary screen-mode features.

## Recommended PR Sequence

Keep each PR small enough to revert.

PR 1: voice gateway identity and configuration.

- Rename `backend/cmd/webrtc-relay` to `backend/cmd/voice-gateway`.
- Rename logs, help text, environment variable names, docs, and build targets
  that refer to the standalone voice binary.
- Keep `internal/server/voicertc` as the lower-level transport package.
- Add `~/.config/voice-gateway/config.toml` loading and validation.
- Add reusable gateway static config types and validation that do not import
  caic server packages.
- Add caic `[voice-gateway]` parsing for `external`, `embedded`, and `disabled`
  modes.
- Keep standalone config canonical; embedded caic config embeds the same static
  config only for colocated deployments.
- Update caic config API metadata so clients can distinguish embedded gateway,
  external preferred gateway, and disabled voice. Keep `webrtcAvailable` only as
  a temporary legacy compatibility field if needed.
- Remove caic `settings.json` and `users.json` reads from the gateway.
- Add gateway `GET /health` and `GET /compat`.
- Add tests for standalone config defaults, embedded caic gateway config,
  external gateway config, config API metadata, config validation, and
  compatibility response.

PR 2: gateway user auth.

- Add gateway-owned session storage, cookies, middleware, logout, and active
  voice-session invalidation.
- Add OAuth routes for Google, GitHub, and GitLab.
- Add self-hosted GitLab support through configurable provider base URL.
- Add allowed-user and allowed-domain enforcement.
- Add Tailscale/private-network mode checks.
- Add explicit local pairing mode for private tailnet deployments without a
  stable OAuth callback URL.
- Add tests for OAuth state validation, provider config validation, allowed-user
  enforcement, session invalidation, and pairing code expiry/one-time use.

PR 3: scoped service auth and caic binding skeleton.

- Add trusted issuer configuration to the gateway.
- Add scoped-token validation skeleton with audience, expiry, issuer, service
  kind, service instance ID, and capability checks.
- Add caic compatibility metadata describing voice gateway requirements.
- Add a caic endpoint that can issue a short-lived voice-gateway token from the
  authenticated web session.
- Add a caic voice binding API skeleton for service tool calls.
- Add tests for token issuance, token rejection, logout/revocation behavior, and
  binding authorization.

PR 4: gateway-owned Gemini setup and caic voice tools.

- Move Gemini setup construction and function declarations out of Android/web
  clients into the gateway.
- Implement caic service adapter calls for task create, answer, send message,
  stop, purge, revive, fork, status/detail, usage, web fetch/search, and CI
  helpers.
- Return tool errors to Gemini as tool responses and surface them through the
  client status/transcript channel.
- Keep Android/web local `FunctionHandlers` behind a temporary fallback only if
  needed for transition, and mark the fallback for removal.

PR 5: reversible Android WebView shell.

- Add feature-flagged remote WebView shell mode.
- Keep native settings fallback.
- Preserve native multi-server configuration.
- Add Android compatibility checks for both active service backend and voice
  gateway.
- Add Android host-mode marker in the frontend.
- Suppress browser voice and browser notifications in Android host mode.
- Do not delete Compose screen mode.
- Do not implement bundled frontend assets.
- Do not extract a second app yet.

PR 6: app-scoped monitoring and notification routing.

- Move task monitoring startup to an idempotent app-scoped path.
- Add task ID or route extras to Android notification intents.
- Ensure notification routing works for multiple configured caic backends.
- Keep notification suppression tied to Android voice endpoint state.

PR 7: Android voice endpoint to voice gateway.

- Point Android WebRTC signaling at the configured voice gateway.
- Require a valid gateway session before creating voice sessions.
- Send selected service instance, route/task context, selected voice settings,
  and Halo context to the gateway.
- Use service-issued voice-gateway tokens, not caic private config files.
- Remove Android Gemini setup and local caic voice tool execution once gateway
  tools pass acceptance checks.

Only after these PRs pass should Compose screen-mode retirement start.
