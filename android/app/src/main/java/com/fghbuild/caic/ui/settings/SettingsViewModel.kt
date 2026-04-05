// ViewModel for the Settings screen, managing connection testing and preference updates.
package com.fghbuild.caic.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.CacheMappingResp
import com.caic.sdk.v1.UpdatePreferencesReq
import com.caic.sdk.v1.UserSettings
import com.caic.sdk.v1.WellKnownCache
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
    val autoFixCI: Boolean = false,
    val autoFixPR: Boolean = false,
    val baseImage: String = "",
    val gitHubTokenAccess: String = "",
    val useDefaultCaches: Boolean = true,
    val wellKnownCaches: Map<String, Boolean> = emptyMap(),
    val wellKnownCachesList: List<WellKnownCache> = emptyList(),
    val cacheMappings: List<CacheMappingResp> = emptyList(),
    val serverVersion: String = "",
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
                if (settings.serverURL.isNotBlank()) loadServerPreferences(settings.serverURL, settings.authToken)
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

    private fun loadServerPreferences(serverURL: String, authToken: String?) {
        viewModelScope.launch {
            try {
                val client = ApiClient(serverURL, tokenProvider = { authToken })
                val prefs = client.getPreferences()
                val caches = try { client.listCaches() } catch (_: Exception) { null }
                val config = try { client.getConfig() } catch (_: Exception) { null }
                _state.update { prev ->
                    prev.copy(
                        autoFixCI = prefs.settings.autoFixOnCIFailure,
                        autoFixPR = prefs.settings.autoFixOnPROpen ?: false,
                        baseImage = prefs.settings.baseImage ?: "",
                        gitHubTokenAccess = prefs.settings.gitHubTokenAccess ?: "",
                        useDefaultCaches = prefs.settings.useDefaultCaches ?: true,
                        wellKnownCaches = prefs.settings.wellKnownCaches ?: emptyMap(),
                        wellKnownCachesList = caches?.wellKnown ?: emptyList(),
                        cacheMappings = prefs.settings.cacheMappings ?: emptyList(),
                        serverVersion = config?.version ?: "",
                    )
                }
            } catch (_: Exception) {
                // Server may not be reachable; leave defaults.
            }
        }
    }

    fun updateAutoFixCI(enabled: Boolean) {
        _state.update { it.copy(autoFixCI = enabled) }
        saveSettings { it.copy(autoFixOnCIFailure = enabled) }
    }

    fun updateAutoFixPR(enabled: Boolean) {
        _state.update { it.copy(autoFixPR = enabled) }
        saveSettings { it.copy(autoFixOnPROpen = enabled) }
    }

    fun updateBaseImage(image: String) {
        _state.update { it.copy(baseImage = image) }
    }

    fun saveBaseImage() {
        saveSettings { it.copy(baseImage = _state.value.baseImage.ifBlank { null }) }
    }

    fun updateGitHubTokenAccess(access: String) {
        _state.update { it.copy(gitHubTokenAccess = access) }
        saveSettings { it.copy(gitHubTokenAccess = access.ifEmpty { null }) }
    }

    fun updateUseDefaultCaches(enabled: Boolean) {
        _state.update { it.copy(useDefaultCaches = enabled) }
        saveSettings { it.copy(useDefaultCaches = enabled) }
    }

    fun updateWellKnownCache(cache: String, enabled: Boolean) {
        val current = _state.value.wellKnownCaches.toMutableMap()
        if (enabled) {
            current[cache] = true
        } else {
            current[cache] = false
        }
        _state.update { it.copy(wellKnownCaches = current) }
        saveSettings { it.copy(wellKnownCaches = current.ifEmpty { null }) }
    }

    fun addCacheMapping() {
        val current = _state.value.cacheMappings.toMutableList()
        current.add(CacheMappingResp("", ""))
        _state.update { it.copy(cacheMappings = current) }
    }

    fun updateCacheMapping(index: Int, hostPath: String, containerPath: String) {
        val current = _state.value.cacheMappings.toMutableList()
        if (index in current.indices) {
            current[index] = CacheMappingResp(hostPath, containerPath)
            _state.update { it.copy(cacheMappings = current) }
        }
    }

    fun removeCacheMapping(index: Int) {
        val current = _state.value.cacheMappings.toMutableList()
        if (index in current.indices) {
            current.removeAt(index)
            _state.update { it.copy(cacheMappings = current) }
            saveSettings { it.copy(cacheMappings = current.ifEmpty { null }) }
        }
    }

    fun saveCacheMappings() {
        saveSettings { it.copy(cacheMappings = _state.value.cacheMappings.ifEmpty { null }) }
    }

    private fun saveSettings(update: (UserSettings) -> UserSettings) {
        val snapshot = _state.value
        viewModelScope.launch {
            try {
                val settings = settingsRepository.settings.value
                val client = ApiClient(settings.serverURL, tokenProvider = { settings.authToken })
                val current = UserSettings(
                    autoFixOnCIFailure = snapshot.autoFixCI,
                    autoFixOnPROpen = snapshot.autoFixPR,
                    baseImage = snapshot.baseImage.ifBlank { null },
                    gitHubTokenAccess = snapshot.gitHubTokenAccess.ifEmpty { null },
                    useDefaultCaches = snapshot.useDefaultCaches,
                    wellKnownCaches = snapshot.wellKnownCaches.ifEmpty { null },
                    cacheMappings = snapshot.cacheMappings.ifEmpty { null },
                )
                client.updatePreferences(UpdatePreferencesReq(settings = update(current)))
            } catch (_: Exception) {
                // Revert optimistic update on failure.
                _state.update {
                    it.copy(
                        autoFixCI = snapshot.autoFixCI,
                        autoFixPR = snapshot.autoFixPR,
                        baseImage = snapshot.baseImage,
                        gitHubTokenAccess = snapshot.gitHubTokenAccess,
                        useDefaultCaches = snapshot.useDefaultCaches,
                        wellKnownCaches = snapshot.wellKnownCaches,
                        cacheMappings = snapshot.cacheMappings,
                    )
                }
            }
        }
    }
}
