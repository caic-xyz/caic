# Android Guidelines

Kotlin/Compose Android app for caic. Voice-first companion for managing coding agents.

## Current State

SDK module (generated API client + types), Compose UI, Hilt DI, and voice mode
(Phase 1) are implemented. The app has a task list screen, settings screen, and
a voice overlay with Gemini Live WebSocket integration. Phase 2 (full screen mode
with TaskDetail, message grouping, etc.) is not yet started.

## Architecture

See `docs/` for full design specs:
- `docs/sdk-design.md` — Kotlin SDK: generated API client + types
- `docs/app-design.md` — App: screens, voice mode, state management

### Layer Summary

```
UI (Compose screens) → ViewModels (StateFlow) → Repositories → SDK (ApiClient)
                                                             → Gemini Live (voice)
                                                             → DataStore (settings)
```

No business logic in Compose. No Android dependencies in the SDK module.

## Conventions

- **Package**: `com.fghbuild.caic` (app), `com.caic.sdk` (SDK module)
- **DI**: Hilt
- **Serialization**: `kotlinx.serialization` (not Gson/Moshi)
- **Networking**: OkHttp (HTTP + SSE + WebSocket)
- **Async**: Coroutines + `StateFlow` (not LiveData, not RxJava)
- **Navigation**: Compose Navigation with type-safe routes
- **Compose naming**: `PascalCase` for composables (detekt `functionPattern` allows this)
- **Line length**: 120 chars (detekt config)
- **No wildcard imports** (detekt enforced)

## Build & Lint

```bash
make lint-android   # detekt + Android lint
make android-build  # assembleDebug
make android-test   # JVM unit tests
```

Lint is strict: `warningsAsErrors = true`, `maxIssues: 0`.

## Gemini Live API

Official docs:
- https://ai.google.dev/api/live — WebSocket reference
- https://ai.google.dev/gemini-api/docs/ephemeral-tokens — token provisioning

### Ephemeral token flow

The app never holds a long-lived API key. Instead:

1. Android calls `GET /api/v1/voice/token` on the caic backend.
2. Backend creates a short-lived ephemeral token via
   `POST https://generativelanguage.googleapis.com/v1alpha/auth_tokens`
   (header `x-goog-api-key`). Tokens: `uses: 1`, 30 min expiry, 2 min
   new-session window.
3. Android opens a WebSocket with the token as a query parameter.

### API versioning (v1alpha)

Ephemeral tokens are **v1alpha only** — both token creation and the
WebSocket endpoint must use `v1alpha`. Using `v1beta` for `auth_tokens`
returns 404. See https://ai.google.dev/gemini-api/docs/ephemeral-tokens.

- Token endpoint: `POST /v1alpha/auth_tokens`
- WebSocket URL (with ephemeral token):
  `wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained`
- Auth: `?access_token=<token>` query param or `Authorization: Token <token>` header.

The **non-ephemeral** Live API (direct API key) uses `v1beta` +
`BidiGenerateContent`, but we don't use that path.

### Protocol notes

- Client must wait for `BidiGenerateContentSetupComplete` before sending other messages.
- `mediaChunks` in `realtimeInput` is **deprecated** — use `audio`, `video`, or `text` fields instead.

## Development Notes

- `minSdk = 34`, `targetSdk = 36`, `compileSdk = 36`
- Version catalog at `gradle/libs.versions.toml`
- detekt config at `detekt.yml`
- The web frontend (SolidJS) in `../frontend/` is the reference implementation for
  screen behavior, event grouping, and formatting. Match it.

## Implementation Order

The app's unique value is voice control. Screen mode is secondary (the web
frontend already exists). Follow the design docs in this order:

1. ~~**SDK module** (`docs/sdk-design.md`)~~ — Done.
2. ~~**Voice mode** (`docs/app-design.md` Phase 1)~~ — Done.
3. **Screen mode** (`docs/app-design.md` Phase 2): full Compose UI with feature
   parity to the web frontend — TaskDetail, message grouping, tool call display,
   turn elision, background service, notifications.

Each step should build, lint, and test clean before proceeding.
