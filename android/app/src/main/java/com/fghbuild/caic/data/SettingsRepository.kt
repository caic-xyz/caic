// Persisted user settings backed by DataStore preferences.
package com.fghbuild.caic.data

import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.SharingStarted
import javax.inject.Inject
import javax.inject.Singleton

private const val MAX_RECENT_REPOS = 5

data class SettingsState(
    val serverURL: String = "",
    val voiceEnabled: Boolean = true,
    val voiceName: String = "Orus",
    val recentRepos: List<String> = emptyList(),
    val lastModels: Map<String, String> = emptyMap(),
)

@Singleton
class SettingsRepository @Inject constructor(private val dataStore: DataStore<Preferences>) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    private object Keys {
        val SERVER_URL = stringPreferencesKey("SERVER_URL")
        val VOICE_ENABLED = booleanPreferencesKey("VOICE_ENABLED")
        val VOICE_NAME = stringPreferencesKey("VOICE_NAME")
        val RECENT_REPOS = stringPreferencesKey("RECENT_REPOS")
        // Comma-separated "harness=model" pairs.
        val LAST_MODELS = stringPreferencesKey("LAST_MODELS")
    }

    private fun parseLastModels(raw: String): Map<String, String> =
        raw.split(",").mapNotNull { entry ->
            val eq = entry.indexOf('=')
            if (eq < 0) null else entry.substring(0, eq) to entry.substring(eq + 1)
        }.toMap()

    val settings: StateFlow<SettingsState> = dataStore.data
        .map { prefs ->
            SettingsState(
                serverURL = prefs[Keys.SERVER_URL] ?: "",
                voiceEnabled = prefs[Keys.VOICE_ENABLED] ?: true,
                voiceName = prefs[Keys.VOICE_NAME] ?: "Orus",
                recentRepos = (prefs[Keys.RECENT_REPOS] ?: "")
                    .split(",")
                    .filter { it.isNotEmpty() },
                lastModels = parseLastModels(prefs[Keys.LAST_MODELS] ?: ""),
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

    suspend fun updateLastModel(harness: String, model: String) {
        dataStore.edit { prefs ->
            val current = parseLastModels(prefs[Keys.LAST_MODELS] ?: "").toMutableMap()
            if (model.isBlank()) current.remove(harness) else current[harness] = model
            prefs[Keys.LAST_MODELS] = current.entries.joinToString(",") { "${it.key}=${it.value}" }
        }
    }

    suspend fun addRecentRepo(path: String) {
        dataStore.edit { prefs ->
            val current = (prefs[Keys.RECENT_REPOS] ?: "")
                .split(",")
                .filter { it.isNotEmpty() }
            val updated = (listOf(path) + current.filter { it != path })
                .take(MAX_RECENT_REPOS)
            prefs[Keys.RECENT_REPOS] = updated.joinToString(",")
        }
    }
}
