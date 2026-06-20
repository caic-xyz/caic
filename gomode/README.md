# Go Mode Server Library

`gomode` is the host-neutral server library for Go Mode discovery and voice authorization. It contains the public manifest contract served at `/.well-known/gomode.json` and the scoped voice token helpers shared by hosts and gateways.

## Package responsibilities

- `gomode`: discovery manifest types, manifest HTTP handler, scoped voice token contract, and SDK generation spec.
- `gomode/voicegateway`: voice gateway HTTP server, standalone gateway config, and service-token verification.
- `gomode/voicegateway/api/v1`: voice gateway signaling and data-channel DTOs.
- `gomode/voicegateway/voicertc`: WebRTC bridge and backend adapters.

## Host adapter rules

Hosts build `gomode.Settings` from product state and mount `gomode.NewHandler` at the HTTP root. Keep adapters thin: product auth, product APIs, hosted frontend content, MCP resource semantics, and deployment policy stay in the host repository.

The root package must not import `gomode/voicegateway`, pion, opus, model/provider clients, or `backend/internal/*`. Heavy gateway dependencies belong under `gomode/voicegateway`.
