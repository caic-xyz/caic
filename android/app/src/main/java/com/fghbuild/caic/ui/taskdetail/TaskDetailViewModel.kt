// ViewModel for the task detail screen: SSE message stream, grouping, and actions.
package com.fghbuild.caic.ui.taskdetail

import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.EventMessage
import kotlinx.serialization.json.JsonElement
import com.caic.sdk.v1.TodoItem
import com.caic.sdk.v1.HarnessInfo
import com.caic.sdk.v1.ImageData
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.RestartReq
import com.caic.sdk.v1.SafetyIssue
import com.caic.sdk.v1.SyncReq
import com.caic.sdk.v1.Task
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.data.TaskSSEEvent
import com.fghbuild.caic.navigation.Screen
import com.fghbuild.caic.util.IncrementalGrouped
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
import kotlinx.coroutines.flow.scan
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

data class TaskDetailState(
    val task: Task? = null,
    val hasMessages: Boolean = false,
    val messageCount: Int = 0,
    val turns: List<Turn> = emptyList(),
    val todos: List<TodoItem> = emptyList(),
    val isReady: Boolean = false,
    val sending: Boolean = false,
    val pendingAction: String? = null,
    val actionError: String? = null,
    val safetyIssues: List<SafetyIssue> = emptyList(),
    val inputDraft: String = "",
    val pendingImages: List<ImageData> = emptyList(),
    val supportsImages: Boolean = false,
)

private val TerminalStates = setOf("terminated", "failed")

@HiltViewModel
class TaskDetailViewModel @Inject constructor(
    private val taskRepository: TaskRepository,
    savedStateHandle: SavedStateHandle,
) : ViewModel() {

    private val taskId: String = savedStateHandle[Screen.TaskDetail.ARG_TASK_ID] ?: ""

    private val _messages = MutableStateFlow<List<EventMessage>>(emptyList())
    private val _isReady = MutableStateFlow(false)
    private val _sending = MutableStateFlow(false)
    private val _pendingAction = MutableStateFlow<String?>(null)
    private val _actionError = MutableStateFlow<String?>(null)
    private val _safetyIssues = MutableStateFlow<List<SafetyIssue>>(emptyList())
    private val _inputDraft = MutableStateFlow("")
    private val _pendingImages = MutableStateFlow<List<ImageData>>(emptyList())
    private val _harnesses = MutableStateFlow<List<HarnessInfo>>(emptyList())

    private var sseJob: Job? = null

    /**
     * Incrementally grouped state derived from [_messages]. On append-only updates only the
     * current (incomplete) turn is regrouped; completed turns are cached unchanged.
     */
    private val _grouped: StateFlow<IncrementalGrouped> = _messages
        .scan(IncrementalGrouped()) { prev, msgs -> nextGrouped(prev, msgs) }
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), IncrementalGrouped())

    @Suppress("UNCHECKED_CAST")
    val state: StateFlow<TaskDetailState> = combine(
        listOf(
            taskRepository.tasks, _grouped, _isReady, _sending,
            _pendingAction, _actionError, _safetyIssues, _inputDraft,
            _pendingImages, _harnesses,
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
        val task = tasks.firstOrNull { it.id == taskId }
        val imgSupport = task != null &&
            harnesses.any { it.name == task.harness && it.supportsImages }
        val msgCount = _messages.value.size
        TaskDetailState(
            task = task,
            hasMessages = msgCount > 0,
            messageCount = msgCount,
            turns = grouped.turns,
            todos = grouped.todos,
            isReady = ready,
            sending = sending,
            pendingAction = action,
            actionError = error,
            safetyIssues = safety,
            inputDraft = draft,
            pendingImages = images,
            supportsImages = imgSupport,
        )
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), TaskDetailState())

    init {
        connectSSE()
        loadHarnesses()
    }

    private fun loadHarnesses() {
        viewModelScope.launch {
            val url = taskRepository.serverURL()
            if (url.isBlank()) return@launch
            try {
                _harnesses.value = ApiClient(url).listHarnesses()
            } catch (_: Exception) {
                // Non-critical; attach button will just stay hidden.
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
                                                val clearsOldPlan = batch.any { msg ->
                                                    val isExitWithPlan = msg.toolUse?.let {
                                                        it.name == "ExitPlanMode" &&
                                                            it.planContent != null
                                                    } ?: false
                                                    isExitWithPlan ||
                                                        msg.system?.subtype == "context_cleared"
                                                }
                                                val base = if (clearsOldPlan) {
                                                    _messages.value.map { m ->
                                                        val hasStale = m.toolUse?.let {
                                                            it.name == "ExitPlanMode" &&
                                                                it.planContent != null
                                                        } ?: false
                                                        if (hasStale) {
                                                            m.copy(
                                                                toolUse = m.toolUse!!.copy(
                                                                    planContent = null,
                                                                ),
                                                            )
                                                        } else {
                                                            m
                                                        }
                                                    }
                                                } else {
                                                    _messages.value
                                                }
                                                _messages.value = base + batch
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
                delayMs = (delayMs * 3 / 2).coerceAtMost(4000L)
            }
        }
    }

    companion object {
        /** Batching interval for live SSE events (ms). Balances responsiveness vs CPU. */
        private const val LIVE_BATCH_MS = 100L
    }

    fun updateInputDraft(text: String) {
        _inputDraft.value = text
    }

    fun addImages(images: List<ImageData>) {
        _pendingImages.value = _pendingImages.value + images
    }

    fun removeImage(index: Int) {
        _pendingImages.value = _pendingImages.value.filterIndexed { i, _ -> i != index }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun sendInput() {
        val text = _inputDraft.value.trim()
        val images = _pendingImages.value
        if (text.isBlank() && images.isEmpty()) return
        _sending.value = true
        viewModelScope.launch {
            try {
                val client = ApiClient(taskRepository.serverURL())
                client.sendInput(
                    taskId,
                    InputReq(
                        prompt = Prompt(text = text, images = images.ifEmpty { null }),
                    ),
                )
                _inputDraft.value = ""
                _pendingImages.value = emptyList()
            } catch (e: Exception) {
                showActionError("send failed: ${e.message}")
            } finally {
                _sending.value = false
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun syncTask(force: Boolean = false) {
        _pendingAction.value = "sync"
        viewModelScope.launch {
            try {
                val client = ApiClient(taskRepository.serverURL())
                val resp = client.syncTask(taskId, SyncReq(force = if (force) true else null))
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
    fun terminateTask() {
        _pendingAction.value = "terminate"
        viewModelScope.launch {
            try {
                val client = ApiClient(taskRepository.serverURL())
                client.terminateTask(taskId)
            } catch (e: Exception) {
                showActionError("terminate failed: ${e.message}")
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
                val client = ApiClient(taskRepository.serverURL())
                client.restartTask(taskId, RestartReq(prompt = Prompt(text = prompt)))
            } catch (e: Exception) {
                showActionError("restart failed: ${e.message}")
            } finally {
                _pendingAction.value = null
            }
        }
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures as null.
    suspend fun loadToolInput(toolUseID: String): JsonElement? = try {
        ApiClient(taskRepository.serverURL()).getTaskToolInput(taskId, toolUseID).input
    } catch (_: Exception) {
        null
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
