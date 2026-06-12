// Service discovery client and compatibility helpers for the Go Mode root settings document.
package com.fghbuild.gomode.service

import com.fghbuild.gomode.sdk.v1.ApiClient
import com.fghbuild.gomode.sdk.v1.ApiException
import com.fghbuild.gomode.sdk.v1.Settings
import java.net.URI

class ServiceSettingsClient {
    suspend fun fetch(baseURL: String): Settings {
        try {
            return ApiClient(serviceOrigin(baseURL)).getSettings()
        } catch (e: ApiException) {
            throw ServiceSettingsException("Service settings request failed: HTTP ${e.statusCode}", e)
        }
    }
}

class ServiceSettingsException(message: String, cause: Throwable? = null) : Exception(message, cause)

fun serviceOrigin(url: String): String {
    val uri = URI(url.trim())
    require(!uri.scheme.isNullOrBlank()) { "Service URL must include a scheme" }
    require(!uri.rawAuthority.isNullOrBlank()) { "Service URL must include a host" }
    return "${uri.scheme}://${uri.rawAuthority}"
}

data class ShellCompatibility(
    val bridgeVersion: Int = GoModeShellContract.BridgeVersion,
)

object GoModeShellContract {
    const val API_VERSION = 1
    const val BridgeVersion = 1
}

fun Settings.compatibilityError(
    compatibility: ShellCompatibility = ShellCompatibility(),
): String? = when {
    apiVersion != GoModeShellContract.API_VERSION ->
        "Unsupported Go Mode service API version $apiVersion. This app supports ${GoModeShellContract.API_VERSION}."
    compatibility.bridgeVersion != webShell.bridgeVersion ->
        "Service requires bridge version ${webShell.bridgeVersion}. This app provides ${compatibility.bridgeVersion}."
    else -> null
}
