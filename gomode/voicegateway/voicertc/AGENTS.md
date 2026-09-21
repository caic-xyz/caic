# Voice RTC Bridge

## Gemini Live Protocol

Keep Gemini Live wire types and message translation behavior in sync with
the upstream source in
[`googleapis/go-genai/live.go`](https://github.com/googleapis/go-genai/blob/main/live.go).
Use or create a local checkout of `https://github.com/googleapis/go-genai`
at maintenance time; do not assume it already exists at a fixed path.

Before changing Gemini setup, realtime input, tool call, or tool response
payloads, compare this package's Gemini-specific types and translations against
that checkout's `live.go`, `types.go`, and `live_converters.go`.

## Model upgrades

The Gemini Live model comes from the top-level `model` config key
(`voicegateway.DefaultGeminiModel` when unset) and the adapter qualifies bare
IDs with the `models/` prefix. A model bump is a setup-shape change, not just a
string change, so verify each rule below against primary sources and record
what was consulted.

Current default: `gemini-3.8-live`, upgraded from
`gemini-3.1-flash-live-preview`.

Sources for the 3.8 upgrade, checked September 2026:

- <https://ai.google.dev/gemini-api/docs/live-api/capabilities> - model
  comparison table and capability list.
- <https://ai.google.dev/gemini-api/docs/models/gemini-3.8-live> - model card
  and the "Migrating from Gemini 3.1 Flash Live" section.
- `googleapis/go-genai` at commit `2e52c0acf4` (2026-09-18), files `live.go`,
  `types.go`, `live_converters.go`, and `transformer.go`. `transformer.go`
  qualifies a bare model ID with the `models/` prefix.

Setup-shape rules for `gemini-3.8-live`:

- Omit `thinkingConfig`. The model rejects `thinkingLevel`; the migration guide
  says to omit `thinking_config` entirely.
- Do not send `enableAffectiveDialog` or `proactivity`. Affective dialogue was
  removed from the API, and proactive audio is permanently enabled with an
  explicit `proactive_audio: false` rejected.
- Declarations set `behavior: BLOCKING`. Gemini 3.8 Live defaults to
  `NON_BLOCKING`, but the gateway's tool round trip is synchronous and its
  protocol carries no `willContinue` or scheduling state.
- Pin `turnCoverage` to `TURN_INCLUDES_ONLY_ACTIVITY`. Gemini 3.8 Live otherwise
  includes all video since the last turn, and this adapter sends no video.
- Keep the response modality at `AUDIO`; text reaches clients through output
  audio transcription.

New capabilities that this adapter does not use yet, recorded for future work:

- `behavior: NON_BLOCKING` with `SILENT`, `WHEN_IDLE`, or `INTERRUPT`
  scheduling. Adopting it per tool requires measuring voice tool-call duration
  first: a tool whose measured call exceeds one second should switch to
  NON_BLOCKING. It also requires carrying behavior and scheduling through the
  provider-neutral `ToolDeclaration` in `voicegateway/api/v1` plus a
  `willContinue` tool flow.
- `interactionStatus` (`IN_PROGRESS` or `IDLE`). It matters for
  `gemini-3.8-live-extended-thinking`, where `turnComplete` does not mean the
  session is idle.
- `interimInputTranscription`, low-latency text while the user is speaking.

Keep the model examples in `contrib/voice-gateway-config.toml` and
`contrib/config.toml` in step with the default.
