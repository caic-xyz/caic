// ViewModel for the Settings screen, managing connection testing and preference updates.
package com.fghbuild.caic.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import android.util.Log
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.CacheMappingResp
import com.caic.sdk.v1.MountMappingResp
import com.caic.sdk.v1.Platform
import com.caic.sdk.v1.UpdatePreferencesReq
import com.caic.sdk.v1.UserSettings
import com.caic.sdk.v1.VersionResp
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
    val containerPlatform: String = "",
    val maxCPUs: String = "",
    val useDefaultCaches: Boolean = true,
    val wellKnownCaches: Map<String, Boolean> = emptyMap(),
    val wellKnownCachesList: List<WellKnownCache> = emptyList(),
    val cacheMappings: List<CacheMappingResp> = emptyList(),
    val customMounts: List<MountMappingResp> = emptyList(),
    val serverVersion: String = "",
    val versionInfo: VersionResp? = null,
    val checkingUpdate: Boolean = false,
    val updateStatus: String = "",
    val updating: Boolean = false,
)

private const val DEBOUNCE_MS = 500L

private const val TAG = "SettingsViewModel"

private fun String.toPlatformOrNull(): Platform? = when (this) {
    "" -> null
    "linux/arm64" -> Platform.LinuxARM64
    "linux/amd64" -> Platform.LinuxAMD64
    else -> Platform.Other(this)
}

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
            } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                Log.w(TAG, "Connection test failed", e)
                _state.update { it.copy(connectionStatus = ConnectionStatus.Failed) }
            }
        }
    }

    private fun loadServerPreferences(serverURL: String, authToken: String?) {
        viewModelScope.launch {
            try {
                val client = ApiClient(serverURL, tokenProvider = { authToken })
                val prefs = client.getPreferences()
                val caches = try { client.listCaches() } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                    Log.w(TAG, "Failed to list caches", e)
                    null
                }
                val config = try { client.getConfig() } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                    Log.w(TAG, "Failed to get config", e)
                    null
                }
                _state.update { prev ->
                    prev.copy(
                        autoFixCI = prefs.settings.autoFixOnCIFailure,
                        autoFixPR = prefs.settings.autoFixOnPROpen ?: false,
                        baseImage = prefs.settings.baseImage ?: "",
                        containerPlatform = prefs.settings.containerPlatform?.value ?: "",
                        maxCPUs = prefs.settings.maxCPUs?.toString() ?: "",
                        useDefaultCaches = prefs.settings.useDefaultCaches,
                        wellKnownCaches = prefs.settings.wellKnownCaches ?: emptyMap(),
                        wellKnownCachesList = caches?.wellKnown ?: emptyList(),
                        cacheMappings = prefs.settings.cacheMappings ?: emptyList(),
                        customMounts = prefs.settings.customMounts ?: emptyList(),
                        serverVersion = config?.version ?: "",
                    )
                }
                // Fetch version info after preferences are loaded.
                checkForUpdates()
            } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                Log.w(TAG, "Failed to load server preferences", e)
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

    fun updateContainerPlatform(platform: String) {
        _state.update { it.copy(containerPlatform = platform) }
        saveSettings { it.copy(containerPlatform = platform.toPlatformOrNull()) }
    }

    fun updateMaxCPUs(cpus: String) {
        _state.update { it.copy(maxCPUs = cpus) }
    }

    fun saveMaxCPUs() {
        val v = _state.value.maxCPUs.toIntOrNull() ?: 0
        saveSettings { it.copy(maxCPUs = if (v > 0) v else null) }
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
        val mappings = _state.value.cacheMappings
        if (mappings.any { it.hostPath.isBlank() || it.containerPath.isBlank() }) return
        saveSettings { it.copy(cacheMappings = mappings.ifEmpty { null }) }
    }

    fun addCustomMount() {
        val current = _state.value.customMounts.toMutableList()
        current.add(MountMappingResp("", ""))
        _state.update { it.copy(customMounts = current) }
    }

    fun updateCustomMount(index: Int, hostPath: String, containerPath: String) {
        val current = _state.value.customMounts.toMutableList()
        if (index in current.indices) {
            current[index] = MountMappingResp(hostPath, containerPath)
            _state.update { it.copy(customMounts = current) }
        }
    }

    fun removeCustomMount(index: Int) {
        val current = _state.value.customMounts.toMutableList()
        if (index in current.indices) {
            current.removeAt(index)
            _state.update { it.copy(customMounts = current) }
            saveSettings { it.copy(customMounts = current.ifEmpty { null }) }
        }
    }

    fun saveCustomMounts() {
        val mounts = _state.value.customMounts
        if (mounts.any { it.hostPath.isBlank() || it.containerPath.isBlank() }) return
        saveSettings { it.copy(customMounts = mounts.ifEmpty { null }) }
    }

    fun checkForUpdates() {
        val settings = settingsRepository.settings.value
        if (settings.serverURL.isBlank()) return
        _state.update { it.copy(checkingUpdate = true, updateStatus = "") }
        viewModelScope.launch {
            try {
                val client = ApiClient(settings.serverURL, tokenProvider = { settings.authToken })
                val info = client.getVersion()
                _state.update { it.copy(versionInfo = info, checkingUpdate = false) }
            } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                Log.w(TAG, "Version check failed", e)
                _state.update { it.copy(checkingUpdate = false, updateStatus = "Check failed: ${e.message}") }
            }
        }
    }

    fun triggerUpdate() {
        val settings = settingsRepository.settings.value
        if (settings.serverURL.isBlank()) return
        _state.update { it.copy(updating = true, updateStatus = "") }
        viewModelScope.launch {
            try {
                val client = ApiClient(settings.serverURL, tokenProvider = { settings.authToken })
                val resp = client.triggerUpdate()
                _state.update {
                    it.copy(
                        updating = false,
                        updateStatus = if (resp.status == "started") {
                            "Update started. Server will restart shortly."
                        } else {
                            "Already up to date."
                        },
                    )
                }
            } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                Log.w(TAG, "Update trigger failed", e)
                _state.update { it.copy(updating = false, updateStatus = "Update failed: ${e.message}") }
            }
        }
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
                    containerPlatform = snapshot.containerPlatform.toPlatformOrNull(),
                    maxCPUs = snapshot.maxCPUs.toIntOrNull(),
                    useDefaultCaches = snapshot.useDefaultCaches,
                    wellKnownCaches = snapshot.wellKnownCaches.ifEmpty { null },
                    cacheMappings = snapshot.cacheMappings.ifEmpty { null },
                    customMounts = snapshot.customMounts.ifEmpty { null },
                )
                client.updatePreferences(UpdatePreferencesReq(settings = update(current)))
            } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
                Log.w(TAG, "Failed to save settings", e)
                // Revert optimistic update on failure.
                _state.update {
                    it.copy(
                        autoFixCI = snapshot.autoFixCI,
                        autoFixPR = snapshot.autoFixPR,
                        baseImage = snapshot.baseImage,
                        containerPlatform = snapshot.containerPlatform,
                        useDefaultCaches = snapshot.useDefaultCaches,
                        wellKnownCaches = snapshot.wellKnownCaches,
                        cacheMappings = snapshot.cacheMappings,
                        customMounts = snapshot.customMounts,
                    )
                }
            }
        }
    }
}
