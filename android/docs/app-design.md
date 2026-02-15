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

```kotlin
sealed class Screen(val route: String) {
    data object TaskList : Screen("tasks")
    data class TaskDetail(val taskId: String) : Screen("tasks/{taskId}")
    data object Settings : Screen("settings")
}
```

- Deep link: `caic://task/{taskId}` (for notifications)
- Voice `set_active_task` navigates programmatically

## Module Structure

```
android/app/src/main/kotlin/com/fghbuild/caic/
├── CaicApp.kt                       # Application class, Hilt entry point
├── MainActivity.kt                  # Single activity, Compose host
├── navigation/NavGraph.kt           # Routes, deep links
├── data/
│   ├── TaskRepository.kt            # SSE connections, API calls
│   └── SettingsRepository.kt        # DataStore wrapper
├── service/TaskMonitorService.kt    # Foreground service for background SSE
├── voice/
│   ├── VoiceSessionManager.kt       # Gemini Live session + tool dispatch
│   ├── VoiceViewModel.kt            # Activity-scoped ViewModel
│   ├── VoiceOverlay.kt              # Floating mic composable
│   ├── FunctionDeclarations.kt      # Tool definitions for Gemini
│   └── FunctionHandlers.kt          # Tool execution implementations
├── ui/
│   ├── theme/Theme.kt               # Material 3, state colors
│   ├── tasklist/
│   │   ├── TaskListScreen.kt
│   │   ├── TaskListViewModel.kt
│   │   ├── TaskCard.kt
│   │   └── UsageBar.kt
│   ├── taskdetail/
│   │   ├── TaskDetailScreen.kt
│   │   ├── TaskDetailViewModel.kt
│   │   ├── MessageList.kt           # Turn/group rendering
│   │   ├── ToolCallCard.kt
│   │   ├── AskQuestionCard.kt
│   │   ├── TodoPanel.kt
│   │   ├── ResultCard.kt
│   │   ├── InputBar.kt
│   │   ├── SafetyDialog.kt
│   │   └── Grouping.kt              # Message/turn grouping logic
│   └── settings/
│       ├── SettingsScreen.kt
│       └── SettingsViewModel.kt
└── util/Formatting.kt               # Token, duration, elapsed formatters
```

---

## Phase 1: Voice Mode

Voice mode is the primary deliverable. This phase produces a working app where the
user can manage all coding agents by voice, with a minimal screen UI for setup and
verification.

### Minimal Screen UI (Phase 1 only)

Just enough UI to support voice development and testing:

- **Settings screen**: server URL input, test connection, voice toggle, voice selector
- **Task list screen**: simple list of tasks (name, state dot, cost) — verifies that
  voice create/terminate/sync actions are reflected on screen
- No TaskDetail screen yet (voice handles all task interaction)

### Settings State & DataStore

```kotlin
data class SettingsState(
    val serverURL: String = "",
    val notificationsEnabled: Boolean = true,
    val voiceEnabled: Boolean = true,
    val voiceName: String = "ORUS",
)

object PreferenceKeys {
    val SERVER_URL = stringPreferencesKey("server_url")
    val NOTIFICATIONS_ENABLED = booleanPreferencesKey("notifications_enabled")
    val LAST_REPO = stringPreferencesKey("last_repo")
    val LAST_HARNESS = stringPreferencesKey("last_harness")
    val VOICE_ENABLED = booleanPreferencesKey("voice_enabled")
    val VOICE_NAME = stringPreferencesKey("voice_name")
}
```

### Authentication: Ephemeral Tokens

The Gemini API key lives on the caic backend server, never on the Android device.
The app obtains short-lived ephemeral tokens from the backend:

1. App calls `GET /api/v1/voice/token` on the caic backend
2. Backend calls Gemini's `POST /v1alpha/auth_tokens` with its API key
3. Backend returns the ephemeral token to the app
4. App uses the token as the API key for the Google AI Client SDK

Token defaults: 1 min to start a session (`newSessionExpireTime`), 30 min for the
session itself (`expireTime`), single use. The app refreshes before expiry.

See `sdk-design.md` for the backend endpoint spec.

### Gemini Live Session

Direct WebSocket connection to the Gemini Live API using OkHttp. No Firebase
dependency, no Google SDK dependency. The protocol is JSON over WebSocket.

Configured with:
- Ephemeral token from caic backend (not a raw API key)
- Native audio I/O (PCM 16kHz in, 24kHz out)
- System instruction (caic domain + tools)
- Function declarations for all caic operations

#### WebSocket endpoint

```
wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?access_token={ephemeralToken}
```

Note: use `v1alpha` (required for ephemeral tokens), not `v1beta`.

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
    "systemInstruction": { "parts": [{ "text": "..." }] },
    "tools": [{ "functionDeclarations": [...] }]
  }
}
```

Server responds with `setupComplete`, then bidirectional streaming begins.

Client sends audio as `realtimeInput` with base64 PCM chunks. Server sends
`serverContent` (audio/text) and `toolCall` messages. Client responds to tool
calls with `toolResponse`.

See the WebSocket API reference for full message schemas.

### System Instruction

```
You are a voice assistant for caic, a system that manages AI coding agents (Claude
Code, Gemini CLI) running in containers. The user is a software engineer who controls
multiple concurrent coding tasks by voice.

You have tools to create tasks, send messages to agents, answer agent questions, check
task status, sync changes, terminate tasks, and restart tasks.

Behavior guidelines:
- Be concise. The user is often away from the screen.
- Summarize task status: state, elapsed time, cost, what agent is doing.
- When agent asks a question, read question and options clearly. Wait for verbal
  answer, then call answer_question.
- Confirm repo and prompt before creating a task.
- Refer to tasks by short name (first few words of prompt).
- Proactively notify when tasks finish or need input.
- For safety issues during sync, describe each issue and ask whether to force.
```

### Function Declarations

11 tools, all `NON_BLOCKING`:

| Tool | Parameters | Scheduling | Purpose |
|------|-----------|------------|---------|
| `list_tasks` | — | `WHEN_IDLE` | Overview of all tasks |
| `create_task` | prompt, repo, model?, harness? | `INTERRUPT` | Start new task |
| `get_task_detail` | task_id | `WHEN_IDLE` | Recent activity for one task |
| `send_message` | task_id, message | `INTERRUPT` | Send input to waiting agent |
| `answer_question` | task_id, answer | `INTERRUPT` | Answer agent's question |
| `sync_task` | task_id, force? | `INTERRUPT` | Push changes to remote |
| `terminate_task` | task_id | `INTERRUPT` | Stop running task |
| `restart_task` | task_id, prompt | `INTERRUPT` | Restart terminated task |
| `get_usage` | — | `WHEN_IDLE` | Check quota utilization |
| `set_active_task` | task_id | `SILENT` | Switch screen to task |
| `list_repos` | — | `WHEN_IDLE` | Available repositories |

### VoiceSessionManager

Owns Gemini Live session lifecycle. Bridges voice ↔ caic API.

- `connect()` → fetch ephemeral token from backend → create Live session
- `startConversation()` → bidirectional audio with `handleFunctionCall` callback
- `handleFunctionCall(call)` → dispatch to handler by name → return `FunctionResponsePart`
- `disconnect()` → stop session
- Token refresh: re-fetch before `expireTime` if session is long-lived

### Task Monitoring & Proactive Notifications

`VoiceSessionManager.onTasksUpdated(tasks)` compares previous/current states.
Injects bracketed text into Gemini session on transitions:

- `asking` → `"[Task 'shortName' needs input] Question: ... Options: ..."`
- `waiting` → `"[Task 'shortName' is waiting for input]"`
- `terminated` → `"[Task 'shortName' completed: ...]"`
- `failed` → `"[Task 'shortName' failed: ...]"`

Gemini uses these to proactively inform the user.

### Task Resolution

Users refer to tasks by natural language. Gemini resolves using `list_tasks` context
and short names from prompt. Ambiguous → Gemini asks conversationally.

### Voice State

```kotlin
data class VoiceState(
    val connected: Boolean = false,
    val listening: Boolean = false,
    val speaking: Boolean = false,
    val activeTool: String? = null,
    val error: String? = null,
)
```

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

`TaskMonitorService` foreground service maintains `/api/v1/events` SSE when
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

### Permissions

```xml
<uses-permission android:name="android.permission.RECORD_AUDIO" />
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE" />
<uses-permission android:name="android.permission.POST_NOTIFICATIONS" />
```

`RECORD_AUDIO` runtime permission on first mic tap.

---

## Phase 2: Screen Mode

Full Compose UI with feature parity to the web frontend. Lower priority — the web
frontend already provides this functionality. Implement after voice mode is working.

### TaskListViewModel

```kotlin
data class TaskListState(
    val tasks: List<TaskJSON> = emptyList(),
    val usage: UsageResp? = null,
    val connected: Boolean = false,
    val reconnecting: Boolean = false,
    val selectedHarness: Harness = Harnesses.Claude,
    val selectedRepo: String = "",
    val selectedModel: String = "",
    val repos: List<RepoJSON> = emptyList(),
    val harnesses: List<HarnessJSON> = emptyList(),
    val submitting: Boolean = false,
    val error: String? = null,
)
```

Subscribes to `/api/v1/events` SSE on init. On `"tasks"` events: update task list.
On `"usage"` events: update usage. Reconnection uses SDK's backoff wrapper.

### TaskDetailViewModel

```kotlin
data class TaskDetailState(
    val task: TaskJSON? = null,
    val messages: List<EventMessage> = emptyList(),
    val turns: List<Turn> = emptyList(),
    val todos: List<TodoItem> = emptyList(),
    val sending: Boolean = false,
    val pendingAction: String? = null,  // "sync" | "terminate" | "restart"
    val actionError: String? = null,
    val safetyIssues: List<SafetyIssue> = emptyList(),
    val inputDraft: String = "",
    val isReady: Boolean = false,
)
```

#### SSE Buffer-and-Swap

Match web frontend (`TaskView.tsx`): buffer events until `system` event with
subtype `"ready"`, then swap atomically. Prevents flash of empty content during replay.

#### Message Grouping

Replicate web frontend grouping into `MessageGroup` and `Turn`:

```kotlin
data class ToolCall(val use: EventToolUse, val result: EventToolResult? = null)

data class MessageGroup(
    val kind: GroupKind,  // TEXT, TOOL, ASK, USER_INPUT, OTHER
    val events: List<EventMessage>,
    val toolCalls: List<ToolCall> = emptyList(),
    val ask: EventAsk? = null,
    val answerText: String? = null,
)

data class Turn(val groups: List<MessageGroup>, val toolCount: Int, val textCount: Int)
```

Grouping rules:
1. Consecutive `text` events → one `TEXT` group
2. `toolUse` starts `TOOL` group; `toolResult` paired by `toolUseID`
3. `usage` events append to preceding group
4. `ask` → `ASK` group; next `userInput` becomes answer
5. `result` events are turn boundaries

Recomputed as derived value when `messages` changes.

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

### Settings Screen

Server URL (editable, validated), "Test Connection" button (`listHarnesses()`),
voice toggle + voice selector, notification toggle, version info.

Default empty server URL → setup prompt on first launch.

### Utilities

#### State Colors

```kotlin
fun stateColor(state: String): Color = when (state) {
    "running" -> Color(0xFFD4EDDA)
    "asking" -> Color(0xFFCCE5FF)
    "failed" -> Color(0xFFF8D7DA)
    "terminating" -> Color(0xFFFDE2C8)
    "terminated" -> Color(0xFFE2E3E5)
    else -> Color(0xFFFFF3CD)
}
```

#### State Detection

```kotlin
val activeStates = setOf("running", "branching", "provisioning",
    "starting", "waiting", "asking", "terminating")
val waitingStates = setOf("waiting", "asking")
```

#### Formatting

```kotlin
fun formatTokens(n: Int): String  // see above
fun formatDuration(ms: Long): String  // "3.1s", "120ms"
fun formatElapsed(ms: Long): String   // "1h 15m", "2m 30s", "15s"
fun formatCost(usd: Double): String   // "$1.23", "<$0.01"
```

#### Tool Call Detail Extraction

```kotlin
fun toolCallDetail(name: String, input: JsonElement): String?
// Read/Edit/Write → file_path
// Bash → command (truncated 60 chars)
// Grep/Glob → pattern
// WebFetch → url
// Task → description
```

#### Markdown

Library: `com.mikepenz:multiplatform-markdown-renderer-m3:0.28.0` (+coil3 variant).
Config: GFM + line breaks (matching web frontend's `marked` with `{ breaks: true, gfm: true }`).

---

## Build Configuration

```kotlin
android {
    compileSdk = 36
    defaultConfig {
        minSdk = 34
        targetSdk = 36
    }
}
```

### Key Dependencies

```kotlin
// Compose
implementation(platform("androidx.compose:compose-bom:2024.12.01"))
implementation("androidx.compose.material3:material3")
implementation("androidx.activity:activity-compose:1.9.3")
implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")
implementation("androidx.navigation:navigation-compose:2.8.5")

// Hilt
implementation("com.google.dagger:hilt-android:2.53.1")
kapt("com.google.dagger:hilt-compiler:2.53.1")
implementation("androidx.hilt:hilt-navigation-compose:1.2.0")

// DataStore
implementation("androidx.datastore:datastore-preferences:1.1.2")

// Markdown (Phase 2)
implementation("com.mikepenz:multiplatform-markdown-renderer-m3:0.28.0")
implementation("com.mikepenz:multiplatform-markdown-renderer-coil3:0.28.0")

// Voice mode uses OkHttp WebSocket directly — no extra SDK dependency.
// OkHttp is already pulled in via the SDK module.
```

## References

### Gemini Live API
- [Live API overview](https://ai.google.dev/gemini-api/docs/live) — getting started, audio config, function calling
- [WebSocket API reference](https://ai.google.dev/api/live) — full message schemas for `BidiGenerateContent`
- [Ephemeral tokens](https://ai.google.dev/gemini-api/docs/ephemeral-tokens) — creating and using short-lived tokens
- [Live API on Android](https://developer.android.com/ai/gemini/live) — Android-specific guide (Firebase-based, for reference only)

### Sample code
- [gemini-live-todo](https://github.com/android/ai-samples/tree/main/samples/gemini-live-todo) — Google's reference app for Live API + function calling on Android (Firebase-based)
- [Firebase AI quickstart — live](https://github.com/firebase/quickstart-android/tree/master/firebase-ai/app/src/main/java/com/google/firebase/quickstart/ai/feature/live) — Firebase Live API sample

### SDKs (for reference, not used directly)
- [google-genai Python SDK — tokens.py](https://github.com/googleapis/python-genai/blob/main/google/genai/tokens.py) — ephemeral token creation implementation
- [google-genai Go SDK](https://github.com/googleapis/go-genai) — Go SDK (no Live API yet, but useful for understanding the Gemini API surface)

### caic web frontend (behavior reference)
- `frontend/src/App.tsx` — SSE connection, global state, reconnection logic
- `frontend/src/TaskView.tsx` — per-task SSE, buffer-and-swap, message grouping, tool call display
- `frontend/src/TaskItemSummary.tsx` — task card rendering, state colors, token formatting
