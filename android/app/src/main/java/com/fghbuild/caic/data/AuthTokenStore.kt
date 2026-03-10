// Thin wrapper around SettingsRepository that provides the current auth token for ApiClient injection.
package com.fghbuild.caic.data

import javax.inject.Inject
import javax.inject.Singleton

@Singleton
class AuthTokenStore @Inject constructor(private val settingsRepository: SettingsRepository) {
    fun getToken(): String? = settingsRepository.settings.value.authToken

    suspend fun setToken(token: String) = settingsRepository.updateAuthToken(token)

    suspend fun clearToken() = settingsRepository.updateAuthToken(null)
}
