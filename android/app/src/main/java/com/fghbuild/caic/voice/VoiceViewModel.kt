// Activity-scoped ViewModel bridging VoiceSessionManager to the voice overlay UI.
package com.fghbuild.caic.voice

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.TaskJSON
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch
import java.io.IOException
import javax.inject.Inject

@HiltViewModel
class VoiceViewModel @Inject constructor(
    private val voiceSessionManager: VoiceSessionManager,
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
) : ViewModel() {

    val voiceState: StateFlow<VoiceState> = voiceSessionManager.state

    val settings = settingsRepository.settings

    private var previousTaskStates: Map<String, String> = emptyMap()

    init {
        viewModelScope.launch {
            taskRepository.tasks.collect { tasks ->
                notifyTaskChanges(tasks)
                previousTaskStates = tasks.associate { it.id to it.state }
            }
        }
    }

    fun connect() {
        viewModelScope.launch {
            try {
                voiceSessionManager.connect()
            } catch (e: IOException) {
                voiceSessionManager.setError(e.message ?: "Connection failed")
            } catch (e: IllegalArgumentException) {
                voiceSessionManager.setError(e.message ?: "Invalid configuration")
            }
        }
    }

    fun startListening() {
        voiceSessionManager.startAudio()
    }

    fun stopListening() {
        voiceSessionManager.stopAudio()
    }

    fun disconnect() {
        voiceSessionManager.disconnect()
    }

    fun setActiveTaskCallback(callback: (String) -> Unit) {
        voiceSessionManager.onSetActiveTask = callback
    }

    private fun notifyTaskChanges(tasks: List<TaskJSON>) {
        if (!voiceSessionManager.state.value.connected) return
        tasks
            .filter { task ->
                val prev = previousTaskStates[task.id]
                prev != null && prev != task.state
            }
            .forEach { task ->
                val shortName = task.task.lines().firstOrNull()?.take(SHORT_NAME_MAX) ?: task.id
                val notification = buildNotification(task, shortName)
                if (notification != null) {
                    voiceSessionManager.injectText(notification)
                }
            }
    }

    private fun buildNotification(task: TaskJSON, shortName: String): String? = when {
        task.state == "asking" ->
            "[Task '$shortName' needs input]"
        task.state == "waiting" ->
            "[Task '$shortName' is waiting for input]"
        task.state == "terminated" && task.result != null ->
            "[Task '$shortName' completed: ${task.result}]"
        task.state == "failed" ->
            "[Task '$shortName' failed: ${task.error ?: "unknown error"}]"
        else -> null
    }

    override fun onCleared() {
        super.onCleared()
        voiceSessionManager.disconnect()
    }

    companion object {
        private const val SHORT_NAME_MAX = 30
    }
}
