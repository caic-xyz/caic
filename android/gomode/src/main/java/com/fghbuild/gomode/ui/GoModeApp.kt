// Root Compose surface that chooses native settings or the remote WebView shell.
package com.fghbuild.gomode.ui

import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.platform.LocalContext
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.halo.HaloController
import com.fghbuild.gomode.ui.halo.HaloScreen
import com.fghbuild.gomode.ui.settings.SettingsScreen
import com.fghbuild.gomode.ui.web.WebShellScreen

private enum class NativeScreen {
    Settings,
    Halo,
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
    var activeNativeScreen by remember { mutableStateOf<NativeScreen?>(null) }
    val activeURL = settings.activeServiceURL

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
            WebShellScreen(
                initialURL = activeURL,
                onOpenSettings = { activeNativeScreen = NativeScreen.Settings },
            )
        }
    }
}
