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
