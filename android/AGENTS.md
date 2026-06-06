# Android Compose Client

Kotlin/Compose Android app for caic. Voice-first companion for managing coding agents.

## Linting

**Mandatory after every Android/Kotlin change — do not skip.** Run these and fix all failures before considering the change complete:

```bash
make lint-android   # detekt + Android lint
make android-test   # JVM unit tests
make android-build  # assembleDebug — must pass, never leave broken
```

## Current State

SDK module (generated API client + types), Compose UI, Hilt DI, and voice mode
(Phase 1) are implemented. The app has a task list screen, settings screen, and
a voice overlay with Gemini Live WebSocket integration. Phase 2 (full screen mode
with TaskDetail, message grouping, etc.) is not yet started.

## Architecture

See `docs/` for full design specs:
- `docs/sdk-design.md` — Kotlin SDK: generated API client + types
- `docs/app-design.md` — App: screens, voice mode, state management

### Layer Summary

```
UI (Compose screens) → ViewModels (StateFlow) → Repositories → SDK (ApiClient)
                                                             → Gemini Live (voice)
                                                             → DataStore (settings)
```

No business logic in Compose. No Android dependencies in the SDK module.

## Conventions

- **Package**: `com.fghbuild.caic` (app), `com.caic.sdk` (SDK module)
- **DI**: Hilt
- **Serialization**: `kotlinx.serialization` (not Gson/Moshi)
- **Networking**: OkHttp (HTTP + SSE + WebSocket)
- **Async**: Coroutines + `StateFlow` (not LiveData, not RxJava)
- **Navigation**: Compose Navigation with type-safe routes
- **Compose naming**: `PascalCase` for composables (detekt `functionPattern` allows this)
- **Line length**: 120 chars (detekt config)
- **No wildcard imports** (detekt enforced)

## Build & Lint

```bash
make lint-android   # detekt + Android lint
make android-build  # assembleDebug
make android-test   # JVM unit tests
make android-e2e    # Android instrumented tests and generate screenshots
```

Lint is strict: `warningsAsErrors = true`, `maxIssues: 0`.

## Emulator Tips & Tricks

### Quick Setup

```bash
# One-time setup (installs SDK, system image, creates AVD)
make android-setup-emulator

# Start/stop the emulator
make android-start-emulator
make android-stop-emulator

# Build, install, and launch the app
make android-push
```

### Connecting to Fake Backend

The emulator uses `10.0.2.2` to reach the host machine. To test with the fake backend:

```bash
# Terminal 1: Start fake backend
make dev-fake  # or: go run -tags e2e ./backend/cmd/caic -config-dir /tmp/caic-test

# Terminal 2: Set up reverse tunnel (alternative to 10.0.2.2)
adb reverse tcp:2242 tcp:2242
```

Then in the app Settings, add a server with URL `http://10.0.2.2:2242`.

### UI Automation Gotchas

- **Use `uiautomator dump`** to find element bounds before tapping:
  ```bash
  adb shell uiautomator dump /sdcard/ui.xml
  adb pull /sdcard/ui.xml /tmp/ui.xml
  grep -i "button\|EditText" /tmp/ui.xml
  ```
- **Compose elements** often don't have resource-ids. Use `content-desc` or bounds from the XML.
- **Text input** goes to the focused field. Tap the target field first, then type.
- **Keyboard dismissal**: `adb shell input keyevent KEYCODE_BACK`
- **Clear app data**: `adb shell pm clear com.fghbuild.caic`

### Screenshots

```bash
# Capture and pull screenshot
adb shell screencap -p /sdcard/screenshot.png
adb pull /sdcard/screenshot.png /tmp/screenshot.png
```



## E2E Testing

Android e2e tests mirror the frontend Playwright tests (`e2e/tests/*.spec.ts`)
and run against the same fake backend (`go run -tags e2e`).

### Infrastructure

- **Test runner**: `HiltTestRunner` (uses `HiltTestApplication` so Hilt DI works
  in instrumented tests). All tests that launch `MainActivity` must have
  `@HiltAndroidTest` + `HiltAndroidRule(order = 0)`.
- **Base class**: `e2e/E2eTestBase.kt` — injects `SettingsRepository`, creates
  an `ApiClient` pointed at the fake backend, provides `createTaskAPI()` and
  `waitForTaskState()` helpers.
- **Server URL**: read from instrumentation arg `baseUrl` (default
  `http://localhost:8090`). Written to `SettingsRepository` in `@Before`.
- **Screenshots**: saved to `e2e/screenshots/android/` (frontend saves to
  `e2e/screenshots/frontend/`).

### Running via make (recommended)

```bash
make android-e2e
```

This runs `scripts/android_e2e.py` which:
1. Builds the fake Go backend (`go build -tags e2e`).
2. Starts it on a free port.
3. Sets up `adb reverse tcp:PORT tcp:PORT` so the device can reach the host.
4. Runs `./gradlew connectedAndroidTest` with the dynamic baseUrl.
5. Pulls and converts screenshots to webp.

### Running manually (from Android Studio or adb)

If you need to run a specific test or debug without the full script:

```bash
# 1. Start the fake backend
cd /home/user/src/caic
go build -tags e2e -o /tmp/caic-e2e ./backend/cmd/caic
mkdir -p /tmp/caic-e2e-config
echo '[server]
http = ":8090"' > /tmp/caic-e2e-config/config.toml
/tmp/caic-e2e -config-dir /tmp/caic-e2e-config &

# 2. Set up the reverse tunnel (device localhost → host localhost)
adb reverse tcp:8090 tcp:8090

# 3. Run the test with the baseUrl argument
cd android
./gradlew connectedAndroidTest \
    -Pandroid.testInstrumentationRunnerArguments.baseUrl=http://localhost:8090

# 4. Cleanup
kill %1
adb reverse --remove tcp:8090
```

If you use a different port, update all three places (config.toml, adb reverse,
and the gradle argument).

### Writing a new e2e test

```kotlin
@HiltAndroidTest
@RunWith(AndroidJUnit4::class)
class MyE2eTest : E2eTestBase() {
    @get:Rule(order = 1)
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    @Test
    fun myTest() = runBlocking {
        val id = createTaskAPI("Fix the bug")
        waitForTaskState(id, "waiting")
        // ... Compose UI assertions ...
    }
}
```

API-only tests (no UI) can skip the compose rule and just use `api` directly.

## Gemini Live API

Official docs:
- https://ai.google.dev/api/live — WebSocket reference
- https://ai.google.dev/gemini-api/docs/ephemeral-tokens — token provisioning

### Voice token flow

1. Android calls `GET /api/v1/voice/token` on the caic backend.
2. Backend returns a token and an `ephemeral` boolean.
3. Android picks the WebSocket URL and auth parameter based on `ephemeral`.

The response's `ephemeral` field controls which path the client uses:

| Mode | `ephemeral` | Token source | WebSocket endpoint | Auth param |
|------|-------------|--------------|-------------------|------------|
| Raw key (current) | `false` | `GEMINI_API_KEY` directly | `v1beta.GenerativeService.BidiGenerateContent` | `?key=` |
| Ephemeral (disabled) | `true` | `POST /v1alpha/auth_tokens` | `v1alpha.GenerativeService.BidiGenerateContentConstrained` | `?access_token=` |

### API versioning

The raw key path uses **v1beta** + `BidiGenerateContent` and produces
higher-quality voice responses.

Ephemeral tokens are **v1alpha only** — both token creation and the
WebSocket endpoint must use `v1alpha`. Using `v1beta` for `auth_tokens`
returns 404. The v1alpha path works but produces lower-quality responses.
The ephemeral code is kept in the backend (`getVoiceTokenEphemeral`) for
future use once Google stabilises v1beta ephemeral tokens.

See https://ai.google.dev/gemini-api/docs/ephemeral-tokens.

### Audio configuration

See `docs/app-design.md` § "Audio gotchas" and the
[Firebase AI SDK AudioHelper.kt](https://github.com/firebase/firebase-android-sdk/blob/main/firebase-ai/src/main/kotlin/com/google/firebase/ai/type/AudioHelper.kt).

- **AudioTrack must use `USAGE_MEDIA`** — `VOICE_COMMUNICATION` clips first 1–2s.
- **BT disconnect**: car HFP hang-up doesn't remove the device — listen for `ACTION_SCO_AUDIO_STATE_UPDATED`.

### Protocol notes

- Client must wait for `BidiGenerateContentSetupComplete` before sending other messages.
- `mediaChunks` in `realtimeInput` is **deprecated** — use `audio`, `video`, or `text` fields instead.

## Development Notes

- `minSdk = 33`, `targetSdk = 36`, `compileSdk = 36`
- Version catalog at `gradle/libs.versions.toml`
- detekt config at `detekt.yml`
- The web frontend (SolidJS) in `../frontend/` is the reference implementation for
  screen behavior, event grouping, and formatting. Match it.

## Implementation Order

The app's unique value is voice control. Screen mode is secondary (the web
frontend already exists). Follow the design docs in this order:

1. ~~**SDK module** (`docs/sdk-design.md`)~~ — Done.
2. ~~**Voice mode** (`docs/app-design.md` Phase 1)~~ — Done.
3. **Screen mode** (`docs/app-design.md` Phase 2): full Compose UI with feature
   parity to the web frontend — TaskDetail, message grouping, tool call display,
   turn elision, background service, notifications.

Each step should build, lint, and test clean before proceeding.

<!-- BEGIN FILE INDEX -->
## File Index

Autogenerated from first-line comments. Run scripts/update_agents_file_index.py to refresh.

- `caic/src/androidTest/java/com/fghbuild/caic/ExampleInstrumentedTest.kt`: Verifies the app context package name on a real device or emulator, with accessibility checks enabled.
- `caic/src/androidTest/java/com/fghbuild/caic/HiltTestRunner.kt`: Custom test runner that uses HiltTestApplication for dependency injection in instrumented tests.
- `caic/src/androidTest/java/com/fghbuild/caic/NavigationTest.kt`: Instrumented tests that verify the app launches and renders the task list without unknown translation keys.
- `caic/src/androidTest/java/com/fghbuild/caic/e2e/E2eTestBase.kt`: Base class for Android e2e tests against the fake backend.
- `caic/src/androidTest/java/com/fghbuild/caic/e2e/GenScreenshotsTest.kt`: Generate documentation screenshots using the fake backend. Replaces scripts/gen-android-screenshots.sh.
- `caic/src/androidTest/java/com/fghbuild/caic/e2e/MultiTurnTest.kt`: Multi-turn interaction and concurrent task tests. Mirrors e2e/tests/multi-turn.spec.ts.
- `caic/src/androidTest/java/com/fghbuild/caic/e2e/PlanAndAskTest.kt`: E2E tests for plan mode and ask question state transitions. Mirrors e2e/tests/plan-and-ask.spec.ts.
- `caic/src/androidTest/java/com/fghbuild/caic/e2e/TasksApiTest.kt`: API-only e2e tests for the task lifecycle (no UI). Mirrors e2e/tests/tasks-api.spec.ts.
- `caic/src/androidTest/java/com/fghbuild/caic/ui/taskdetail/InputBarScreenshotTest.kt`: Compose UI tests for the InputBar screenshot attach flow.
- `caic/src/androidTest/java/com/fghbuild/caic/ui/taskdetail/TaskDetailBodyTest.kt`: Compose UI tests for TaskDetailBody: initial prompt visibility across task states.
- `caic/src/androidTest/java/com/fghbuild/caic/util/ImageUtilsTest.kt`: Instrumented tests for screenshot capture image utility functions.
- `caic/src/main/java/com/fghbuild/caic/CaicApp.kt`: Hilt application entry point.
- `caic/src/main/java/com/fghbuild/caic/CaicNavGraph.kt`: Top-level composable hosting the navigation graph with voice panel.
- `caic/src/main/java/com/fghbuild/caic/MainActivity.kt`: Single activity host for Jetpack Compose UI.
- `caic/src/main/java/com/fghbuild/caic/data/AuthTokenStore.kt`: Thin wrapper around SettingsRepository that provides the current auth token for ApiClient injection.
- `caic/src/main/java/com/fghbuild/caic/data/DraftStore.kt`: In-memory store for per-task input drafts (text + images) that survive task switching.
- `caic/src/main/java/com/fghbuild/caic/data/SettingsRepository.kt`: Persisted user settings backed by DataStore preferences.
- `caic/src/main/java/com/fghbuild/caic/data/TaskNotifier.kt`: Manages Android notifications for tasks that need user attention, with auto-dismiss on state change.
- `caic/src/main/java/com/fghbuild/caic/data/TaskRepository.kt`: Singleton repository managing the global SSE connection, task list, and per-task event streams.
- `caic/src/main/java/com/fghbuild/caic/di/DataModule.kt`: Hilt module providing DataStore and ApiClient singletons.
- `caic/src/main/java/com/fghbuild/caic/di/HaloModule.kt`: Hilt module providing Halo BLE dependencies as singletons.
- `caic/src/main/java/com/fghbuild/caic/halo/HaloService.kt`: HaloService: bridge between caic task state and a Halo smart glasses device over BLE.
- `caic/src/main/java/com/fghbuild/caic/navigation/Screen.kt`: Navigation routes for the app.
- `caic/src/main/java/com/fghbuild/caic/ui/common/AttachMenu.kt`: Reusable attach dropdown menu with Take photo, Screenshot, and Choose from gallery options.
- `caic/src/main/java/com/fghbuild/caic/ui/common/NotificationPermission.kt`: Composable helper to request POST_NOTIFICATIONS permission on first user action.
- `caic/src/main/java/com/fghbuild/caic/ui/common/RepoChipStrip.kt`: Reusable repo chip strip with branch editing and add-repo dropdown.
- `caic/src/main/java/com/fghbuild/caic/ui/diff/DiffScreen.kt`: Full-screen diff viewer showing per-file diffs for a task.
- `caic/src/main/java/com/fghbuild/caic/ui/diff/DiffViewModel.kt`: ViewModel for the diff screen: fetches full diff once, splits by file.
- `caic/src/main/java/com/fghbuild/caic/ui/login/LoginScreen.kt`: Login screen: shows OAuth provider buttons when auth is enabled.
- `caic/src/main/java/com/fghbuild/caic/ui/settings/SettingsScreen.kt`: Compose Settings screen for configuring servers and voice.
- `caic/src/main/java/com/fghbuild/caic/ui/settings/SettingsViewModel.kt`: ViewModel for the Settings screen, managing connection testing and preference updates.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/AskQuestionCard.kt`: Card for an ask question with options and answer display.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/ElidedTurn.kt`: Collapsed past turn: shows summary; tap to expand via the parent LazyColumn.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/InputBar.kt`: Bottom input bar with send, sync, fork, stop, purge, revive, clear context, compact, and optional image attach actions.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/ProgressPanel.kt`: Collapsible panel showing active todos and subagent count.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/ResultCard.kt`: Card for a result event: success/error with metadata.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/StatsIcon.kt`: StatsIcon renders a 2×2 bar-chart icon in the task header that opens a popup
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/TaskDetailScreen.kt`: Full-screen task detail view with live SSE message stream, grouping, and actions.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/TaskDetailViewModel.kt`: ViewModel for the task detail screen: SSE message stream, grouping, and actions.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/TextMessageGroup.kt`: Renders a text group: combines textDelta fragments, renders markdown or isolated HTML.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/ThinkingCard.kt`: Collapsed card for an agent thinking block, analogous to ToolCallCard.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/ToolCallCard.kt`: Expandable card for a single tool call: name, detail, duration, error.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/ToolMessageGroup.kt`: Renders a tool group: single card, or a header item used when tool calls are lazy list items.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/TurnContent.kt`: Renders all message groups within a single turn.
- `caic/src/main/java/com/fghbuild/caic/ui/taskdetail/WidgetCard.kt`: Sandboxed WebView widget card for agent-generated HTML widgets.
- `caic/src/main/java/com/fghbuild/caic/ui/tasklist/TaskCard.kt`: Rich task card matching TaskItemSummary.tsx: state badge, plan mode, error, branch, tokens.
- `caic/src/main/java/com/fghbuild/caic/ui/tasklist/TaskListScreen.kt`: Task list screen with creation form, usage badges, and task navigation.
- `caic/src/main/java/com/fghbuild/caic/ui/tasklist/TaskListViewModel.kt`: ViewModel for the task list screen: SSE tasks, usage, creation form, and config.
- `caic/src/main/java/com/fghbuild/caic/ui/tasklist/UsageBadges.kt`: Usage badges: per-provider grouped pills with color-coded thresholds.
- `caic/src/main/java/com/fghbuild/caic/ui/theme/Theme.kt`: Material 3 theme with state-based task colors and centralized app color system. Keep color values in sync with frontend/src/global.css.
- `caic/src/main/java/com/fghbuild/caic/util/Formatting.kt`: Display formatting utilities for tasks: tokens, cost, elapsed time, and tool detail.
- `caic/src/main/java/com/fghbuild/caic/util/Grouping.kt`: Message grouping and turn splitting, ported from frontend/src/grouping.ts.
- `caic/src/main/java/com/fghbuild/caic/util/Harness.kt`: Shared Harness utilities: string-to-type conversion and effort level mapping.
- `caic/src/main/java/com/fghbuild/caic/util/ImageUtils.kt`: Utilities for converting content URIs to base64 ImageData for the API.
- `caic/src/main/java/com/fghbuild/caic/util/ProcessNode.kt`: Builds a process tree from a flat process list using pid/ppid relationships.
- `caic/src/main/java/com/fghbuild/caic/util/ScreenshotService.kt`: One-shot screenshot capture using MediaProjection with a transient foreground service.
- `caic/src/main/java/com/fghbuild/caic/voice/FunctionDeclarations.kt`: Gemini Live functions/tools for voice mode, sync with frontend/src/FunctionDeclarations.ts
- `caic/src/main/java/com/fghbuild/caic/voice/FunctionHandlers.kt`: Dispatches Gemini function calls to the caic API.
- `caic/src/main/java/com/fghbuild/caic/voice/LiveApiProto.kt`: Kotlin data classes for the Gemini Live API WebSocket protocol.
- `caic/src/main/java/com/fghbuild/caic/voice/TaskNumberMap.kt`: Bidirectional map between task IDs and stable 1-based human-friendly numbers.
- `caic/src/main/java/com/fghbuild/caic/voice/VoiceOverlay.kt`: Full-width bottom voice panel composable: mic button, status, and transcription display.
- `caic/src/main/java/com/fghbuild/caic/voice/VoiceService.kt`: Foreground service that keeps the app alive during a voice session.
- `caic/src/main/java/com/fghbuild/caic/voice/VoiceSession.kt`: Manages Gemini Live voice session via WebRTC, audio I/O, and function call dispatch. Keep in sync with frontend/src/VoiceSession.ts
- `caic/src/main/java/com/fghbuild/caic/voice/VoiceViewModel.kt`: Activity-scoped ViewModel bridging VoiceSession to the voice overlay UI.
- `caic/src/test/java/com/fghbuild/caic/ExampleUnitTest.kt`: Placeholder unit test verifying the JVM test harness is functional.
- `caic/src/test/java/com/fghbuild/caic/data/DraftStoreTest.kt`: Unit tests for the per-task draft store.
- `caic/src/test/java/com/fghbuild/caic/data/SettingsRepositoryTest.kt`: Unit tests for SettingsRepository: server CRUD and preference management.
- `caic/src/test/java/com/fghbuild/caic/halo/HaloServiceTest.kt`: Unit tests for HaloService: pure functions (primaryTask, stateLabel, buildStatusString, diffTasks).
- `caic/src/test/java/com/fghbuild/caic/ui/diff/DiffViewModelTest.kt`: Unit tests for the diff splitting and file path extraction logic.
- `caic/src/test/java/com/fghbuild/caic/ui/settings/SettingsViewModelTest.kt`: Unit tests for SettingsViewModel: state transitions and preferences.
- `caic/src/test/java/com/fghbuild/caic/ui/tasklist/TaskListScreenTest.kt`: Unit tests for TaskListScreen presentation helpers.
- `caic/src/test/java/com/fghbuild/caic/ui/tasklist/TaskListViewModelTest.kt`: Integration tests for the task list ViewModel: repo/harness loading and selector state.
- `caic/src/test/java/com/fghbuild/caic/util/FormattingTest.kt`: Unit tests for formatting utilities.
- `caic/src/test/java/com/fghbuild/caic/util/GroupingTest.kt`: Unit tests for message grouping and turn splitting logic.
- `caic/src/test/java/com/fghbuild/caic/util/HarnessTest.kt`: Unit tests for harness conversion and effort option utilities.
- `caic/src/test/java/com/fghbuild/caic/util/ProcessNodeTest.kt`: Unit tests for the process tree builder.
- `caic/src/test/java/com/fghbuild/caic/voice/FunctionDeclarationsTest.kt`: Unit tests for Gemini Live function declaration schema generation.
- `caic/src/test/java/com/fghbuild/caic/voice/FunctionHandlerHelpersTest.kt`: Unit tests for JSON argument parsing helpers used by FunctionHandlers.
- `caic/src/test/java/com/fghbuild/caic/voice/FunctionHandlersTest.kt`: Unit tests for FunctionHandlers dispatch and handler logic.
- `caic/src/test/java/com/fghbuild/caic/voice/TaskNumberMapTest.kt`: Unit tests for the bidirectional task ID to number map.
- `caic/src/test/java/com/fghbuild/caic/voice/TaskSummaryTest.kt`: Unit tests for task summary formatting helpers used by FunctionHandlers.
- `detekt.yml`: Detekt configuration for caic Android project.
- `docs/DEBUGGING_EMULATOR.md`: Debugging with the Android Emulator
- `docs/HALO.md`: Halo Device Support
- `docs/LOCAL_VOICE_STACK.md`: Local Voice Stack Plan
- `docs/WEB_SHELL.md`: Android Web Shell Plan
- `docs/app-design.md`: Android App Design
- `docs/sdk-design.md`: Kotlin SDK Design
- `gomode/src/androidTest/java/com/fghbuild/gomode/WebShellSmokeTest.kt`: Instrumented smoke coverage for the Go Mode hosted WebView shell.
- `gomode/src/main/java/com/fghbuild/gomode/MainActivity.kt`: Single activity host for the Go Mode WebView shell.
- `gomode/src/main/java/com/fghbuild/gomode/data/SettingsRepository.kt`: Persisted Go Mode service-instance settings backed by DataStore preferences.
- `gomode/src/main/java/com/fghbuild/gomode/ui/GoModeApp.kt`: Root Compose surface that chooses native settings or the remote WebView shell.
- `gomode/src/main/java/com/fghbuild/gomode/ui/settings/SettingsScreen.kt`: Native settings fallback for configuring the active Go Mode service instance.
- `gomode/src/main/java/com/fghbuild/gomode/ui/theme/Theme.kt`: Material theme for the Go Mode Android shell.
- `gomode/src/main/java/com/fghbuild/gomode/ui/web/WebShellScreen.kt`: WebView shell that loads the active backend-hosted frontend.
- `gomode/src/test/java/com/fghbuild/gomode/data/SettingsRepositoryTest.kt`: Unit tests for Go Mode service-instance settings.
<!-- END FILE INDEX -->
