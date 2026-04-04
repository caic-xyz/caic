// ViewModel for the task detail screen: SSE message stream, grouping, and actions.
package com.fghbuild.caic.ui.taskdetail

import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.BotFixPRReq
import com.caic.sdk.v1.EventMessage
import com.caic.sdk.v1.EventStats
import kotlinx.serialization.json.JsonElement
import com.caic.sdk.v1.TodoItem
import com.caic.sdk.v1.HarnessInfo
import com.caic.sdk.v1.ImageData
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.ForkTaskReq
import com.caic.sdk.v1.Repo
import com.caic.sdk.v1.RepoSpec
import com.caic.sdk.v1.RestartReq
import com.caic.sdk.v1.SafetyIssue
import com.caic.sdk.v1.SyncReq
import com.caic.sdk.v1.Task
import com.fghbuild.caic.data.DraftStore
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.SseAuthException
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.data.TaskSSEEvent
import com.fghbuild.caic.navigation.Screen
import com.fghbuild.caic.util.IncrementalGrouped
import com.fghbuild.caic.util.Session
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.nextGrouped
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.scan
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

data class TaskDetailState(
    val task: Task? = null,
    val hasMessages: Boolean = false,
    val messageCount: Int = 0,
    val completedSessions: List<Session> = emptyList(),
    val currentSessionBoundaryEvent: EventMessage? = null,
    val currentSessionCompletedTurns: List<Turn> = emptyList(),
    val liveTurn: Turn? = null,
    val todos: List<TodoItem> = emptyList(),
    val statsHistory: List<EventStats> = emptyList(),
    val activeAgentDescriptions: List<String> = emptyList(),
    val isReady: Boolean = false,
    val sending: Boolean = false,
    val pendingAction: String? = null,
    val actionError: String? = null,
    val safetyIssues: List<SafetyIssue> = emptyList(),
    val inputDraft: String = "",
    val pendingImages: List<ImageData> = emptyList(),
    val supportsImages: Boolean = false,
    val supportsCompact: Boolean = false,
    val harnesses: List<HarnessInfo> = emptyList(),
    val allRepos: List<Repo> = emptyList(),
)

private val TerminalStates = setOf("stopping", "stopped", "purging", "purged", "failed")

@HiltViewModel
class TaskDetailViewModel @Inject constructor(
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
    private val draftStore: DraftStore,
    savedStateHandle: SavedStateHandle,
) : ViewModel() {

    private val taskId: String = savedStateHandle[Screen.TaskDetail.ARG_TASK_ID] ?: ""

    private val _messages = MutableStateFlow<List<EventMessage>>(emptyList())
    private val _isReady = MutableStateFlow(false)
    private val _sending = MutableStateFlow(false)
    private val _pendingAction = MutableStateFlow<String?>(null)
    private val _actionError = MutableStateFlow<String?>(null)
    private val _safetyIssues = MutableStateFlow<List<SafetyIssue>>(emptyList())
    private val _inputDraft = MutableStateFlow(draftStore.get(taskId).text)
    private val _pendingImages = MutableStateFlow(draftStore.get(taskId).images)
    private val _harnesses = MutableStateFlow<List<HarnessInfo>>(emptyList())
    private val _repos = MutableStateFlow<List<Repo>>(emptyList())

    private var sseJob: Job? = null

    /**
     * Incrementally grouped state derived from [_messages]. On append-only updates only the
     * current (incomplete) turn is regrouped; completed turns are cached unchanged.
     */
    private val _grouped: StateFlow<IncrementalGrouped> = _messages
        .scan(IncrementalGrouped()) { prev, msgs -> nextGrouped(prev, msgs) }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), IncrementalGrouped())

    /** Last 60 container resource snapshots extracted from the stats event stream. */
    private val _statsHistory: StateFlow<List<EventStats>> = _messages
        .map { msgs -> msgs.mapNotNull { if (it.kind == "stats") it.stats else null }.takeLast(60) }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), emptyList())

    @Suppress("UNCHECKED_CAST")
    val state: StateFlow<TaskDetailState> = combine(
        listOf(
            taskRepository.tasks, _grouped, _isReady, _sending,
            _pendingAction, _actionError, _safetyIssues, _inputDraft,
            _pendingImages, _harnesses, _statsHistory, _repos,
        )
    ) { values ->
        val tasks = values[0] as List<Task>
        val grouped = values[1] as IncrementalGrouped
        val ready = values[2] as Boolean
        val sending = values[3] as Boolean
        val action = values[4] as String?
        val error = values[5] as String?
        val safety = values[6] as List<SafetyIssue>
        val draft = values[7] as String
        val images = values[8] as List<ImageData>
        val harnesses = values[9] as List<HarnessInfo>
        @Suppress("UNCHECKED_CAST")
        val statsHist = values[10] as List<EventStats>
        val repos = values[11] as List<Repo>
        val task = tasks.firstOrNull { it.id == taskId }
        val taskHarness = harnesses.firstOrNull { it.name == task?.harness }
        val imgSupport = task != null && taskHarness?.supportsImages == true
        val compactSupport = task != null && taskHarness?.supportsCompact == true
        val msgCount = _messages.value.size
        TaskDetailState(
            task = task,
            hasMessages = msgCount > 0,
            messageCount = msgCount,
            completedSessions = grouped.completedSessions,
            currentSessionBoundaryEvent = grouped.currentSessionBoundaryEvent,
            currentSessionCompletedTurns = grouped.currentSessionCompletedTurns,
            liveTurn = grouped.currentTurn,
            todos = grouped.todos,
            statsHistory = statsHist,
            activeAgentDescriptions = grouped.activeAgents.values.toList(),
            isReady = ready,
            sending = sending,
            pendingAction = action,
            actionError = error,
            safetyIssues = safety,
            inputDraft = draft,
            pendingImages = images,
            supportsImages = imgSupport,
            supportsCompact = compactSupport,
            harnesses = harnesses,
            allRepos = repos,
        )
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), TaskDetailState())

    init {
        connectSSE()
        loadHarnesses()
        loadRepos()
    }

    private fun apiClient(): ApiClient =
        ApiClient(taskRepository.serverURL(), tokenProvider = { settingsRepository.settings.value.authToken })

    private fun loadHarnesses() {
        viewModelScope.launch {
            val url = taskRepository.serverURL()
            if (url.isBlank()) return@launch
            try {
                _harnesses.value = apiClient().listHarnesses()
            } catch (_: Exception) {
                // Non-critical; attach button will just stay hidden.
            }
        }
    }

    private fun loadRepos() {
        viewModelScope.launch {
            val url = taskRepository.serverURL()
            if (url.isBlank()) return@launch
            try {
                _repos.value = apiClient().listRepos()
            } catch (_: Exception) {
                // Non-critical; fork dialog will just show no extra repos.
            }
        }
    }

    @Suppress("CyclomaticComplexMethod")
    private fun connectSSE() {
        sseJob?.cancel()
        sseJob = viewModelScope.launch {
            val baseURL = taskRepository.serverURL()
            if (baseURL.isBlank()) return@launch

            var delayMs = 500L
            val buf = mutableListOf<EventMessage>()
            var live = false
            // Pending live events batched between flushes.
            val pending = mutableListOf<EventMessage>()
            var flushJob: Job? = null

            while (true) {
                buf.clear()
                live = false
                pending.clear()
                flushJob?.cancel()
                flushJob = null
                _isReady.value = false
                try {
                    taskRepository.taskRawEventsWithReady(baseURL, taskId).collect { event ->
                        delayMs = 500L
                        when (event) {
                            is TaskSSEEvent.Ready -> {
                                live = true
                                _messages.value = buf.toList()
                                _isReady.value = true
                            }
                            is TaskSSEEvent.Event -> {
                                if (live) {
                                    pending.add(event.msg)
                                    if (flushJob == null) {
                                        flushJob = launch {
                                            delay(LIVE_BATCH_MS)
                                            if (pending.isNotEmpty()) {
                                                val batch = pending.toList()
                                                pending.clear()
                                                // Each ExitPlanMode event keeps its own planContent snapshot
                                                // so the evolution of the plan is visible at each point it was written.
                                                _messages.value = _messages.value + batch
                                            }
                                            flushJob = null
                                        }
                                    }
                                } else {
                                    buf.add(event.msg)
                                }
                            }
                        }
                    }
                } catch (e: CancellationException) {
                    throw e
                } catch (_: SseAuthException) {
                    return@launch // Stop retrying on 401.
                } catch (_: Exception) {
                    // Fall through to reconnect.
                } finally {
                    flushJob?.cancel()
                    // Flush any remaining pending events so they're not lost.
                    if (pending.isNotEmpty()) {
                        _messages.value = _messages.value + pending.toList()
                        pending.clear()
                    }
                    flushJob = null
                }
                // For terminal tasks with messages, stop reconnecting.
                val currentTask = taskRepository.tasks.value.firstOrNull { it.id == taskId }
                if (live && _messages.value.isNotEmpty() && currentTask?.state in TerminalStates) {
                    return@launch
                }
                delay(delayMs)
                delayMs = (delayMs * 3 / 2).coerceAtMost(DELAY_CAP)
            }
        }
    }

    companion object {
        /** Batching interval for live SSE events (ms). Balances responsiveness vs CPU. */
        private const val LIVE_BATCH_MS = 100L
        private const val DELAY_CAP = 4000L
    }

    fun updateInputDraft(text: String) {
        _inputDraft.value = text
        draftStore.setText(taskId, text)
    }

    fun addImages(images: List<ImageData>) {
        val updated = _pendingImages.value + images
        _pendingImages.value = updated
        draftStore.setImages(taskId, updated)
    }

    fun removeImage(index: Int) {
        val updated = _pendingImages.value.filterIndexed { i, _ -> i != index }
        _pendingImages.value = updated
        draftStore.setImages(taskId, updated)
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun sendInput() {
        val text = _inputDraft.value.trim()
        val images = _pendingImages.value
        if (text.isBlank() && images.isEmpty()) return
        _sending.value = true
        viewModelScope.launch {
            try {
                val client = apiClient()
                client.sendInput(
                    taskId,
                    InputReq(
                        prompt = Prompt(text = text, images = images.ifEmpty { null }),
                    ),
                )
                _inputDraft.value = ""
                _pendingImages.value = emptyList()
                draftStore.clear(taskId)
            } catch (e: Exception) {
                showActionError("send failed: ${e.message}")
            } finally {
                _sending.value = false
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun syncTask(force: Boolean = false, target: String? = null) {
        _pendingAction.value = "sync"
        viewModelScope.launch {
            try {
                val client = apiClient()
                val resp = client.syncTask(taskId, SyncReq(force = if (force) true else null, target = target))
                val issues = resp.safetyIssues.orEmpty()
                if (issues.isNotEmpty() && !force) {
                    _safetyIssues.value = issues
                } else {
                    _safetyIssues.value = emptyList()
                }
            } catch (e: Exception) {
                showActionError("sync failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun stopTask() {
        _pendingAction.value = "stop"
        viewModelScope.launch {
            try {
                val client = apiClient()
                client.stopTask(taskId)
            } catch (e: Exception) {
                showActionError("stop failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun purgeTask() {
        _pendingAction.value = "purge"
        viewModelScope.launch {
            try {
                val client = apiClient()
                client.purgeTask(taskId)
            } catch (e: Exception) {
                showActionError("purge failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun reviveTask() {
        _pendingAction.value = "revive"
        viewModelScope.launch {
            try {
                val client = apiClient()
                client.reviveTask(taskId)
            } catch (e: Exception) {
                showActionError("revive failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun forkTask(prompt: String, harness: String? = null, model: String? = null, extraRepos: List<RepoSpec>? = null) {
        _pendingAction.value = "fork"
        viewModelScope.launch {
            try {
                val client = apiClient()
                client.forkTask(
                    taskId,
                    ForkTaskReq(
                        prompt = Prompt(text = prompt),
                        harness = harness,
                        model = model?.ifBlank { null },
                        extraRepos = extraRepos,
                    ),
                )
            } catch (e: Exception) {
                showActionError("fork failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun restartTask(prompt: String) {
        _pendingAction.value = "restart"
        viewModelScope.launch {
            try {
                val client = apiClient()
                client.restartTask(taskId, RestartReq(prompt = Prompt(text = prompt)))
            } catch (e: Exception) {
                showActionError("restart failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun clearContext() {
        _pendingAction.value = "clear-context"
        viewModelScope.launch {
            try {
                apiClient().clearContext(taskId)
            } catch (e: Exception) {
                showActionError("clear context failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun compactContext() {
        _pendingAction.value = "compact"
        viewModelScope.launch {
            try {
                apiClient().compactContext(taskId, com.caic.sdk.v1.CompactReq())
            } catch (e: Exception) {
                showActionError("compact failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures as null.
    suspend fun loadToolInput(toolUseID: String): JsonElement? = try {
        apiClient().getTaskToolInput(taskId, toolUseID).input
    } catch (_: Exception) {
        null
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun fixPR() {
        _pendingAction.value = "fixPR"
        viewModelScope.launch {
            try {
                apiClient().botFixPR(BotFixPRReq(taskId = taskId))
            } catch (e: Exception) {
                showActionError("fix PR failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    fun dismissSafetyIssues() {
        _safetyIssues.value = emptyList()
    }

    private fun showActionError(msg: String) {
        _actionError.value = msg
        viewModelScope.launch {
            delay(5000)
            if (_actionError.value == msg) _actionError.value = null
        }
    }
}
