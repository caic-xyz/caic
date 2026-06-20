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
- which Go Mode skills are advertised
- voice gateway deployment mode and URL
- token issuance policy for external gateway mode

The host adapter should be thin. For caic, it builds `gomode.Settings`, exposes
the `tasks` skill through `/api/caic/v1/mcp`, and mounts the embedded gateway
when configured.

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
- `apiVersion`: Go Mode discovery schema version. Clients must reject versions
  they do not explicitly support.
- `webShell.bridgeVersion`: hosted-frontend/native bridge compatibility.
  Clients must reject manifests whose bridge version differs from the native
  bridge they implement.
- `webShell.toolGroups`: bootstrap skill catalog for native features
- `webShell.voiceGateway`: selected voice gateway metadata

There is no `webShell.mcp` field. MCP is advertised as part of Go Mode skills.

## Skill Catalog And SKILL.md

A Go Mode skill is a `SKILL.md` file. Its Markdown body carries human and model
instructions. Its YAML frontmatter carries machine-readable activation hints and
MCP tool wiring.

The manifest keeps a compact `webShell.toolGroups` bootstrap catalog so a client
can check compatibility before loading skill files. Each entry names the skill
and includes the current MCP endpoint contract:

- `name`
- `description`
- `endpoint`
- `protocolVersion`: MCP protocol version spoken by this group endpoint
- `authRequired`
- `skillUrl`: optional URL for the canonical `SKILL.md` file

The authoritative skill context is the `SKILL.md` frontmatter. See
`../examples/tasks/SKILL.md` for location-triggered activation and multiple-MCP
examples.

Rules:

- `gomode.activation` belongs in the skill frontmatter, not in the manifest.
- `gomode.mcpServers[].tools` is an explicit allowlist of MCP tool names that
  become active with the skill.
- The shell matches activation hints locally. The initial schema supports
  `locations[]` entries with either `wifi.ssids` or `physicalPosition` with a
  friendly name and radius in meters; more activation signals can be added later
  without moving activation out of skill frontmatter.
- The service must not receive shell location or context just because a skill
  was considered.
- Instructions, descriptions, and tool schemas are untrusted text.

## SDK Surfaces

Keep two SDKs:

- `sdk/gomode`: discovery manifest, settings client, and SKILL.md frontmatter DTOs.
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
