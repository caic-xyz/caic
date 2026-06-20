# Go Mode Implementation Plan

This plan converges the codebase on the specific documents:

- `gomode/docs/SERVER_LIBRARY.md`
- `gomode/docs/ANDROID_SHELL.md`
- `gomode/docs/VOICE_GATEWAY.md`
- `gomode/docs/VOICE_LOCAL_STACK.md`

## Phase 1: Server Library Boundary

Tasks:

1. Add `gomode/README.md` with package responsibilities and host adapter rules.
2. Split `gomode/http.go` into:
   - `discovery.go` for manifest types and validation
   - `handler.go` for HTTP serving, cache headers, and ETag handling
3. Move `voicegateway/scoped_token.go` and tests to the `gomode` root.
4. Update `voicegateway` to import `gomode` for token verification.

Acceptance:

- `go list ./gomode ./gomode/voicegateway/...` succeeds.
- `gomode` imports no `voicegateway`, pion, opus, genai, or
  `backend/internal/*`.
- Discovery and voice gateway tests pass.

## Phase 2: Discovery Contract Cleanup

Tasks:

1. Decide whether `ToolGroup.protocolVersion` belongs in the manifest. Keep it
   with clear docs, or remove it and regenerate SDKs.
2. Decide whether `ToolGroupActivation.routes` belongs in v1. Keep it only if
   Android will use it for first activation policy.
3. Add validation helpers for `Settings`, `ToolGroup`, and
   `VoiceGatewaySettings`.
4. Document `apiVersion` and `bridgeVersion` compatibility rules in SDK docs.

Acceptance:

- No TODO comments leak into generated Go Mode SDK docs.
- caic serves a valid manifest at `/.well-known/gomode.json`.
- Android tests reject malformed required fields.
- `make refresh-generated` and `make lint-fix` pass.

## Phase 3: External Gateway Token Flow

Tasks:

1. Add a caic voice-token endpoint under caic session auth.
2. Populate `webShell.voiceGateway.tokenEndpoint` for external gateway auth.
3. Make Android request the token before opening an external gateway session.
4. Keep embedded gateway mode token-free.
5. Hide the transitional ed25519 format behind root `gomode` token APIs.

Acceptance:

- Embedded sessions work without a voice token.
- External sessions fail without a token and succeed with a valid token.
- Token claims bind service kind, service instance, backend origin, audience,
  expiry, and capabilities.
- Reject logs do not leak token contents.

## Phase 4: OAuth JWT Federation

Tasks:

1. Add gateway-side multi-issuer verification:
   - parse `iss`
   - require issuer origin allowlist
   - fetch authorization server metadata
   - fetch and cache JWKS
   - verify JWT locally
2. Teach caic to mint OAuth access tokens with `aud=voice-gateway` and narrow
   voice scopes.
3. Support key rotation through JWKS cache refresh.
4. Keep ed25519 only as a compatibility or offline fallback.

Acceptance:

- Gateway verifies tokens from at least two configured issuers in tests.
- Unknown issuers are rejected before JWKS fetch.
- Rotated JWKS keys are accepted after cache refresh.
- Expired, wrong-audience, and insufficient-scope tokens are rejected.

## Phase 5: Android Bootstrap Robustness

Tasks:

1. Implement explicit unvalidated, compatible, and incompatible states.
2. Let WebView load in limited mode when manifest fetch or decode fails.
3. Disable native MCP-backed features until manifest compatibility passes.
4. Surface clear disabled or incompatible states for voice and monitoring.

Acceptance:

- WebView load does not depend on MCP discovery.
- Missing or malformed manifest disables native features instead of crashing.
- Voice setup uses the manifest gateway URL and selected tool-group endpoint.

## Phase 6: Android MCP Resources And Monitoring

Tasks:

1. Extend Android MCP client with:
   - `server/discover`
   - `resources/list`
   - `resources/templates/list`
   - `resources/read`
   - POST-based SSE `subscriptions/listen`
2. Add service adapter interface keyed by `service` and `apiVersion`.
3. Add caic adapter support for `caic://tasks`.
4. Feed adapter output into notifications, attention state, and voice context.
5. Treat subscription notifications as invalidations and re-read resources.

Acceptance:

- Android reads fake task resources without caic REST DTO imports.
- Resource update events trigger re-read and update native monitoring state.
- Unsupported resources disable monitoring cleanly.

## Phase 7: Backend Subscription Invalidation

Tasks:

1. Add an internal change-notifier interface at the MCP registry or handler
   boundary.
2. Emit `notifications/resources/list_changed` when resource lists change.
3. Emit `notifications/resources/updated` for subscribed URIs when content
   changes.
4. Keep polling only as a safe fallback where no notifier exists.
5. Bound stream lifetime, memory, and goroutines.

Acceptance:

- Subscription tests do not wait on arbitrary polling intervals.
- Canceled Android connections release resources.
- Events are delivered without unbounded queue growth.

## Phase 8: Progressive Tool-Group Activation

Tasks:

1. Load all group names and descriptions at bootstrap.
2. Implement local activation scoring from supported hints.
3. Cap concurrently active groups.
4. Namespace or reject colliding tool names.
5. Show active groups and tools in native voice state.
6. Deactivate groups when context no longer matches.

Acceptance:

- Single caic group still activates by default.
- Fake multi-group manifests activate only matching groups.
- Deactivation removes tools from the voice session.

## Phase 9: Local Voice Stack

Tasks:

1. Run target-Mac smoke tests for managed llama.cpp ASR/LLM and candidate TTS.
2. Compare Qwen3-ASR and whisper.cpp Parakeet support for ASR latency and
   quality.
3. Wire the selected TTS adapter behind `ttsAdapter`.
4. Stream assistant audio over RTP.
5. Preserve interruption and tool-call behavior across local and Gemini
   backends.

Acceptance:

- Local backend completes a voice turn without Gemini Live.
- Local backend calls client tools and continues after tool results.
- User interruption stops TTS and cancels pending model work.

## Phase 10: Repository Split

Tasks:

1. Move `gomode/` to `github.com/caic-xyz/gomode`.
2. Move or mirror the standalone voice gateway binary with the library.
3. Rename generated SDK packages to final Go Mode namespaces where needed.
4. Update caic imports to the external module.
5. Keep host adapters in host repositories.

Acceptance:

- caic builds against external `github.com/caic-xyz/gomode`.
- A second host can serve the manifest and mount the gateway without caic
  dependencies.
- Go Mode packages still have no `backend/internal/*` dependency.
