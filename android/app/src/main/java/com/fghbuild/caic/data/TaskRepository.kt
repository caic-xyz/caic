// Singleton repository managing the global SSE connection, task list, and per-task event streams.
package com.fghbuild.caic.data

import com.caic.sdk.v1.EventMessage
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskListEvent
import com.caic.sdk.v1.UsageResp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.decodeFromJsonElement
import kotlinx.serialization.json.encodeToJsonElement
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.sse.EventSource
import okhttp3.sse.EventSourceListener
import okhttp3.sse.EventSources
import java.io.IOException
import java.net.HttpURLConnection.HTTP_UNAUTHORIZED
import javax.inject.Inject
import javax.inject.Singleton

private const val DELAY_CAP = 4000L

/** Thrown when an SSE connection receives HTTP 401, used to throttle reconnect backoff. */
class SseAuthException : IOException("SSE connection returned 401")

/** Per-task SSE event wrapper distinguishing the "ready" sentinel from data events. */
sealed class TaskSSEEvent {
    data object Ready : TaskSSEEvent()
    data class Event(val msg: EventMessage) : TaskSSEEvent()
}

@Singleton
class TaskRepository @Inject constructor(
    private val settingsRepository: SettingsRepository,
) {
    private val _tasks = MutableStateFlow<List<Task>>(emptyList())
    val tasks: StateFlow<List<Task>> = _tasks.asStateFlow()

    private val _tasksConnected = MutableStateFlow(false)
    private val _usageConnected = MutableStateFlow(false)
    private val _connected = MutableStateFlow(false)
    val connected: StateFlow<Boolean> = _connected.asStateFlow()

    private val _usage = MutableStateFlow<UsageResp?>(null)
    val usage: StateFlow<UsageResp?> = _usage.asStateFlow()

    private val client = OkHttpClient()
    private val json = Json { ignoreUnknownKeys = true }

    /**
     * Begins observing [SettingsRepository.settings]; restarts the SSE connection whenever the server URL changes.
     * Must be called once with a long-lived scope (e.g. viewModelScope).
     */
    fun start(scope: CoroutineScope) {
        scope.launch {
            settingsRepository.settings
                .map { it.serverURL to it.authToken }
                .distinctUntilChanged()
                .collectLatest { (serverURL, _) ->
                if (serverURL.isBlank()) {
                    _tasksConnected.value = false
                    _usageConnected.value = false
                    updateConnected()
                    _tasks.value = emptyList()
                    _usage.value = null
                    return@collectLatest
                }
                launch {
                    try {
                        taskEventsReconnecting(serverURL, _tasksConnected).collect { tasks ->
                            _tasks.value = tasks
                        }
                    } catch (e: CancellationException) {
                        throw e
                    } catch (_: Exception) {
                        _tasksConnected.value = false
                        updateConnected()
                    }
                }
                launch {
                    try {
                        usageEventsReconnecting(serverURL, _usageConnected).collect { usage ->
                            _usage.value = usage
                        }
                    } catch (e: CancellationException) {
                        throw e
                    } catch (_: Exception) {
                        // Usage connection failure is non-critical.
                    }
                }
            }
        }
    }

    private fun updateConnected() {
        _connected.value = _tasksConnected.value || _usageConnected.value
    }

    /** Returns the current server URL, or empty if not configured. */
    fun serverURL(): String = settingsRepository.settings.value.serverURL

    /**
     * Per-task raw SSE flow that emits [TaskSSEEvent.Event] for message events and
     * [TaskSSEEvent.Ready] when the server signals history replay is complete.
     * The SSE "type" field is "ready" for the sentinel, absent for data events.
     */
    fun taskRawEventsWithReady(baseURL: String, taskId: String): Flow<TaskSSEEvent> = callbackFlow {
        val request = Request.Builder()
            .url("$baseURL/api/v1/tasks/$taskId/raw_events")
            .header("Accept", "text/event-stream")
            .apply { settingsRepository.settings.value.authToken?.let { header("Authorization", "Bearer $it") } }
            .build()
        val factory = EventSources.createFactory(client)
        val source = factory.newEventSource(request, object : EventSourceListener() {
            override fun onEvent(eventSource: EventSource, id: String?, type: String?, data: String) {
                if (type == "ready") {
                    trySend(TaskSSEEvent.Ready)
                    return
                }
                try {
                    val msg = json.decodeFromString<EventMessage>(data)
                    trySend(TaskSSEEvent.Event(msg))
                } catch (_: Exception) {
                    // Skip malformed events.
                }
            }

            override fun onFailure(eventSource: EventSource, t: Throwable?, response: Response?) {
                close(sseFailureException(t, response))
            }

            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    /** Merges patch fields from [TaskListEvent.patch] into an existing [Task] via JSON round-trip. */
    private fun applyPatch(existing: Task, patch: Map<String, kotlinx.serialization.json.JsonElement>): Task {
        val existingJson = json.encodeToJsonElement(existing) as? JsonObject ?: return existing
        val merged = JsonObject(existingJson.toMutableMap().apply { putAll(patch) })
        return json.decodeFromJsonElement<Task>(merged)
    }

    /** SSE flow for the task list events endpoint. Maintains a local task map and applies patch events. */
    private fun taskListEvents(baseURL: String): Flow<List<Task>> = callbackFlow {
        val taskMap = LinkedHashMap<String, Task>()
        val request = Request.Builder()
            .url("$baseURL/api/v1/server/tasks/events")
            .header("Accept", "text/event-stream")
            .apply { settingsRepository.settings.value.authToken?.let { header("Authorization", "Bearer $it") } }
            .build()
        val factory = EventSources.createFactory(client)
        val source = factory.newEventSource(request, object : EventSourceListener() {
            override fun onEvent(eventSource: EventSource, id: String?, type: String?, data: String) {
                try {
                    val event = json.decodeFromString<TaskListEvent>(data)
                    when (event.kind) {
                        "snapshot" -> {
                            taskMap.clear()
                            event.tasks?.forEach { taskMap[it.id] = it }
                        }
                        "upsert" -> event.task?.let { taskMap[it.id] = it }
                        "patch" -> event.patch?.let { patch ->
                            val id = (patch["id"] as? JsonPrimitive)?.content ?: return@let
                            taskMap[id] = applyPatch(taskMap[id] ?: return@let, patch)
                        }
                        "delete" -> event.id?.let { taskMap.remove(it) }
                    }
                    trySend(taskMap.values.toList())
                } catch (_: Exception) {
                    // Skip malformed events.
                }
            }

            override fun onFailure(eventSource: EventSource, t: Throwable?, response: Response?) {
                close(sseFailureException(t, response))
            }

            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    /** SSE flow for the usage events endpoint. */
    private fun usageEvents(baseURL: String): Flow<UsageResp> = sseFlow("$baseURL/api/v1/server/usage/events")

    /** Generic SSE flow that deserializes each message event as [T]. */
    private inline fun <reified T> sseFlow(url: String): Flow<T> = callbackFlow {
        val request = Request.Builder()
            .url(url)
            .header("Accept", "text/event-stream")
            .apply { settingsRepository.settings.value.authToken?.let { header("Authorization", "Bearer $it") } }
            .build()
        val factory = EventSources.createFactory(client)
        val source = factory.newEventSource(request, object : EventSourceListener() {
            override fun onEvent(eventSource: EventSource, id: String?, type: String?, data: String) {
                try {
                    trySend(json.decodeFromString<T>(data))
                } catch (_: Exception) {
                    // Skip malformed events.
                }
            }

            override fun onFailure(eventSource: EventSource, t: Throwable?, response: Response?) {
                close(sseFailureException(t, response))
            }

            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    /** Returns [SseAuthException] for HTTP 401 responses, generic [IOException] otherwise. */
    private fun sseFailureException(t: Throwable?, response: Response?): IOException? {
        if (response?.code == HTTP_UNAUTHORIZED) return SseAuthException()
        return t?.let { IOException("SSE connection failed", it) }
    }

    /** Reconnecting wrapper with exponential backoff (500ms initial, 1.5x, max 4s). Stops on 401. */
    private fun taskEventsReconnecting(baseURL: String, flag: MutableStateFlow<Boolean>): Flow<List<Task>> =
        reconnectingFlow(flag) { taskListEvents(baseURL) }

    /** Reconnecting wrapper with exponential backoff (500ms initial, 1.5x, max 4s). Stops on 401. */
    private fun usageEventsReconnecting(baseURL: String, flag: MutableStateFlow<Boolean>): Flow<UsageResp> =
        reconnectingFlow(flag) { usageEvents(baseURL) }

    private fun <T> reconnectingFlow(flag: MutableStateFlow<Boolean>, connect: () -> Flow<T>): Flow<T> = flow {
        var delayMs = 500L
        while (true) {
            try {
                connect().onEach {
                    delayMs = 500L
                    flag.value = true
                    updateConnected()
                }.collect { emit(it) }
            } catch (e: CancellationException) {
                throw e
            } catch (_: SseAuthException) {
                flag.value = false
                updateConnected()
                return@flow // Stop retrying; collectLatest restarts after re-auth.
            } catch (_: Exception) {
                flag.value = false
                updateConnected()
                delay(delayMs)
                delayMs = (delayMs * 3 / 2).coerceAtMost(DELAY_CAP)
            }
        }
    }
}
