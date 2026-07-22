# Go Mode Android Shell

The Android shell is the native half of Go Mode. It hosts a service frontend in a
WebView and adds native capabilities through service-neutral contracts.

The shell must not import product SDK DTOs, hard-code product REST routes, or
branch on voice provider names. Product-specific state is accessed through the
Go Mode manifest and MCP resources.

## Responsibilities

Android owns:

- service URL bootstrap
- WebView hosting
- native bridge compatibility checks
- microphone permission and audio routing
- WebRTC client setup
- local service tool execution during voice sessions
- notification and background monitoring behavior
- local SKILL.md activation policy

The host owns product auth, hosted UI, MCP endpoints, and product resource
semantics.

## Bootstrap

1. Load the configured service URL in the WebView.
2. Fetch `/.well-known/gomode.json` outside the WebView critical path.
3. Decode and validate Go Mode compatibility.
4. Enable native MCP-backed features only after compatibility passes.
5. Retry manifest fetch after page loads, app resume, network recovery, and user
   actions that need native features.

States:

- **Unvalidated**: manifest is unavailable or not decoded yet. WebView can load;
  native MCP-backed features are disabled.
- **Compatible**: manifest is available, API/bridge versions pass, and the
  skill catalog and voice gateway metadata decode.
- **Incompatible**: manifest is available but required fields or versions are
  unsupported.

## Skills

`webShell.toolGroups` is the bootstrap skill catalog. The canonical skill is a
`SKILL.md` file. The Markdown body is context for the voice/model session; the
frontmatter is the machine-readable contract:

- `name` and `description`
- `gomode.activation`: local `locations[]` hints with either `wifi.ssids` or
  `physicalPosition`; more activation signals can be added later
- `gomode.mcpServers[]`: MCP endpoint, protocol version, auth policy, and tool
  allowlist

See `../examples/tasks/SKILL.md` for a complete example with location-triggered
activation and multiple-MCP data.

Shell behavior:

- Load every skill's `name` and `description` from the manifest at bootstrap.
- Fetch the skill `SKILL.md` from `skillUrl` when available.
- Treat `gomode.activation` as local-only state; do not report considered
  locations to the service.
- On activation, connect to the skill's MCP endpoint, read `serverInstructions`,
  call `tools/list`, and register only tools in `gomode.mcpServers[].tools`.
- Deactivate skills when context no longer matches.
- Cap concurrently active skills.
- Namespace or reject colliding tool names.
- Surface active skills and tools, especially for screen-off voice.

caic currently exposes one `tasks` skill. The shell should still use the catalog
path so more skills can be added without a manifest contract change.

## MCP Client

The Android MCP client should support:

- `server/discover`
- `tools/list`
- `tools/call`
- `resources/list`
- `resources/templates/list`
- `resources/read`
- POST-based SSE `subscriptions/listen`

Use `server/discover` capabilities as the source of truth. Skip unsupported
features cleanly.

Subscription events are invalidations. On `notifications/resources/updated`,
re-read the resource. On `notifications/resources/list_changed`, re-read the
resource list.

## Service Status

The shell reads the generic `gomode://items` resource for native status:

- `id`: stable service-item identity
- `title`: user-visible item label
- `state`: optional user-visible status label
- `needsAttention`: whether the item requires user attention

Hosts map product concepts to this schema. The shell parses only this generic
resource, maps only the native state it needs, and treats all product text as
untrusted. It does not import product SDK DTOs or hard-code product HTTP routes.

## Service Notifications

A host may expose `gomode://notifications`, a JSON array of service-authored
notification events. Each event has a stable `id`, user-visible `title` and
`body`, plus `occurredAt` and `expiresAt` timestamps. The shell treats these
fields as untrusted display data, deduplicates by `id`, and publishes them on
its native alert channel. It does not infer product conditions from other
product resources.

Hosts update this resource through `notifications/resources/updated`; clients
re-read it as an invalidation. Hosts own event generation, retention, and
expiry. The shell owns permission handling and delivery.

## Voice Session Setup

Voice setup uses the manifest, not product constants:

1. Resolve `webShell.voiceGateway.url` relative to the service URL.
2. If external gateway auth is required, request a token from
   `webShell.voiceGateway.tokenEndpoint`.
3. Open the voice gateway signaling route.
4. Attach the `voice-gateway` data channel.
5. Merge active service MCP tools with Android-native tools.
6. Execute service tool calls locally through the active skill MCP endpoint.

The shell must not branch on Gemini, local stack, Parakeet, Qwen, Gemma, or any
other provider/runtime.

## Tests Needed

Unit tests:

- unavailable manifest leaves shell unvalidated
- incompatible versions disable native features
- missing skills disable MCP-backed features
- `server/discover` capability negotiation
- `resources/list` and `resources/read` request envelopes
- POST-based SSE parsing for `subscriptions/listen`
- resource update notification triggers re-read
- unsupported resource capabilities disable monitoring cleanly

Android/e2e tests:

- WebView loads in limited mode without valid manifest
- broken MCP does not block WebView load
- fake MCP resource listing drives monitoring without product REST routes
- fake resource update changes native monitoring state
- voice setup merges service MCP tools with Android-native tools
- multi-skill activation and deactivation change the active tool set
- untrusted skill instructions and tool descriptions are handled as data
