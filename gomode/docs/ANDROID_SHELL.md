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
- local tool-group activation policy

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
  tool-group catalog and voice gateway metadata decode.
- **Incompatible**: manifest is available but required fields or versions are
  unsupported.

## Tool Groups As Skills

`webShell.toolGroups` is the MCP endpoint catalog. Each group is a skill: a
static tool set plus instructions behind one endpoint.

Shell behavior:

- Load every group's `name` and `description` from the manifest at bootstrap.
- Activate groups locally from supported hints such as route, location tag, or
  keyword.
- On activation, connect to the group's MCP endpoint, read
  `serverInstructions`, and register `tools/list` output into the voice session.
- Deactivate groups when context no longer matches.
- Cap concurrently active groups.
- Namespace or reject colliding tool names.
- Surface active groups and tools, especially for screen-off voice.

caic currently exposes one `tasks` group. The shell should still use the catalog
path so more groups can be added without a manifest contract change.

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

## Service Adapters

A shell-side adapter maps discovered MCP resources to neutral native needs:

- task or job monitoring
- attention state
- notification text
- voice context

Adapter rules:

- Select by manifest `service` and `apiVersion`.
- Parse generic JSON first.
- Map only fields the native shell needs.
- Do not import product SDK DTOs.
- Do not hard-code product HTTP routes.
- Treat all product text as untrusted.

For caic, the adapter may recognize `caic://tasks` from `resources/list`, read it
through `resources/read`, and subscribe to it through `subscriptions/listen`.

## Voice Session Setup

Voice setup uses the manifest, not product constants:

1. Resolve `webShell.voiceGateway.url` relative to the service URL.
2. If external gateway auth is required, request a token from
   `webShell.voiceGateway.tokenEndpoint`.
3. Open the voice gateway signaling route.
4. Attach the `voice-gateway` data channel.
5. Merge active service MCP tools with Android-native tools.
6. Execute service tool calls locally through the active MCP group endpoint.

The shell must not branch on Gemini, local stack, Parakeet, Qwen, Gemma, or any
other provider/runtime.

## Tests Needed

Unit tests:

- unavailable manifest leaves shell unvalidated
- incompatible versions disable native features
- missing tool groups disable MCP-backed features
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
- multi-group activation and deactivation change the active tool set
- untrusted group instructions and tool descriptions are handled as data
