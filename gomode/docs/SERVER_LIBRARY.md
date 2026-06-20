# Go Mode Server Library

The Go Mode server library lets a product backend become a Go Mode host. It
publishes the service discovery manifest, provides the voice gateway server
contract, and defines the voice authorization token contract.

caic is the first host. mddb is the next target host. Host-specific auth,
product APIs, hosted frontend content, and MCP resource semantics stay outside
Go Mode.

## Package Boundary

```text
gomode/                    service discovery, token contract, and SDK spec
  discovery.go             manifest types and validation
  handler.go               /.well-known/gomode.json handler
  token.go                 voice token claims, issue, verify helpers
  sdk.go                   discovery SDK spec

gomode/voicegateway/       voice gateway HTTP server and config
  api/v1/                  signaling and data-channel DTOs, SDK spec
  voicertc/                WebRTC bridge and backend adapters
```

## Dependency Rules

- `gomode` root must not import `gomode/voicegateway`.
- `gomode/voicegateway` may import `gomode` for token verification.
- Go Mode packages must not import `backend/internal/*`.
- Host adapters may import Go Mode packages; Go Mode packages must not import
  host adapters.
- Heavy dependencies such as pion, opus, and model/provider clients stay in
  `gomode/voicegateway`.

These rules keep manifest-only hosts cheap and make a later split to
`github.com/caic-xyz/gomode` mechanical.

## Host Responsibilities

A host backend owns:

- product identity: `service` and `serviceVersion`
- product auth and session policy
- hosted frontend content
- product APIs
- MCP tool/resource endpoints
- which tool groups are advertised
- voice gateway deployment mode and URL
- token issuance policy for external gateway mode

The host adapter should be thin. For caic, it builds `gomode.Settings`, exposes
the `tasks` MCP group at `/api/caic/v1/mcp`, and mounts the embedded gateway when
configured.

## Discovery Manifest

The host serves:

```text
GET /.well-known/gomode.json
```

The manifest is a public, cacheable discovery document, not a REST API. It uses
ETag revalidation.

Fields:

- `service`: host product identity, such as `caic`
- `serviceVersion`: optional host version
- `apiVersion`: Go Mode discovery schema version
- `webShell.bridgeVersion`: hosted-frontend/native bridge compatibility
- `webShell.toolGroups`: MCP endpoint catalog
- `webShell.voiceGateway`: selected voice gateway metadata

There is no `webShell.mcp` field. MCP is advertised through
`webShell.toolGroups`.

## Tool Group Manifest Shape

A tool group is a service MCP endpoint treated as a native shell skill: a static
set of tools plus instructions behind one endpoint.

Manifest fields:

- `name`
- `description`
- `endpoint`
- `protocolVersion`
- `authRequired`
- `activation` hints

Rules:

- One endpoint per group.
- Tools are static within a group.
- Active tools change by activating or deactivating groups.
- The shell matches activation hints locally.
- The service must not receive shell route, location, or context just because a
  group was considered.
- Instructions and tool descriptions are untrusted text.

## SDK Surfaces

Keep two SDKs:

- `sdk/gomode`: discovery manifest and settings client.
- `sdk/voicegateway`: voice gateway signaling and data-channel types.

They are distinct wire surfaces and match the package boundary.

## Compatibility

Android treats bootstrap as one of three states:

- **Unvalidated**: WebView may load; native MCP-backed features are disabled.
- **Compatible**: manifest decodes and API/bridge checks pass.
- **Incompatible**: manifest exists but required fields or versions are
  unsupported.

The WebView login path must not depend on MCP discovery succeeding. Native
features show disabled or incompatible state instead of blocking hosted UI load.

## Extraction Target

The package should be extractable to `github.com/caic-xyz/gomode` once two hosts
validate the contract. The extraction should require only a `go.mod` change and
import-path rewrites, not a boundary redesign.
