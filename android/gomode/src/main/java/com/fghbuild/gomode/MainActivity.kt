// Single activity host for the Go Mode WebView shell.
package com.fghbuild.gomode

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.datastore.preferences.preferencesDataStore
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.ui.GoModeApp
import com.fghbuild.gomode.ui.theme.GoModeTheme

private val ComponentActivity.dataStore by preferencesDataStore(name = "gomode_settings")

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val settingsRepository = SettingsRepository(dataStore)
        setContent {
            GoModeTheme {
                GoModeApp(settingsRepository = settingsRepository)
            }
        }
    }
}
