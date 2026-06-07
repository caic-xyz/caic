# Android App Design

The caic Android companion app has two interaction modes:

1. **Voice mode** (primary) — Gemini Live API as voice dispatcher for caic. Manage
   agents by voice while away from the screen.
2. **Screen mode** — Full visual UI with feature parity to the web frontend.

Both share state. Voice actions update the screen; screen navigation is visible to voice.

**Priority**: Voice mode is the app's unique value — the web frontend already covers
screen mode. Get voice working end-to-end first, with just enough screen UI to
configure the server and verify voice actions.

## Phase 1: Voice Mode (implemented)

### Why not Firebase AI Logic SDK?

The Firebase SDK's `startAudioConversation()` handles AudioRecord/AudioTrack
setup and the base64 encode/decode loop (~100 lines of straightforward Android
audio plumbing). But it requires a Firebase project + `google-services.json`,
and critically does **not** support:
- Ephemeral tokens (our auth model)
- VAD parameter configuration (sensitivity, silence duration, barge-in mode)
- Session resumption
- Context window compression

Going raw gives us full access to the Live API's VAD tuning and ephemeral token
auth, at the cost of implementing the audio plumbing ourselves.

### Voice + Screen Integration

| Voice action | Screen effect |
|---|---|
| "Create a task..." | Task appears in list, auto-navigate to detail |
| "Show me the test task" | Navigate to task detail |
| "Send it: use JWT tokens" | Input appears in message list |
| "What's the status?" | No screen change |
| "Purge the auth task" | Task state updates |
| User taps task in list | Voice session gains context |

`VoiceViewModel` observes `TaskRepository` to keep voice context current.

### Session Lifecycle

1. App launch → if voice enabled → `connect()` (session idle, mic visible)
2. User taps mic → `startAudioConversation()` (bidirectional audio, VAD)
3. User taps End → audio stops, session connected (can resume)
4. App backgrounded → audio stops, session disconnects after 30s idle,
   foreground service continues SSE
5. App foregrounded → reconnect if previously active

### Background SSE & Notifications

`TaskMonitorService` foreground service maintains `/api/caic/v1/server/tasks/events` SSE when
backgrounded. Detects state transitions → Android notifications + voice context.

**Notification triggers**:
- `asking`/`waiting` → "Task needs your input"
- `failed` → "Task failed" with error snippet
- `purged` with result → "Task completed"

Tapping opens `TaskDetail` via deep link.

**Lifecycle**: starts on app launch (if server URL configured), `START_STICKY`,
persistent notification shows connection status.

```kotlin
object NotificationChannels {
    const val MONITOR = "task_monitor"       // Foreground service (silent)
    const val TASK_ALERTS = "task_alerts"    // State changes (default importance)
}
```

## Phase 2: Screen Mode

Full Compose UI with feature parity to the web frontend. Lower priority — the web
frontend already provides this functionality. Implement after voice mode is working.

See `frontend/src/` (SolidJS) as the reference implementation for:
- Event grouping and turn splitting (`frontend/src/grouping.ts`)
- Message rendering and tool call display
- Token formatting and elapsed time
- Markdown rendering config (GFM + line breaks)

### Implementation status

Task list, task detail, message grouping, tool call cards, elided turns,
ask questions, input bar, action buttons, widgets, and screenshot capture
are implemented. The web frontend (`frontend/src/`) is the source of truth
for matching behavior.

---

## References

### Gemini Live API
- [Live API overview](https://ai.google.dev/gemini-api/docs/live) — getting started, audio config, function calling
- [WebSocket API reference](https://ai.google.dev/api/live) — full message schemas for `BidiGenerateContent`
- [Live API Tools](https://ai.google.dev/gemini-api/docs/live-tools) — NON_BLOCKING behavior, scheduling hints
- [Ephemeral tokens](https://ai.google.dev/gemini-api/docs/ephemeral-tokens) — creating and using short-lived tokens
- [Live API on Android](https://developer.android.com/ai/gemini/live) — Android-specific guide (Firebase-based, for reference only)

### Sample code
- [gemini-live-todo](https://github.com/android/ai-samples/tree/main/samples/gemini-live-todo) — Google's reference app for Live API + function calling on Android (Firebase-based)
- [Firebase AI quickstart — live](https://github.com/firebase/quickstart-android/tree/master/firebase-ai/app/src/main/java/com/google/firebase/quickstart/ai/feature/live) — Firebase Live API sample

### SDKs (for reference, not used directly)
- [Firebase AI SDK — AudioHelper.kt](https://github.com/firebase/firebase-android-sdk/blob/main/firebase-ai/src/main/kotlin/com/google/firebase/ai/type/AudioHelper.kt) — reference audio config: `USAGE_MEDIA` for AudioTrack, `VOICE_COMMUNICATION` source for AudioRecord, AEC
- [Firebase AI SDK — LiveSession.kt](https://github.com/firebase/firebase-android-sdk/blob/main/firebase-ai/src/main/kotlin/com/google/firebase/ai/type/LiveSession.kt) — half-duplex mic pause, audio thread priority, playback accumulation
- [google-genai Python SDK — tokens.py](https://github.com/googleapis/python-genai/blob/main/google/genai/tokens.py) — ephemeral token creation implementation
- [google-genai Go SDK](https://github.com/googleapis/go-genai) — Go SDK (no Live API yet, but useful for understanding the Gemini API surface)
