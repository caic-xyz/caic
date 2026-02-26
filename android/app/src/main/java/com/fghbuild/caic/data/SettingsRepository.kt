// Persisted user settings backed by DataStore preferences.
package com.fghbuild.caic.data

import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import com.caic.sdk.v1.PreferencesResp
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.SharingStarted
import javax.inject.Inject
import javax.inject.Singleton

data class SettingsState(
    val serverURL: String = "",
    val voiceEnabled: Boolean = true,
    val voiceName: String = "Orus",
)

@Singleton
class SettingsRepository @Inject constructor(private val dataStore: DataStore<Preferences>) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    private object Keys {
        val SERVER_URL = stringPreferencesKey("SERVER_URL")
        val VOICE_ENABLED = booleanPreferencesKey("VOICE_ENABLED")
        val VOICE_NAME = stringPreferencesKey("VOICE_NAME")
    }

    val settings: StateFlow<SettingsState> = dataStore.data
        .map { prefs ->
            SettingsState(
                serverURL = prefs[Keys.SERVER_URL] ?: "",
                voiceEnabled = prefs[Keys.VOICE_ENABLED] ?: true,
                voiceName = prefs[Keys.VOICE_NAME] ?: "Orus",
            )
        }
        .stateIn(scope, SharingStarted.Eagerly, SettingsState())

    suspend fun updateServerURL(url: String) {
        dataStore.edit { it[Keys.SERVER_URL] = url.trimEnd('/') }
    }

    suspend fun updateVoiceEnabled(enabled: Boolean) {
        dataStore.edit { it[Keys.VOICE_ENABLED] = enabled }
    }

    suspend fun updateVoiceName(name: String) {
        dataStore.edit { it[Keys.VOICE_NAME] = name }
    }

    // Server preferences cached after first fetch by TaskListViewModel.
    private val _serverPreferences = MutableStateFlow<PreferencesResp?>(null)
    val serverPreferences: StateFlow<PreferencesResp?> = _serverPreferences.asStateFlow()

    fun updateServerPreferences(prefs: PreferencesResp?) {
        _serverPreferences.value = prefs
    }
}
