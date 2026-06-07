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
  orchestration, turn state, and interruption handling.
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
  public types, data-channel messages, and UI state.
- `/api/voicegateway/v1/...` remains the only advertised voice gateway contract
  until the abstraction and backend split are complete. Add `/api/v2/...` only
  after that work lands.
- The first local stack should be half-duplex unless smoke tests prove that
  streaming ASR and streaming local TTS are reliable on the target Mac.

## Current State

**One backend per instance.** A voice-gateway instance serves exactly one
backend, chosen by config. Offering more than one profile (e.g. Gemini Live vs
local models) means running more than one instance and pointing clients at
different URLs. Selection happens by URL, not by a per-request profile.

Phases 1–3 are complete (history is in git):

- The `voicertc` bridge owns WebRTC session lifecycle and delegates
  provider/runtime behavior through a backend connector/session/sink boundary.
  The Gemini Live adapter owns all Gemini Live setup, model constants, WebSocket
  URL, and translation. Gateway core tests run against a fake backend without
  Gemini types.
- Gateway config selects one backend: `backend = "gemini-live"` or
  `backend = "local-cascade"`, validated against the known backend set.
  `NewBridge` constructs exactly that backend; the realtime bridge needs a Gemini
  key, the local cascade needs none.
- The gateway core owns `session.ready`: a provider-neutral readiness signal
  (no profile id, no capabilities). v1 has no capability negotiation; all
  instances share the same baseline protocol.
- A `local-cascade` backend implements a half-duplex cascade: energy
  VAD/segmentation over decoded mic PCM, pluggable ASR/LLM/TTS adapters
  (placeholder implementations for now), normalized `tool.call`/`tool.result`
  round trips, RTP/PCM assistant audio, and barge-in (cancels the active turn,
  clears queued audio, sends `interrupted`).
- Generated voice gateway SDKs (Go, Kotlin, TypeScript, Swift) carry the
  provider-neutral data-channel message types.
- Android and frontend speak the provider-neutral protocol and persist no
  backend IDs. Frontend golden DTO tests cover the data-channel messages.

Not yet done (Phases 4–6): no real local model adapters are wired. The
`local-cascade` backend uses deterministic placeholder ASR/LLM/TTS until a
target Mac and runtimes are selected. `~/src/genai` has audio modality concepts
and provider-specific audio support, but it is not yet a complete realtime
speech orchestration layer.

## Target Architecture

```text
Go Mode Android / Hosted web frontend
  -> VoiceSession
      -> media endpoint
      -> WebRTC transport
      -> URL-selected gateway protocol
      -> service tool registry
      -> active service API client

voice-gateway (one instance == one backend)
  -> /api/voicegateway/v1/voice/rtc/offer
  -> WebRTC session manager
  -> gateway protocol normalizer (session.ready readiness signal)
  -> provider-neutral session core
      -> the one configured backend adapter:
           gemini-live (full duplex) OR local-cascade (half duplex)

Active service backend
  -> hosted web frontend, service auth, token issuer, product API, tool manifest
  -> lists the available gateway URLs (one per backend/profile)
```

Public client contracts:

- WebRTC carries microphone RTP Opus and assistant RTP Opus.
- The data channel label is `voice-gateway`.
- Data-channel messages are UTF-8 gateway JSON messages.
- The HTTP signaling route version selects the data-channel schema before the
  session starts.
- Tool schemas and tool calls use provider-neutral JSON Schema-like shapes.

Internal gateway contracts:

- A backend adapter maps the normalized session into one provider/runtime.
- Backend IDs are allowed in gateway config, gateway logs, and gateway tests.
  They are not part of Android or frontend feature gates.

## Client Contract

Android and frontend use the generated voice gateway SDK and the route-selected
data-channel schema as their abstraction. They must not branch on backend IDs or
provider names.

Client rules (still binding):

- No Android or frontend source file constructs provider setup messages, imports
  provider realtime protocol types, contains provider WebSocket URLs/model
  constants/`BidiGenerateContent`/auth params, or branches on `gemini-live`.
- Provider-specific tool schema conversion happens inside gateway backend
  adapters.
- Clients persist no backend IDs and do not branch on provider/backend names.

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
{"kind":"session.ready"}
{"kind":"transcript.delta","speaker":"user","text":"..."}
{"kind":"assistant.text.delta","text":"..."}
{"kind":"speech.started","speaker":"assistant"}
{"kind":"speech.ended","speaker":"assistant"}
{"kind":"tool.call","id":"call-id","name":"tasks_list","args":{}}
{"kind":"interrupted","source":"user"}
{"kind":"error","message":"...","recoverable":true}
```

## Gateway Config

A gateway instance selects its single backend in static config:

```toml
# Gemini Live (full duplex, needs GEMINI_API_KEY):
backend = "gemini-live"

# or the local cascade (half duplex, no key):
# backend = "local-cascade"
```

v1 has no capability negotiation: all instances share the same baseline protocol
and `session.ready` is a bare readiness signal. If public feature discovery is
needed later, add it under a future API version.

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

## Remaining Phases

The placeholder ASR/LLM/TTS adapters in the `local-cascade` backend
(`backend/internal/voicegateway/voicertc/localcascade.go`) are the seam the next
phases replace. Phases 4–6 require a target Mac and real model runtimes, so they
are not runnable in CI.

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

- Replace the placeholder ASR/LLM/TTS adapters with selected first-platform
  adapters behind the existing `asrAdapter`/`llmAdapter`/`ttsAdapter` seams.
- Feed ASR final text into Gemma conversation state.
- Convert Gemma tool-call output into normalized `tool.call`.
- Chunk assistant text into TTS-safe segments.
- Stream TTS output over RTP.

Acceptance:

- Local backend can complete a voice turn without the Gemini Live backend.
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

- Go unit tests for protocol encoding/decoding/validation, backend config
  validation, and session setup failures (done).
- WebRTC bridge tests with fake media and fake model backends, including the
  local cascade turn flow, tool round trips, and barge-in (done for the cascade
  session; full WebRTC end-to-end still relies on manual/hardware runs).
- Frontend unit tests for normalized data-channel message handling (done).
- Runtime smoke tests for ASR, LLM, and TTS adapters, starting with macOS
  (Phase 4).
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
- How the active service backend advertises and selects among multiple gateway
  instances/URLs (one backend each) now that profile selection is by URL.

## Next Step

Start Phase 4: build a command-line smoke test on the target Mac for one ASR,
one LLM, and one TTS adapter behind the existing cascade adapter interfaces, and
record measured latency/resource use to choose the first real runtimes.
