// Root Compose surface that chooses native settings or the remote WebView shell.
package com.fghbuild.gomode.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.ui.settings.SettingsScreen
import com.fghbuild.gomode.ui.web.WebShellScreen

@Composable
fun GoModeApp(settingsRepository: SettingsRepository) {
    val settings by settingsRepository.settings.collectAsStateWithLifecycle()
    var forceSettings by remember { mutableStateOf(false) }
    val activeURL = settings.activeServiceURL

    if (activeURL.isBlank() || forceSettings) {
        SettingsScreen(
            settings = settings,
            settingsRepository = settingsRepository,
            onDone = { forceSettings = false },
        )
    } else {
        WebShellScreen(
            initialURL = activeURL,
            onOpenSettings = { forceSettings = true },
        )
    }
}
