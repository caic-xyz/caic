// Persisted user settings backed by DataStore preferences.
package com.fghbuild.caic.data

import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import com.caic.sdk.v1.PreferencesResp
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.util.UUID
import javax.inject.Inject
import javax.inject.Singleton

/** A single server's connection configuration. */
@Serializable
data class ServerConfig(
    val id: String,
    val label: String = "",
    val url: String = "",
    val authToken: String? = null,
)

data class SettingsState(
    val serverURL: String = "",
    val voiceEnabled: Boolean = true,
    val voiceName: String = "Orus",
    val authToken: String? = null,
    val servers: List<ServerConfig> = emptyList(),
    val activeServerId: String = "",
)

@Singleton
class SettingsRepository @Inject constructor(private val dataStore: DataStore<Preferences>) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val json = Json { ignoreUnknownKeys = true }

    private object Keys {
        val SERVERS = stringPreferencesKey("SERVERS")
        val ACTIVE_SERVER_ID = stringPreferencesKey("ACTIVE_SERVER_ID")
        val VOICE_ENABLED = booleanPreferencesKey("VOICE_ENABLED")
        val VOICE_NAME = stringPreferencesKey("VOICE_NAME")
    }

    val settings: StateFlow<SettingsState> = dataStore.data
        .map { prefs ->
            val servers = decodeServers(prefs)
            val activeId = prefs[Keys.ACTIVE_SERVER_ID] ?: servers.firstOrNull()?.id ?: ""
            val active = servers.firstOrNull { it.id == activeId } ?: servers.firstOrNull()
            SettingsState(
                serverURL = active?.url ?: "",
                authToken = active?.authToken,
                voiceEnabled = prefs[Keys.VOICE_ENABLED] ?: true,
                voiceName = prefs[Keys.VOICE_NAME] ?: "Orus",
                servers = servers,
                activeServerId = active?.id ?: "",
            )
        }
        .stateIn(scope, SharingStarted.Eagerly, SettingsState())

    private fun decodeServers(prefs: Preferences): List<ServerConfig> =
        prefs[Keys.SERVERS]?.let {
            try { json.decodeFromString<List<ServerConfig>>(it) } catch (_: Exception) { null }
        } ?: emptyList()

    /** Update the active server's config within an edit block. */
    private suspend fun updateActiveServer(transform: (ServerConfig) -> ServerConfig) {
        dataStore.edit { prefs ->
            val servers = decodeServers(prefs)
            val activeId = prefs[Keys.ACTIVE_SERVER_ID] ?: servers.firstOrNull()?.id ?: return@edit
            val updated = servers.map { if (it.id == activeId) transform(it) else it }
            prefs[Keys.SERVERS] = json.encodeToString(updated)
        }
    }

    suspend fun updateServerURL(url: String) {
        updateActiveServer { it.copy(url = url.trimEnd('/')) }
    }

    suspend fun updateServerLabel(label: String) {
        updateActiveServer { it.copy(label = label) }
    }

    suspend fun updateVoiceEnabled(enabled: Boolean) {
        dataStore.edit { it[Keys.VOICE_ENABLED] = enabled }
    }

    suspend fun updateVoiceName(name: String) {
        dataStore.edit { it[Keys.VOICE_NAME] = name }
    }

    suspend fun updateAuthToken(token: String?) {
        updateActiveServer { it.copy(authToken = token) }
    }

    /** Add a new server and make it active. Returns the new server's ID. */
    suspend fun addServer(label: String = ""): String {
        val id = UUID.randomUUID().toString()
        dataStore.edit { prefs ->
            val servers = decodeServers(prefs)
            val newLabel = label.ifBlank { "Server ${servers.size + 1}" }
            prefs[Keys.SERVERS] = json.encodeToString(servers + ServerConfig(id = id, label = newLabel))
            prefs[Keys.ACTIVE_SERVER_ID] = id
        }
        return id
    }

    /** Remove a server. Switches to the first remaining server if the removed one was active. */
    suspend fun removeServer(id: String) {
        dataStore.edit { prefs ->
            val servers = decodeServers(prefs).filter { it.id != id }
            prefs[Keys.SERVERS] = json.encodeToString(servers)
            if (prefs[Keys.ACTIVE_SERVER_ID] == id) {
                prefs[Keys.ACTIVE_SERVER_ID] = servers.firstOrNull()?.id ?: ""
            }
        }
    }

    /** Switch the active server. */
    suspend fun switchServer(id: String) {
        dataStore.edit { prefs ->
            prefs[Keys.ACTIVE_SERVER_ID] = id
        }
    }

    // Server preferences cached after first fetch by TaskListViewModel.
    private val _serverPreferences = MutableStateFlow<PreferencesResp?>(null)
    val serverPreferences: StateFlow<PreferencesResp?> = _serverPreferences.asStateFlow()

    fun updateServerPreferences(prefs: PreferencesResp?) {
        _serverPreferences.value = prefs
    }
}
