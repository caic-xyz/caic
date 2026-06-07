# Local Voice Stack Plan

This plan defines the remaining gateway-side work to add a local speech stack
behind the voice gateway contract. Android and the hosted web frontend should
keep talking to the generated `sdk/voicegateway` API and data-channel message
types; they should not know which gateway backend serves a session.

## Goal

Support voice sessions through one gateway protocol that can be used by both
Android and browser clients while hiding backend implementation details:

- Android owns microphone permission, audio routing, foreground service
  lifecycle, WebRTC client setup, local tool execution, and service HTTP calls.
- The browser frontend owns browser media APIs, WebRTC client setup, local tool
  execution, and service HTTP calls.
- The gateway owns media conversion, model/provider transport, ASR/LLM/TTS
  orchestration, turn state, interruption handling, and capability reporting.
- Service backends own product APIs, auth, hosted frontend content, service
  context, and tool manifests.

The client-visible contract is a voice-gateway session. It is not Gemini Live,
Gemini Bidi, local cascade, Parakeet, Gemma, Qwen, or any other
provider/runtime.

## Constraints

- The gateway/backend can run on macOS or Linux.
- The first local stack implementation targets macOS.
- Android WebRTC stays in scope for both first backends.
- Browser WebRTC uses the same gateway protocol when hosted frontend voice is
  enabled.
- `sdk/voicegateway/API.md` is the client contract. The gateway is the
  abstraction boundary between clients and model providers.
- Service-specific tool execution stays out of the gateway. Android or the
  frontend executes tool calls and returns tool results.
- Provider names and provider message schemas stay out of Android and frontend
  public types, data-channel messages, compatibility gates, and UI state.
- `/api/voicegateway/v1/...` remains the only advertised voice gateway contract
  until the abstraction and backend split are complete. Add `/api/v2/...` only
  after that work lands.
- The first local stack should be half-duplex unless smoke tests prove that
  streaming ASR and streaming local TTS are reliable on the target Mac.

## Current State

- `android/docs/WEB_SHELL.md` requires Android to own local voice endpoint
  behavior: WebRTC setup, audio routing, microphone permission, and voice
  foreground service.
- The gateway exposes `/api/voicegateway/v1/voice/token`,
  `/api/voicegateway/v1/voice/rtc/offer`, and
  `/api/voicegateway/v1/voice/rtc/{sessionID}`.
- The WebRTC data channel label is `voice-gateway`.
- Android and the frontend send provider-neutral gateway setup, context, and
  tool-result messages over the data channel.
- Generated voice gateway SDKs contain the route-selected v1 protocol types for
  Go, Kotlin, TypeScript, and Swift.
- Android and frontend tool schemas are provider-neutral, but the source names
  still use `FunctionDeclarations`.
- The frontend has golden DTO tests for generated gateway message JSON.
- The checked-in `voicertc` bridge owns WebRTC session lifecycle and delegates
  provider/runtime behavior through a minimal backend connector/session/sink
  boundary.
- The realtime speech bridge adapter owns Gemini Live setup, model constants,
  WebSocket URL, realtime input translation, tool response translation, provider
  event translation, and Gemini PCM JSON conversion.
- Fake backend tests cover the gateway-side connector/session/sink boundary
  without Gemini types.
- Android and frontend `VoiceSession` implementations are still broad
  orchestrators that mix session state, signaling, WebRTC transport, protocol
  encoding/decoding, media endpoint behavior, context, and tool dispatch. This
  is client maintainability work, not a blocker for local gateway backends.
- The gateway does not expose compatibility metadata. Public profile discovery
  is still deferred until profile selection needs it.
- `~/src/genai` has audio modality concepts and provider-specific audio
  support, but it is not yet a complete realtime speech orchestration layer.

## Target Architecture

```text
Go Mode Android
  -> VoiceSession
      -> Android media endpoint
      -> Android WebRTC transport
      -> URL-selected gateway protocol
      -> service tool registry
      -> active service API client

Hosted web frontend
  -> VoiceSession
      -> browser media endpoint
      -> browser WebRTC transport
      -> URL-selected gateway protocol
      -> service tool registry
      -> active service API client

voice-gateway
  -> /api/voicegateway/v1/voice/rtc/offer
  -> WebRTC session manager
  -> gateway protocol normalizer
  -> provider-neutral session core
      -> backend adapter: realtime speech bridge
      -> backend adapter: local cascade

Active service backend
  -> hosted web frontend
  -> service auth
  -> voice-gateway token issuer
  -> product API
  -> service tool manifest
```

Public client contracts:

- WebRTC carries microphone RTP Opus and assistant RTP Opus.
- The data channel label is `voice-gateway`.
- Data-channel messages are UTF-8 gateway JSON messages.
- The HTTP signaling route version selects the data-channel schema before the
  session starts.
- Tool schemas and tool calls use provider-neutral JSON Schema-like shapes.
- Future compatibility metadata advertises voice capabilities, not model
  providers or backend implementation IDs.

Internal gateway contracts:

- A backend adapter maps the normalized session into one provider/runtime.
- The realtime speech bridge backend maps the route-selected gateway protocol
  to the existing Gemini Live path.
- The local cascade backend maps the route-selected gateway protocol to VAD,
  ASR, LLM, and TTS adapters.
- Backend IDs are allowed in gateway config, gateway logs, and gateway tests.
  They are not part of Android or frontend feature gates.

## Client Contract

Android and frontend should continue to use the generated voice gateway SDK and
the route-selected data-channel schema as their abstraction. They should not
branch on backend IDs or provider names.

Optional client cleanup can converge on these conceptual layers even if the
implementations are language-specific:

```text
VoiceSession
  -> VoiceGatewayClient
      -> VoiceGatewaySignaling
      -> VoiceGatewayTransport
      -> VoiceGatewayProtocol
  -> VoiceMediaEndpoint
  -> VoiceToolRegistry
  -> VoiceContextProvider
```

Layer responsibilities:

- `VoiceSession`: user-visible state, lifecycle, transcript state, active tool
  state, reconnect/close behavior, and error surfacing.
- `VoiceGatewayClient`: provider-neutral session orchestration.
- `VoiceGatewaySignaling`: service token fetch,
  `/api/voicegateway/v1/voice/rtc/offer`, and session close HTTP calls.
- `VoiceGatewayTransport`: PeerConnection, data channel, RTP tracks, and media
  device replacement.
- `VoiceGatewayProtocol`: typed encode/decode/validation for route-selected
  gateway messages.
- `VoiceMediaEndpoint`: platform audio capture/playback and mute/device state.
- `VoiceToolRegistry`: tool schema export, tool-call dispatch, and tool-result
  serialization for the active service.
- `VoiceContextProvider`: current task snapshot, default repo/harness/model
  context, notification context, and service metadata.

Client rules:

- No Android or frontend source file should construct provider setup messages.
- No Android or frontend source file should import provider realtime protocol
  types.
- No Android or frontend source file should contain provider WebSocket URLs,
  provider model constants, `BidiGenerateContent`, or provider auth
  parameters.
- No Android or frontend source file should branch on `gemini-live`.
- Provider-specific tool schema conversion happens inside gateway backend
  adapters.

## Gateway Backend Abstraction

The gateway should have a provider-neutral session core and backend adapters.
The core owns WebRTC session wiring, protocol validation, cancellation, queues,
tool-call correlation, and capability projection.

```text
GatewaySession
  -> MediaIO
      -> mic PCM in
      -> assistant PCM out
  -> ClientEventSink
      -> transcripts
      -> assistant text
      -> speech status
      -> tool calls
      -> errors
  -> BackendSession
      -> start
      -> accept client context/tool result/cancel
      -> accept mic audio
      -> close
```

Adapter rules:

- Provider request/response types live under backend adapter packages.
- Provider auth, model names, WebSocket URLs, and token formats are read from
  gateway config or provider-specific runtime config.
- Backend adapters emit normalized gateway events only.
- Backend adapters receive normalized tool results only.
- The gateway core does not know Gemini Bidi message shapes, Parakeet runtime
  details, Gemma serving APIs, or Qwen TTS APIs except through adapter
  interfaces.

### Realtime Speech Bridge Backend

Keep the existing Gemini Live behavior as the first backend adapter:

- Translate normalized session setup into provider setup.
- Translate mic RTP/PCM to provider realtime audio input.
- Translate provider output audio into RTP.
- Translate provider transcripts into normalized transcript events.
- Translate provider function calls into normalized `tool.call` messages.
- Translate normalized `tool.result` messages back into provider tool
  responses.

This preserves existing behavior while moving provider protocol details out of
client and gateway core layers.

### Local Cascade Backend

Start with a half-duplex cascade:

1. Decode incoming RTP Opus to PCM.
2. Run VAD and utterance segmentation.
3. Feed completed utterances to ASR.
4. Send final user text into the local LLM conversation.
5. Let the LLM either produce text or request a tool.
6. If a tool is requested, emit normalized `tool.call` and wait for the client.
7. Feed assistant text to TTS in sentence-sized chunks.
8. Encode generated PCM to RTP Opus and stream it to the client.

The local backend should own all queues and cancellation:

- Barge-in cancels active LLM generation, clears queued TTS, and drains the RTP
  output buffer.
- New user speech during assistant audio sends `interrupted` to the client.
- Tool calls pause assistant generation until the client returns a result.
- ASR, LLM, and TTS adapters receive context cancellation per voice turn.

## Provider-Neutral Data Channel

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
{"kind":"session.ready","profile":"default","capabilities":[...]}
{"kind":"transcript.delta","speaker":"user","text":"..."}
{"kind":"assistant.text.delta","text":"..."}
{"kind":"speech.started","speaker":"assistant"}
{"kind":"speech.ended","speaker":"assistant"}
{"kind":"tool.call","id":"call-id","name":"tasks_list","args":{}}
{"kind":"interrupted","source":"user"}
{"kind":"error","message":"...","recoverable":true}
```

Rules:

- Data-channel messages are UTF-8 JSON.
- Audio never goes through the data channel except optional diagnostics.
- The HTTP signaling route version selects the data-channel schema.
- Unknown `kind` values are ignored only when explicitly marked optional by the
  API version used for the session.
- Tool call IDs are gateway-generated and unique within a session.
- Clients send provider-neutral service tools, not provider function
  declarations.
- `session.ready` reports the public profile used for the session, not the
  provider/backend implementation.

## Future Compatibility Metadata

Gateway compatibility remains deferred until profile selection needs public
feature discovery. When added, it should describe public feature support:

```json
{
  "service": "voice-gateway",
  "serviceKinds": ["caic", "mddb"],
  "profiles": [
    {
      "id": "default",
      "capabilities": [
        "voice.protocol.v1",
        "voice.transport.webrtc",
        "voice.audio.rtpOpus",
        "voice.turns.fullDuplex",
        "voice.transcripts",
        "voice.tools.normalized"
      ]
    },
    {
      "id": "half-duplex-local",
      "capabilities": [
        "voice.protocol.v1",
        "voice.transport.webrtc",
        "voice.audio.rtpOpus",
        "voice.turns.halfDuplex",
        "voice.transcripts",
        "voice.assistantText",
        "voice.tools.normalized"
      ]
    }
  ],
  "defaultProfile": "default"
}
```

Service metadata should advertise a preferred public profile and required
capabilities, not a provider implementation detail:

- `voiceGateway.profilePreferred`: public profile ID.
- `voiceGateway.capabilities`: service-required public capabilities.

Gateway static config may still use internal backend IDs:

```toml
[profiles.default]
backend = "realtime-speech-bridge"

[profiles.half-duplex-local]
backend = "local-cascade"
```

Android and frontend should persist only profile/capability decisions. They
should not persist or display backend IDs.

## Runtime Strategy

Use process adapters so the gateway is not coupled to one runtime or host OS.
The first implementation targets macOS. Linux support should use the same
adapter interfaces with different runtime choices when CUDA or other Linux
accelerators are available.

ASR backend candidates:

- `parakeet-nemo`: target model path. Requires smoke testing on macOS because
  official performance assumptions are CUDA-oriented.
- `parakeet-cpp`: investigate `mudler/parakeet.cpp` as a local C++ Parakeet
  runtime candidate for macOS feasibility.
- `parakeet-onnx`: investigate community or export path for CPU/CoreML/Metal
  feasibility.
- `whisper-cpp`: fallback baseline for macOS latency and audio pipeline
  testing, not the target stack.
- Linux CUDA deployments may use the same ASR adapter contract with
  CUDA-oriented Parakeet/NeMo serving.

LLM backend candidates:

- `llamacpp`: GGUF quantization with Metal.
- `mlx`: MLX-format model or OpenAI-compatible MLX server.
- `openai-compatible`: generic adapter to any local HTTP server.
- Linux deployments may use the same `openai-compatible` adapter with vLLM,
  SGLang, llama.cpp, or another local server.

TTS backend candidates:

- `qwen-tts-python`: target path, smoke tested on macOS CPU/MPS if available.
- `qwen-tts-onnx`: investigate for lower-dependency local serving.
- `dashscope-qwen-tts`: optional hosted fallback for validating gateway
  protocol, not the local target.
- Linux CUDA deployments may use the same TTS adapter contract with the
  official CUDA/PyTorch-oriented Qwen3-TTS path.

## Implementation Phases

### Phase 1: Gateway Core And Backend Adapter Split

Status: done for the first realtime speech bridge backend.

- Introduce a minimal gateway core boundary for backend connector, backend
  session, and gateway event sink behavior.
- Move Gemini Live setup, model constants, WebSocket URL, realtime input
  translation, tool response translation, and provider event translation behind
  a realtime speech bridge backend adapter.
- Keep WebRTC session wiring, data-channel validation, tool-call correlation,
  cancellation, and client event emission in provider-neutral gateway core
  code.
- Move provider config into backend-specific config fields.
- Keep `/api/voicegateway/v1/...` as the advertised contract.

Acceptance:

- Existing voice behavior still works through the realtime speech bridge.
- Provider request/response types live only in backend adapter packages.
- Gateway core tests can run against a fake backend without Gemini types.
- Realtime speech bridge tests cover provider translation at the adapter
  boundary.

### Phase 2: Add Profile Selection

- Add static gateway config mapping public profiles to internal backend IDs.
- Add profile-specific config validation.
- Add compatibility metadata for available public profiles.
- Add session creation errors for unavailable or incompatible profiles.
- Let Android and frontend request a public profile or accept the gateway
  default.

Acceptance:

- Operator can choose which backend serves each public profile in gateway
  config.
- Missing backend dependencies fail at gateway startup or session setup with a
  clear diagnostic.
- Android and frontend disable unsupported voice features from public
  compatibility metadata.
- Android and frontend persist only public profile/capability decisions.

### Phase 3: Build Local Cascade With Fake Model Backends

- Implement VAD and turn segmentation around decoded WebRTC PCM.
- Add fake ASR, fake LLM, and fake TTS adapters.
- Exercise tool-call round trips through the normalized protocol.
- Encode fake TTS PCM to RTP so clients receive assistant audio.

Acceptance:

- End-to-end WebRTC session works without the realtime speech bridge backend.
- Synthetic utterances produce transcripts, tool calls, tool results, and
  assistant audio.
- Barge-in cancels active assistant output.

### Phase 4: Run First-Platform Model Smoke Tests

- Build command-line smoke tests for each model adapter.
- Measure first-token or first-audio latency, total turn latency, CPU/GPU
  usage, memory, and failure modes on the first target Mac.
- Validate whether Parakeet and Qwen3-TTS can run acceptably without CUDA.
- Select initial runtime implementations based on measured behavior.

Acceptance:

- Gemma backend can stream text/tool calls from local macOS serving.
- ASR backend transcribes short microphone-quality clips.
- TTS backend generates playable audio chunks.
- Runtime requirements are documented in gateway config docs, with macOS as the
  first documented platform and Linux as a later deployment target.

### Phase 5: Wire The Local Model Stack

- Replace fake ASR/LLM/TTS with selected first-platform adapters.
- Feed ASR final text into Gemma conversation state.
- Convert Gemma tool-call output into normalized `tool.call`.
- Chunk assistant text into TTS-safe segments.
- Stream TTS output over RTP.

Acceptance:

- Local backend can complete a voice turn without the realtime speech bridge
  backend.
- Local backend can call client tools and continue after tool results.
- Interruptions stop TTS output and cancel pending model work.

### Phase 6: Improve Streaming

Only after half-duplex is reliable:

- Add partial ASR transcript support if the chosen ASR runtime can provide it.
- Start TTS on stable sentence fragments while Gemma continues generating.
- Add latency-aware chunking and backpressure.
- Add diagnostics for ASR, LLM, TTS, and RTP buffer timing.

Acceptance:

- Latency improves without breaking turn correctness.
- Partial transcripts are clearly marked as non-final.
- TTS never speaks text that is later invalidated by a tool result or canceled
  generation.

## Testing

- Go unit tests for protocol encoding, decoding, validation, and unknown-kind
  handling.
- Gateway tests for profile selection, backend selection, compatibility
  metadata, and session setup failures.
- WebRTC bridge tests with fake media and fake model backends.
- Android unit tests for normalized message handling and tool response
  dispatch.
- Frontend unit tests for normalized message handling and tool response
  dispatch.
- Android instrumented smoke test against a fake gateway through WebRTC.
- Frontend e2e test against a fake gateway through WebRTC when browser voice
  remains enabled.
- Runtime smoke tests for ASR, LLM, and TTS adapters, starting with macOS.
- Manual audio tests on real Android hardware for microphone routing,
  Bluetooth/SCO behavior, interruptions, and foreground service behavior.

## Open Decisions

- Exact target Mac model and RAM size for the first implementation.
- Minimum supported Linux runtime profile for later deployments.
- Whether Gemma is served by llama.cpp, MLX, or another OpenAI-compatible local
  server.
- Whether Parakeet is viable on macOS without CUDA at acceptable latency.
- Whether Qwen3-TTS is viable on macOS locally, and whether streaming is
  available in the selected runtime.
- Whether Android and frontend consume a generated service tool manifest or
  maintain separate schema builders tested against the same golden manifest.
- Whether browser voice remains a product feature once Go Mode owns native
  voice, or becomes only a development/debug path.

## Next Step

Start Phase 2:

1. Add static gateway config mapping public profiles to internal backend IDs.
2. Add profile-specific config validation.
3. Add compatibility metadata for available public profiles.
4. Let Android and frontend request a public profile or accept the gateway
   default through the voice gateway contract.

Client `VoiceSession` splitting remains useful cleanup, but it should run as a
separate hardening track after the gateway backend boundary is clear.
