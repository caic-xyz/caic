# Android App Design

## Overview

The caic Android companion app has two interaction modes:

1. **Voice mode** (primary) — The user talks to a Gemini Live API session that acts as a voice dispatcher for caic. Gemini has tools to create tasks, send messages, handle agent questions, check status, and more. The user can manage multiple coding agents entirely by voice while walking, driving, etc.

2. **Screen mode** — Full visual UI with feature parity to the web frontend: real-time task streaming, tool call display, todo panel, task actions, and usage display. Always available as a complement to voice.

Both modes share the same underlying state and repositories. Voice actions update screen UI in real time, and screen interactions are visible context for the voice session.

## Technology Stack

| Layer | Technology |
|-------|-----------|
| Language | Kotlin |
| UI | Jetpack Compose + Material 3 |
| Architecture | MVVM (ViewModel + StateFlow) |
| Networking | caic Kotlin SDK (OkHttp + kotlinx.serialization) |
| Voice | Gemini Live API via Firebase AI Logic |
| DI | Hilt |
| Navigation | Compose Navigation (type-safe) |
| Background | Foreground Service + coroutines |
| Notifications | Android NotificationManager |
| Persistence | DataStore (settings only) |

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  UI Layer (Compose)                                         │
│  ┌───────────┐ ┌────────────┐ ┌──────────┐ ┌────────────┐ │
│  │ TaskList   │ │ TaskDetail │ │ Settings │ │ VoiceOverlay│ │
│  └─────┬─────┘ └─────┬──────┘ └────┬─────┘ └─────┬──────┘ │
│        │              │             │              │         │
│  ┌─────▼──────────────▼─────────────▼──────────────▼──────┐ │
│  │          ViewModels                                     │ │
│  │  TaskListVM  │  TaskDetailVM  │ SettVM │  VoiceVM      │ │
│  └──────────────┼────────────────┼────────┼───────────────┘ │
│                 │                │        │                  │
│  ┌──────────────▼────────────────▼────────▼───────────────┐ │
│  │          Repository / Service Layer                     │ │
│  │  TaskRepository │ SettingsRepo │ VoiceSessionManager   │ │
│  └──────────┬──────┘──────┬───────┘──────────┬────────────┘ │
│             │             │                  │               │
│  ┌──────────▼──────┐ ┌────▼─────┐ ┌─────────▼────────────┐ │
│  │  ApiClient (SDK) │ │ DataStore│ │ Gemini Live Session  │ │
│  └─────────────────┘ └──────────┘ └──────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

### Layer Responsibilities

**UI Layer**: Compose screens observe `StateFlow` from ViewModels. No business logic. `VoiceOverlay` is a persistent floating UI element available on all screens.

**ViewModel Layer**: Holds UI state as `StateFlow`, launches coroutines for API calls, SSE subscriptions, and voice session management. `VoiceViewModel` is scoped to the Activity (shared across all screens).

**Repository Layer**: `TaskRepository` manages SSE connections and API calls. `SettingsRepository` wraps DataStore. `VoiceSessionManager` owns the Gemini Live session lifecycle and tool execution.

**SDK Layer**: Generated `ApiClient` from `sdk-design.md`. Pure Kotlin, no Android dependencies.

## Voice Mode Design

### Gemini Live Session

The app maintains a single Gemini Live API session via Firebase AI Logic. The session is configured with:

- **Native audio** input/output (PCM 16kHz in, 24kHz out)
- **System instruction** describing the caic domain and available tools
- **Function declarations** for all caic operations
- **NON_BLOCKING behavior** for long-running tools so the user can keep talking

```kotlin
val liveModel = Firebase.ai(backend = GenerativeBackend.googleAI()).liveModel(
    modelName = "gemini-2.5-flash-native-audio-preview-12-2025",
    generationConfig = liveGenerationConfig {
        responseModality = ResponseModality.AUDIO
        speechConfig = SpeechConfig(voice = Voice("ORUS"))
    },
    systemInstruction = content { text(SYSTEM_INSTRUCTION) },
    tools = listOf(Tool.functionDeclarations(caicFunctionDeclarations)),
)
```

### System Instruction

```
You are a voice assistant for caic, a system that manages AI coding agents (Claude Code,
Gemini CLI) running in containers. The user is a software engineer who controls multiple
concurrent coding tasks by voice.

You have tools to create tasks, send messages to agents, answer agent questions, check
task status, sync changes, terminate tasks, and restart tasks.

Behavior guidelines:
- Be concise. The user is often away from the screen (walking, driving).
- When reporting task status, summarize: state, elapsed time, cost, what the agent is
  doing (from the latest text/tool events). Don't read tool call details unless asked.
- When an agent asks a question (via the ask_pending notification), read the question
  and options clearly. Wait for the user's verbal answer, then call answer_question.
- When creating a task, confirm the repo and prompt before calling create_task.
- When multiple tasks are running, refer to them by short name (first few words of the
  prompt) so the user can disambiguate.
- Proactively notify when a task finishes or needs input — you'll receive these as
  tool call results from the monitoring tools.
- For safety issues during sync, describe each issue and ask whether to force.
```

### Function Declarations

All functions use `NON_BLOCKING` behavior with appropriate scheduling so the voice conversation continues while caic API calls execute.

#### 1. `list_tasks` — Get all tasks

```kotlin
FunctionDeclaration(
    name = "list_tasks",
    description = "List all coding tasks with their current state, repo, cost, and duration. " +
        "Use this when the user asks what's running, task status, or for an overview.",
    parameters = emptyMap(),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.listTasks()`. Returns summary:
```json
{
  "tasks": [
    {"id": "abc", "shortName": "fix auth bug", "repo": "backend", "branch": "main",
     "state": "running", "elapsed": "5m 30s", "cost": "$0.12", "turns": 8}
  ]
}
```

**Scheduling**: `WHEN_IDLE` — not urgent, report when Gemini finishes speaking.

#### 2. `create_task` — Start a new coding task

```kotlin
FunctionDeclaration(
    name = "create_task",
    description = "Create a new coding task. Spins up a container and starts an AI agent. " +
        "Requires a prompt describing what to do and a repo path.",
    parameters = mapOf(
        "prompt" to Schema.string("What the agent should do"),
        "repo" to Schema.string("Repository path, e.g. '/home/user/myproject'"),
        "model" to Schema.string("Optional model: 'opus', 'sonnet', 'haiku', or empty for default"),
        "harness" to Schema.string("Agent harness: 'claude' (default) or 'gemini'"),
    ),
    optionalParameters = listOf("model", "harness"),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.createTask(...)`. Returns task ID and confirmation. Starts SSE monitoring for the new task.

**Scheduling**: `INTERRUPT` — user wants immediate confirmation that the task started.

#### 3. `get_task_detail` — Get detailed status of a specific task

```kotlin
FunctionDeclaration(
    name = "get_task_detail",
    description = "Get detailed information about a specific task including recent agent " +
        "activity (last few text messages, current tool calls, todo list, errors). " +
        "Use when the user asks what a specific task is doing.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID"),
    ),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Reads the latest messages from the task's SSE event buffer. Builds a summary of recent activity: last text output (truncated to ~500 chars), in-progress tool calls, current todos, and any errors.

**Scheduling**: `WHEN_IDLE`

#### 4. `send_message` — Send a message to a waiting agent

```kotlin
FunctionDeclaration(
    name = "send_message",
    description = "Send a text message to a coding agent that is waiting for input. " +
        "The task must be in 'waiting' or 'asking' state.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID"),
        "message" to Schema.string("The message to send to the agent"),
    ),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.sendInput(id, InputReq(message))`. Returns confirmation or error if task isn't waiting.

**Scheduling**: `INTERRUPT` — confirms the message was sent.

#### 5. `answer_question` — Answer an agent's AskUserQuestion

```kotlin
FunctionDeclaration(
    name = "answer_question",
    description = "Answer a question that a coding agent is asking. The agent presented " +
        "options; provide the selected option label(s) or custom text. " +
        "Use this after reading the question to the user and getting their verbal answer.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID"),
        "answer" to Schema.string("The answer: an option label, comma-separated labels " +
            "for multi-select, or free-form text for 'Other'"),
    ),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Formats the answer the same way the web frontend does (option label or custom text), then calls `apiClient.sendInput(id, InputReq(formattedAnswer))`.

**Scheduling**: `INTERRUPT`

#### 6. `sync_task` — Push agent changes to the remote

```kotlin
FunctionDeclaration(
    name = "sync_task",
    description = "Sync (push) a task's code changes to the remote git repository. " +
        "May be blocked by safety issues (large binaries, secrets). " +
        "If blocked, report the issues and ask the user whether to force.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID"),
        "force" to Schema.boolean("Force sync even with safety issues. Default false."),
    ),
    optionalParameters = listOf("force"),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.syncTask(id, SyncReq(force))`. If response is `"blocked"`, returns the safety issues as structured text for Gemini to read aloud. Gemini then asks the user and can re-call with `force=true`.

**Scheduling**: `INTERRUPT`

#### 7. `terminate_task` — Stop a running task

```kotlin
FunctionDeclaration(
    name = "terminate_task",
    description = "Terminate a running coding task. The agent will be stopped and the " +
        "container will be cleaned up.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID"),
    ),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.terminateTask(id)`.

**Scheduling**: `INTERRUPT`

#### 8. `restart_task` — Restart a completed/failed task

```kotlin
FunctionDeclaration(
    name = "restart_task",
    description = "Restart a task that has terminated or failed, with a new or amended prompt.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID"),
        "prompt" to Schema.string("New prompt for the restarted task"),
    ),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.restartTask(id, RestartReq(prompt))`.

**Scheduling**: `INTERRUPT`

#### 9. `get_usage` — Check API quota utilization

```kotlin
FunctionDeclaration(
    name = "get_usage",
    description = "Check current API usage and quota utilization (5-hour and 7-day windows).",
    parameters = emptyMap(),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.getUsage()`. Formats as readable summary: "5-hour window: 45% used, resets in 2h. 7-day window: 72% used."

**Scheduling**: `WHEN_IDLE`

#### 10. `set_active_task` — Switch which task is displayed on screen

```kotlin
FunctionDeclaration(
    name = "set_active_task",
    description = "Switch the screen to show a specific task's detail view. " +
        "Use when the user says 'show me the auth task' or 'switch to task X'. " +
        "Also useful before reading detailed status.",
    parameters = mapOf(
        "task_id" to Schema.string("The task ID to display"),
    ),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Navigates the Compose UI to `TaskDetail(taskId)`. Returns confirmation.

**Scheduling**: `SILENT` — purely visual, no need to announce.

#### 11. `list_repos` — List available repositories

```kotlin
FunctionDeclaration(
    name = "list_repos",
    description = "List available repositories that can be used for new tasks.",
    parameters = emptyMap(),
    behavior = FunctionCallingBehavior.NON_BLOCKING,
)
```

**Handler**: Calls `apiClient.listRepos()`. Returns repo paths and default branches.

**Scheduling**: `WHEN_IDLE`

### Task Monitoring and Proactive Notifications

The `VoiceSessionManager` monitors all active tasks via SSE and proactively injects context into the Gemini session when important state transitions occur. This is done by sending text messages into the session (not tool calls — Gemini receives these as context updates).

```kotlin
class VoiceSessionManager @Inject constructor(
    private val taskRepository: TaskRepository,
    private val apiClient: ApiClient,
) {
    private var session: LiveSession? = null
    private val prevStates = mutableMapOf<String, String>()

    fun onTasksUpdated(tasks: List<TaskJSON>) {
        for (task in tasks) {
            val prev = prevStates[task.id]
            prevStates[task.id] = task.state

            if (prev == null) continue
            val shortName = task.task.take(40)

            val notification = when {
                // Task started asking a question
                task.state == "asking" && prev != "asking" -> {
                    val ask = taskRepository.getLatestAsk(task.id)
                    if (ask != null) {
                        buildString {
                            append("[Task '$shortName' needs input] ")
                            append("Question: ${ask.questions.first().question} ")
                            append("Options: ${ask.questions.first().options.joinToString { it.label }}")
                        }
                    } else {
                        "[Task '$shortName' is waiting for input]"
                    }
                }
                // Task waiting (no specific question)
                task.state == "waiting" && prev != "waiting" ->
                    "[Task '$shortName' is waiting for input]"
                // Task completed
                task.state == "terminated" && prev != "terminated" ->
                    "[Task '$shortName' completed: ${task.result?.take(100) ?: "no result"}]"
                // Task failed
                task.state == "failed" && prev != "failed" ->
                    "[Task '$shortName' failed: ${task.error?.take(100) ?: "unknown error"}]"
                else -> null
            }

            if (notification != null) {
                session?.sendText(notification)
            }
        }
    }
}
```

These bracketed text injections give Gemini the context to proactively inform the user: "Hey, your auth fix task just finished — it says it fixed the missing token validation. Want me to sync those changes?"

### Resolving Task References

Users refer to tasks by natural language ("the auth task", "that test one", "the first one"). Gemini resolves these using `list_tasks` context. The system instruction tells Gemini to use short names derived from the task prompt.

For ambiguous references, Gemini asks: "I see two tasks related to tests — 'add unit tests for auth' and 'fix flaky integration tests'. Which one?"

No special tool is needed for disambiguation — Gemini handles this conversationally using the task list it retrieves.

### VoiceSessionManager

Owns the Gemini Live session lifecycle and bridges between the voice session and the caic API:

```kotlin
class VoiceSessionManager @Inject constructor(
    private val taskRepository: TaskRepository,
    private val apiClient: ApiClient,
    private val settingsRepository: SettingsRepository,
) {
    private var session: LiveSession? = null
    private val _state = MutableStateFlow(VoiceState())

    val state: StateFlow<VoiceState> = _state.asStateFlow()

    suspend fun connect() {
        val model = buildLiveModel()
        session = model.connect()
        _state.update { it.copy(connected = true) }
    }

    suspend fun startConversation() {
        session?.startAudioConversation(::handleFunctionCall)
    }

    private fun handleFunctionCall(call: FunctionCallPart): FunctionResponsePart {
        val result = runBlocking {
            when (call.name) {
                "list_tasks" -> handleListTasks()
                "create_task" -> handleCreateTask(call.args)
                "get_task_detail" -> handleGetTaskDetail(call.args)
                "send_message" -> handleSendMessage(call.args)
                "answer_question" -> handleAnswerQuestion(call.args)
                "sync_task" -> handleSyncTask(call.args)
                "terminate_task" -> handleTerminateTask(call.args)
                "restart_task" -> handleRestartTask(call.args)
                "get_usage" -> handleGetUsage()
                "set_active_task" -> handleSetActiveTask(call.args)
                "list_repos" -> handleListRepos()
                else -> errorResponse("Unknown function: ${call.name}")
            }
        }
        return FunctionResponsePart(call.name, result)
    }

    fun disconnect() {
        session?.disconnect()
        session = null
        _state.update { it.copy(connected = false) }
    }
}
```

### Voice State

```kotlin
data class VoiceState(
    val connected: Boolean = false,
    val listening: Boolean = false,      // Gemini is listening to user audio
    val speaking: Boolean = false,       // Gemini is producing audio output
    val activeTool: String? = null,      // Currently executing tool call name
    val error: String? = null,
)
```

### Voice Overlay UI

A floating composable anchored to the bottom of the screen, visible on all screens. Minimal footprint when idle, expands during active conversation.

```
Idle (collapsed):
┌──────┐
│  🎙  │   ← Tap to start listening, long-press for push-to-talk
└──────┘

Active (expanded):
┌──────────────────────────────┐
│  ◉ Listening...              │  ← Pulsing indicator
│  ┌──────────────────────┐    │
│  │ ░░░▓▓▓▓░░░▓▓░░░░▓▓░ │    │  ← Audio waveform visualization
│  └──────────────────────┘    │
│  Creating task...            │  ← Current tool execution status
│  [End]                       │  ← End conversation button
└──────────────────────────────┘

Speaking (expanded):
┌──────────────────────────────┐
│  ◉ Speaking...               │
│  ┌──────────────────────┐    │
│  │ ░▓▓░░▓▓▓▓░░▓░░░▓▓▓░ │    │  ← Output waveform
│  └──────────────────────┘    │
│  [End]                       │
└──────────────────────────────┘
```

**Behavior**:
- Tap mic button: starts/resumes audio conversation
- Gemini's VAD (voice activity detection) handles turn-taking automatically
- While Gemini speaks, the user can interrupt by speaking (barge-in)
- Tool execution status shows which caic function is running
- The overlay floats above all screens via `Scaffold` overlay slot
- Swipe down or tap "End" to disconnect the voice session

### Voice + Screen Integration

Voice and screen modes operate on the same state simultaneously:

| Voice action | Screen effect |
|---|---|
| "Create a task to fix the auth bug" | Task appears in task list, auto-navigates to detail |
| "Show me the test task" | Navigates to that task's detail view |
| "Send it: use JWT tokens" | Input appears in task detail message list |
| "What's the status?" | No screen change (voice-only response) |
| "Terminate the auth task" | Task state updates in list and detail view |
| "Sync the changes" | Sync status updates on task detail |
| User taps task in list | Voice session gains context (active task changes) |
| User types message in input bar | Message sent, voice session unaffected |

The `VoiceViewModel` observes `TaskRepository` state to keep the voice session's task list context current. When the user navigates to a task via screen touch, `VoiceSessionManager` notes the active task so Gemini can reference it contextually ("this task" = the one on screen).

### Session Lifecycle

```
App launch
  → If voice enabled in settings:
      → VoiceSessionManager.connect()
      → Session idle (mic button visible but not recording)
  → User taps mic:
      → startAudioConversation() — begins bidirectional audio
      → VAD handles turn-taking
  → User taps End or swipes down:
      → Audio stops, session remains connected (can resume)
  → App backgrounded:
      → Audio stops. Session disconnects after 30s idle.
      → Foreground service continues SSE monitoring for notifications.
  → App foregrounded:
      → Reconnect voice session if it was previously active.
```

## State Management

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

The ViewModel subscribes to the global `/api/v1/events` SSE stream on init. On `"tasks"` events it updates the task list. On `"usage"` events it updates usage. SSE reconnection uses the same backoff as the web frontend (500ms initial, ×1.5, 4s cap).

State transitions (e.g., a task moving to `"asking"`) are detected by comparing previous and current task states, and trigger both Android notifications (via `NotificationService`) and voice session context updates (via `VoiceSessionManager.onTasksUpdated`).

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
    val isReady: Boolean = false,       // true after replay buffer swap
)
```

#### SSE Buffer-and-Swap

Matches the web frontend pattern from `TaskView.tsx`: events from the per-task SSE stream are buffered until a `system` event with subtype `"ready"` arrives. At that point, the buffer is swapped atomically into `messages`. This prevents a flash of empty content during SSE replay.

```kotlin
private fun collectTaskEvents(taskId: String) {
    viewModelScope.launch {
        val buf = mutableListOf<EventMessage>()
        var ready = false
        repository.taskEventsReconnecting(taskId).collect { msg ->
            if (!ready) {
                if (msg.kind == EventKinds.System &&
                    msg.system?.subtype == "ready") {
                    ready = true
                    _state.update { it.copy(messages = buf.toList(), isReady = true) }
                } else {
                    buf.add(msg)
                }
            } else {
                _state.update { s ->
                    s.copy(messages = s.messages + msg)
                }
            }
            // Update todos from todo events
            if (msg.kind == EventKinds.Todo) {
                _state.update { it.copy(todos = msg.todo?.todos.orEmpty()) }
            }
        }
    }
}
```

#### Message Grouping

The web frontend (`TaskView.tsx`) groups consecutive events into `MessageGroup` and `Turn` structures. The app replicates this:

```kotlin
data class ToolCall(
    val use: EventToolUse,
    val result: EventToolResult? = null,
)

data class MessageGroup(
    val kind: GroupKind,         // TEXT, TOOL, ASK, USER_INPUT, OTHER
    val events: List<EventMessage>,
    val toolCalls: List<ToolCall> = emptyList(),
    val ask: EventAsk? = null,
    val answerText: String? = null,
)

data class Turn(
    val groups: List<MessageGroup>,
    val toolCount: Int,
    val textCount: Int,
)
```

Grouping logic:
1. Consecutive `text` events merge into one `TEXT` group.
2. `toolUse` starts a `TOOL` group; subsequent `toolResult` events are paired by `toolUseID`.
3. `usage` events append to the preceding group.
4. `ask` events form an `ASK` group; the next `userInput` event becomes the answer.
5. `result` events are turn boundaries.

This is computed as a derived value whenever `messages` changes.

### VoiceViewModel

```kotlin
@HiltViewModel
class VoiceViewModel @Inject constructor(
    private val voiceSessionManager: VoiceSessionManager,
) : ViewModel() {

    val voiceState: StateFlow<VoiceState> = voiceSessionManager.state

    fun toggleVoice() {
        viewModelScope.launch {
            if (voiceState.value.connected) {
                if (voiceState.value.listening) {
                    voiceSessionManager.stopAudio()
                } else {
                    voiceSessionManager.startConversation()
                }
            } else {
                voiceSessionManager.connect()
                voiceSessionManager.startConversation()
            }
        }
    }

    fun endConversation() {
        voiceSessionManager.disconnect()
    }

    override fun onCleared() {
        voiceSessionManager.disconnect()
    }
}
```

Scoped to the Activity so it survives navigation between screens.

### SettingsViewModel

```kotlin
data class SettingsState(
    val serverURL: String = "",
    val notificationsEnabled: Boolean = true,
    val voiceEnabled: Boolean = true,
    val voiceName: String = "ORUS",
)
```

## Screens

### 1. TaskList Screen

The main screen. Mirrors the web frontend's sidebar + creation form.

**Layout**:
```
┌──────────────────────────────┐
│  caic                   [⚙]  │  ← TopAppBar, settings icon
├──────────────────────────────┤
│  ┌────────────────────────┐  │
│  │ Repo: [dropdown    ▼]  │  │  ← Repo selector
│  │ Model: [default    ▼]  │  │  ← Model selector (default/opus/sonnet/haiku)
│  │ ┌──────────────────┐   │  │
│  │ │ Describe task...  │   │  │  ← Prompt input
│  │ └──────────────────┘   │  │
│  │          [Create Task]  │  │
│  └────────────────────────┘  │
├──────────────────────────────┤
│  ┌────────────────────────┐  │
│  │ ● fix auth bug    0.05$ │  │  ← Task card (state dot, title, cost)
│  │   repo:main  2m 15s     │  │  ← Repo, branch, elapsed
│  ├────────────────────────┤  │
│  │ ● add tests       0.12$ │  │
│  │   repo:feat  5m 30s     │  │
│  └────────────────────────┘  │
├──────────────────────────────┤
│  5h: ██░░ 45%  7d: ███░ 72% │  ← Usage bar
├──────────────────────────────┤
│              [🎙]             │  ← Voice overlay (collapsed)
└──────────────────────────────┘
```

**Task Card** mirrors `TaskItemSummary.tsx`:
- State indicator dot with color mapping:
  - `running` → green
  - `asking` → blue
  - `failed` → red
  - `terminating` → orange
  - `terminated` → gray
  - default → yellow
- Title (first line of task prompt), cost, duration
- Repo/branch, harness badge (if not claude), model badge
- Error text in red (if present)
- Plan mode "P" badge (if `inPlanMode`)

**Token Formatting** (matching `TaskItemSummary.tsx`):
- `>= 1_000_000` → `"${n/1_000_000}Mt"`
- `>= 1_000` → `"${n/1_000}kt"`
- else → `"${n}t"`

**Elapsed Time**: Updates every second via `LaunchedEffect` tick, formatted as the web frontend's `formatElapsed`: `"15s"`, `"2m 30s"`, `"1h 15m"`.

**Pull-to-Refresh**: Triggers full task list reload.

### 2. TaskDetail Screen

Displays real-time agent output for a single task. Mirrors `TaskView.tsx`.

**Layout**:
```
┌──────────────────────────────┐
│  [←] fix auth bug       [P]  │  ← TopAppBar, back, plan mode badge
│  repo:main  ● running        │  ← Subtitle: repo, branch, state
├──────────────────────────────┤
│                               │
│  ┌ Turn 1 ──────────────┐    │  ← Collapsed previous turns
│  │ 3 messages, 5 tools   │    │
│  └───────────────────────┘    │
│                               │
│  ▸ Read src/main.go    1.2s  │  ← Tool call (collapsed)
│  ▸ Edit src/main.go    0.5s  │
│  ▸ Bash: go test       3.1s  │
│                               │
│  The authentication bug was   │  ← Text message (markdown)
│  caused by missing token...   │
│                               │
│  ┌ Todo ─────────────────┐   │  ← Todo panel
│  │ ✓ Write failing test  │   │
│  │ ● Fix auth handler    │   │
│  │ ○ Run full test suite │   │
│  └───────────────────────┘   │
│                               │
├──────────────────────────────┤
│  ┌──────────────────┐        │
│  │ Type message...   │  [▶]  │  ← Input (when waiting/asking)
│  └──────────────────┘        │
│  [Sync] [Terminate]          │  ← Action buttons
├──────────────────────────────┤
│              [🎙]             │  ← Voice overlay (collapsed)
└──────────────────────────────┘
```

**Message Rendering** by event kind:

| Kind | Rendering |
|------|-----------|
| `init` | System chip: "Model: claude-opus-4-6, v1.2.3" |
| `text` | Markdown text block (CommonMark via `compose-markdown` library) |
| `toolUse` + `toolResult` | Expandable card: tool name, summary detail, duration, error indicator |
| `ask` | Question card with option chips; multi-select support; "Other" text field |
| `usage` | Compact token summary (inline, appended to preceding block) |
| `result` | Result card: success/error, diff stat, cost, duration, turns |
| `system` | System chip (dimmed); `context_cleared` shows divider |
| `userInput` | User message bubble (right-aligned) |
| `todo` | Updates the todo panel (not rendered inline) |

**Tool Call Display** (mirrors `ToolCallBlock` from `TaskView.tsx`):
- Summary line: tool name + extracted detail + duration + error icon
- Detail extraction (`toolCallDetail`): shows filename for Read/Edit/Write, command for Bash, URL for WebFetch, pattern for Grep/Glob
- Expandable body: input parameters as key-value pairs (flat) or JSON (nested)
- Tool groups: multiple consecutive tool calls show count in header, expand to list

**Turn Elision** (mirrors `ElidedTurn` from `TaskView.tsx`):
- Previous turns collapse to a single tappable row: "N messages, M tool calls"
- Tapping expands the turn inline
- Current (last) turn always shows all groups

**Ask Questions** (mirrors `AskQuestionGroup`):
- Renders each question with header chip
- Options as selectable chips (single or multi-select)
- "Other" option opens text field
- Submit sends formatted answer via `sendInput`
- After submission, shows the selected answer (dimmed)
- In voice mode, Gemini reads the question aloud and the user answers verbally

**Input Bar**:
- Visible when task state is `waiting` or `asking`
- TextField with send button
- Character count indicator
- Disabled with spinner when `sending`

**Action Buttons**:
- **Sync**: Calls `syncTask`. If response is `"blocked"`, shows safety issues dialog with force option.
- **Terminate**: Calls `terminateTask`. Confirmation dialog first.
- **Restart**: Calls `restartTask` with prompt. Only shown for terminal states.
- Actions show loading indicator and disable other buttons while in flight (`pendingAction`).
- Errors display as Snackbar, auto-dismiss after 5s (matching web frontend's `actionError` timeout).

**Safety Issues Dialog** (mirrors web frontend):
- Lists each `SafetyIssue` with file path, kind icon (binary/secret), detail
- "Cancel" and "Force Sync" buttons
- Force Sync calls `syncTask(id, SyncReq(force = true))`

### 3. Settings Screen

**Layout**:
```
┌──────────────────────────────┐
│  [←] Settings                │
├──────────────────────────────┤
│                               │
│  Server                       │
│  ┌──────────────────────────┐│
│  │ http://100.64.0.1:8080   ││  ← Editable, validated
│  └──────────────────────────┘│
│  [Test Connection]            │
│                               │
│  Voice                        │
│  ┌──────────────────────────┐│
│  │ Voice mode enabled  [✓]  ││
│  │ Voice: [Orus         ▼]  ││  ← Puck, Charon, Kore, Fenrir, Orus
│  └──────────────────────────┘│
│                               │
│  Notifications               │
│  ┌──────────────────────────┐│
│  │ Task needs input    [✓]  ││
│  └──────────────────────────┘│
│                               │
│  About                        │
│  Version: 1.0.0               │
│  Server: caic v0.3.2          │
│                               │
└──────────────────────────────┘
```

**Server URL**:
- Persisted in DataStore
- Validated: must be valid HTTP/HTTPS URL
- "Test Connection" calls `listHarnesses()` — success shows green check, failure shows error message
- Default empty; app shows setup prompt on first launch

**Voice**:
- Toggle to enable/disable voice mode (hides/shows mic overlay)
- Voice selector for Gemini Live TTS voice

**Notifications**:
- Toggle for task-needs-input notifications
- Persisted in DataStore

## Navigation

```kotlin
sealed class Screen(val route: String) {
    data object TaskList : Screen("tasks")
    data class TaskDetail(val taskId: String) : Screen("tasks/{taskId}")
    data object Settings : Screen("settings")
}
```

- `TaskList → TaskDetail`: tap task card
- `TaskDetail → TaskList`: back button or gesture
- `TaskList → Settings`: gear icon in top bar
- Deep link: `caic://task/{taskId}` opens TaskDetail directly (for notifications)
- Voice `set_active_task` tool: programmatic navigation to TaskDetail

## Background SSE and Notifications

### Foreground Service

A foreground service maintains the global SSE connection (`/api/v1/events`) when the app is backgrounded. This enables notifications for state transitions.

```kotlin
class TaskMonitorService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForeground(NOTIFICATION_ID, buildPersistentNotification())
        scope.launch {
            repository.globalEventsReconnecting().collect { event ->
                when (event.type) {
                    "tasks" -> {
                        checkForNotifications(event.tasks)
                        voiceSessionManager.onTasksUpdated(event.tasks)
                    }
                }
            }
        }
        return START_STICKY
    }
}
```

**Notification Triggers**:
- Task transitions to `"asking"` or `"waiting"` → "Task needs your input" notification
- Task transitions to `"failed"` → "Task failed" notification with error snippet
- Task transitions to `"terminated"` with result → "Task completed" notification

**Notification Content**:
```
┌──────────────────────────────┐
│ caic: Task needs input       │
│ fix auth bug                  │
│ The agent is asking: Which... │
│ [Open]                        │
└──────────────────────────────┘
```

Tapping opens `TaskDetail` via deep link.

**Lifecycle**:
- Service starts when app launches (if server URL is configured)
- Stops when user explicitly disconnects or clears server URL
- Uses `START_STICKY` to restart after system kill
- Persistent notification shows connection status: "Connected to caic" / "Reconnecting..."

### Notification Channels

```kotlin
object NotificationChannels {
    const val MONITOR = "task_monitor"          // Foreground service (silent)
    const val TASK_ALERTS = "task_alerts"       // Task state changes (default importance)
}
```

## Module Structure

```
android/
├── docs/
│   ├── sdk-design.md
│   └── app-design.md
├── sdk/                                    # Pure Kotlin module (no Android deps)
│   ├── build.gradle.kts
│   └── src/main/kotlin/com/caic/sdk/
│       ├── Types.kt                        # Generated
│       └── ApiClient.kt                    # Generated
├── app/
│   ├── build.gradle.kts
│   └── src/main/kotlin/com/caic/app/
│       ├── CaicApp.kt                      # Application class, Hilt entry point
│       ├── MainActivity.kt                 # Single activity
│       ├── navigation/
│       │   └── NavGraph.kt                 # Screen routes, deep links
│       ├── data/
│       │   ├── TaskRepository.kt           # SSE connections, API calls
│       │   └── SettingsRepository.kt       # DataStore wrapper
│       ├── service/
│       │   └── TaskMonitorService.kt       # Foreground service for background SSE
│       ├── voice/
│       │   ├── VoiceSessionManager.kt      # Gemini Live session + tool dispatch
│       │   ├── VoiceViewModel.kt           # Activity-scoped ViewModel
│       │   ├── VoiceOverlay.kt             # Floating mic UI composable
│       │   ├── FunctionDeclarations.kt     # Tool definitions for Gemini
│       │   └── FunctionHandlers.kt         # Tool execution implementations
│       ├── ui/
│       │   ├── theme/
│       │   │   └── Theme.kt               # Material 3 theme, state colors
│       │   ├── tasklist/
│       │   │   ├── TaskListScreen.kt       # Task list + creation form
│       │   │   ├── TaskListViewModel.kt
│       │   │   ├── TaskCard.kt             # Single task summary card
│       │   │   └── UsageBar.kt             # Quota utilization display
│       │   ├── taskdetail/
│       │   │   ├── TaskDetailScreen.kt     # Full task view
│       │   │   ├── TaskDetailViewModel.kt
│       │   │   ├── MessageList.kt          # Turn/group rendering
│       │   │   ├── ToolCallCard.kt         # Expandable tool call
│       │   │   ├── AskQuestionCard.kt      # Interactive question UI
│       │   │   ├── TodoPanel.kt            # Todo list display
│       │   │   ├── ResultCard.kt           # Task result summary
│       │   │   ├── InputBar.kt             # User input + action buttons
│       │   │   ├── SafetyDialog.kt         # Safety issues warning
│       │   │   └── Grouping.kt             # Message/turn grouping logic
│       │   └── settings/
│       │       ├── SettingsScreen.kt
│       │       └── SettingsViewModel.kt
│       └── util/
│           └── Formatting.kt              # Token, duration, elapsed formatters
└── build.gradle.kts                        # Root build file
```

## Key Implementation Details

### State Colors (Theme.kt)

Matching `stateColor()` from `TaskItemSummary.tsx`:

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

### Active/Waiting State Detection

Matching `TaskView.tsx`:

```kotlin
val activeStates = setOf(
    "running", "branching", "provisioning",
    "starting", "waiting", "asking", "terminating",
)
val waitingStates = setOf("waiting", "asking")

fun TaskJSON.isActive(): Boolean = state in activeStates
fun TaskJSON.isWaiting(): Boolean = state in waitingStates
```

### Tool Call Detail Extraction

Matching `toolCallDetail()` from `TaskView.tsx` — extracts a short summary for the tool call header:

```kotlin
fun toolCallDetail(name: String, input: JsonElement): String? {
    val obj = input as? JsonObject ?: return null
    return when (name) {
        "Read", "Write", "Edit" -> obj["file_path"]?.jsonPrimitive?.contentOrNull
        "Bash" -> obj["command"]?.jsonPrimitive?.contentOrNull
            ?.take(60)?.let { if (it.length == 60) "$it..." else it }
        "Grep" -> obj["pattern"]?.jsonPrimitive?.contentOrNull
        "Glob" -> obj["pattern"]?.jsonPrimitive?.contentOrNull
        "WebFetch" -> obj["url"]?.jsonPrimitive?.contentOrNull
        "Task" -> obj["description"]?.jsonPrimitive?.contentOrNull
        else -> null
    }
}
```

### Formatting Utilities

```kotlin
fun formatTokens(n: Int): String = when {
    n >= 1_000_000 -> "${"%.1f".format(n / 1_000_000.0)}Mt"
    n >= 1_000 -> "${"%.0f".format(n / 1_000.0)}kt"
    else -> "${n}t"
}

fun formatDuration(ms: Long): String = when {
    ms < 1000 -> "${ms}ms"
    else -> "${"%.1f".format(ms / 1000.0)}s"
}

fun formatElapsed(ms: Long): String {
    val s = ms / 1000
    val m = s / 60
    val h = m / 60
    return when {
        h > 0 -> "${h}h ${m % 60}m"
        m > 0 -> "${m}m ${s % 60}s"
        else -> "${s}s"
    }
}

fun formatCost(usd: Double): String =
    if (usd < 0.01) "<$0.01" else "$${"%,.2f".format(usd)}"
```

### Markdown Rendering

Use `compose-markdown` library for rendering agent text output:

```kotlin
// build.gradle.kts
implementation("com.mikepenz:multiplatform-markdown-renderer-m3:0.28.0")
implementation("com.mikepenz:multiplatform-markdown-renderer-coil3:0.28.0")
```

Configured with GFM (GitHub Flavored Markdown) and line breaks enabled, matching the web frontend's `marked` configuration (`{ breaks: true, gfm: true }`).

### DataStore Schema

```kotlin
object PreferenceKeys {
    val SERVER_URL = stringPreferencesKey("server_url")
    val NOTIFICATIONS_ENABLED = booleanPreferencesKey("notifications_enabled")
    val LAST_REPO = stringPreferencesKey("last_repo")
    val LAST_HARNESS = stringPreferencesKey("last_harness")
    val VOICE_ENABLED = booleanPreferencesKey("voice_enabled")
    val VOICE_NAME = stringPreferencesKey("voice_name")
}
```

`LAST_REPO` and `LAST_HARNESS` mirror the web frontend's `localStorage` persistence for selected repo.

## Build Configuration

### Minimum SDK and Dependencies

```kotlin
// app/build.gradle.kts
android {
    compileSdk = 35
    defaultConfig {
        minSdk = 26  // Android 8.0 — notification channels required
        targetSdk = 35
    }
}

dependencies {
    implementation(project(":sdk"))

    // Compose
    implementation(platform("androidx.compose:compose-bom:2024.12.01"))
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")
    implementation("androidx.navigation:navigation-compose:2.8.5")

    // Hilt
    implementation("com.google.dagger:hilt-android:2.53.1")
    kapt("com.google.dagger:hilt-compiler:2.53.1")
    implementation("androidx.hilt:hilt-navigation-compose:1.2.0")

    // DataStore
    implementation("androidx.datastore:datastore-preferences:1.1.2")

    // Markdown
    implementation("com.mikepenz:multiplatform-markdown-renderer-m3:0.28.0")
    implementation("com.mikepenz:multiplatform-markdown-renderer-coil3:0.28.0")

    // Firebase AI Logic (Gemini Live API)
    implementation(platform("com.google.firebase:firebase-bom:34.9.0"))
    implementation("com.google.firebase:firebase-ai")
}
```

### Permissions

```xml
<!-- AndroidManifest.xml -->
<uses-permission android:name="android.permission.RECORD_AUDIO" />
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE" />
<uses-permission android:name="android.permission.POST_NOTIFICATIONS" />
```

Runtime permission request for `RECORD_AUDIO` is triggered on first mic tap.

## Example Voice Interaction

```
User: [taps mic]
User: "Hey, start a task on the backend repo to fix the flaky auth test"

Gemini: "I'll create a task on the backend repo to fix the flaky auth test."
  → calls create_task(prompt="fix the flaky auth test", repo="/home/user/backend")
  → [screen navigates to new task detail]
Gemini: "Task created and running."

  ... 3 minutes later, SSE detects task state → "asking" ...

Gemini: [via onTasksUpdated context injection]
  "The auth test task has a question for you. It's asking: which authentication
   method should I use? Options are: JWT tokens, session cookies, or OAuth2."

User: "Go with JWT tokens"

Gemini: "Sending that answer."
  → calls answer_question(task_id="abc", answer="JWT tokens")
Gemini: "Done, the agent is continuing."

  ... 5 minutes later, task completes ...

Gemini: "The auth test task just finished. It fixed the flaky test by adding
  proper token refresh handling. Cost was 12 cents, took 8 minutes.
  Want me to sync the changes?"

User: "Yeah, sync it"

Gemini: → calls sync_task(task_id="abc")
Gemini: "Changes synced to the remote."

User: "What else is running?"

Gemini: → calls list_tasks()
Gemini: "You have one other task: 'add integration tests' on the frontend repo.
  It's been running for 12 minutes, currently on turn 15, cost is 28 cents."

User: "Show me that one"

Gemini: → calls set_active_task(task_id="def")
  → [screen navigates to that task]
Gemini: "Switched to the integration tests task."

User: [taps End]
```
