# Go Mode Android Web Shell Boundary

Go Mode is the Android app with application ID `com.fghbuild.gomode`. It is a
thin native shell around backend-hosted mobile web frontends such as caic and,
later, mddb.

The reusable Go Mode contract is documented under `../../gomode/docs/`:

- [`SERVER_LIBRARY.md`](../../gomode/docs/SERVER_LIBRARY.md): host/server
  manifest, token, SDK, and extraction boundary.
- [`ANDROID_SHELL.md`](../../gomode/docs/ANDROID_SHELL.md): Android manifest
  bootstrap, SKILL.md activation, MCP client behavior, service adapters, and
  voice setup.
- [`VOICE_GATEWAY.md`](../../gomode/docs/VOICE_GATEWAY.md): public voice gateway
  protocol, deployment modes, data-channel messages, and authorization.
- [`VOICE_LOCAL_STACK.md`](../../gomode/docs/VOICE_LOCAL_STACK.md): local ASR,
  LLM, and TTS backend plan behind the voice gateway contract.
- [`IMPLEMENTATION_PLAN.md`](../../gomode/docs/IMPLEMENTATION_PLAN.md): active
  convergence plan.

This file only records Android-app policy, source anchors, and validation notes.
Do not duplicate the reusable contract here.

## Product Boundary

Go Mode owns native capabilities that require Android APIs:

- settings bootstrap before a WebView can load a server
- WebView hosting, route recovery, and shell compatibility negotiation
- Android notification permission, channels, and hosted-service notification
  routing
- microphone permission, audio routing, foreground voice services, and voice
  gateway WebRTC client setup
- native screenshot capture when the hosted web attach flow cannot provide the
  required Android experience
- Halo/BLE integration

Backend-hosted services own:

- product UI and routes
- product APIs and generated SDK contracts
- authentication session state unless a native capability needs a scoped token
- service-specific settings, workspace state, and authorization
- service-issued tokens for voice gateway access when voice is enabled

The former native caic Android app has been removed. Do not recreate caic task
list/detail/diff/process/widget screens in Compose. The hosted web frontend owns
screen-mode product UI; Go Mode owns Android platform capabilities.

## Android Non-Goals

Reject these in `android/gomode/`:

- Trusted Web Activity conversion; Go Mode needs native state and services
- bundled frontend assets without a proven offline requirement
- JavaScript bridge methods that proxy normal service API calls
- caic-specific SDK DTOs or `/api/caic/...` route knowledge in shell services
- Android-specific forks of normal service APIs
- browser JavaScript owning Android voice endpoint behavior
- service-specific tool execution inside the voice gateway

## Current Source Anchors

- `android/gomode/src/main/java/com/fghbuild/gomode/MainActivity.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/data/SettingsRepository.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/ui/GoModeApp.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/ui/settings/SettingsScreen.kt`
- `android/gomode/src/main/java/com/fghbuild/gomode/ui/web/WebShellScreen.kt`

## Android Settings Ownership

Native Go Mode settings:

- configured service instances, initially caic servers
- active service instance
- voice gateway URL and last-known compatibility state
- notification policy and Android permission state
- voice endpoint settings, microphone permission recovery, and audio behavior
- Halo/BLE device binding
- shell compatibility state and native diagnostics

Backend-owned settings:

- product preferences and workspace settings
- auth session state exposed through the hosted web UI
- product notification preferences if supported by that service
- service-specific voice gateway token policy

Do not mirror backend settings into Android unless a native capability needs that
state.

## Android Integration Rules

- WebView login and hosted UI loading must not depend on manifest, MCP, or voice
  gateway discovery succeeding.
- Native MCP-backed features are disabled until manifest compatibility passes;
  see [`ANDROID_SHELL.md`](../../gomode/docs/ANDROID_SHELL.md) for the
  unvalidated, compatible, and incompatible states.
- Keep the JavaScript bridge narrow, versioned, and capability-oriented.
- Notification deep links route to hosted WebView routes through a neutral route
  contract.
- Android back handling must account for SPA route ownership, not only
  `WebView.canGoBack()`.
- When loaded inside Go Mode, the hosted frontend should suppress browser-owned
  voice and notification behavior that native Android owns. Normal browser/PWA
  behavior must remain unchanged outside Go Mode.

## Voice And Halo

Android owns microphone permission, audio routing, foreground service lifecycle,
WebRTC client setup, and local execution of active SKILL.md tools. The gateway
contract and backend split are canonical in
[`VOICE_GATEWAY.md`](../../gomode/docs/VOICE_GATEWAY.md).

Halo/BLE policy and emulator notes live in [`HALO.md`](HALO.md). Keep Halo state
service-neutral unless the hosted frontend exposes a shell capability for richer
status.

## Screenshots And E2E

Android E2E uses the fake backend and passes its base URL to instrumentation. Go
Mode screenshot tests load the hosted caic frontend in WebView and navigate
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
