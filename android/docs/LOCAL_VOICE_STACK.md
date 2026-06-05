# Local Voice Stack Plan

This plan extends the Android Web Shell voice direction with a second
voice-gateway backend. Go Mode Android keeps WebRTC as the transport. The
voice gateway hides whether a session is served by Gemini Live or by a local
stack built from Parakeet STT, Gemma 4 26B-A4B, and Qwen3-TTS.

## Goal

Support two voice gateway session backends behind the same Android contract:

- `gemini-live`: existing Gemini Live transport bridge.
- `local-cascade`: local speech-to-text, text/tool reasoning, and text-to-speech
  orchestration running on the gateway host.

Android must not know the upstream model protocol. Android owns microphone
permission, audio routing, foreground service lifecycle, WebRTC setup, local
tool execution, and service HTTP calls. The gateway owns media conversion,
model transport, ASR/LLM/TTS orchestration, turn state, interruption handling,
and backend capability reporting.

## Constraints

- The gateway/backend can run on macOS or Linux.
- The first local stack implementation targets macOS.
- Android WebRTC stays in scope for both backends.
- The gateway is the abstraction boundary between Android and model providers.
- Service-specific tool execution stays out of the gateway. Android executes
  tool calls and returns tool results.
- The current Gemini-specific data-channel protocol should become an
  implementation detail of the Gemini backend, not the Android contract.
- The first local stack should be half-duplex unless smoke tests prove that
  streaming ASR and streaming local TTS are reliable on the target Mac.

## Current Grounding

- `android/docs/WEB_SHELL.md` already requires Android to own local voice
  endpoint behavior: WebRTC setup, audio routing, microphone permission, and
  voice foreground service.
- The checked-in Android voice session currently sends Gemini Bidi setup and
  tool messages over the WebRTC data channel.
- The checked-in `voicertc` bridge currently dials Gemini Live and converts
  WebRTC Opus RTP to Gemini PCM JSON and back.
- The gateway compatibility metadata currently advertises Gemini-specific
  capability names such as `voice.gatewayGeminiLive`.
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
  -> WebRTC PeerConnection
      -> mic RTP Opus
      -> assistant RTP Opus
      -> provider-neutral data channel
          -> setup/context updates
          -> transcript/status events
          -> normalized tool calls
          -> normalized tool results

voice-gateway
  -> /compat
  -> /offer
  -> WebRTC session manager
  -> voice protocol normalizer
  -> backend selector
      -> Gemini Live backend
          -> Gemini BidiGenerateContent WebSocket
      -> local cascade backend
          -> VAD / turn segmenter
          -> ASR backend: Parakeet target
          -> LLM backend: Gemma 4 26B-A4B via local server
          -> TTS backend: Qwen3-TTS target

Active service backend
  -> hosted web frontend
  -> service auth
  -> voice-gateway token issuer
  -> product API

Go Mode Android tool handlers
  -> receive normalized tool calls
  -> call active service backend APIs
  -> return normalized tool results
```

## Gateway Backends

### Gemini Live Backend

Keep the existing behavior as a backend adapter:

- Translate normalized session setup into Gemini Bidi setup.
- Translate mic RTP to Gemini realtime audio input.
- Translate Gemini output audio into RTP.
- Translate Gemini transcripts into normalized transcript events.
- Translate Gemini function calls into normalized `tool.call` messages.
- Translate normalized `tool.result` messages back into Gemini tool responses.

This preserves the existing path while moving Gemini protocol details out of
Android.

### Local Cascade Backend

Start with a half-duplex cascade:

1. Decode incoming RTP Opus to PCM.
2. Run VAD and utterance segmentation.
3. Feed completed utterances to ASR.
4. Send final user text into the local LLM conversation.
5. Let the LLM either produce text or request a tool.
6. If a tool is requested, emit normalized `tool.call` and wait for Android.
7. Feed assistant text to TTS in sentence-sized chunks.
8. Encode generated PCM to RTP Opus and stream it to Android.

The local backend should own all queues and cancellation:

- Barge-in cancels active LLM generation, clears queued TTS, and drains the RTP
  output buffer.
- New user speech during assistant audio sends `interrupted` to Android.
- Tool calls pause assistant generation until Android returns a result.
- ASR, LLM, and TTS backends receive context cancellation per voice turn.

## Provider-Neutral Data Channel

Replace the Android-visible Gemini data channel with versioned gateway messages.

Client to gateway:

```json
{"version":2,"kind":"session.setup","service":{"kind":"caic","instanceID":"...","baseURL":"..."},"voice":{"name":"...","language":"en"},"tools":[...],"context":{...}}
{"version":2,"kind":"context.update","context":{...}}
{"version":2,"kind":"tool.result","id":"call-id","name":"tasks_list","result":{}}
{"version":2,"kind":"turn.cancel","reason":"user_interruption"}
{"version":2,"kind":"session.close"}
```

Gateway to client:

```json
{"version":2,"kind":"session.ready","backend":"local-cascade","capabilities":[...]}
{"version":2,"kind":"transcript.delta","speaker":"user","text":"..."}
{"version":2,"kind":"transcript.final","speaker":"user","text":"..."}
{"version":2,"kind":"assistant.text.delta","text":"..."}
{"version":2,"kind":"speech.started","speaker":"assistant"}
{"version":2,"kind":"speech.ended","speaker":"assistant"}
{"version":2,"kind":"tool.call","id":"call-id","name":"tasks_list","args":{}}
{"version":2,"kind":"interrupted","source":"user"}
{"version":2,"kind":"error","message":"...","recoverable":true}
```

Rules:

- Data-channel messages are UTF-8 JSON.
- Audio never goes through the data channel except optional diagnostics.
- Message `version` is mandatory.
- Unknown `kind` values are ignored only when explicitly marked optional by the
  negotiated protocol range.
- Tool call IDs are gateway-generated and unique within a session.
- Android may keep using existing local function declaration builders at first,
  but the wire schema must not be Gemini-specific.

## Compatibility Metadata

Extend gateway compatibility with backend and protocol capabilities:

```json
{
  "service": "voice-gateway",
  "gatewayProtocol": 2,
  "serviceKinds": ["caic", "mddb"],
  "backends": [
    {
      "id": "gemini-live",
      "capabilities": [
        "voice.backend.geminiLive",
        "voice.protocol.normalized.v2",
        "voice.realtime.fullDuplex",
        "voice.toolCalls.normalized"
      ]
    },
    {
      "id": "local-cascade",
      "capabilities": [
        "voice.backend.localCascade",
        "voice.protocol.normalized.v2",
        "voice.asr.parakeet",
        "voice.llm.gemma4",
        "voice.tts.qwen3",
        "voice.turns.halfDuplex",
        "voice.toolCalls.normalized"
      ]
    }
  ]
}
```

Service metadata should advertise a preferred backend policy, not a provider
implementation detail:

- `voiceGateway.backendsPreferred`: ordered backend IDs.
- `voiceGateway.minGatewayProtocol`: minimum gateway protocol.
- `voiceGateway.capabilities`: service-required capabilities.
- `voiceGateway.tokenEndpoint`: service-issued token endpoint.

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
- `dashscope-qwen-tts`: optional hosted fallback for validating gateway protocol,
  not the local target.
- Linux CUDA deployments may use the same TTS adapter contract with the
  official CUDA/PyTorch-oriented Qwen3-TTS path.

## Implementation Phases

### Phase 1: Normalize The Android-Gateway Protocol

- Add gateway protocol v2 message types and tests.
- Keep Android WebRTC audio setup unchanged.
- Rename Android data-channel ownership conceptually from Gemini to gateway.
- Add a Gemini backend adapter that maps protocol v2 to current Gemini Live
  messages.
- Keep existing voice behavior passing through the Gemini backend.

Acceptance:

- Gemini Live voice still works.
- Android no longer constructs Gemini setup directly for gateway sessions.
- Tool calls still execute in Android and call the active service backend.
- Gateway `/compat` reports protocol v2 and Gemini backend support.

### Phase 2: Add Backend Selection

- Add static gateway config for backend selection.
- Add backend-specific config validation.
- Add compatibility metadata for available backends.
- Add session creation errors for unavailable or incompatible backends.

Acceptance:

- Operator can choose `gemini-live` or `local-cascade`.
- Missing backend dependencies fail at gateway startup or session setup with a
  clear diagnostic.
- Android disables unsupported voice features from compatibility metadata.

### Phase 3: Build Local Cascade With Fake Model Backends

- Implement VAD and turn segmentation around decoded WebRTC PCM.
- Add fake ASR, fake LLM, and fake TTS adapters.
- Exercise tool-call round trips through the normalized protocol.
- Encode fake TTS PCM to RTP so Android receives assistant audio.

Acceptance:

- End-to-end WebRTC session works without Gemini.
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

- Local backend can complete a voice turn without Gemini.
- Local backend can call Android tools and continue after tool results.
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
- Gateway tests for backend selection, compatibility metadata, and session setup
  failures.
- WebRTC bridge tests with fake media and fake model backends.
- Android unit tests for normalized message handling and tool response dispatch.
- Android instrumented smoke test against a fake gateway through WebRTC.
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
- Whether Android sends tool schemas from copied local declarations only, or
  merges them with future service-provided tool manifests.
- Whether protocol v2 replaces the current Gemini data-channel path in-place or
  runs temporarily beside it for migration.

## Next Step

Implement protocol v2 with Gemini Live as the only concrete backend first. That
isolates Android from Gemini protocol details before adding the local cascade
and keeps the behavior reversible while the macOS model runtime risks are
measured.
