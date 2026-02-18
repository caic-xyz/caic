// ViewModel for the task detail screen: SSE message stream, grouping, and actions.
package com.fghbuild.caic.ui.taskdetail

import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.ApiClient
import com.caic.sdk.ClaudeEventMessage
import com.caic.sdk.ClaudeTodoItem
import com.caic.sdk.EventKinds
import com.caic.sdk.InputReq
import com.caic.sdk.RestartReq
import com.caic.sdk.SafetyIssue
import com.caic.sdk.SyncReq
import com.caic.sdk.TaskJSON
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.data.TaskSSEEvent
import com.fghbuild.caic.navigation.Screen
import com.fghbuild.caic.util.MessageGroup
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.groupMessages
import com.fghbuild.caic.util.groupTurns
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

data class TaskDetailState(
    val task: TaskJSON? = null,
    val messages: List<ClaudeEventMessage> = emptyList(),
    val groups: List<MessageGroup> = emptyList(),
    val turns: List<Turn> = emptyList(),
    val todos: List<ClaudeTodoItem> = emptyList(),
    val isReady: Boolean = false,
    val sending: Boolean = false,
    val pendingAction: String? = null,
    val actionError: String? = null,
    val safetyIssues: List<SafetyIssue> = emptyList(),
    val inputDraft: String = "",
)

private val TerminalStates = setOf("terminated", "failed")

@HiltViewModel
class TaskDetailViewModel @Inject constructor(
    private val taskRepository: TaskRepository,
    savedStateHandle: SavedStateHandle,
) : ViewModel() {

    private val taskId: String = savedStateHandle[Screen.TaskDetail.ARG_TASK_ID] ?: ""

    private val _messages = MutableStateFlow<List<ClaudeEventMessage>>(emptyList())
    private val _isReady = MutableStateFlow(false)
    private val _sending = MutableStateFlow(false)
    private val _pendingAction = MutableStateFlow<String?>(null)
    private val _actionError = MutableStateFlow<String?>(null)
    private val _safetyIssues = MutableStateFlow<List<SafetyIssue>>(emptyList())
    private val _inputDraft = MutableStateFlow("")

    private var sseJob: Job? = null

    @Suppress("UNCHECKED_CAST")
    val state: StateFlow<TaskDetailState> = combine(
        listOf(
            taskRepository.tasks, _messages, _isReady, _sending,
            _pendingAction, _actionError, _safetyIssues, _inputDraft,
        )
    ) { values ->
        val tasks = values[0] as List<TaskJSON>
        val msgs = values[1] as List<ClaudeEventMessage>
        val ready = values[2] as Boolean
        val sending = values[3] as Boolean
        val action = values[4] as String?
        val error = values[5] as String?
        val safety = values[6] as List<SafetyIssue>
        val draft = values[7] as String
        val task = tasks.firstOrNull { it.id == taskId }
        val groups = groupMessages(msgs)
        val turns = groupTurns(groups)
        val lastTodo = msgs.lastOrNull { it.kind == EventKinds.Todo }?.todo?.todos.orEmpty()
        TaskDetailState(
            task = task,
            messages = msgs,
            groups = groups,
            turns = turns,
            todos = lastTodo,
            isReady = ready,
            sending = sending,
            pendingAction = action,
            actionError = error,
            safetyIssues = safety,
            inputDraft = draft,
        )
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), TaskDetailState())

    init {
        connectSSE()
    }

    private fun connectSSE() {
        sseJob?.cancel()
        sseJob = viewModelScope.launch {
            val baseURL = taskRepository.serverURL()
            if (baseURL.isBlank()) return@launch

            var delayMs = 500L
            val buf = mutableListOf<ClaudeEventMessage>()
            var live = false

            while (true) {
                buf.clear()
                live = false
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
                                    _messages.value = _messages.value + event.msg
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

    fun updateInputDraft(text: String) {
        _inputDraft.value = text
    }

    @Suppress("TooGenericExceptionCaught") // Error boundary: surface all API failures to UI.
    fun sendInput() {
        val text = _inputDraft.value.trim()
        if (text.isBlank()) return
        _sending.value = true
        viewModelScope.launch {
            try {
                val client = ApiClient(taskRepository.serverURL())
                client.sendInput(taskId, InputReq(prompt = text))
                _inputDraft.value = ""
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
                client.restartTask(taskId, RestartReq(prompt = prompt))
            } catch (e: Exception) {
                showActionError("restart failed: ${e.message}")
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
