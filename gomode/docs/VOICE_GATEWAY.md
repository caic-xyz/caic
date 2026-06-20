# Go Mode Voice Gateway

The voice gateway is the service-neutral voice contract for Go Mode clients. The
client-visible protocol is not Gemini Live, Gemini Bidi, local stack, Parakeet,
Gemma, Qwen, or any other provider/runtime.

## Public Contract

Public surface:

- HTTP signaling under `/api/voicegateway/v1/...`
- WebRTC microphone RTP Opus from client to gateway
- WebRTC assistant RTP Opus from gateway to client
- data channel label `voice-gateway`
- UTF-8 JSON data-channel messages from `gomode/voicegateway/api/v1`

The HTTP signaling route version selects the data-channel schema before the
session starts.

## Responsibility Split

The gateway owns:

- HTTP signaling
- WebRTC session management
- media conversion
- provider/runtime transport
- ASR/LLM/TTS orchestration for local stack
- turn state
- interruption handling
- provider-specific tool schema conversion

The client owns:

- microphone permission and capture
- audio output routing
- local service tool execution for active SKILL.md skills
- returning tool results over the data channel
- session close and user cancellation intent

The host owns:

- product auth
- SKILL.md files and MCP tool manifests
- product APIs
- hosted frontend content
- voice token issuance policy

## Deployment Modes

### Embedded Gateway

The host process mounts the voice gateway handlers. RTC routes ride the host's
existing session auth. The discovery manifest usually advertises URL `/` and
sets gateway `authRequired` false.

Use embedded mode for single-host deployments and development.

### External Gateway

A separate process serves the voice gateway. It has no login UI and holds no
host session credentials. It verifies short-lived host-issued voice tokens and
brokers media.

Use external mode for shared gateway deployments, separate scaling, or local
model runtime isolation.

## Authorization

Roles are separate:

- **Host**: authorization server. Authenticates users and mints voice tokens.
- **Go Mode**: token contract. Defines claims and audience.
- **Gateway**: resource server. Verifies tokens and serves media.

Current transitional token:

- ed25519 scoped token
- static `TrustedIssuers` key list in gateway config
- claims bind service kind, service instance, backend origin, subject,
  capabilities, audience, and expiry

Final shared-gateway token:

- OAuth JWT with `aud=voice-gateway`
- short expiry
- narrow voice scopes
- issuer origin allowlist in gateway config
- JWKS discovery through `/.well-known/oauth-authorization-server`
- local verification with cached keys

This is issuer federation, not user SSO. The gateway verifies tokens from trusted
hosts; it does not authenticate users.

## Data Channel Messages

Client to gateway:

```json
{"kind":"session.setup","voice":{"name":"...","language":"en"},"tools":[...],"context":{...}}
{"kind":"context.update","context":{...}}
{"kind":"tool.result","id":"call-id","name":"tasks_list","result":{}}
{"kind":"turn.cancel","reason":"user_interruption"}
{"kind":"session.close"}
```

Gateway to client:

```json
{"kind":"session.ready"}
{"kind":"transcript.delta","speaker":"user","text":"..."}
{"kind":"assistant.text.delta","text":"..."}
{"kind":"speech.started","speaker":"assistant"}
{"kind":"speech.ended","speaker":"assistant"}
{"kind":"tool.call","id":"call-id","name":"tasks_list","args":{}}
{"kind":"interrupted","source":"user"}
{"kind":"error","message":"...","recoverable":true}
```

The client builds `tools` from active SKILL.md frontmatter: each active skill
selects one or more MCP servers and an explicit tool allowlist. The gateway sees
only provider-neutral tool declarations and tool results; it does not fetch skill
files or call service MCP endpoints.

Provider-specific messages stay inside gateway adapters.

## Backend Adapters

The gateway has one configured backend per instance:

- `gemini-live`: full-duplex hosted backend
- `local-stack`: half-duplex local ASR + LLM + TTS orchestration

Clients select a gateway by URL, not by provider name. A host that offers more
than one backend advertises or chooses among multiple gateway URLs outside the
v1 voice session protocol.

Local stack runtime details live in `gomode/docs/VOICE_LOCAL_STACK.md`.

## Tests Needed

- signaling request validation
- data-channel DTO validation
- token verification success and failure cases
- embedded mode without voice token
- external mode with required token
- WebRTC bridge media setup
- tool call and tool result round trip
- interruption cancels active work
- backend adapter errors become recoverable protocol errors where possible
