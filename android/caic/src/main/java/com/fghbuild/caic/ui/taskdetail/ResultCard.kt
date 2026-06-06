// Card for a result event: success/error with metadata.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.DiffFileStat
import com.caic.sdk.v1.EventResult
import com.fghbuild.caic.ui.theme.appColors
import java.util.Locale

@Composable
fun ResultCard(result: EventResult, onNavigateToDiff: (() -> Unit)? = null) {
    val isError = result.isError
    Surface(
        modifier = Modifier.fillMaxWidth(),
        tonalElevation = 2.dp,
        shape = MaterialTheme.shapes.medium,
        color = if (isError) MaterialTheme.colorScheme.errorContainer else MaterialTheme.appColors.successBg,
    ) {
        Column(
            modifier = Modifier.padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            Text(
                text = if (isError) "Error" else "Done",
                style = MaterialTheme.typography.bodyMedium,
                fontWeight = FontWeight.Bold,
            )
            if (result.result.isNotBlank()) {
                MarkdownWithRawToggle(text = result.result, modifier = Modifier.fillMaxWidth())
            }

            result.diffStat?.let { stats ->
                if (stats.isNotEmpty()) {
                    val clickModifier = if (onNavigateToDiff != null) {
                        Modifier.fillMaxWidth().clickable { onNavigateToDiff() }
                    } else {
                        Modifier.fillMaxWidth()
                    }
                    Surface(
                        modifier = Modifier.fillMaxWidth(),
                        shape = MaterialTheme.shapes.small,
                        color = MaterialTheme.colorScheme.scrim.copy(alpha = 0.05f),
                    ) {
                        Column(
                            modifier = clickModifier.padding(horizontal = 6.dp, vertical = 4.dp),
                            verticalArrangement = Arrangement.spacedBy(2.dp),
                        ) {
                            stats.forEach { f -> DiffFileRow(f) }
                        }
                    }
                }
            }

            val costStr = if (result.totalCostUSD != 0.0) {
                String.format(Locale.US, "\$%.4f \u00b7 ", result.totalCostUSD)
            } else {
                ""
            }
            val durationStr = String.format(Locale.US, "%.1fs", result.duration)
            Text(
                text = "$costStr$durationStr \u00b7 ${result.numTurns} turns",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}

@Composable
private fun DiffFileRow(f: DiffFileStat) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        Text(
            text = f.path,
            style = MaterialTheme.typography.bodySmall,
            modifier = Modifier.weight(1f),
        )
        if (f.binary == true) {
            Text(
                text = "binary",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.appColors.diffBinary,
            )
        } else {
            if (f.added > 0) {
                Text(
                    text = "+${f.added}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.appColors.diffAddedStat,
                )
            }
            if (f.deleted > 0) {
                Text(
                    text = "\u2212${f.deleted}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.appColors.diffDeletedStat,
                )
            }
        }
    }
}
