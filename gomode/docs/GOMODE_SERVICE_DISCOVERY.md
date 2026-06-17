# Remaining Go Mode MCP Discovery Work

> **Superseded for architecture/ownership by `gomode/docs/DESIGN.md`.** That doc
> defines the Go Mode server library boundary (contract owned by Go Mode, hosts
> are consumers) and the staged extraction. The items below remain valid as
> consumer-side work and will be reassigned by owner (library / caic adapter /
> Android shell) once the extraction lands.

This document tracks unfinished Go Mode discovery work. Root settings, generated SDKs, the backend MCP endpoint, MCP tools/resources, and native voice tool calls already exist.

MCP is mandatory once service metadata is available. Root settings must advertise `webShell.mcp`; Android may reject authenticated services that omit it.

Go Mode metadata and MCP metadata may be auth-gated. Do not put either on the WebView critical path. If root settings or MCP discovery is unavailable because the user is not signed in, load the hosted frontend in a limited, unvalidated mode so the user can log in. Disable native MCP-backed features until root settings and MCP discovery succeed. Individual MCP capabilities remain optional and must be checked before use.

## Root Settings Manifest

Root settings are a static, publicly readable discovery manifest, not a REST
API. The backend serves them at the well-known path `/.well-known/gomode.json`
(RFC 8615), outside the auth-gated API surface and ahead of the `/.well-known/`
catch-all. The response carries `Cache-Control` and an `ETag`; clients may cache
it briefly and revalidate cheaply with `If-None-Match` after likely auth changes.
The body advertises `service`, `apiVersion`, `webShell.bridgeVersion`,
`webShell.toolGroups`, and `webShell.voiceGateway`. Note `service` is the host
product identity used for compatibility; `webShell.toolGroups` is the list of
MCP tool groups that host exposes.

## Auth-Gated Bootstrap

Android bootstrap should support three states:

- **Unvalidated**: the configured URL loads in the WebView, but root settings are not available yet. Native MCP-backed features are disabled.
- **Compatible**: root settings are available, API/bridge compatibility passes, and `webShell.mcp` is present.
- **Incompatible**: root settings are available but API/bridge compatibility fails or mandatory MCP metadata is missing.

Retry root settings after likely auth changes, such as WebView page loads, cookie changes, app resume, or a user action that needs a native MCP-backed feature.

## Android MCP Resource Client

Add Android support for service-backed resources beyond voice tools:

- Call `server/discover` during MCP setup and use its capabilities as the source of truth.
- Call `resources/list` and `resources/templates/list` when native shell features need service state.
- Call `resources/read` for selected resource URIs.
- Support POST-based SSE for `subscriptions/listen`.
- Treat subscription events as invalidations. On `notifications/resources/updated`, re-read the resource.
- Skip resource subscriptions unless `capabilities.resources.subscribe == true`.

## Tool Groups As Skills

`webShell.toolGroups` is a catalog of MCP tool groups. Each group is a skill: a
static set of tools plus instructions behind one endpoint. The shell, not the
service, manages progressive disclosure and decides which groups are active.

- **Discovery**: at bootstrap, load every group's `name` and `description` from
  the manifest. This is cheap and stays in context.
- **Activation**: when current context matches a group's `activation` hints
  (`routes`, `locationTags`, `keywords`), connect to that group's `endpoint`,
  read its `serverInstructions`, and register its `tools/list` into the voice
  session. Matching is on-device; the service never receives context or location.
- **Execution**: the agent calls the group's tools. A group's tools are static,
  so no tool-list-change notifications or subscriptions are needed.
- **Deactivation**: when context no longer matches, drop the group's tools from
  the session. The active tool set changes by activating groups, not by mutating
  a group's tools.

Rules:

- Model each group as its own MCP endpoint so `tools/list` and
  `serverInstructions` are naturally scoped.
- Cap concurrently active groups and order them by relevance; too many tools
  degrades agent tool selection.
- Namespace tools by group to avoid collisions across groups.
- Treat each group's `serverInstructions` and tool descriptions as untrusted
  text injected into the agent.
- Surface which groups and tools are active, especially for voice while the
  screen is off.

caic exposes a single group today; the shell still loads it through the catalog
path, so adding groups later needs no manifest contract change.

## Service Adapters

Add a small adapter layer selected from root settings `service` and `apiVersion`.

Adapters map discovered MCP resources to neutral Go Mode needs:

- task or job monitoring
- attention state
- notification text
- voice context

Keep parsing narrow:

- Parse generic JSON first.
- Map only fields the native shell needs.
- Do not import product SDK DTOs into Go Mode shell code.
- Do not hard-code product HTTP routes.

For caic, the adapter may recognize `caic://tasks` from `resources/list`, read it through `resources/read`, and subscribe to it through `subscriptions/listen`.

## Shell Monitoring And Notifications

Use adapter output to drive native shell behavior:

- Track service state that matters while the WebView is backgrounded.
- Notify when monitored work finishes or needs user input.
- Feed concise state updates into the voice session context.
- Show native MCP-backed affordances, such as voice, as disabled rather than hiding them while metadata is unavailable.
- Treat user-facing MCP text as untrusted.

## Backend Subscription Improvement

`subscriptions/listen` currently works by polling resource snapshots. Replace polling with event-driven invalidation where the backend has a change notifier.

Expected behavior:

- Resource-list changes emit `notifications/resources/list_changed`.
- Resource content changes emit `notifications/resources/updated` for subscribed URIs.
- Streams remain bounded and cancel cleanly when Android disconnects.

## Testing Still Needed

Android unit tests:

- `server/discover` capability negotiation.
- `resources/list` and `resources/read` request envelopes.
- POST-based SSE parsing for `subscriptions/listen`.
- Resource update notification triggers a re-read.
- Missing or unsupported resource capabilities disable monitoring cleanly.

Android/e2e tests:

- Auth-gated root settings still allow the WebView login flow to load.
- Missing MCP metadata fails visibly once authenticated root settings are available.
- Broken MCP disables native MCP-backed features without delaying WebView load.
- Unsupported compatibility metadata fails visibly once root settings are available.
- Fake MCP resource listing drives shell monitoring without product route assertions.
- Fake resource-update notifications update native monitoring state.
- Voice setup merges service MCP tools with Android-native tools without product SDK imports.
- The `toolGroups` catalog decodes and a single group activates and registers its tools.
- `activation` hints select the matching group; non-matching groups stay unloaded.
- Deactivation removes a group's tools from the voice session.
- Untrusted group instructions and tool descriptions are handled as data, not commands.

Backend tests:

- Event-driven resource invalidation once polling is replaced.
- Subscription cancellation cleanup.
