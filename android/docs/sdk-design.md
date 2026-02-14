# Kotlin SDK Design

## Overview

The Kotlin SDK provides a type-safe client for the caic API, mirroring the generated TypeScript SDK (`sdk/api.gen.ts`). It is produced by extending `gen-api-client` with `--lang=kotlin` to emit Kotlin data classes and API functions from the same `dto.Routes` and Go structs used for the TS client.

## Code Generation

### Extending `gen-api-client`

The existing generator (`backend/internal/cmd/gen-api-client/main.go`) reads `dto.Routes` and emits TypeScript. We add a `--lang` flag:

```
gen-api-client --lang=kotlin --out=android/sdk/src/main/kotlin/com/caic/sdk
```

When `--lang=kotlin`, the generator produces two files:

1. **`Types.kt`** — data classes for all request/response types and event payloads
2. **`ApiClient.kt`** — suspend functions for JSON endpoints, `Flow<EventMessage>` for SSE

### Go-to-Kotlin Type Mapping

| Go type | Kotlin type |
|---------|-------------|
| `string` | `String` |
| `int`, `int64` | `Long` |
| `float64` | `Double` |
| `bool` | `Boolean` |
| `[]T` | `List<T>` |
| `map[string]any` | `Map<String, JsonElement>` |
| `json.RawMessage` | `JsonElement` |
| `ksid.ID` | `String` |
| pointer (`*T`) | `T?` |
| `omitempty` tag | nullable (`T?`) with `@EncodeDefault(NEVER)` |

### Generated Types (`Types.kt`)

#### Type Aliases and Constants

```kotlin
package com.caic.sdk

import kotlinx.serialization.*
import kotlinx.serialization.json.*

typealias Harness = String

object Harnesses {
    const val Claude: Harness = "claude"
    const val Gemini: Harness = "gemini"
}

typealias EventKind = String

object EventKinds {
    const val Init: EventKind = "init"
    const val Text: EventKind = "text"
    const val ToolUse: EventKind = "toolUse"
    const val ToolResult: EventKind = "toolResult"
    const val Ask: EventKind = "ask"
    const val Usage: EventKind = "usage"
    const val Result: EventKind = "result"
    const val System: EventKind = "system"
    const val UserInput: EventKind = "userInput"
    const val Todo: EventKind = "todo"
}
```

#### Core Data Classes

```kotlin
@Serializable
data class HarnessJSON(val name: String)

@Serializable
data class RepoJSON(
    val path: String,
    val baseBranch: String,
    val repoURL: String? = null,
)

@Serializable
data class TaskJSON(
    val id: String,
    val task: String,
    val repo: String,
    val repoURL: String? = null,
    val branch: String,
    val container: String,
    val state: String,
    val stateUpdatedAt: Double,
    val diffStat: List<DiffFileStat>,
    val costUSD: Double,
    val durationMs: Long,
    val numTurns: Int,
    val inputTokens: Int,
    val outputTokens: Int,
    val cacheCreationInputTokens: Int,
    val cacheReadInputTokens: Int,
    val error: String? = null,
    val result: String? = null,
    val harness: Harness,
    val model: String? = null,
    val agentVersion: String? = null,
    val sessionID: String? = null,
    val containerUptimeMs: Long? = null,
    val inPlanMode: Boolean? = null,
)

@Serializable
data class DiffFileStat(
    val path: String,
    val added: Int,
    val deleted: Int,
    val binary: Boolean,
)

@Serializable
data class CreateTaskReq(
    val prompt: String,
    val repo: String,
    val model: String? = null,
    val harness: Harness,
)

@Serializable
data class CreateTaskResp(
    val status: String,
    val id: String,
)

@Serializable
data class InputReq(val prompt: String)

@Serializable
data class RestartReq(val prompt: String)

@Serializable
data class SyncReq(val force: Boolean? = null)

@Serializable
data class SyncResp(
    val status: String,
    val diffStat: List<DiffFileStat>,
    val safetyIssues: List<SafetyIssue>? = null,
)

@Serializable
data class SafetyIssue(
    val file: String,
    val kind: String,
    val detail: String,
)

@Serializable
data class StatusResp(val status: String)

@Serializable
data class UsageWindow(
    val utilization: Double,
    val resetsAt: String,
)

@Serializable
data class ExtraUsage(
    val isEnabled: Boolean,
    val monthlyLimit: Double,
    val usedCredits: Double,
    val utilization: Double,
)

@Serializable
data class UsageResp(
    val fiveHour: UsageWindow,
    val sevenDay: UsageWindow,
    val extraUsage: ExtraUsage,
)
```

#### Event Types

```kotlin
@Serializable
data class EventMessage(
    val kind: EventKind,
    val ts: Long,
    val init: EventInit? = null,
    val text: EventText? = null,
    val toolUse: EventToolUse? = null,
    val toolResult: EventToolResult? = null,
    val ask: EventAsk? = null,
    val usage: EventUsage? = null,
    val result: EventResult? = null,
    val system: EventSystem? = null,
    val userInput: EventUserInput? = null,
    val todo: EventTodo? = null,
)

@Serializable
data class EventInit(
    val model: String,
    val agentVersion: String,
    val sessionID: String,
    val tools: List<String>,
    val cwd: String,
)

@Serializable
data class EventText(val text: String)

@Serializable
data class EventToolUse(
    val toolUseID: String,
    val name: String,
    val input: JsonElement,
)

@Serializable
data class EventToolResult(
    val toolUseID: String,
    val durationMs: Long,
    val error: String? = null,
)

@Serializable
data class AskOption(
    val label: String,
    val description: String? = null,
)

@Serializable
data class AskQuestion(
    val question: String,
    val header: String? = null,
    val options: List<AskOption>,
    val multiSelect: Boolean? = null,
)

@Serializable
data class EventAsk(
    val toolUseID: String,
    val questions: List<AskQuestion>,
)

@Serializable
data class EventUsage(
    val inputTokens: Int,
    val outputTokens: Int,
    val cacheCreationInputTokens: Int,
    val cacheReadInputTokens: Int,
    val serviceTier: String? = null,
    val model: String,
)

@Serializable
data class EventResult(
    val subtype: String,
    val isError: Boolean,
    val result: String,
    val diffStat: List<DiffFileStat>,
    val totalCostUSD: Double,
    val durationMs: Long,
    val durationAPIMs: Long,
    val numTurns: Int,
    val usage: EventUsage,
)

@Serializable
data class EventSystem(val subtype: String)

@Serializable
data class EventUserInput(val text: String)

@Serializable
data class TodoItem(
    val content: String,
    val status: String,
    val activeForm: String? = null,
)

@Serializable
data class EventTodo(
    val toolUseID: String,
    val todos: List<TodoItem>,
)
```

#### Error Types

```kotlin
@Serializable
data class ErrorResponse(
    val error: ErrorDetails,
    val details: Map<String, JsonElement>? = null,
)

@Serializable
data class ErrorDetails(
    val code: String,
    val message: String,
)

object ErrorCodes {
    const val BadRequest = "BAD_REQUEST"
    const val NotFound = "NOT_FOUND"
    const val Conflict = "CONFLICT"
    const val InternalError = "INTERNAL_ERROR"
}
```

## API Client (`ApiClient.kt`)

### Dependencies

```kotlin
// build.gradle.kts
dependencies {
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    implementation("com.squareup.okhttp3:okhttp-sse:4.12.0")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.9.0")
}
```

### Client Structure

```kotlin
package com.caic.sdk

import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.serialization.json.Json
import okhttp3.*
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.sse.EventSource
import okhttp3.sse.EventSourceListener
import okhttp3.sse.EventSources

class ApiClient(private val baseURL: String) {

    private val client = OkHttpClient()
    private val json = Json { ignoreUnknownKeys = true }

    class ApiException(
        val statusCode: Int,
        val code: String,
        override val message: String,
        val details: Map<String, JsonElement>? = null,
    ) : Exception(message)

    private suspend inline fun <reified T> request(
        method: String,
        path: String,
        body: Any? = null,
    ): T = withContext(Dispatchers.IO) {
        val reqBuilder = Request.Builder().url("$baseURL$path")
        if (body != null) {
            val jsonBody = json.encodeToString(
                serializer(body::class.java), body
            ).toRequestBody("application/json".toMediaType())
            reqBuilder.method(method, jsonBody)
        } else if (method == "POST") {
            reqBuilder.method(method, "".toRequestBody(null))
        } else {
            reqBuilder.method(method, null)
        }
        val resp = client.newCall(reqBuilder.build()).await()
        val respBody = resp.body?.string()
            ?: throw ApiException(resp.code, ErrorCodes.InternalError, "empty response")
        if (resp.code != 200) {
            val err = json.decodeFromString<ErrorResponse>(respBody)
            throw ApiException(resp.code, err.error.code, err.error.message, err.details)
        }
        json.decodeFromString<T>(respBody)
    }
```

### Generated API Functions

All 10 endpoints from `dto.Routes`:

```kotlin
    // GET /api/v1/harnesses → List<HarnessJSON>
    suspend fun listHarnesses(): List<HarnessJSON> =
        request("GET", "/api/v1/harnesses")

    // GET /api/v1/repos → List<RepoJSON>
    suspend fun listRepos(): List<RepoJSON> =
        request("GET", "/api/v1/repos")

    // GET /api/v1/tasks → List<TaskJSON>
    suspend fun listTasks(): List<TaskJSON> =
        request("GET", "/api/v1/tasks")

    // POST /api/v1/tasks
    suspend fun createTask(req: CreateTaskReq): CreateTaskResp =
        request("POST", "/api/v1/tasks", req)

    // GET /api/v1/tasks/{id}/events → Flow<EventMessage> (SSE)
    fun taskEvents(id: String): Flow<EventMessage> = callbackFlow {
        val request = Request.Builder()
            .url("$baseURL/api/v1/tasks/$id/events")
            .build()
        val factory = EventSources.createFactory(client)
        val source = factory.newEventSource(request, object : EventSourceListener() {
            override fun onEvent(
                eventSource: EventSource,
                id: String?,
                type: String?,
                data: String,
            ) {
                val msg = json.decodeFromString<EventMessage>(data)
                trySend(msg)
            }

            override fun onFailure(
                eventSource: EventSource,
                t: Throwable?,
                response: Response?,
            ) {
                close(t ?: Exception("SSE connection failed"))
            }

            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    // POST /api/v1/tasks/{id}/input
    suspend fun sendInput(id: String, req: InputReq): StatusResp =
        request("POST", "/api/v1/tasks/$id/input", req)

    // POST /api/v1/tasks/{id}/restart
    suspend fun restartTask(id: String, req: RestartReq): StatusResp =
        request("POST", "/api/v1/tasks/$id/restart", req)

    // POST /api/v1/tasks/{id}/terminate
    suspend fun terminateTask(id: String): StatusResp =
        request("POST", "/api/v1/tasks/$id/terminate")

    // POST /api/v1/tasks/{id}/sync
    suspend fun syncTask(id: String, req: SyncReq): SyncResp =
        request("POST", "/api/v1/tasks/$id/sync", req)

    // GET /api/v1/usage
    suspend fun getUsage(): UsageResp =
        request("GET", "/api/v1/usage")
}
```

## SSE Reconnection

The web frontend (`App.tsx`, `TaskView.tsx`) uses exponential backoff: 500ms initial, ×1.5 per failure, capped at 4s, reset on successful connection. The SDK provides a reconnecting wrapper:

```kotlin
fun taskEventsReconnecting(
    id: String,
    initialDelayMs: Long = 500,
    maxDelayMs: Long = 4000,
    backoffFactor: Double = 1.5,
): Flow<EventMessage> = flow {
    var delay = initialDelayMs
    while (true) {
        try {
            taskEvents(id).collect { msg ->
                delay = initialDelayMs // reset on success
                emit(msg)
            }
        } catch (e: CancellationException) {
            throw e
        } catch (_: Exception) {
            // connection lost, reconnect after backoff
        }
        kotlinx.coroutines.delay(delay)
        delay = (delay * backoffFactor).toLong().coerceAtMost(maxDelayMs)
    }
}
```

The web frontend also uses a buffer-and-swap pattern for SSE replay: events are accumulated until a `"ready"` system event, then swapped atomically to avoid a flash of empty content. The SDK does not implement this — it is an app-layer concern handled by the ViewModel (see `app-design.md`).

## Error Handling

`ApiException` mirrors the Go `dto.APIError` and TS `APIError`:

| HTTP Status | ErrorCode | Typical Cause |
|-------------|-----------|---------------|
| 400 | `BAD_REQUEST` | Invalid request body, missing required field |
| 404 | `NOT_FOUND` | Task/resource does not exist |
| 409 | `CONFLICT` | Task in incompatible state for action |
| 500 | `INTERNAL_ERROR` | Server error |

The `details` map carries structured context (e.g., `{"field": "repo", "reason": "not found"}`). Callers handle errors by catching `ApiException` and branching on `code`:

```kotlin
try {
    client.sendInput(taskId, InputReq("hello"))
} catch (e: ApiClient.ApiException) {
    when (e.code) {
        ErrorCodes.NotFound -> // task deleted
        ErrorCodes.Conflict -> // task not in waiting state
        else -> // show generic error
    }
}
```

## Code Generation Implementation

### Changes to `gen-api-client/main.go`

Add to `run()`:

```go
lang := flag.String("lang", "ts", "output language: ts or kotlin")
flag.Parse()

switch *lang {
case "ts":
    return generateTS()
case "kotlin":
    return generateKotlin()
}
```

New function `generateKotlin()`:

1. **Type emission**: Walk all types referenced in `dto.Routes` via reflection. For each Go struct:
   - Emit `@Serializable data class` with fields mapped per the type table above
   - Handle `json` tags for field naming (`@SerialName`)
   - Handle `omitempty` → nullable with default `null`

2. **Route emission**: For each `dto.Route`:
   - JSON endpoint → `suspend fun name(params..., req?): RespType`
   - SSE endpoint → `fun name(params...): Flow<EventMessage>`

3. **Path parameters**: Same `extractPathParams` logic, emit Kotlin string interpolation (`"$baseURL/api/v1/tasks/$id/events"`)

### Go Struct → Kotlin Example

Given:
```go
type TaskJSON struct {
    ID    ksid.ID `json:"id"`
    Task  string  `json:"task"`
    Error string  `json:"error,omitempty"`
}
```

Generator emits:
```kotlin
@Serializable
data class TaskJSON(
    val id: String,
    val task: String,
    val error: String? = null,
)
```

### Build Integration

```makefile
# Makefile addition
types: ## Generate types (tygo + gen-api-client for TS and Kotlin)
	go generate ./...
	go run ./backend/internal/cmd/gen-api-client --lang=kotlin \
		--out=android/sdk/src/main/kotlin/com/caic/sdk
```

## Module Structure

```
android/
├── sdk/
│   ├── build.gradle.kts
│   └── src/main/kotlin/com/caic/sdk/
│       ├── Types.kt          # Generated data classes
│       └── ApiClient.kt      # Generated API client
└── app/                      # See app-design.md
```

The SDK is a standalone Gradle module with no Android dependencies, so it can be unit-tested on JVM.

## Testing Strategy

### Unit Tests (JVM, no Android)

Use `MockWebServer` from OkHttp to verify request/response serialization:

```kotlin
class ApiClientTest {
    private val server = MockWebServer()
    private val client = ApiClient(server.url("/").toString())

    @Test
    fun listTasks_deserializes() = runTest {
        server.enqueue(MockResponse().setBody("""[{
            "id": "abc123", "task": "fix bug", "repo": "/src",
            "branch": "main", "container": "ctr-1",
            "state": "running", "stateUpdatedAt": 1700000000.0,
            "diffStat": [], "costUSD": 0.05, "durationMs": 3000,
            "numTurns": 2, "inputTokens": 100, "outputTokens": 50,
            "cacheCreationInputTokens": 0, "cacheReadInputTokens": 0,
            "harness": "claude"
        }]"""))
        val tasks = client.listTasks()
        assertEquals(1, tasks.size)
        assertEquals("abc123", tasks[0].id)
        assertEquals("running", tasks[0].state)
    }

    @Test
    fun createTask_sendsBody() = runTest {
        server.enqueue(MockResponse().setBody("""{"status":"ok","id":"new1"}"""))
        val resp = client.createTask(CreateTaskReq(
            prompt = "add tests",
            repo = "/src",
            harness = Harnesses.Claude,
        ))
        assertEquals("new1", resp.id)
        val req = server.takeRequest()
        assertEquals("POST", req.method)
        assertTrue(req.body.readUtf8().contains("add tests"))
    }

    @Test
    fun apiException_onError() = runTest {
        server.enqueue(MockResponse()
            .setResponseCode(404)
            .setBody("""{"error":{"code":"NOT_FOUND","message":"task not found"}}"""))
        val ex = assertThrows<ApiClient.ApiException> {
            client.listTasks()
        }
        assertEquals(404, ex.statusCode)
        assertEquals(ErrorCodes.NotFound, ex.code)
    }
}
```

### SSE Tests

```kotlin
@Test
fun taskEvents_emitsMessages() = runTest {
    server.enqueue(MockResponse()
        .setHeader("Content-Type", "text/event-stream")
        .setBody("""
            data: {"kind":"text","ts":1700000000000,"text":{"text":"hello"}}

            data: {"kind":"system","ts":1700000001000,"system":{"subtype":"status"}}

        """.trimIndent()))
    val events = client.taskEvents("task1").toList()
    assertEquals(2, events.size)
    assertEquals(EventKinds.Text, events[0].kind)
    assertEquals("hello", events[0].text?.text)
}
```

### Serialization Round-Trip Tests

Verify every generated data class serializes and deserializes correctly with `kotlinx.serialization`:

```kotlin
@Test
fun eventMessage_roundTrip() {
    val original = EventMessage(
        kind = EventKinds.ToolUse,
        ts = 1700000000000,
        toolUse = EventToolUse(
            toolUseID = "tu-1",
            name = "Read",
            input = buildJsonObject { put("file_path", "/src/main.go") },
        ),
    )
    val encoded = json.encodeToString(original)
    val decoded = json.decodeFromString<EventMessage>(encoded)
    assertEquals(original, decoded)
}
```
