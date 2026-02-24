// Material 3 theme with state-based task colors.
package com.fghbuild.caic.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import com.mikepenz.markdown.m3.markdownTypography
import com.mikepenz.markdown.model.MarkdownTypography

fun stateColor(state: String): Color = when (state) {
    "running" -> Color(0xFFD4EDDA)
    "asking" -> Color(0xFFCCE5FF)
    "failed" -> Color(0xFFF8D7DA)
    "terminating" -> Color(0xFFFDE2C8)
    "terminated" -> Color(0xFFE2E3E5)
    else -> Color(0xFFFFF3CD)
}

val activeStates = setOf("running", "branching", "provisioning", "starting", "waiting", "asking", "terminating")
val waitingStates = setOf("waiting", "asking")

private val LightColorScheme = lightColorScheme()
private val DarkColorScheme = darkColorScheme()

/** Scaled-down markdown heading typography for inline content. */
@Composable
fun markdownTypography(): MarkdownTypography = markdownTypography(
    h1 = MaterialTheme.typography.titleMedium,
    h2 = MaterialTheme.typography.titleSmall,
    h3 = MaterialTheme.typography.bodyLarge,
    h4 = MaterialTheme.typography.bodyMedium,
    h5 = MaterialTheme.typography.bodyMedium,
    h6 = MaterialTheme.typography.bodyMedium,
)

@Composable
fun CaicTheme(darkTheme: Boolean = isSystemInDarkTheme(), content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = if (darkTheme) DarkColorScheme else LightColorScheme,
        content = content,
    )
}
