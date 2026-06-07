# Local Voice Stack Plan

This plan defines a provider-neutral voice layer for Android and the hosted web
frontend, then adds multiple gateway backends behind that layer. Gemini Live is
kept as the first backend adapter, but it must no longer be part of any Android
or frontend contract.

## Goal

Support voice sessions through one gateway protocol that can be used by both
Go Mode Android and browser frontend clients:

- Android owns microphone permission, audio routing, foreground service
  lifecycle, WebRTC client setup, local tool execution, and service HTTP calls.
- The browser frontend owns browser media APIs, WebRTC client setup, local tool
  execution, and service HTTP calls.
- The gateway owns media conversion, model/provider transport, ASR/LLM/TTS
  orchestration, turn state, interruption handling, and capability reporting.
- Service backends own product APIs, auth, hosted frontend content, service
  context, and tool manifests.

The client-visible contract is a voice-gateway session. It is not Gemini Live,
Gemini Bidi, local cascade, Parakeet, Gemma, Qwen, or any other provider/runtime.

## Constraints

- The gateway/backend can run on macOS or Linux.
- The first local stack implementation targets macOS.
- Android WebRTC stays in scope for both first backends.
- Browser WebRTC must use the same gateway protocol when the hosted frontend
  exposes voice.
- The gateway is the abstraction boundary between clients and model providers.
- Service-specific tool execution stays out of the gateway. Android or the
  frontend executes tool calls and returns tool results.
- Provider names and provider message schemas stay out of Android and frontend
  public types, data-channel messages, compatibility gates, and UI state.
- Backward compatibility is out of scope until this plan is fully executed.
  During execution, `/api/v1/...` is the only advertised voice gateway contract
  and callers may break across intermediate commits. Add `/api/v2/...` only
  after the new abstraction is complete.
- The first local stack should be half-duplex unless smoke tests prove that
  streaming ASR and streaming local TTS are reliable on the target Mac.

## Current Grounding

- `android/docs/WEB_SHELL.md` requires Android to own local voice endpoint
  behavior: WebRTC setup, audio routing, microphone permission, and voice
  foreground service.
- The checked-in Android voice session sends provider-neutral gateway setup and
  tool messages over the WebRTC data channel.
- The checked-in frontend voice session sends provider-neutral gateway setup and
  tool messages over the WebRTC data channel.
- The checked-in `voicertc` bridge currently dials Gemini Live and converts
  WebRTC Opus RTP to Gemini PCM JSON and back.
- The gateway does not currently expose compatibility metadata. Public profile
  discovery is deferred until profile selection needs it.
- `~/src/genai` has audio modality concepts and provider-specific audio support,
  but it is not yet a complete realtime speech orchestration layer.

Reference facts:

- Parakeet TDT 0.6B v2 is a 600M-parameter NVIDIA ASR model for 16 kHz mono
  WAV/FLAC input with punctuation, capitalization, and word timestamps.
  NVIDIA states these models are optimized for NVIDIA GPU-accelerated systems,
  so macOS viability must be validated separately for the first version. Linux
  CUDA serving remains a supported future deployment target.
  <https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2>
- Gemma 4 26B-A4B is a text/image MoE model with 25.2B total parameters and
  3.8B active parameters. It does not provide native audio; use it only as the
  local reasoning/tool model.
  <https://huggingface.co/google/gemma-4-26B-A4B-it>
- Qwen3-TTS publishes 0.6B and 1.7B TTS models with streaming support in model
  descriptions, but official local examples are CUDA/PyTorch oriented and
  vLLM-Omni currently documents local offline inference only. macOS local
  serving must be validated separately for the first version.
  <https://github.com/QwenLM/Qwen3-TTS>
- llama.cpp supports Apple Silicon through ARM NEON, Accelerate, and Metal.
  <https://github.com/ggml-org/llama.cpp>
- MLX is Apple's array framework optimized for Apple Silicon unified memory.
  <https://opensource.apple.com/projects/mlx/>

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
  -> /api/v1/voice/rtc/offer
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
- Data-channel messages are gateway JSON messages. The HTTP signaling route
  version selects the data-channel schema before the session starts.
- Clients that support multiple gateway route versions should try the newest
  supported `/api/vN/voice/...` route first and fall back only on 404 or 405
  before creating a session.
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

## Client Abstraction Layer

Android and frontend should converge on the same conceptual layers even if the
implementations are language-specific.

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
- `VoiceGatewaySignaling`: service token fetch, `/api/v1/voice/rtc/offer`, and
  session close HTTP calls.
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

- No Android or frontend source file should construct Gemini setup messages.
- No Android or frontend source file should import Gemini Live protocol types.
- No Android or frontend source file should contain Gemini WebSocket URLs,
  Gemini model constants, `BidiGenerateContent`, or provider auth parameters.
- No Android or frontend source file should branch on `gemini-live`.
- The old `FunctionDeclarations` concept becomes provider-neutral service tool
  schema generation. Provider-specific conversion happens inside gateway
  backend adapters.
- The only provider leak tolerated during migration is a temporary internal
  adapter shim with explicit removal criteria in Phase 1.

Expected client file direction:

- Android: rename Gemini-specific session/protocol classes toward
  `VoiceGatewaySession`, `GatewayProtocol`, `ServiceToolSchema`, and
  `ServiceToolHandlers`.
- Frontend: split the current `VoiceSession.ts` into provider-neutral session,
  gateway transport/protocol, service context, and service tools.
- Keep Android/frontend tool semantics in sync by sharing the service tool
  schema source when practical, or by testing both generated schemas against
  the same golden manifest.

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

## Gateway Backends

### Realtime Speech Bridge Backend

Use the existing Gemini Live behavior as the first backend adapter:

- Translate normalized session setup into provider setup.
- Translate mic RTP/PCM to provider realtime audio input.
- Translate provider output audio into RTP.
- Translate provider transcripts into normalized transcript events.
- Translate provider function calls into normalized `tool.call` messages.
- Translate normalized `tool.result` messages back into provider tool
  responses.

This preserves the existing behavior while moving provider protocol details out
of Android and frontend.

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

Replace the Android/frontend-visible provider data channel with versioned
gateway messages.

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
{"kind":"transcript.final","speaker":"user","text":"..."}
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
- Clients send provider-neutral service tools, not Gemini function declarations.
- `session.ready` reports the public profile used for the session, not the
  provider/backend implementation.

## Future Compatibility Metadata

Gateway compatibility is deferred until profile selection needs public feature
discovery. When added, it should describe public feature support:

```json
{
  "service": "voice-gateway",
  "serviceKinds": ["caic", "mddb"],
  "profiles": [
    {
      "id": "default",
      "capabilities": [
        "voice.protocol.v2",
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
        "voice.protocol.v2",
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
- `voiceGateway.tokenEndpoint`: service-issued token endpoint.

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
- `parakeet-onnx`: investigate community or export path for CPU/CoreML/Metal
  feasibility.
- `whisper-cpp`: fallback baseline for macOS latency and audio pipeline testing,
  not the target stack.
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

### Phase 1: Build The Voice Abstraction Layer

- Add route-selected gateway message types, validation, and golden tests.
- Keep the advertised gateway contract on `/api/v1/...` during migration.
- Add provider-neutral service tool schema generation.
- Split Android voice code into session, gateway signaling, gateway transport,
  protocol, media endpoint, context provider, and tool registry layers.
- Split frontend voice code into the same conceptual layers.
- Rename the WebRTC data channel from `gemini` to `voice-gateway`.
- Move Gemini setup, model constants, and Gemini message translation into a
  gateway backend adapter.
- Keep existing behavior passing through the realtime speech bridge backend.

Acceptance:

- Existing voice behavior still works through the gateway.
- Android no longer constructs Gemini setup directly for gateway sessions.
- Frontend no longer constructs Gemini setup directly for gateway sessions.
- Android and frontend no longer branch on Gemini-specific compatibility names.
- Android and frontend use the same route-selected wire messages and service
  tool schema semantics.
- Gateway `/api/v1/...` remains the only advertised contract until the plan is
  complete.

### Phase 2: Add Profile Selection

- Add static gateway config mapping public profiles to internal backends.
- Add profile-specific config validation.
- Add compatibility metadata for available public profiles.
- Add session creation errors for unavailable or incompatible profiles.

Acceptance:

- Operator can choose which backend serves each public profile in gateway
  config.
- Android and frontend request a public profile or accept the gateway default.
- Missing backend dependencies fail at gateway startup or session setup with a
  clear diagnostic.
- Android and frontend disable unsupported voice features from public
  compatibility metadata.

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

### Phase 4: First-Platform Model Smoke Tests

- Build command-line smoke tests for each model adapter.
- Measure first-token or first-audio latency, total turn latency, CPU/GPU usage,
  memory, and failure modes on the first target Mac.
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
- Android unit tests for normalized message handling and tool response dispatch.
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
- Whether browser voice remains a product feature once Go Mode owns native voice,
  or becomes only a development/debug path.

## Next Step

Implement the client abstraction layer with the realtime speech bridge as the
only concrete backend first. That removes provider protocol details from Android
and frontend before adding the local cascade, while keeping current voice
behavior available during migration.
