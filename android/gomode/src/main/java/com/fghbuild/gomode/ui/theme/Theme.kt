// Material theme for the Go Mode Android shell.
package com.fghbuild.gomode.ui.theme

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

private val LightColors = lightColorScheme(
    primary = Color(0xFF1E5A46),
    onPrimary = Color(0xFFFFFFFF),
    secondary = Color(0xFF6D5B30),
    background = Color(0xFFFFFDF7),
    surface = Color(0xFFFFFDF7),
    surfaceVariant = Color(0xFFE6E1D7),
)

@Composable
fun GoModeTheme(content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = LightColors,
        content = content,
    )
}
