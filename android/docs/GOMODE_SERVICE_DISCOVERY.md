# Go Mode Service Discovery And MCP Tooling

## Goal

Define how Go Mode Android bootstraps a hosted service and then learns service capabilities through MCP.

Go Mode discovery is intentionally small. It answers only: "can this Android shell host this service, and where is the service MCP endpoint?" Service tools, resources, resource subscriptions, and tool calls belong to MCP.

## Root Service Settings

Endpoint:

```text
GET /api/gomode/v1/settings
```

Purpose:

- Identify the service kind and service version.
- Declare Go Mode shell compatibility.
- Provide the service MCP endpoint and initial protocol version.
- Provide the service voice gateway base URL when voice is available.

Root settings are public. They must not contain secrets, user-specific state, task data, or private configuration. MCP and runtime calls may still require the normal service session.

The generated Go Mode SDK and API reference define the wire schema in `sdk/gomode/`. Keep this document focused on ownership and flow, not duplicated JSON examples.

`webShell.mcp.protocolVersion` is the version Android sends in `Mcp-Protocol-Version` and `_meta.io.modelcontextprotocol/protocolVersion` for initial MCP requests. MCP `server/discover` remains authoritative for supported MCP versions and capabilities.

## Division Of Responsibility

Go Mode discovery owns:

- WebView lifecycle compatibility.
- Android permission and native capability gating.
- Native bridge version negotiation.
- Voice gateway selection.
- Halo/BLE, screenshot, and notification ownership.

MCP owns:

- Tool descriptors and JSON schemas via `tools/list`.
- Tool execution via `tools/call`.
- Resource discovery via `resources/list` and `resources/templates/list`.
- Resource reads via `resources/read`.
- Resource/tool invalidation via `subscriptions/listen`.

The voice gateway remains transport-only. It must not grow service-specific tool execution.

## Superseded Design

Do not add Go Mode-specific tool manifests or tool-call endpoints:

```text
GET  /api/gomode/v1/tools/manifest
POST /api/gomode/v1/tools/call
GET  /api/gomode/v1/tools/session
```

Android uses the MCP endpoint advertised by root settings instead:

```text
POST {webShell.mcp.endpoint}  server/discover
POST {webShell.mcp.endpoint}  tools/list
POST {webShell.mcp.endpoint}  tools/call
POST {webShell.mcp.endpoint}  resources/list
POST {webShell.mcp.endpoint}  resources/read
POST {webShell.mcp.endpoint}  subscriptions/listen
```

## Android Bootstrap Flow

1. User configures a service URL in Go Mode settings.
2. Android fetches `/api/gomode/v1/settings`.
3. Android selects a service adapter from `service` and `apiVersion`.
4. Android validates shell compatibility:
   - `bridgeVersion` is supported.
5. Android loads the hosted WebView frontend.
6. Android uses `webShell.mcp` to call MCP `server/discover`.
7. Android calls MCP `tools/list` and `resources/list` when native voice, notifications, or monitoring need service-backed context.
8. Android combines service MCP tools with Android-owned native tools.
9. Android executes service-backed tools through MCP `tools/call`; native-only tools stay in Android.

Go Mode shell code must not import service product SDK DTOs or hard-code product HTTP routes. Product-specific interpretation belongs in the selected service adapter or in the service MCP implementation.

## Learning Resource Subscriptions

Go Mode learns subscribable service state from MCP, not from root settings.

Flow:

1. Call `server/discover`.
2. Confirm `capabilities.resources.subscribe == true` before opening a resource subscription.
3. Call `resources/list`.
4. Let the selected service adapter choose the resource URIs it understands.
5. Read each selected resource with `resources/read`.
6. Open `subscriptions/listen` for those resource URIs.
7. Treat subscription notifications as invalidations. On `notifications/resources/updated`, call `resources/read` again.

The subscription request is a normal MCP `subscriptions/listen` JSON-RPC request with `resourcesListChanged` and the selected `resourceSubscriptions` URIs. The first event is an acknowledgment. Later `notifications/resources/updated` events identify the changed resource by URI.

A coding-service adapter can, for example, look for a task-summary resource in `resources/list`. If that service names the resource `caic://tasks`, the adapter subscribes to `caic://tasks` and re-reads it whenever the MCP stream reports it updated. Other services can expose different URIs without changing Go Mode shell code.

## Voice Gateway Interaction

The discovery contract advertises the voice gateway base URL. Android still talks to the voice gateway API for signaling.

`voiceGateway.url` may be relative to the configured service URL or absolute. A missing URL means voice is unavailable. Android does not need to know whether the gateway is hosted by the same process, the same origin, or another service.

Voice gateway tokens, if needed, remain service-owned and normal-service-authenticated. Tool authorization remains with the service MCP endpoint, not with the voice gateway.

## Security Model

- Root settings are public and non-sensitive.
- MCP requests use the service's normal API authentication when `authRequired` is true.
- MCP tool calls are authorized by the service.
- Tool schemas and resource contents must not expose secrets or private configuration.
- Android must treat service-provided titles, descriptions, schemas, resources, and tool output as untrusted text.
- Service tool calls should include enough service-instance context to prevent accidental cross-service execution.

## Implementation Plan

Backend:

- Serve `GET /api/gomode/v1/settings` with `webShell.mcp`.
- Serve MCP `server/discover`, `tools/list`, `tools/call`, `resources/list`, `resources/read`, and `subscriptions/listen`.
- Prefer event-driven resource invalidation over polling when the service has an internal change notifier.

Android:

- Add a root settings client and DTOs.
- Add an MCP JSON-RPC client that supports both normal JSON responses and POST-based SSE for `subscriptions/listen`.
- Add service adapters that map discovered MCP resources to neutral Go Mode needs such as task monitoring, attention state, or voice context.
- Keep resource payload parsing narrow. Parse generic JSON first; map only the fields the native shell needs.
- Skip MCP setup when `webShell.mcp.endpoint` is absent.

## Testing Plan

Backend:

- Root settings JSON contract tests, including `webShell.mcp`.
- Auth-enabled routing tests proving root settings stays public.
- MCP JSON-RPC tests for discovery, tools, resources, subscriptions, and protocol/header validation.

Android:

- Settings bootstrap fetches root discovery before WebView load.
- Unsupported bridge version shows a visible upgrade message.
- MCP setup is skipped when not advertised.
- Resource subscriptions trigger re-reads after update notifications.
- Voice setup merges service MCP tools and Android-native tools without product SDK imports in shell code.

E2E:

- Fake backend serves root settings and a minimal hosted page.
- Go Mode loads the hosted page after compatibility validation.
- Unsupported compatibility metadata fails visibly.
- Fake MCP resource listing and resource-update notifications drive shell-level monitoring without asserting product routes.

## Open Questions

- Should root discovery also be exposed at `/.well-known/gomode.json` for generic service discovery?
- Should `webShell.mcp.endpoint` allow absolute URLs for delegated MCP services, or remain service-origin-relative?
- Which Android-native capabilities need small capability documents outside root settings?
