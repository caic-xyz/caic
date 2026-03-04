// Singleton repository managing the global SSE connection, task list, and per-task event streams.
package com.fghbuild.caic.data

import com.caic.sdk.v1.EventMessage
import com.caic.sdk.v1.Task
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
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.sse.EventSource
import okhttp3.sse.EventSourceListener
import okhttp3.sse.EventSources
import java.io.IOException
import javax.inject.Inject
import javax.inject.Singleton

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
            settingsRepository.settings.collectLatest { settings ->
                if (settings.serverURL.isBlank()) {
                    _tasksConnected.value = false
                    _usageConnected.value = false
                    updateConnected()
                    _tasks.value = emptyList()
                    _usage.value = null
                    return@collectLatest
                }
                launch {
                    try {
                        taskEventsReconnecting(settings.serverURL, _tasksConnected).collect { tasks ->
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
                        usageEventsReconnecting(settings.serverURL, _usageConnected).collect { usage ->
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
                close(t?.let { IOException("SSE connection failed", it) })
            }

            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    /** SSE flow for the task list events endpoint. */
    private fun taskListEvents(baseURL: String): Flow<List<Task>> = sseFlow("$baseURL/api/v1/server/tasks/events")

    /** SSE flow for the usage events endpoint. */
    private fun usageEvents(baseURL: String): Flow<UsageResp> = sseFlow("$baseURL/api/v1/server/usage/events")

    /** Generic SSE flow that deserializes each message event as [T]. */
    private inline fun <reified T> sseFlow(url: String): Flow<T> = callbackFlow {
        val request = Request.Builder()
            .url(url)
            .header("Accept", "text/event-stream")
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
                close(t?.let { IOException("SSE connection failed", it) })
            }

            override fun onClosed(eventSource: EventSource) {
                close()
            }
        })
        awaitClose { source.cancel() }
    }

    /** Reconnecting wrapper with exponential backoff (500ms initial, 1.5x, max 4s). */
    private fun taskEventsReconnecting(baseURL: String, flag: MutableStateFlow<Boolean>): Flow<List<Task>> =
        reconnectingFlow(flag) { taskListEvents(baseURL) }

    /** Reconnecting wrapper with exponential backoff (500ms initial, 1.5x, max 4s). */
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
            } catch (_: Exception) {
                flag.value = false
                updateConnected()
                delay(delayMs)
                delayMs = (delayMs * 3 / 2).coerceAtMost(4000L)
            }
        }
    }
}
