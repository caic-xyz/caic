// Activity-scoped ViewModel bridging server voice availability and VoiceSession to the voice overlay UI.
package com.fghbuild.caic.voice

import android.util.Log
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.CIStatus
import com.caic.sdk.v1.Config
import com.caic.sdk.v1.ForgePRState
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskState
import com.caic.sdk.v1.VoiceGatewayMode
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.ui.theme.terminalStates
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import javax.inject.Inject

private const val TAG = "VoiceViewModel"

internal fun isVoiceAvailable(config: Config?): Boolean =
    config?.voiceGateway?.mode?.let { it != VoiceGatewayMode.Disabled } == true

@HiltViewModel
class VoiceViewModel @Inject constructor(
    private val voiceSessionManager: VoiceSession,
    private val taskRepository: TaskRepository,
    private val settingsRepository: SettingsRepository,
) : ViewModel() {

    val voiceState: StateFlow<VoiceState> = voiceSessionManager.state

    val voiceAvailable: StateFlow<Boolean> = settingsRepository.serverConfig
        .map(::isVoiceAvailable)
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5000), false)

    val serverWarnings = taskRepository.warnings

    private val taskNumberMap: TaskNumberMap
        get() = voiceSessionManager.taskNumberMap

    private var previousTaskStates: Map<String, TaskState> = emptyMap()
    private var previousCIStatuses: Map<String, CIStatus?> = emptyMap()

    /** Task IDs that were already purged/failed when the voice session connected. */
    private var prePurgedIds: Set<String> = emptySet()

    init {
        observeVoiceGatewayConfig()
        observeVoiceGatewayReconnects()
        // Inject snapshot when the session transitions to connected.
        viewModelScope.launch {
            voiceSessionManager.state
                .map { it.connected }
                .distinctUntilChanged()
                .collect { connected ->
                    if (connected) {
                        val tasks = taskRepository.tasks.value
                        prePurgedIds = tasks
                            .filter { it.state in terminalStates }
                            .map { it.id }
                            .toSet()
                        voiceSessionManager.excludedTaskIds = prePurgedIds
                        val active = tasks.filter { it.id !in prePurgedIds }
                            .sortedWith(compareBy<Task> { it.id.length }.thenBy { it.id })
                        taskNumberMap.reset()
                        taskNumberMap.update(active)
                        val prefs = settingsRepository.serverPreferences.value
                        val recentRepo = prefs?.repositories?.firstOrNull()?.path
                        val defaultHarness = prefs?.harness?.ifBlank { null }
                        val defaultModel = prefs?.harness?.let { h -> prefs.models?.get(h) }?.ifBlank { null }
                        voiceSessionManager.injectText(buildSnapshot(active, recentRepo, defaultHarness, defaultModel))
                        previousTaskStates = tasks.associate { it.id to it.state }
                        previousCIStatuses = tasks.associate { it.id to it.ciStatus }
                    }
                }
        }
        viewModelScope.launch {
            voiceAvailable.collect { available ->
                if (!available) voiceSessionManager.disconnect()
            }
        }
        // Track state changes for diff-based notifications while connected.
        viewModelScope.launch {
            taskRepository.tasks.collect { tasks ->
                if (voiceSessionManager.state.value.connected) {
                    taskNumberMap.update(tasks.filter { it.id !in prePurgedIds })
                    notifyTaskChanges(tasks)
                }
                previousTaskStates = tasks.associate { it.id to it.state }
                previousCIStatuses = tasks.associate { it.id to it.ciStatus }
            }
        }
    }

    fun connect() {
        if (!voiceAvailable.value) return
        voiceSessionManager.connect()
    }

    fun disconnect() {
        voiceSessionManager.disconnect()
    }

    fun toggleMute() {
        voiceSessionManager.toggleMute()
    }

    fun selectAudioDevice(deviceId: Int) {
        voiceSessionManager.selectAudioDevice(deviceId)
    }

    fun clearTranscript() {
        voiceSessionManager.clearTranscript()
    }

    private fun observeVoiceGatewayConfig() {
        viewModelScope.launch {
            settingsRepository.settings
                .map { it.serverURL to it.authToken }
                .distinctUntilChanged()
                .collect { refreshVoiceGatewayConfig() }
        }
    }

    private fun observeVoiceGatewayReconnects() {
        viewModelScope.launch {
            taskRepository.connected.collect { connected ->
                if (connected) refreshVoiceGatewayConfig()
            }
        }
    }

    private suspend fun refreshVoiceGatewayConfig() {
        val serverURL = settingsRepository.settings.value.serverURL
        if (serverURL.isBlank()) {
            settingsRepository.updateServerConfig(null)
            return
        }
        try {
            val client = ApiClient(
                serverURL,
                tokenProvider = { settingsRepository.settings.value.authToken },
            )
            settingsRepository.updateServerConfig(client.getConfig())
        } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
            Log.w(TAG, "Failed to load voice gateway config", e)
            settingsRepository.updateServerConfig(null)
        }
    }

    /** Returns the voice-mode task number for [id], or null if not mapped or session not connected. */
    fun getTaskNumber(id: String): Int? {
        if (!voiceSessionManager.state.value.connected) return null
        return taskNumberMap.toNumber(id)
    }

    private fun notifyTaskChanges(tasks: List<Task>) {
        for (task in tasks) {
            val prev = previousTaskStates[task.id]
            if (prev != null && prev != task.state) {
                val notification = buildNotification(task)
                if (notification != null) voiceSessionManager.injectText(notification)
            }
            val prevCI = previousCIStatuses[task.id]
            if (prevCI != null && prevCI != CIStatus.Failure && task.ciStatus == CIStatus.Failure) {
                val notification = buildCIFailureNotification(task)
                if (notification != null) voiceSessionManager.injectText(notification)
            }
        }
    }

    private fun buildSnapshot(
        tasks: List<Task>,
        recentRepo: String?,
        defaultHarness: String? = null,
        defaultModel: String? = null,
    ): String {
        val parts = mutableListOf<String>()
        if (recentRepo != null) parts.add("[Default repo: $recentRepo]")
        if (!defaultHarness.isNullOrBlank()) parts.add("[Default harness: $defaultHarness]")
        if (!defaultModel.isNullOrBlank()) parts.add("[Default model: $defaultModel]")
        if (tasks.isNotEmpty()) {
            val lines = tasks.joinToString("\n") { task ->
                val num = taskNumberMap.toNumber(task.id) ?: 0
                val shortName = task.title.ifBlank { task.id }
                "- Task #$num: $shortName (${task.state}, ${formatElapsed(task.duration)}" +
                    ", ${formatCost(task.costUSD)}, ${task.harness})"
            }
            parts.add("[Current tasks at session start]\n$lines")
        } else if (parts.isEmpty()) {
            return "[No active tasks]"
        }
        return parts.joinToString("\n")
    }

    private fun buildCIFailureNotification(task: Task): String? {
        val num = taskNumberMap.toNumber(task.id) ?: return null
        val shortName = task.title.ifBlank { task.id }
        val pr = task.forgePR?.takeIf {
            it > 0 && task.forgePRState != ForgePRState.Closed &&
                task.forgePRState != ForgePRState.Merged
        }?.let { " PR #$it" } ?: ""
        return "[Task #$num ($shortName)$pr — CI: failure]"
    }

    private fun buildNotification(task: Task): String? {
        val num = taskNumberMap.toNumber(task.id) ?: return null
        val shortName = task.title.ifBlank { task.id }
        return when (task.state) {
            is TaskState.Asking, is TaskState.Waiting, is TaskState.HasPlan ->
                "[Task #$num ($shortName) — ${task.state.value}]"
            is TaskState.Purged ->
                task.result?.let { "[Task #$num ($shortName) — completed: $it]" }
            is TaskState.Stopped ->
                "[Task #$num ($shortName) — stopped: container died]"
            is TaskState.Failed ->
                "[Task #$num ($shortName) — failed: ${task.error ?: "unknown"}]"
            else -> null
        }
    }

    override fun onCleared() {
        super.onCleared()
        voiceSessionManager.disconnect()
    }
}
