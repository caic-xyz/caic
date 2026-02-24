// Card for a result event: success/error with metadata.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ClaudeEventResult
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatDuration
import com.fghbuild.caic.ui.theme.markdownTypography
import com.mikepenz.markdown.m3.Markdown

@Composable
fun ResultCard(result: ClaudeEventResult) {
    val isError = result.isError
    Surface(
        modifier = Modifier.fillMaxWidth(),
        tonalElevation = 2.dp,
        shape = MaterialTheme.shapes.medium,
        color = if (isError) MaterialTheme.colorScheme.errorContainer else MaterialTheme.colorScheme.primaryContainer,
    ) {
        Column(
            modifier = Modifier.padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            if (result.result.isNotBlank()) {
                Markdown(
                    content = result.result,
                    modifier = Modifier.fillMaxWidth(),
                    typography = markdownTypography(),
                )
            }

            result.diffStat?.let { stats ->
                if (stats.isNotEmpty()) {
                    Text(
                        text = stats.joinToString(", ") { "${it.path} +${it.added}/-${it.deleted}" },
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }

            Text(
                text = "${formatCost(result.totalCostUSD)} \u00b7 ${formatDuration(result.duration)}" +
                    " \u00b7 ${result.numTurns} turns",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
}
