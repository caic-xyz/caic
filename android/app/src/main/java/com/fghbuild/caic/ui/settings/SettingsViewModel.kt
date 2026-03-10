// ViewModel for the Settings screen, managing connection testing and preference updates.
package com.fghbuild.caic.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.SettingsState
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.FlowPreview
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.debounce
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import javax.inject.Inject

enum class ConnectionStatus { Idle, Testing, Success, Failed }

data class SettingsScreenState(
    val settings: SettingsState = SettingsState(),
    val connectionStatus: ConnectionStatus = ConnectionStatus.Idle,
    val serverLabel: String = "",
)

private const val DEBOUNCE_MS = 500L

@OptIn(FlowPreview::class)
@HiltViewModel
class SettingsViewModel @Inject constructor(
    private val settingsRepository: SettingsRepository,
    @Suppress("UnusedPrivateProperty") private val apiClient: ApiClient,
) : ViewModel() {
    private val _state = MutableStateFlow(SettingsScreenState())
    val state: StateFlow<SettingsScreenState> = _state.asStateFlow()

    // Local buffers for the active server's text fields so keystrokes aren't blocked by DataStore round-trips.
    private val serverURLDraft = MutableStateFlow("")
    private val serverLabelDraft = MutableStateFlow("")

    init {
        viewModelScope.launch {
            var previousServerId = ""
            settingsRepository.settings.collect { settings ->
                val serverChanged = settings.activeServerId != previousServerId && previousServerId.isNotEmpty()
                previousServerId = settings.activeServerId
                _state.update { prev ->
                    val seedDrafts = serverChanged ||
                        (prev.settings.serverURL.isEmpty() && settings.serverURL.isNotEmpty())
                    if (seedDrafts) {
                        serverURLDraft.value = settings.serverURL
                        val active = settings.servers.firstOrNull { it.id == settings.activeServerId }
                        serverLabelDraft.value = active?.label ?: ""
                    }
                    prev.copy(
                        settings = settings.copy(serverURL = serverURLDraft.value),
                        serverLabel = serverLabelDraft.value,
                        connectionStatus = if (serverChanged) ConnectionStatus.Idle else prev.connectionStatus,
                    )
                }
            }
        }
        // Debounce URL writes to DataStore.
        viewModelScope.launch {
            serverURLDraft.drop(1).debounce(DEBOUNCE_MS).collect { url ->
                settingsRepository.updateServerURL(url)
            }
        }
        // Debounce label writes to DataStore.
        viewModelScope.launch {
            serverLabelDraft.drop(1).debounce(DEBOUNCE_MS).collect { label ->
                settingsRepository.updateServerLabel(label)
            }
        }
    }

    fun updateServerURL(url: String) {
        serverURLDraft.value = url
        _state.update { it.copy(settings = it.settings.copy(serverURL = url)) }
    }

    fun updateServerLabel(label: String) {
        serverLabelDraft.value = label
        _state.update { it.copy(serverLabel = label) }
    }

    fun updateVoiceEnabled(enabled: Boolean) {
        viewModelScope.launch { settingsRepository.updateVoiceEnabled(enabled) }
    }

    fun updateVoiceName(name: String) {
        viewModelScope.launch { settingsRepository.updateVoiceName(name) }
    }

    fun addServer() {
        viewModelScope.launch { settingsRepository.addServer() }
    }

    fun removeServer(id: String) {
        viewModelScope.launch { settingsRepository.removeServer(id) }
    }

    fun switchServer(id: String) {
        viewModelScope.launch { settingsRepository.switchServer(id) }
    }

    fun testConnection() {
        val url = _state.value.settings.serverURL.trimEnd('/')
        if (url.isBlank()) {
            _state.update { it.copy(connectionStatus = ConnectionStatus.Failed) }
            return
        }
        // Persist the trimmed URL immediately so subsequent navigations use it.
        serverURLDraft.value = url
        _state.update {
            it.copy(settings = it.settings.copy(serverURL = url), connectionStatus = ConnectionStatus.Testing)
        }
        viewModelScope.launch {
            settingsRepository.updateServerURL(url)
            try {
                val client = ApiClient(url, tokenProvider = { settingsRepository.settings.value.authToken })
                client.getConfig()
                _state.update { it.copy(connectionStatus = ConnectionStatus.Success) }
            } catch (_: Exception) {
                _state.update { it.copy(connectionStatus = ConnectionStatus.Failed) }
            }
        }
    }
}
