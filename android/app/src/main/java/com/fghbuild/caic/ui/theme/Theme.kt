// Material 3 theme with state-based task colors and centralized app color system. Keep color values in sync with frontend/src/global.css.
package com.fghbuild.caic.ui.theme

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.ReadOnlyComposable
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.graphics.Color
import com.mikepenz.markdown.m3.markdownTypography
import com.mikepenz.markdown.model.MarkdownTypography

fun stateColor(state: String): Color = when (state) {
    "running" -> Color(0xFFD4EDDA)
    "asking" -> Color(0xFFCCE5FF)
    "has_plan" -> Color(0xFFEDE9FE)
    "failed" -> Color(0xFFF8D7DA)
    "stopping" -> Color(0xFFFDE2C8)
    "purging" -> Color(0xFFFDE2C8)
    "purged" -> Color(0xFFE2E3E5)
    "stopped" -> Color(0xFFC8DAF0)
    else -> Color(0xFFFFF3CD)
}

val activeStates = setOf(
    "running", "branching", "provisioning", "starting",
    "waiting", "asking", "has_plan", "stopping", "purging",
)
val terminalStates = setOf("failed", "purged")
val waitingStates = setOf("waiting", "asking", "has_plan")

private val LightColorScheme = lightColorScheme(
    primary = Color(0xFF4A90D9),         // --color-primary
    onPrimary = Color.White,
    primaryContainer = Color(0xFFF0F6FF), // ask active bg
    secondary = Color(0xFF6C757D),        // --color-gray
    onSecondary = Color.White,
    secondaryContainer = Color(0xFFF8F9FA), // ask inactive bg
    tertiary = Color(0xFF7C3AED),         // --color-plan
    onTertiary = Color.White,
    tertiaryContainer = Color(0xFFEDE9FE), // --color-plan-bg
    error = Color(0xFFCC0000),            // --color-error
    onError = Color.White,
    errorContainer = Color(0xFFF8D7DA),   // --color-danger-bg
    onErrorContainer = Color(0xFF721C24), // --color-danger-text
    surfaceVariant = Color(0xFFE8E8E8),   // --color-bg-code
    outline = Color(0xFFDDDDDD),          // --color-border
)

private val DarkColorScheme = darkColorScheme()

/** App-specific colors not covered by Material 3 semantic roles. */
data class AppColors(
    val userMsgBg: Color,       // #DBE9F9  --color-user-msg-bg
    val imageBorder: Color,     // #B8D0EA  --color-image-border
    val success: Color,         // #28A745  --color-success
    val successBg: Color,       // #D4EDDA  --color-success-bg
    val successText: Color,     // #155724  --color-success-text
    val warningBg: Color,       // #FFF3CD  --color-warning-bg
    val warningBorder: Color,   // #FFC107  --color-warning-border
    val warningText: Color,     // #856404  --color-warning-text
    val safetyBorder: Color,    // #FFECB5  --color-safety-border
    val planSurface: Color,     // #F5F3FF  --color-plan-surface
    val planBorder: Color,      // #DDD6FE  --color-plan-border
    val featureBadgeBg: Color,  // #DFEEFA  --color-feature-badge-bg
    val featureBadgeFg: Color,  // #2563EB  --color-feature-badge-fg
    val badgeDisabledBg: Color, // #E2E3E5  --color-badge-disabled-bg
    val toolBlockBg: Color,     // #F0F0F0  --color-bg-input
    val toolErrorBg: Color,     // #FFF0F0  --color-tool-error-bg
    val thinkingBorder: Color,  // #9B8FD4  --color-thinking-border
    val elidedBg: Color,        // #E4EAF1  --color-elided-bg
    val elidedText: Color,      // #4A6785  --color-elided-text
    val diffAddedStat: Color,   // #22863A  --color-diff-added-stat
    val diffDeletedStat: Color, // #CB2431  --color-diff-deleted-stat
    val diffBinary: Color,      // #6A737D  --color-diff-binary
    val diffAddedLine: Color,   // #4EC94E  --color-diff-added-line
    val diffDeletedLine: Color, // #FF4444  --color-diff-deleted-line
    val diffHunk: Color,        // #B48EAD  --color-diff-hunk
    val diffHeader: Color,      // #888888  --color-text-muted
    val diffCodeBg: Color,      // #1E1E1E  --color-diff-code-bg
    val diffCodeFg: Color,      // #D4D4D4  --color-diff-code-fg
)

private val LightAppColors = AppColors(
    userMsgBg = Color(0xFFDBE9F9),
    imageBorder = Color(0xFFB8D0EA),
    success = Color(0xFF28A745),
    successBg = Color(0xFFD4EDDA),
    successText = Color(0xFF155724),
    warningBg = Color(0xFFFFF3CD),
    warningBorder = Color(0xFFFFC107),
    warningText = Color(0xFF856404),
    safetyBorder = Color(0xFFFFECB5),
    planSurface = Color(0xFFF5F3FF),
    planBorder = Color(0xFFDDD6FE),
    featureBadgeBg = Color(0xFFDFEEFA),
    featureBadgeFg = Color(0xFF2563EB),
    badgeDisabledBg = Color(0xFFE2E3E5),
    toolBlockBg = Color(0xFFF0F0F0),
    toolErrorBg = Color(0xFFFFF0F0),
    thinkingBorder = Color(0xFF9B8FD4),
    elidedBg = Color(0xFFE4EAF1),
    elidedText = Color(0xFF4A6785),
    diffAddedStat = Color(0xFF22863A),
    diffDeletedStat = Color(0xFFCB2431),
    diffBinary = Color(0xFF6A737D),
    diffAddedLine = Color(0xFF4EC94E),
    diffDeletedLine = Color(0xFFFF4444),
    diffHunk = Color(0xFFB48EAD),
    diffHeader = Color(0xFF888888),
    diffCodeBg = Color(0xFF1E1E1E),
    diffCodeFg = Color(0xFFD4D4D4),
)

val LocalAppColors = staticCompositionLocalOf { LightAppColors }

/** Access app-specific colors from any composable via [MaterialTheme.appColors]. */
val MaterialTheme.appColors: AppColors
    @Composable
    @ReadOnlyComposable
    get() = LocalAppColors.current

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
    ) {
        CompositionLocalProvider(LocalAppColors provides LightAppColors) {
            content()
        }
    }
}
