// Root Compose surface that bootstraps service settings and coordinates shell recovery with voice controls.
package com.fghbuild.gomode.ui

import android.Manifest
import android.content.pm.PackageManager
import android.webkit.CookieManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.ime
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.data.SettingsState
import com.fghbuild.gomode.halo.HaloController
import com.fghbuild.gomode.sdk.v1.Settings as ServiceSettings
import com.fghbuild.gomode.service.ServiceMonitor
import com.fghbuild.gomode.service.ServiceNotification
import com.fghbuild.gomode.service.ServiceNotificationPublisher
import com.fghbuild.gomode.service.ServiceSettingsClient
import com.fghbuild.gomode.service.ServiceSettingsException
import com.fghbuild.gomode.service.compatibilityError
import com.fghbuild.gomode.ui.halo.HaloScreen
import com.fghbuild.gomode.ui.settings.SettingsScreen
import com.fghbuild.gomode.ui.web.WebShellLoadState
import com.fghbuild.gomode.ui.web.WebShellScreen
import com.fghbuild.gomode.voice.McpClient
import com.fghbuild.gomode.voice.VoicePanel
import com.fghbuild.gomode.voice.VoiceService
import com.fghbuild.gomode.voice.VoiceSession
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import kotlinx.serialization.SerializationException
import java.io.IOException

private enum class NativeScreen {
    Settings,
    Halo,
}

internal sealed interface ServiceBootstrapState {
    data class Unvalidated(val reason: String? = null) : ServiceBootstrapState
    data class Ready(val settings: ServiceSettings) : ServiceBootstrapState
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
    var bootstrapState by remember(activeURL) {
        mutableStateOf<ServiceBootstrapState>(ServiceBootstrapState.Unvalidated())
    }
    var webLoadState by remember(activeURL) { mutableStateOf<WebShellLoadState>(WebShellLoadState.Loading) }
    var webReloadToken by remember { mutableStateOf(0) }

    LaunchedEffect(activeURL, reloadToken, settingsClient) {
        if (activeURL.isBlank()) return@LaunchedEffect
        bootstrapState = fetchBootstrapState(activeURL, settingsClient)
    }

    val snackbarHostState = remember { SnackbarHostState() }
    val notificationPublisher = remember(context) { ServiceNotificationPublisher(context.applicationContext) }
    var pendingNotifications by remember { mutableStateOf(emptyList<ServiceNotification>()) }
    val notificationPermissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) pendingNotifications.forEach(notificationPublisher::publish)
        pendingNotifications = emptyList()
    }
    val scope = rememberCoroutineScope()
    val voiceSession = remember(settingsRepository) {
        VoiceSession(context.applicationContext, settingsRepository, settingsClient)
    }
    DisposableEffect(voiceSession) {
        onDispose { voiceSession.disconnect() }
    }
    // Voice is tied to the selected service instance, unlike transient WebView recovery.
    LaunchedEffect(activeURL, voiceSession) {
        voiceSession.disconnect()
    }
    val voiceState by voiceSession.state.collectAsStateWithLifecycle()
    val serviceMonitor = remember(scope) {
        ServiceMonitor(scope = scope) { endpointURL, protocolVersion ->
            McpClient(
                endpointURL = endpointURL,
                protocolVersion = protocolVersion,
                cookieProvider = { CookieManager.getInstance().getCookie(endpointURL) },
            )
        }
    }
    DisposableEffect(serviceMonitor) {
        onDispose { serviceMonitor.stop() }
    }
    val serviceMonitorState by serviceMonitor.state.collectAsStateWithLifecycle()
    var onMicGranted by remember { mutableStateOf<(() -> Unit)?>(null) }
    val micPermissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { granted ->
        if (granted) {
            onMicGranted?.invoke()
        } else {
            scope.launch {
                snackbarHostState.showSnackbar("Microphone permission is required for voice mode")
            }
        }
        onMicGranted = null
    }
    val shellRecovery = shellRecoveryState(
        bootstrapError = (bootstrapState as? ServiceBootstrapState.Error)?.message,
        webLoadState = webLoadState,
    ).takeIf { activeNativeScreen == null && activeURL.isNotBlank() }
    val voiceSessionActive = voiceState.connected || voiceState.connectStatus != null ||
        voiceState.listening || voiceState.speaking
    val configuredVoiceAvailable = (bootstrapState as? ServiceBootstrapState.Ready)
        ?.settings
        ?.webShell
        ?.voiceGateway
        ?.url
        ?.isNotBlank() == true
    val voiceAvailable = voiceSessionActive || (configuredVoiceAvailable && shellRecovery == null)
    val density = LocalDensity.current
    val keyboardOpen = WindowInsets.ime.getBottom(density) > 0

    LaunchedEffect(activeURL, bootstrapState, serviceMonitor) {
        val ready = bootstrapState as? ServiceBootstrapState.Ready
        if (activeURL.isBlank() || ready == null) {
            serviceMonitor.stop()
            return@LaunchedEffect
        }
        serviceMonitor.start(activeURL, ready.settings)
    }

    LaunchedEffect(serviceMonitorState.voiceContext, voiceSession) {
        voiceSession.setServiceContext(serviceMonitorState.voiceContext)
    }

    LaunchedEffect(serviceMonitorState.notificationText) {
        VoiceService.setServiceNotificationText(serviceMonitorState.notificationText)
    }

    LaunchedEffect(serviceMonitorState.notifications) {
        val notifications = serviceMonitorState.notifications
        if (notifications.isEmpty()) return@LaunchedEffect
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) {
            notifications.forEach(notificationPublisher::publish)
        } else {
            pendingNotifications = notifications
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    Scaffold(
        contentWindowInsets = WindowInsets(0, 0, 0, 0),
        snackbarHost = {
            SnackbarHost(snackbarHostState) { data ->
                Snackbar {
                    Text(
                        text = data.visuals.message,
                        maxLines = 5,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
            }
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding),
        ) {
            // Resize hosted WebViews above the IME. Without native IME insets, Android WebView
            // keeps reporting a full-height viewport and fixed bottom web inputs sit under the keyboard.
            Box(
                modifier = Modifier
                    .weight(1f)
                    .imePadding(),
            ) {
                GoModeContent(
                    settings = settings,
                    settingsRepository = settingsRepository,
                    haloController = haloController,
                    activeURL = activeURL,
                    bootstrapState = bootstrapState,
                    onSetNativeScreen = { activeNativeScreen = it },
                    activeNativeScreen = activeNativeScreen,
                    webReloadToken = webReloadToken,
                    onWebLoadStateChanged = { webLoadState = it },
                    onHostedPageLoaded = {
                        if (bootstrapState is ServiceBootstrapState.Unvalidated) {
                            reloadToken += 1
                        }
                    },
                )
            }
            shellRecovery?.let { recovery ->
                ShellRecoveryStrip(
                    recovery = recovery,
                    voiceSessionActive = voiceSessionActive,
                    onRetry = {
                        when (recovery.retryTarget) {
                            ShellRecoveryRetryTarget.BOOTSTRAP -> reloadToken += 1
                            ShellRecoveryRetryTarget.WEB -> webReloadToken += 1
                            null -> Unit
                        }
                    },
                    onOpenSettings = { activeNativeScreen = NativeScreen.Settings },
                    modifier = Modifier.fillMaxWidth().imePadding(),
                )
            }
            if (!keyboardOpen && (shellRecovery == null || voiceSessionActive)) {
                VoicePanel(
                    voiceState = voiceState,
                    voiceEnabled = voiceAvailable,
                    onConnect = {
                        if (ContextCompat.checkSelfPermission(
                                context,
                                Manifest.permission.RECORD_AUDIO,
                            ) == PackageManager.PERMISSION_GRANTED
                        ) {
                            voiceSession.connect()
                        } else {
                            onMicGranted = { voiceSession.connect() }
                            micPermissionLauncher.launch(Manifest.permission.RECORD_AUDIO)
                        }
                    },
                    onDisconnect = { voiceSession.disconnect() },
                    onToggleMute = { voiceSession.toggleMute() },
                    onSelectDevice = { voiceSession.selectAudioDevice(it) },
                    onClearTranscript = { voiceSession.clearTranscript() },
                    onOpenSettings = { activeNativeScreen = NativeScreen.Settings },
                    serviceAttentionText = serviceMonitorState.notificationText
                        ?: serviceMonitorState.error?.let { "Service updates are reconnecting." },
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        }
    }
}

internal suspend fun fetchBootstrapState(
    activeURL: String,
    settingsClient: ServiceSettingsClient,
): ServiceBootstrapState = try {
    val serviceSettings = settingsClient.fetch(activeURL)
    val compatibilityError = serviceSettings.compatibilityError()
    if (compatibilityError == null) {
        ServiceBootstrapState.Ready(serviceSettings)
    } else {
        ServiceBootstrapState.Error(compatibilityError)
    }
} catch (e: CancellationException) {
    throw e
} catch (e: SerializationException) {
    ServiceBootstrapState.Unvalidated(e.message ?: "Could not parse service settings.")
} catch (e: IllegalArgumentException) {
    ServiceBootstrapState.Error("Invalid service URL: ${e.message.orEmpty()}")
} catch (e: ServiceSettingsException) {
    ServiceBootstrapState.Unvalidated(e.message ?: "Could not fetch service settings.")
} catch (e: IOException) {
    ServiceBootstrapState.Unvalidated(e.message ?: "Could not fetch service settings.")
}

@Composable
private fun GoModeContent(
    settings: SettingsState,
    settingsRepository: SettingsRepository,
    haloController: HaloController,
    activeURL: String,
    bootstrapState: ServiceBootstrapState,
    activeNativeScreen: NativeScreen?,
    onSetNativeScreen: (NativeScreen?) -> Unit,
    webReloadToken: Int,
    onWebLoadStateChanged: (WebShellLoadState) -> Unit,
    onHostedPageLoaded: () -> Unit,
) {
    when {
        activeNativeScreen == NativeScreen.Halo -> {
            HaloScreen(
                haloController = haloController,
                onNavigateBack = { onSetNativeScreen(NativeScreen.Settings) },
            )
        }
        activeURL.isBlank() || activeNativeScreen == NativeScreen.Settings -> {
            SettingsScreen(
                settings = settings,
                settingsRepository = settingsRepository,
                onDone = { onSetNativeScreen(null) },
                onOpenHalo = { onSetNativeScreen(NativeScreen.Halo) },
            )
        }
        else -> {
            when (val state = bootstrapState) {
                is ServiceBootstrapState.Error -> {
                    Box(Modifier.fillMaxSize().testTag("gomode-service-bootstrap"))
                }
                is ServiceBootstrapState.Unvalidated,
                is ServiceBootstrapState.Ready -> {
                    // Keep non-error states in one Compose branch. Otherwise the WebView is disposed
                    // when bootstrap validation completes, which can strand in-flight JavaScript callbacks.
                    WebShellScreen(
                        initialURL = activeURL,
                        reloadToken = webReloadToken,
                        onLoadStateChanged = onWebLoadStateChanged,
                        onHostedPageLoaded = onHostedPageLoaded,
                    )
                }
            }
        }
    }
}

internal enum class ShellRecoveryRetryTarget {
    BOOTSTRAP,
    WEB,
}

internal data class ShellRecoveryState(
    val title: String,
    val message: String,
    val retryTarget: ShellRecoveryRetryTarget?,
)

internal fun shellRecoveryState(
    bootstrapError: String?,
    webLoadState: WebShellLoadState,
): ShellRecoveryState? = when {
    bootstrapError != null -> ShellRecoveryState(
        title = "Could not use service",
        message = bootstrapError,
        retryTarget = ShellRecoveryRetryTarget.BOOTSTRAP,
    )
    webLoadState is WebShellLoadState.Reconnecting -> ShellRecoveryState(
        title = "Reconnecting to service",
        message = "Voice will be available when the service reconnects.",
        retryTarget = null,
    )
    webLoadState is WebShellLoadState.Failed -> ShellRecoveryState(
        title = "Could not load service",
        message = webLoadState.message,
        retryTarget = ShellRecoveryRetryTarget.WEB,
    )
    else -> null
}

@Composable
private fun ShellRecoveryStrip(
    recovery: ShellRecoveryState,
    voiceSessionActive: Boolean,
    onRetry: () -> Unit,
    onOpenSettings: () -> Unit,
    modifier: Modifier = Modifier,
) {
    Surface(modifier = modifier.testTag("gomode-shell-recovery"), tonalElevation = 4.dp) {
        Column(
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Text(recovery.title, style = MaterialTheme.typography.titleMedium)
            Text(recovery.message, style = MaterialTheme.typography.bodyMedium)
            Text(
                if (voiceSessionActive) "Voice remains connected." else "Voice is unavailable until the service reconnects.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                recovery.retryTarget?.let {
                    Button(onClick = onRetry, modifier = Modifier.testTag("gomode-shell-retry")) {
                        Text("Retry service")
                    }
                }
                Button(onClick = onOpenSettings, modifier = Modifier.testTag("gomode-shell-open-settings")) {
                    Text("Settings")
                }
            }
        }
    }
}
