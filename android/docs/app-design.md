# Android App Design

The caic Android companion app has two interaction modes:

1. **Voice mode** (primary) — Gemini Live API as voice dispatcher for caic. Manage
   agents by voice while away from the screen.
2. **Screen mode** — Full visual UI with feature parity to the web frontend.

Both share state. Voice actions update the screen; screen navigation is visible to voice.

**Priority**: Voice mode is the app's unique value — the web frontend already covers
screen mode. Get voice working end-to-end first, with just enough screen UI to
configure the server and verify voice actions. Full screen mode comes later.

## Technology Stack

| Layer | Technology |
|-------|-----------|
| Language | Kotlin |
| UI | Jetpack Compose + Material 3 |
| Architecture | MVVM (ViewModel + StateFlow) |
| Networking | caic Kotlin SDK (OkHttp + kotlinx.serialization) |
| Voice | Gemini Live API via OkHttp WebSocket + ephemeral tokens |
| DI | Hilt |
| Navigation | Compose Navigation (type-safe) |
| Background | Foreground Service + coroutines |
| Notifications | Android NotificationManager |
| Persistence | DataStore (settings only) |

## Architecture

```
UI (Compose)
  TaskListScreen, TaskDetailScreen, SettingsScreen, VoiceOverlay
      ↓
ViewModels (StateFlow)
  TaskListVM, TaskDetailVM, SettingsVM, VoiceVM (activity-scoped)
      ↓
Repositories / Services
  TaskRepository, SettingsRepository, VoiceSessionManager
      ↓
SDK / Platform
  ApiClient (SDK), DataStore, Gemini Live Session
```

**UI Layer**: Compose screens observe `StateFlow`. No business logic.

**ViewModel Layer**: Holds UI state as `StateFlow`, launches coroutines for API calls,
SSE subscriptions, voice session management. `VoiceViewModel` scoped to Activity.

**Repository Layer**: `TaskRepository` manages SSE + API calls. `SettingsRepository`
wraps DataStore. `VoiceSessionManager` owns Gemini Live lifecycle + tool execution.

**SDK Layer**: Generated `ApiClient`. Pure Kotlin, no Android dependencies.

## Navigation

- Deep link: `caic://task/{taskId}` (for notifications)
- Voice `set_active_task` navigates programmatically

---

## Phase 1: Voice Mode

### Authentication: Ephemeral Tokens

The Gemini API key lives on the caic backend server, never on the Android device.
The app obtains short-lived ephemeral tokens from the backend:

1. App calls `GET /api/v1/voice/token` on the caic backend
2. Backend calls Gemini's `POST /v1alpha/auth_tokens` with its API key
3. Backend returns the ephemeral token to the app
4. App passes the token as `access_token` query param on the WebSocket URL

Ephemeral tokens are **v1alpha only** — using `v1beta` for `auth_tokens` returns
404. See https://ai.google.dev/gemini-api/docs/ephemeral-tokens.

Token defaults: 2 min to start a session (`newSessionExpireTime`), 30 min for the
session itself (`expireTime`), single use. The app refreshes before expiry.

See `sdk-design.md` for the backend endpoint spec.

### Gemini Live Session

Direct WebSocket connection to the Gemini Live API using OkHttp. No Firebase
dependency, no Google SDK dependency. The protocol is JSON over WebSocket.

#### Why not Firebase AI Logic SDK?

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

#### Audio gotchas

- **AudioTrack must use `USAGE_MEDIA`**, not `USAGE_VOICE_COMMUNICATION`.
  `VOICE_COMMUNICATION` routes through the telephony DSP, clipping the first
  1–2s of playback. This matches the Firebase AI SDK (`AudioHelper.kt`).
  HFP signaling uses `USAGE_VOICE_COMMUNICATION` on the `AudioFocusRequest`
  only, which doesn't affect the playback path.
- **Half-duplex**: mic is paused during model playback. AEC alone is unreliable.
- **Bluetooth disconnect**: car HFP hang-up tears down the SCO audio link but
  does **not** remove the BT device from `AudioDeviceInfo`. The
  `AudioDeviceCallback` won't fire. A `BroadcastReceiver` for
  `ACTION_SCO_AUDIO_STATE_UPDATED` detects this and calls `disconnect()`.
- **Audio focus**: `AUDIOFOCUS_LOSS` and `AUDIOFOCUS_LOSS_TRANSIENT` (incoming
  phone call) both disconnect — a live WebSocket session can't pause.

#### What the server handles (no client implementation needed)

- **VAD**: server-side voice activity detection is on by default. The server
  detects speech onset/offset from the raw PCM stream. Configurable in setup:
  - `realtimeInputConfig.automaticActivityDetection.startOfSpeechSensitivity`:
    `HIGH` (default) or `LOW`
  - `endOfSpeechSensitivity`: how much silence = end of turn
  - `silenceDuration`: explicit silence threshold
  - `prefixPadding`: min speech duration before committing
- **Barge-in**: `realtimeInputConfig.activityHandling`:
  `START_OF_ACTIVITY_INTERRUPTS` (default) or `NO_INTERRUPTION`
- **Turn-taking**: fully server-managed based on VAD signals

#### WebSocket endpoint

```
wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContentConstrained?access_token={ephemeralToken}
```

Ephemeral tokens require `v1alpha` + `BidiGenerateContentConstrained`. The
non-ephemeral path (`v1beta` + `BidiGenerateContent`) uses a raw API key
directly, which we avoid for security.
See https://ai.google.dev/gemini-api/docs/ephemeral-tokens.

#### Protocol

All messages are JSON with exactly one top-level field. The first client message
must be `setup`:

```json
{
  "setup": {
    "model": "models/gemini-2.5-flash-native-audio-preview-12-2025",
    "generationConfig": {
      "responseModalities": ["AUDIO"],
      "speechConfig": { "voiceConfig": { "prebuiltVoiceConfig": { "voiceName": "ORUS" } } }
    },
    "realtimeInputConfig": {
      "automaticActivityDetection": {
        "startOfSpeechSensitivity": "START_SENSITIVITY_HIGH"
      },
      "activityHandling": "START_OF_ACTIVITY_INTERRUPTS"
    },
    "systemInstruction": { "parts": [{ "text": "..." }] },
    "tools": [{ "functionDeclarations": [...] }]
  }
}
```

Server responds with `setupComplete`, then bidirectional streaming begins.

**Client → Server messages**:
- `realtimeInput`: base64 PCM audio chunks
- `toolResponse`: results of function calls
- `clientContent`: text input (optional)

**Server → Client messages**:
- `setupComplete`: session ready
- `serverContent`: audio/text response chunks
- `toolCall`: function call requests with name + args
- `toolCallCancellation`: cancel in-flight tool calls (on barge-in)

See the [WebSocket API reference](https://ai.google.dev/api/live) for full
message schemas.

### System Instruction

See `VoiceSessionManager.buildSystemInstruction()`.

### Function Declarations

See `FunctionDeclarations.kt` for the full list. All tools are `NON_BLOCKING`.

**Scheduling semantics** (from [Live API Tools](https://ai.google.dev/gemini-api/docs/live-tools)):
- `INTERRUPT` — deliver result to user immediately, interrupting current output
- `WHEN_IDLE` — deliver result after the current turn finishes
- `SILENT` — use result without narrating it to the user

Key protocol note: `scheduling` goes **inside the `response` object** of each
function response, not as a sibling to `id`/`name`.

### Task Monitoring & Proactive Notifications

On connect, inject a snapshot of all current tasks so Gemini starts with full
situational awareness:

```
[Current tasks at session start]
- fix-auth-bug (running, 12m, $0.43, claude)
- add-pagination (asking, 5m, $0.12, gemini) — Question: "Should I use cursor or offset?"
- update-deps (terminated, $0.08, claude) — Completed: "Updated all deps to latest"
```

Each line: `- <shortName> (<state>, <elapsed>, <cost>, <harness>)` with an optional
suffix for `asking` (the question text) and `terminated` (the result summary).

Subsequent state transition notifications (diff-based):

- `asking` → `"[Task 'shortName' needs input] Question: ... Options: ..."`
- `waiting` → `"[Task 'shortName' is waiting for input]"`
- `terminated` → `"[Task 'shortName' completed: ...]"`
- `failed` → `"[Task 'shortName' failed: ...]"`

### Voice Overlay

Floating composable, bottom of screen, visible on all screens.

- **Idle**: mic button. Tap to start, long-press for push-to-talk.
- **Active**: pulsing indicator, audio waveform, tool status, "End" button.
- **Speaking**: waveform for output audio.

VAD handles turn-taking. User can barge in while Gemini speaks.
Swipe down or "End" to disconnect.

### Voice + Screen Integration

| Voice action | Screen effect |
|---|---|
| "Create a task..." | Task appears in list, auto-navigate to detail |
| "Show me the test task" | Navigate to task detail |
| "Send it: use JWT tokens" | Input appears in message list |
| "What's the status?" | No screen change |
| "Terminate the auth task" | Task state updates |
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

`TaskMonitorService` foreground service maintains `/api/v1/server/tasks/events` SSE when
backgrounded. Detects state transitions → Android notifications + voice context.

**Notification triggers**:
- `asking`/`waiting` → "Task needs your input"
- `failed` → "Task failed" with error snippet
- `terminated` with result → "Task completed"

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

### TaskList Screen

Main screen. Mirrors sidebar + creation form from web frontend.

**Layout**: TopAppBar ("caic" + settings gear), repo dropdown, model dropdown,
prompt input, "Create Task" button, scrollable task card list, usage bar.

**Task Card** (mirrors `TaskItemSummary.tsx`):
- State indicator dot: `running`→green, `asking`→blue, `failed`→red,
  `terminating`→orange, `terminated`→gray, default→yellow
- Title (first line of prompt), cost, duration
- Repo/branch, harness badge (if not claude), model badge
- Error text in red, plan mode "P" badge

**Token formatting** (match `TaskItemSummary.tsx`):
- `>= 1_000_000` → `"${n/1_000_000}Mt"`
- `>= 1_000` → `"${n/1_000}kt"`
- else → `"${n}t"`

**Elapsed time**: tick every second via `LaunchedEffect`, format as `"15s"`, `"2m 30s"`, `"1h 15m"`.

**Pull-to-refresh**: triggers full task list reload.

### TaskDetail Screen

Real-time agent output for one task. Mirrors `TaskView.tsx`.

**Layout**: TopAppBar (back + task title + plan badge), subtitle (repo, branch, state),
scrollable message list, todo panel, input bar + action buttons.

**SSE Buffer-and-Swap**: buffer events until `system` event with subtype `"ready"`,
then swap atomically. Prevents flash of empty content during replay.

**Message grouping** into `MessageGroup` and `Turn` (mirrors web frontend):
1. Consecutive `text` events → one `TEXT` group
2. `toolUse` starts `TOOL` group; `toolResult` paired by `toolUseID`
3. `usage` events append to preceding group
4. `ask` → `ASK` group; next `userInput` becomes answer
5. `result` events are turn boundaries

**Message rendering by event kind**:

| Kind | Rendering |
|------|-----------|
| `init` | System chip: "Model: claude-opus-4-6, v1.2.3" |
| `text` | Markdown (CommonMark via compose-markdown) |
| `toolUse`+`toolResult` | Expandable card: name, summary, duration, error icon |
| `ask` | Question card with option chips, multi-select, "Other" text field |
| `usage` | Compact token summary inline |
| `result` | Result card: success/error, diff stat, cost, duration, turns |
| `system` | System chip (dimmed); `context_cleared` → divider |
| `userInput` | User message bubble (right-aligned) |
| `todo` | Updates todo panel (not inline) |

**Tool call display** (mirrors `ToolCallBlock`):
- Summary: tool name + extracted detail + duration + error icon
- Detail extraction: filename for Read/Edit/Write, command for Bash,
  URL for WebFetch, pattern for Grep/Glob
- Expandable body: input as key-value or JSON

**Turn elision** (mirrors `ElidedTurn`):
- Previous turns → single tappable row: "N messages, M tool calls"
- Tap expands inline. Current (last) turn always expanded.

**Ask questions** (mirrors `AskQuestionGroup`):
- Header chip, option chips (single/multi-select), "Other" text field
- Submit sends formatted answer via `sendInput`
- After submission: show selected answer (dimmed)

**Input bar**: visible when `waiting`/`asking`. TextField + send button.
Disabled with spinner when `sending`.

**Action buttons**:
- **Sync**: `syncTask()`. If `"blocked"` → safety issues dialog with force option.
- **Terminate**: `terminateTask()` with confirmation dialog.
- **Restart**: `restartTask(prompt)`. Only for terminal states.
- Loading indicator + disable other buttons while in flight (`pendingAction`).
- Errors → Snackbar, auto-dismiss 5s.

**Safety issues dialog**: list each `SafetyIssue` (file, kind icon, detail).
"Cancel" + "Force Sync" buttons.

**Markdown library**: `com.mikepenz:multiplatform-markdown-renderer-m3:0.28.0` (+coil3 variant).
Config: GFM + line breaks (matching web frontend's `marked` with `{ breaks: true, gfm: true }`).

### Settings Screen

Server URL (editable, validated), "Test Connection" button (`listHarnesses()`),
voice toggle + voice selector, notification toggle, version info.

Default empty server URL → setup prompt on first launch.

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

### caic web frontend (behavior reference)
- `frontend/src/App.tsx` — SSE connection, global state, reconnection logic
- `frontend/src/TaskView.tsx` — per-task SSE, buffer-and-swap, message grouping, tool call display
- `frontend/src/TaskItemSummary.tsx` — task card rendering, state colors, token formatting
