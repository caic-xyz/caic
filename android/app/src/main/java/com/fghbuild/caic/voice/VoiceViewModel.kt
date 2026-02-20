// Activity-scoped ViewModel bridging VoiceSessionManager to the voice overlay UI.
package com.fghbuild.caic.voice

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.Task
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import javax.inject.Inject

@HiltViewModel
class VoiceViewModel @Inject constructor(
    private val voiceSessionManager: VoiceSessionManager,
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
) : ViewModel() {

    val voiceState: StateFlow<VoiceState> = voiceSessionManager.state

    val settings = settingsRepository.settings

    private val taskNumberMap: TaskNumberMap
        get() = voiceSessionManager.taskNumberMap

    private var previousTaskStates: Map<String, String> = emptyMap()

    /** Task IDs that were already terminated/failed when the voice session connected. */
    private var preTerminatedIds: Set<String> = emptySet()

    init {
        // Inject snapshot when the session transitions to connected.
        viewModelScope.launch {
            voiceSessionManager.state
                .map { it.connected }
                .distinctUntilChanged()
                .collect { connected ->
                    if (connected) {
                        val tasks = taskRepository.tasks.value
                        preTerminatedIds = tasks
                            .filter { it.state == "terminated" || it.state == "failed" }
                            .map { it.id }
                            .toSet()
                        voiceSessionManager.excludedTaskIds = preTerminatedIds
                        val active = tasks.filter { it.id !in preTerminatedIds }
                        taskNumberMap.reset()
                        taskNumberMap.update(active)
                        val recentRepo = settingsRepository.settings.value.recentRepos.firstOrNull()
                        voiceSessionManager.injectText(buildSnapshot(active, recentRepo))
                        previousTaskStates = tasks.associate { it.id to it.state }
                    }
                }
        }
        // Track state changes for diff-based notifications while connected.
        viewModelScope.launch {
            taskRepository.tasks.collect { tasks ->
                if (voiceSessionManager.state.value.connected) {
                    taskNumberMap.update(tasks.filter { it.id !in preTerminatedIds })
                    notifyTaskChanges(tasks)
                }
                previousTaskStates = tasks.associate { it.id to it.state }
            }
        }
    }

    fun connect() {
        voiceSessionManager.connect()
    }

    fun disconnect() {
        voiceSessionManager.disconnect()
    }

    fun selectAudioDevice(deviceId: Int) {
        voiceSessionManager.selectAudioDevice(deviceId)
    }

    private fun notifyTaskChanges(tasks: List<Task>) {
        tasks
            .filter { task ->
                val prev = previousTaskStates[task.id]
                prev != null && prev != task.state
            }
            .forEach { task ->
                val notification = buildNotification(task)
                if (notification != null) {
                    voiceSessionManager.injectText(notification)
                }
            }
    }

    private fun buildSnapshot(tasks: List<Task>, recentRepo: String?): String {
        val parts = mutableListOf<String>()
        if (recentRepo != null) {
            parts.add("[Default repo: $recentRepo]")
        }
        if (tasks.isNotEmpty()) {
            val lines = tasks.joinToString("\n") { task ->
                val num = taskNumberMap.toNumber(task.id) ?: 0
                val shortName = task.initialPrompt.lines().firstOrNull()?.take(SHORT_NAME_MAX) ?: task.id
                val base = "- Task #$num: $shortName (${task.state}, ${formatElapsed(task.duration)}" +
                    ", ${formatCost(task.costUSD)}, ${task.harness})"
                if (task.state == "asking") "$base — needs input" else base
            }
            parts.add("[Current tasks at session start]\n$lines")
        } else if (recentRepo == null) {
            return "[No active tasks]"
        }
        return parts.joinToString("\n")
    }

    private fun buildNotification(task: Task): String? {
        val num = taskNumberMap.toNumber(task.id) ?: return null
        val shortName = task.initialPrompt.lines().firstOrNull()?.take(SHORT_NAME_MAX) ?: task.id
        return when {
            task.state == "asking" ->
                "[Task #$num ($shortName) needs input]"
            task.state == "waiting" ->
                "[Task #$num ($shortName) is waiting for input]"
            task.state == "terminated" && task.result != null ->
                "[Task #$num ($shortName) completed: ${task.result}]"
            task.state == "failed" ->
                "[Task #$num ($shortName) failed: ${task.error ?: "unknown error"}]"
            else -> null
        }
    }

    override fun onCleared() {
        super.onCleared()
        voiceSessionManager.disconnect()
    }

    companion object {
        private const val SHORT_NAME_MAX = 30
    }
}
