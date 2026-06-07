// Persisted Go Mode service-instance settings backed by DataStore preferences.
package com.fghbuild.gomode.data

import androidx.datastore.core.DataStore
import androidx.datastore.preferences.core.Preferences
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.util.UUID

@Serializable
data class ServiceInstance(
    val id: String,
    val label: String = "",
    val kind: String = "web",
    val url: String = "",
)

data class SettingsState(
    val activeServiceURL: String = "",
    val haloAddress: String? = null,
    val haloAutoConnect: Boolean = false,
    val services: List<ServiceInstance> = emptyList(),
    val activeServiceId: String = "",
)

class SettingsRepository(private val dataStore: DataStore<Preferences>) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val json = Json { ignoreUnknownKeys = true }

    private object Keys {
        val SERVICES = stringPreferencesKey("SERVICES")
        val ACTIVE_SERVICE_ID = stringPreferencesKey("ACTIVE_SERVICE_ID")
        val HALO_ADDRESS = stringPreferencesKey("HALO_ADDRESS")
        val HALO_AUTO_CONNECT = booleanPreferencesKey("HALO_AUTO_CONNECT")
    }

    val settings: StateFlow<SettingsState> = dataStore.data
        .map { prefs ->
            val services = decodeServices(prefs)
            val activeId = prefs[Keys.ACTIVE_SERVICE_ID] ?: services.firstOrNull()?.id ?: ""
            val active = services.firstOrNull { it.id == activeId } ?: services.firstOrNull()
            SettingsState(
                activeServiceURL = active?.url ?: "",
                haloAddress = prefs[Keys.HALO_ADDRESS],
                haloAutoConnect = prefs[Keys.HALO_AUTO_CONNECT] ?: false,
                services = services,
                activeServiceId = active?.id ?: "",
            )
        }
        .stateIn(scope, SharingStarted.Eagerly, SettingsState())

    suspend fun saveActiveService(label: String, url: String): String {
        val normalizedURL = normalizeURL(url)
        var savedId = ""
        dataStore.edit { prefs ->
            val services = decodeServices(prefs)
            val activeId = prefs[Keys.ACTIVE_SERVICE_ID] ?: services.firstOrNull()?.id ?: ""
            val activeExists = services.any { it.id == activeId }

            if (activeExists) {
                savedId = activeId
                prefs[Keys.SERVICES] = json.encodeToString(
                    services.map { service ->
                        if (service.id == activeId) {
                            service.copy(
                                label = label.ifBlank { service.label.ifBlank { DEFAULT_SERVICE_LABEL } },
                                url = normalizedURL,
                            )
                        } else {
                            service
                        }
                    }
                )
            } else {
                savedId = UUID.randomUUID().toString()
                val service = ServiceInstance(
                    id = savedId,
                    label = label.ifBlank { DEFAULT_SERVICE_LABEL },
                    url = normalizedURL,
                )
                prefs[Keys.SERVICES] = json.encodeToString(services + service)
                prefs[Keys.ACTIVE_SERVICE_ID] = savedId
            }
        }
        return savedId
    }

    suspend fun switchService(id: String) {
        dataStore.edit { prefs ->
            val services = decodeServices(prefs)
            if (services.any { it.id == id }) {
                prefs[Keys.ACTIVE_SERVICE_ID] = id
            }
        }
    }

    suspend fun updateHaloAddress(address: String?) {
        dataStore.edit { prefs ->
            if (address.isNullOrBlank()) {
                prefs.remove(Keys.HALO_ADDRESS)
            } else {
                prefs[Keys.HALO_ADDRESS] = address
            }
        }
    }

    suspend fun updateHaloAutoConnect(enabled: Boolean) {
        dataStore.edit { prefs ->
            prefs[Keys.HALO_AUTO_CONNECT] = enabled
        }
    }

    private fun decodeServices(prefs: Preferences): List<ServiceInstance> =
        prefs[Keys.SERVICES]?.let { encoded ->
            runCatching { json.decodeFromString<List<ServiceInstance>>(encoded) }.getOrNull()
        } ?: emptyList()

    companion object {
        const val DEFAULT_SERVICE_LABEL = "Service"

        fun normalizeURL(url: String): String = url.trim().trimEnd('/')
    }
}
