// Root Compose surface that bootstraps service settings and chooses native settings or the WebView shell.
package com.fghbuild.gomode.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.halo.HaloController
import com.fghbuild.gomode.service.ServiceSettingsClient
import com.fghbuild.gomode.service.ServiceSettingsException
import com.fghbuild.gomode.service.compatibilityError
import com.fghbuild.gomode.ui.halo.HaloScreen
import com.fghbuild.gomode.ui.settings.SettingsScreen
import com.fghbuild.gomode.ui.web.WebShellScreen
import kotlinx.coroutines.CancellationException
import kotlinx.serialization.SerializationException
import java.io.IOException

private enum class NativeScreen {
    Settings,
    Halo,
}

private sealed interface ServiceBootstrapState {
    data object Loading : ServiceBootstrapState
    data object Ready : ServiceBootstrapState
    data class Error(val message: String) : ServiceBootstrapState
}

@Composable
fun GoModeApp(settingsRepository: SettingsRepository) {
    val context = LocalContext.current
    val haloController = remember(settingsRepository) {
        HaloController(context.applicationContext, settingsRepository)
    }
    DisposableEffect(haloController) {
        onDispose { haloController.close() }
    }

    val settings by settingsRepository.settings.collectAsStateWithLifecycle()
    val settingsClient = remember { ServiceSettingsClient() }
    var activeNativeScreen by remember { mutableStateOf<NativeScreen?>(null) }
    val activeURL = settings.activeServiceURL
    var reloadToken by remember(activeURL) { mutableStateOf(0) }
    var bootstrapState by remember(activeURL) { mutableStateOf<ServiceBootstrapState>(ServiceBootstrapState.Loading) }

    LaunchedEffect(activeURL, reloadToken, settingsClient) {
        if (activeURL.isBlank()) return@LaunchedEffect
        bootstrapState = ServiceBootstrapState.Loading
        bootstrapState = try {
            val serviceSettings = settingsClient.fetch(activeURL)
            val compatibilityError = serviceSettings.compatibilityError()
            if (compatibilityError == null) {
                ServiceBootstrapState.Ready
            } else {
                ServiceBootstrapState.Error(compatibilityError)
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: IllegalArgumentException) {
            ServiceBootstrapState.Error("Invalid service URL: ${e.message.orEmpty()}")
        } catch (e: ServiceSettingsException) {
            ServiceBootstrapState.Error(e.message ?: "Could not fetch service settings.")
        } catch (e: IOException) {
            ServiceBootstrapState.Error(e.message ?: "Could not fetch service settings.")
        } catch (e: SerializationException) {
            ServiceBootstrapState.Error(e.message ?: "Could not parse service settings.")
        }
    }

    when {
        activeNativeScreen == NativeScreen.Halo -> {
            HaloScreen(
                haloController = haloController,
                onNavigateBack = { activeNativeScreen = NativeScreen.Settings },
            )
        }
        activeURL.isBlank() || activeNativeScreen == NativeScreen.Settings -> {
            SettingsScreen(
                settings = settings,
                settingsRepository = settingsRepository,
                onDone = { activeNativeScreen = null },
                onOpenHalo = { activeNativeScreen = NativeScreen.Halo },
            )
        }
        else -> {
            when (val state = bootstrapState) {
                ServiceBootstrapState.Loading -> {
                    ServiceBootstrapPanel(
                        title = "Checking service",
                        message = "Loading Go Mode compatibility settings…",
                        onOpenSettings = { activeNativeScreen = NativeScreen.Settings },
                    )
                }
                is ServiceBootstrapState.Error -> {
                    ServiceBootstrapPanel(
                        title = "Could not use service",
                        message = state.message,
                        onRetry = { reloadToken += 1 },
                        onOpenSettings = { activeNativeScreen = NativeScreen.Settings },
                    )
                }
                ServiceBootstrapState.Ready -> {
                    WebShellScreen(
                        initialURL = activeURL,
                        onOpenSettings = { activeNativeScreen = NativeScreen.Settings },
                    )
                }
            }
        }
    }
}

@Composable
private fun ServiceBootstrapPanel(
    title: String,
    message: String,
    onOpenSettings: () -> Unit,
    onRetry: (() -> Unit)? = null,
) {
    Box(Modifier.fillMaxSize().testTag("gomode-service-bootstrap")) {
        Column(
            modifier = Modifier
                .align(Alignment.Center)
                .padding(24.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
        ) {
            if (onRetry == null) {
                CircularProgressIndicator(modifier = Modifier.testTag("gomode-service-bootstrap-loading"))
            }
            Text(title, style = MaterialTheme.typography.titleLarge)
            Text(message, style = MaterialTheme.typography.bodyMedium)
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                onRetry?.let { retry ->
                    Button(onClick = retry, modifier = Modifier.testTag("gomode-service-bootstrap-retry")) {
                        Text("Retry")
                    }
                }
                Button(onClick = onOpenSettings, modifier = Modifier.testTag("gomode-open-settings")) {
                    Text("Settings")
                }
            }
        }
    }
}
