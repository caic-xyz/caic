# Go Mode Web Shell Architecture

Go Mode is the Android app with application ID `com.fghbuild.gomode`. It is a
thin native shell around backend-hosted mobile web frontends such as caic and,
later, mddb.

The former native caic Android app has been removed. Do not recreate caic task
list/detail/diff/process/widget screens in Compose. The hosted web frontend owns
screen-mode product UI; Go Mode owns Android platform capabilities.

## Product Boundary

Go Mode owns native capabilities that require Android APIs:

- Bootstrap settings before a WebView can load a server.
- WebView hosting, route recovery, and shell compatibility negotiation.
- Android notification permission, channels, and hosted-service notification
  routing.
- Microphone permission, audio routing, foreground voice services, and the local
  voice endpoint.
- Native screenshot capture only when the hosted web attach flow cannot provide
  the required Android experience.
- Halo/BLE integration.

Backend-hosted services own:

- Product UI and routes.
- Product APIs and generated SDK contracts.
- Authentication session state unless a native capability needs a scoped token.
- Service-specific settings, workspace state, and authorization.
- Service-issued tokens for voice gateway access when voice is enabled.

## Non-Goals

- Do not use a Trusted Web Activity; Go Mode needs native state and services.
- Do not bundle frontend assets into the APK.
- Do not proxy normal service API calls through JavaScript bridge methods.
- Do not add caic-specific SDK types or `/api/caic/...` route knowledge to Go
  Mode shell services.
- Do not move Android voice endpoint behavior into browser JavaScript.
- Do not put service-specific tool execution in the voice gateway.

## Current Shape

```text
MainActivity
  -> GoModeApp
      -> SettingsRepository
      -> native settings fallback
      -> active service instance
      -> WebShellScreen
          -> WebView loading the backend-hosted frontend
```

Current source anchors:

- `android/gomode/src/main/java/com/fghbuild/gomode/MainActivity.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/data/SettingsRepository.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/ui/GoModeApp.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/ui/settings/SettingsScreen.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/ui/web/WebShellScreen.kt`

## Target Architecture

```text
Go Mode Android
  -> native bootstrap shell
      -> app settings
          -> configured service instances
          -> active service instance
          -> voice gateway settings
          -> notification policy
          -> Halo/BLE settings
      -> ServiceRegistry
          -> ServiceAdapter selected by active service instance
          -> neutral attention, route, voice-token, and capability interfaces
      -> native monitors and notifications
          -> depend only on ServiceAdapter interfaces
      -> VoiceEndpoint / VoiceService
          -> WebRTC client connection to a voice gateway
      -> WebShellScreen
          -> hosted frontend
          -> narrow capability bridge for native-only features

Voice gateway
  -> WebRTC signaling and media endpoint
  -> Gemini Live transport
  -> service-signed token verification

Active service backend
  -> hosted web frontend
  -> product API and auth
  -> shell compatibility metadata
  -> voice gateway metadata and token issuer, when supported
```

Rules:

- Keep the shell service-neutral. Service-specific calls live behind adapters.
- Keep the JavaScript bridge narrow, versioned, and capability-oriented.
- Negotiate compatibility with the loaded server before enabling native bridge
  capabilities.
- Notification deep links route to hosted WebView routes through a neutral route
  contract.
- Android back handling must account for SPA route ownership, not only
  `WebView.canGoBack()`.

## Settings Ownership

Native Go Mode settings:

- Configured service instances, initially caic servers.
- Active service instance.
- Voice gateway URL and last-known compatibility state.
- Notification policy and Android permission state.
- Voice endpoint settings, microphone permission recovery, and audio behavior.
- Halo/BLE device binding.
- Shell compatibility state and native diagnostics.

Backend-owned settings:

- Product preferences and workspace settings.
- Auth session state exposed through the hosted web UI.
- Product notification preferences if supported by that service.
- Service-specific voice gateway token policy.

Do not mirror backend settings into Android unless a native capability needs
that state.

## Auth And Compatibility

Preferred auth model: backend-owned WebView session.

- WebView uses normal web auth, cookies, fetch, and EventSource behavior.
- Android avoids owning service credentials unless a native capability proves it
  needs direct API access.
- Native capabilities should use explicit scoped handoffs, such as a
  service-issued voice gateway token.

Compatibility requirements before broad bridge use:

- Service ID and API version.
- Bridge version range.
- Native capability list.
- Voice gateway mode and endpoint metadata when voice is supported.
- Upgrade guidance for incompatible app/server pairs.

Stop and reassess if auth requires broad WebView request interception, a custom
API proxy layer, or Android-specific forks of normal service APIs.

## Voice Direction

The voice gateway is the transport target. It owns WebRTC signaling/media and
Gemini Live transport. It does not own service-specific tool execution.

Go Mode Android owns:

- Local voice endpoint lifecycle.
- Gemini setup for the active service.
- Tool-call dispatch and tool responses.
- HTTP calls to the active service backend through a service adapter.

Service backends own:

- Public compatibility metadata.
- Service signing keys and short-lived voice gateway tokens.
- Product API authorization for tool actions.

## Frontend Host Mode

When loaded inside Go Mode, the hosted frontend should know it is running under
an Android shell and avoid duplicating native capabilities.

Go Mode host mode should suppress browser-owned behavior when native Android is
responsible, especially browser voice sessions and browser notifications. Normal
browser/PWA behavior must remain unchanged outside Go Mode.

## Screenshots And E2E

Android E2E uses the fake backend and passes its base URL to instrumentation.
Go Mode screenshot tests load the hosted caic frontend in WebView and navigate
through task states there rather than using deleted native caic Compose screens.

Expected documentation screenshots live under `e2e/screenshots/android/` and use
Go Mode names, including:

- `gomode-settings.webp`
- `gomode-web-shell.webp`
- `gomode-settings-from-web.webp`
- `gomode-task-list.webp`
- `gomode-task-detail.webp`
- `gomode-task-plan.webp`
- `gomode-task-ask.webp`

Preferred commands:

```bash
python3 scripts/android_e2e.py --module gomode
make android-e2e
```

## Validation

After Android changes:

```bash
make lint-fix
python3 scripts/android_e2e.py --module gomode
```

For full Android coverage:

```bash
make android-e2e
```

After frontend or backend integration changes, also run the relevant project
checks (`make check`, `make frontend-e2e`) based on touched code.

## Keep / Remove Guidance

Keep in Go Mode:

- Settings and recovery UI.
- WebView shell and compatibility bridge.
- Notification, voice, screenshot, and Halo/BLE native capabilities.
- Service-neutral adapters and routing contracts.

Remove or reject:

- Native caic screen-mode Compose UI.
- Direct caic SDK imports in Go Mode shell code.
- Hard-coded caic task routes in shell services.
- Duplicated browser and native voice/notification execution.
- Bundled frontend assets without a proven offline requirement.
