# Android Web Shell Plan

This is the canonical plan for creating **Go Mode**, a new Android app with
application ID `com.fghbuild.gomode`. Go Mode is a thin native shell around
backend-hosted mobile web frontends such as caic and mddb.

The current caic Android app, `com.fghbuild.caic`, remains source material. Do
not edit it as the shell migration path. Copy the relevant native code into Go
Mode and leave caic-specific screen-mode UI behind.

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

The migration has two gates. First, prove a new app can load backend-hosted web
frontends with auth, native notifications, and Go Mode host mode. Then prove
voice-gateway-mediated voice without a broad bridge or fragile request
interception.

## Immediate Direction

Create a new Android app named Go Mode with package/application ID
`com.fghbuild.gomode`.

The first useful iteration is a backend-hosted WebView shell with native
settings fallback, Go Mode host-mode detection in the frontend, and copied
native monitoring/notification code that does not depend on caic Compose screen
ViewModels. Keep the copied native voice path available during the first spike
only if it is cheaper than stubbing voice until the gateway exists.

Treat the voice gateway as the target architecture, not as a prerequisite for
loading the web shell. Before moving Android voice to the gateway, define the
minimum service compatibility metadata, gateway compatibility metadata, and
service-issued token contract needed by Android.

Do not bring over caic task list/detail/diff/process/widget Compose screens.
Go Mode starts as a WebView host plus native bootstrap, notification, voice,
screenshot, and Halo integrations.

## Current Facts

- `frontend/` is already the richer screen-mode implementation for task list,
  task detail, grouping, widgets, VNC, diff, process views, formatting, and
  mobile behavior.
- The current caic Android app duplicates much of that screen UI in Compose and
  relies on convention for parity with the frontend. Go Mode should not inherit
  that duplicated screen UI.
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
  notifications. Go Mode host mode must suppress both unless they are explicitly
  backed by native Android behavior.
- Go Mode must multiplex multiple service instances, initially caic backend
  servers. Native Android settings and server-specific web settings are
  separate ownership domains.
- mddb is another backend-hosted frontend service with OAuth, generated SDK/API
  docs, e2e coverage, and mobile UI tests. Go Mode should keep the service host
  boundary generic enough for mddb even if caic is implemented first.
- The caic backend currently exposes `/api/v1/voice/rtc/offer` and terminates
  the WebRTC side of a voice session through `internal/server/voicertc`.
- The current caic API reports structured `Config.voiceGateway` metadata so
  Android can distinguish disabled voice, caic-embedded gateway, and external
  preferred gateway modes.
- `backend/cmd/voice-gateway` exists as the standalone voice gateway binary. Its
  static config and compatibility metadata live in `backend/internal/voicegateway`,
  and it no longer reads caic `settings.json` or `users.json`.
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
- Do not edit `com.fghbuild.caic` as the shell migration target. It is source
  material for copying.
- Do not copy caic task list/detail/diff/process/widget Compose UI into Go
  Mode.
- Do not build a caic Compose/WebView mode switch.
- Do not proxy normal caic API calls through JavaScript bridge methods.
- Do not bundle frontend assets into the APK. There is no offline use case; the
  app cannot do useful work without network access to a backend service.
- Do not move Android voice endpoint behavior into browser JavaScript.
- Do not make Android execute caic voice tools. Tool execution belongs on the
  voice gateway side of the Gemini Live bridge, using service-owned tool binding
  APIs.
- Do not reintroduce caic-auth-coupled sidecar behavior into the standalone
  voice gateway.
- Do not weaken notification, microphone, audio routing, foreground service, or
  MediaProjection behavior to fit a pure web model.
- Do not add broad refactors before the WebView, auth, notification, and voice
  risks are proven.

## Target Architecture

```text
com.fghbuild.gomode.MainActivity
  -> native bootstrap shell
      -> Go Mode app settings
          -> configured service instances
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

- Web owns screen-mode task UI from the first Go Mode implementation.
- Go Mode owns native settings needed to configure the first server or recover
  from a broken WebView.
- Go Mode owns multiplexing between configured service instances.
- Each backend frontend owns instance-specific server settings such as cache,
  server preferences, workspace settings, and product-specific configuration.
- The backend web UI should remain the primary owner of authentication unless a
  native capability has a proven need for bearer-token access.
- Go Mode owns local voice endpoint lifecycle and platform permissions.
- The voice gateway owns Gemini Live session orchestration, Gemini setup,
  function declarations, tool-call dispatch, and tool responses.
- Each service backend owns its web UI, auth, product API, and service-specific
  tool binding implementation. The gateway calls these bindings through scoped
  credentials issued by the service backend.
- A service backend may either host the voice-gateway protocol itself or
  advertise a preferred external gateway. Go Mode always talks to a
  voice-gateway protocol endpoint, not to caic-specific voice routes.
- Go Mode owns task notifications.
- Go Mode owns MediaProjection screenshot capture until an equivalent web path
  is proven.
- Go Mode owns Halo/BLE integration.
- The JavaScript bridge is capability-oriented, not a second API client.
- The native shell must negotiate protocol compatibility with the loaded server
  before enabling native bindings.

## App Boundary Decision

The product boundary is decided: build a second Android app named Go Mode.

Go Mode provides microphone/audio routing, foreground services, notifications,
WebView hosting, protocol negotiation, and native bindings as shared
infrastructure. The voice gateway provides the shared Gemini Live bridge and
dispatches to service-specific tool binding APIs.

caic, mddb, and future services are configured as service instances with their
own backend URL, hosted web frontend, capabilities, and bridge schema.

Code-copy policy:

- Copy relevant code from `com.fghbuild.caic` into `com.fghbuild.gomode`.
- Rename packages, DI modules, app labels, notification channels, DataStore
  names, deep-link schemes, test packages, and generated screenshots so the two
  apps can coexist on one device.
- Keep copied code only when Go Mode needs the native capability: settings,
  service instance storage, WebView shell, task monitoring, notifications,
  voice endpoint, screenshot capture, and Halo/BLE.
- Do not copy caic screen-mode Compose task UI.
- Do not share mutable preferences or notification IDs between the two apps.
- Prefer later extraction into shared modules only after duplicated copied code
  has stabilized in Go Mode.

## Settings Model

Native Android settings and backend settings must stay separate.

Go Mode app settings:

- Configured service instances, initially multiple caic backend servers and
  later mddb.
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
- Settings should be keyed by service type and service instance ID, not only by
  URL.

Voice gateway config ownership:

- Standalone `~/.config/voice-gateway/config.toml` is canonical.
- The gateway implementation should expose a reusable static config type and
  validation function.
- caic may embed the same static gateway config under `[voice-gateway.config]`
  when caic hosts the gateway protocol in-process.
- caic should normally store only gateway connection settings: standalone URL,
  compatibility state, and service-side token issuance policy. The effective
  gateway state is derived from the URL and `GEMINI_API_KEY`.
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

Use a dedicated XDG config directory such as
`~/.config/voice-gateway/config.toml`. Do not reuse `~/.config/caic`.

The embedded shape should stay isomorphic to the standalone gateway config, but
standalone remains the source of truth for the gateway product boundary.

Effective API mode meanings:

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

The Go Mode shell must not assume that every configured server supports the same
frontend, API, or native binding version.

Add a small shell compatibility endpoint to caic before relying on native
bindings. The exact path can change, but the shape should be stable:

```json
{
  "service": "caic",
  "serviceVersion": "0.0.0",
  "apiVersion": 1,
  "webShell": {
    "minGoModeShell": 1,
    "maxGoModeShell": 1,
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

- The API must expose gateway mode, gateway URL, minimum gateway protocol, auth
  requirements, token exchange endpoint, and capabilities.
- Gateway mode is one of `embedded`, `external`, or `disabled`.
- For `embedded`, `url` points at the caic server origin and caic serves the
  voice-gateway protocol handlers in-process.
- For `external`, `url` points at the preferred standalone gateway. Android
  connects to that gateway directly. caic does not depend on HTTP redirects for
  signaling.
- For `disabled`, Android disables voice without probing WebRTC routes.
- Go Mode reads compatibility metadata before enabling bridge commands.
- The web frontend also receives shell version and capabilities through
  `window.goMode`.
- Unsupported versions must fail visibly with an upgrade message instead of
  partial behavior.
- Server upgrade prompts should be driven by server metadata, not hardcoded app
  guesses.
- Bridge messages carry a version and must be backward-compatible within a
  declared range.
- Feature checks use capabilities, not only version numbers.
- Voice capabilities distinguish local Android audio endpoint support, service
  backend binding support, and voice gateway Gemini/tool support.
- Clients must use structured gateway metadata.
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
  frontend mode and need Go Mode host-mode suppression.
- Hypothesis: authentication is primarily backend-owned through the web UI, and
  Android only needs native auth state for capabilities that cannot use the
  WebView session.
- Hypothesis: voice gateway can own Gemini setup, function declarations,
  tool-call dispatch, and tool responses, leaving Android as a local voice
  endpoint and service backends as binding owners.
- Hypothesis: caic can tolerate gateway config version skew, unknown fields, or
  unsupported gateway features without making unrelated caic startup fail.
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
- Hypothesis: the first Go Mode spike can copy the relevant caic native code
  without pulling in caic task list/detail/diff/process/widget Compose UI.
- Hypothesis: Go Mode can coexist with `com.fghbuild.caic` on the same device
  after package names, DataStore names, notification channels, deep links, and
  screenshot paths are renamed.

If any hypothesis is false, update this plan before coding.

## Baseline

Read before editing:

- `/home/user/src/AGENTS.md`
- `AGENTS.md`
- `android/AGENTS.md` before copying Android code from the caic app
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
hide pre-existing failures behind Go Mode work.

## Execution Order

Execute in this order unless a phase explicitly proves the direction is wrong:

1. Confirm the hypotheses against current code and record mismatches in this
   plan.
2. Scaffold Go Mode as a new Android app with package/application ID
   `com.fghbuild.gomode`.
3. Copy only native bootstrap/settings code needed to configure service
   instances.
4. Prove backend-hosted WebView shell loading in Go Mode.
5. Add Go Mode host mode to the frontend.
6. Copy and adapt task monitoring and notifications without screen ViewModel
   dependencies.
7. Prove auth and service compatibility negotiation.
8. Add the narrow native-web bridge.
9. Establish the voice gateway boundary and compatibility endpoint.
10. Prove scoped voice-gateway token exchange.
11. Move voice tool execution into the voice gateway and service bindings.
12. Wire Go Mode voice endpoint to the voice gateway.
13. Add notification deep links, screenshot bridge, and Halo context.

## Phase 1: Voice Gateway Foundation

This is required before Go Mode voice moves to a gateway. It is not required
before the first WebView shell spike.

The standalone voice gateway baseline is already in place:
`backend/cmd/voice-gateway` owns the binary, `backend/internal/voicegateway`
owns static config and compatibility metadata, and `internal/server/voicertc`
remains the lower-level WebRTC transport package.

Requirements:

- Add caic `[voice-gateway]` config that derives `external`, `embedded`, and
  `disabled` from the standalone gateway URL and `GEMINI_API_KEY`. External
  config stores only the gateway URL and service-side policy; embedded config
  embeds the same static gateway config shape as standalone.
- Keep `Config.voiceGateway` metadata reporting mode, gateway URL, minimum
  gateway protocol, auth requirements, token endpoint, and capabilities.
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
  gateway modes through structured metadata.
- Gateway login works with configured Google, GitHub, and GitLab providers.
- Gateway login works over a Tailscale-only URL when the client can reach that
  URL.
- Pairing mode can create a gateway session without public inbound access.
- Invalid or expired scoped tokens are rejected.
- A configured caic service can create a voice session without Android executing
  tool calls locally.
- Tests cover config loading, compatibility response, token validation, and
  service selection.

## Phase 2: Go Mode WebView Spike

Goal: prove that Go Mode can host the existing backend-served mobile frontend
without editing the current caic Android app.

Remaining PR 1 work:

- Add Android instrumented smoke coverage against the fake backend for shell
  loading, task list display, task detail navigation, task creation, task input,
  and Android back behavior.
- Manually verify side-by-side installation of `com.fghbuild.gomode` and
  `com.fghbuild.caic` on a device or emulator.
- Prove the no-auth fake backend flow in WebView with `adb reverse`.

Create a new Android app:

- Package/application ID: `com.fghbuild.gomode`.
- App label: `Go Mode`.
- Keep caic's `com.fghbuild.caic` app installable in parallel.
- Copy only native bootstrap/settings code needed for service instance
  configuration.
- Do not copy caic screen-mode Compose task UI.
- Keep the copied native voice panel only if it is needed for the first spike.

Create:

```text
<gomode-app>/src/main/java/com/fghbuild/gomode/ui/web/WebShellScreen.kt
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
- Go Mode and the current caic Android app can both be installed on the same
  device.

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
- WebView host mode exposes Go Mode shell version and native capabilities to the
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
<gomode-app>/src/main/java/com/fghbuild/gomode/data/TaskMonitor.kt
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
- Go Mode task monitoring does not instantiate copied caic screen ViewModels.

## Phase 5: Go Mode Host Mode in the Frontend

Add a narrow frontend host-mode abstraction.

Recommended files:

```text
frontend/src/goModeHost.ts
frontend/src/goModeHost.test.ts
frontend/src/global.d.ts
```

Native should expose one marker before the app code runs:

```ts
window.goMode = {
  version: 1,
  hostMode: true,
  activeService: "caic",
};
```

In Go Mode host mode:

- Do not render or auto-start browser `VoiceOverlay`.
- Do not instantiate browser `VoiceSession`.
- Do not request browser notification permission.
- Do not show browser notifications.
- Prefer native screenshot integration when it is available.
- Keep normal browser and PWA behavior unchanged outside Go Mode host mode.

Frontend contract rules:

- Add typed global declarations. Avoid `any`.
- Keep Go Mode host behavior behind a small module.
- Do not scatter `window.goMode` checks throughout feature code.
- Unit-test browser behavior and Go Mode host behavior separately.

Acceptance checks:

- Browser/PWA voice behavior is unchanged.
- Go Mode WebView does not start duplicate browser voice.
- Go Mode WebView does not emit browser notifications in addition to Android
  notifications.
- Frontend tests cover host-mode detection and suppression.

## Phase 6: Native-Web Bridge

Keep the bridge small, versioned, and capability-oriented. Normal caic API calls
continue through the generated TypeScript SDK.

Use one JavaScript object:

```ts
interface GoModeBridge {
  version: 1;
  hostMode: true;
  activeService: string;
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
- Keep compatibility versioned so future web builds can detect unsupported Go
  Mode shells.
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
- Browser voice code is inactive in Go Mode host mode.

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

## Phase 10: Decide Remaining Native Integrations

Before broadening Go Mode, explicitly decide which Android-only features are
copied from caic and which stay out:

- Native server settings
- Native login screen
- Native image attachment flows
- Native screenshot capture
- Halo BLE support
- Multi-server switcher and per-server diagnostics
- Service host diagnostics for caic and mddb

Recommended defaults:

- Keep native server settings permanently as bootstrap and recovery UI.
- Keep multi-server configuration native.
- Keep or copy the native voice endpoint panel only until gateway-backed voice
  owns Gemini setup and tool dispatch.
- Keep screenshot capture if web image attach is worse on Android.
- Keep Halo native.
- Keep backend-owned settings in the backend web UI.
- Do not copy native task list/detail/diff/process/widget UI.
- Keep shared-host assumptions in Go Mode names and schemas from the start.

## Phase 11: Remove Copied Transitional Code

Only start after all earlier acceptance checks pass.

Remove copied caic code that was useful for bootstrapping but is no longer part
of the Go Mode product boundary:

- caic-specific task list/detail formatting helpers
- local voice function declarations and handlers
- caic-specific navigation constants that should be service metadata
- duplicated grouping and formatting code, if Go Mode voice endpoint code no
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
- Go Mode e2e should exercise WebView-hosted task creation, task detail,
  notifications, voice startup, and screenshot capture where practical.
- Update `AGENTS.md` file indexes if new app files are added.
- Keep Compose only for native Go Mode surfaces such as settings, recovery UI,
  and optional voice controls.

Acceptance checks:

- No copied caic screen-mode task UI is reachable in Go Mode.
- Native services still start and stop correctly.
- Go Mode e2e covers the accepted shell workflows.
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

- caic config API exposes structured gateway metadata for URL-derived external,
  key-derived embedded, and disabled states.
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

- Go Mode host detection.
- Browser voice path remains unchanged without Android bridge.
- Go Mode host mode does not instantiate browser `VoiceSession`.
- Browser notifications are suppressed in Go Mode host mode.
- Android voice endpoint state updates task number badges if web badges are
  implemented.
- Browser/web voice clients stop executing caic voice tools locally once gateway
  voice tools and service bindings are available.

Manual matrix before first Go Mode release:

- Fresh install, no server configured.
- Server configured, no auth.
- Server configured, OAuth enabled.
- Voice gateway configured with Google OAuth.
- Voice gateway configured with GitHub OAuth.
- Voice gateway configured with GitLab OAuth.
- Voice gateway reachable only over Tailscale with OAuth callback URL.
- Voice gateway reachable only over Tailscale with pairing mode.
- caic with no gateway URL and `GEMINI_API_KEY` available.
- caic with standalone gateway URL configured.
- caic with no gateway URL and no `GEMINI_API_KEY`.
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

## Design Goals

- Go Mode builds as `com.fghbuild.gomode` and can coexist with
  `com.fghbuild.caic` on one device.
- caic config metadata uses structured embedded, external, and disabled gateway
  modes.
- Go Mode can multiplex multiple configured service instances, initially caic
  backend servers.
- Native task notifications work independently of WebView lifecycle.
- Go Mode voice endpoint works and can expose enough state for the shell.
- Voice gateway owns Gemini setup and tool dispatch.
- Service backends own scoped token issuance and tool binding implementation.
- Halo remains compatible with the shell architecture.
- Browser and Android voice endpoints do not duplicate each other.
- Browser and Android notifications do not duplicate each other.
- Go Mode avoids importing caic duplicated screen-mode UI.

## Acceptance Criteria

- WebView loads the mobile web UI reliably from the configured server.
- Auth works without broad request interception or a custom API proxy.
- Compatibility negotiation prevents unsupported bridge/API combinations.
- Voice gateway auth works with configured Google, GitHub, and GitLab providers.
- Voice gateway auth works for Tailscale-only deployments without requiring
  public inbound access.
- Go Mode host mode suppresses browser voice and browser notifications.
- Native task notifications route to the correct web task route.
- Go Mode and `com.fghbuild.caic` can be installed and used on one device
  without shared storage, notification, or deep-link collisions.

## Go/No-Go Criteria

Stop and reassess Go Mode if any are true:

- Auth requires fragile WebView request interception.
- EventSource cannot authenticate cleanly.
- Version compatibility cannot be represented cleanly between app and server.
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
- Go Mode cannot coexist with `com.fghbuild.caic` because package names,
  storage, notification channels, or deep links are not isolated.
- Go Mode starts accumulating copied caic screen-mode task UI.

If the migration stops, keep `com.fghbuild.caic` unchanged and narrow Go Mode
to the native host capabilities that remain independently useful.

## Recommended PR Sequence

Keep each PR small enough to revert.

PR 1: Go Mode app scaffold and WebView shell.

- Add smoke coverage for fake-backend shell loading, task list display, task
  detail navigation, task creation, task input, and Android back behavior.
- Verify side-by-side installation of `com.fghbuild.gomode` and
  `com.fghbuild.caic`.

PR 2: Go Mode host mode and app-scoped monitoring.

- Add Go Mode host-mode marker in the frontend.
- Suppress browser voice and browser notifications in Go Mode host mode.
- Copy and adapt task monitoring startup to an idempotent app-scoped path in Go
  Mode.
- Keep notification suppression tied to Android voice endpoint state.
- Ensure Android task notifications work when only WebView screen mode is
  mounted.
- Keep existing browser/PWA behavior unchanged outside Go Mode host mode.

PR 3: service auth and shell compatibility.

- Prove OAuth login, logout, fetch, and EventSource behavior inside WebView.
- Add service compatibility metadata for shell version, bridge version range,
  capabilities, and upgrade guidance.
- Add Android compatibility checks for the active service backend.
- Keep normal caic API calls in the web frontend; do not proxy them through the
  native bridge.
- Stop the shell migration if auth requires broad request interception or a
  custom API proxy layer.

PR 4: narrow native-web bridge and routing.

- Add the versioned capability bridge.
- Support native settings, notification permission, current task context, and
  visible native errors.
- Add task ID or route extras to Android notification intents.
- Ensure notification routing works for multiple configured caic backends.
- Replay pending navigation after WebView load.
- Reject invalid bridge calls visibly.

PR 5: screenshot and retained native integrations.

- Add native screenshot request/result bridge if the web attach path is worse on
  Android.
- Keep MediaProjection countdown, foreground service behavior, cancellation, and
  error handling intact.
- Define the Halo context shape sent from Android to the future gateway.
- Keep Halo native.

PR 6: voice gateway identity and configuration. Done.

Implemented the standalone `backend/cmd/voice-gateway` binary, reusable
`backend/internal/voicegateway` static config, canonical
`~/.config/voice-gateway/config.toml` loading, caic `[voice-gateway]` parsing,
structured `Config.voiceGateway` API metadata, and gateway `GET /health` plus
`GET /compat`. The gateway no longer reads caic `settings.json` or `users.json`.

Remaining follow-up: caic currently rejects unknown gateway config fields with
the rest of `config.toml`; deliberate gateway config version-skew handling is
still unresolved.

PR 7: gateway user auth.

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

PR 8: scoped service auth and caic binding skeleton.

- Add trusted issuer configuration to the gateway.
- Add scoped-token validation skeleton with audience, expiry, issuer, service
  kind, service instance ID, and capability checks.
- Add caic compatibility metadata describing voice gateway requirements.
- Add a caic endpoint that can issue a short-lived voice-gateway token from the
  authenticated web session.
- Add a caic voice binding API skeleton for service tool calls.
- Add tests for token issuance, token rejection, logout/revocation behavior, and
  binding authorization.

PR 9: gateway-owned Gemini setup and caic voice tools.

- Move Gemini setup construction and function declarations out of Android/web
  clients into the gateway.
- Implement caic service adapter calls for task create, answer, send message,
  stop, purge, revive, fork, status/detail, usage, web fetch/search, and CI
  helpers.
- Return tool errors to Gemini as tool responses and surface them through the
  client status/transcript channel.
- Keep Android/web local `FunctionHandlers` behind a temporary fallback only if
  needed for transition, and mark the fallback for removal.

PR 10: Android voice endpoint to voice gateway.

- Point Go Mode WebRTC signaling at the configured voice gateway.
- Require a valid gateway session before creating voice sessions.
- Send selected service instance, route/task context, selected voice settings,
  and Halo context to the gateway.
- Use service-issued voice-gateway tokens, not caic private config files.
- Remove Android Gemini setup and local caic voice tool execution once gateway
  tools pass acceptance checks.

Only after these PRs pass should copied transitional code be removed.
