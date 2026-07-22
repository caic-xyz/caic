# Go Mode Implementation Plan

This plan tracks remaining work to converge the codebase on:

- `gomode/docs/SERVER_LIBRARY.md`
- `gomode/docs/ANDROID_SHELL.md`
- `gomode/docs/VOICE_GATEWAY.md`
- `gomode/docs/VOICE_LOCAL_STACK.md`

## Current Baseline

- The discovery manifest has `ToolGroup.protocolVersion`,
  `SkillFrontmatter`, and validation for `Settings`, `ToolGroup`,
  `SkillFrontmatter`, and `VoiceGatewaySettings`.
- caic serves a single `tasks` tool group at `/.well-known/gomode.json`.
- Android bootstrap has unvalidated, ready, and error states. WebView loading is
  separate from native feature compatibility.
- Android voice can use the first advertised tool group for MCP
  `server/discover`, `tools/list`, and `tools/call`.
- Android `McpClient` supports `resources/list`, `resources/templates/list`,
  `resources/read`, and POST-based SSE `subscriptions/listen`.
- Android service monitoring consumes the generic `gomode://items` resource
  for native attention text, voice session context, and the foreground voice
  notification. Hosts map product state into its item schema; subscription
  invalidations trigger resource re-reads and monitoring state updates.
- Backend MCP supports `server/discover`, resources, resource templates, and
  POST-based SSE `subscriptions/listen`.
- Backend resource subscriptions validate filters, stream task/repo
  invalidations, emit resource-list and resource-content notifications, and stop
  on context cancellation.
- The voice gateway supports embedded and external modes. The standalone gateway
  verifies transitional ed25519 scoped tokens from configured trusted issuers.
- The local voice stack has managed llama.cpp ASR/LLM adapters, a KittenTTS
  adapter, and smoke coverage for managed local-stack turns.

## External Gateway Token Issuance

Tasks:

1. Add a caic voice-token endpoint under caic session auth.
2. Configure caic signing keys for transitional ed25519 scoped voice tokens.
3. Populate `webShell.voiceGateway.tokenEndpoint` for external gateway auth.
4. Make Android request the token before opening an external gateway session.
5. Keep embedded gateway mode token-free.

Acceptance:

- Embedded sessions work without a voice token.
- External sessions fail without a token and succeed with a valid token.
- Token claims bind service kind, service instance, backend origin, audience,
  expiry, subject, and capabilities.
- Reject logs do not leak token contents.

## OAuth JWT Federation

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
4. Keep ed25519 scoped tokens only as a compatibility or offline fallback.

Acceptance:

- Gateway verifies tokens from at least two configured issuers in tests.
- Unknown issuers are rejected before JWKS fetch.
- Rotated JWKS keys are accepted after cache refresh.
- Expired, wrong-audience, and insufficient-scope tokens are rejected.

## Progressive Skill Activation

Tasks:

1. Publish a canonical `SKILL.md` URL for the caic `tasks` tool group.
2. Load all skill names and descriptions at bootstrap.
3. Fetch `SKILL.md` files and parse `gomode.activation` and
   `gomode.mcpServers`.
4. Implement local activation scoring from supported location hints.
5. Cap concurrently active skills.
6. Namespace or reject colliding tool names.
7. Show active skills and tools in native voice state.
8. Deactivate skills when context no longer matches.

Acceptance:

- Single caic skill still activates by default.
- Fake multi-skill manifests activate only matching skills.
- Deactivation removes tools from the voice session.
- Tool-name collisions are deterministic: either namespaced or rejected.

## Local Voice Stack Hardening

Tasks:

1. Run target-Mac smoke tests for managed llama.cpp ASR/LLM and KittenTTS.
2. Compare Qwen3-ASR and Parakeet runtime paths for ASR latency and quality.
3. Keep macOS runtime requirements current for managed llama.cpp and KittenTTS.
4. Improve TTS chunking, backpressure, and perceived latency.
5. Preserve interruption and tool-call behavior across local and Gemini
   backends.

Acceptance:

- Local backend completes a voice turn without Gemini Live on the target Mac.
- Local backend calls client tools and continues after tool results.
- User interruption stops TTS and cancels pending model work.
- Setup and first-turn latency are measured and recorded in smoke-test output.

## Repository Split

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
