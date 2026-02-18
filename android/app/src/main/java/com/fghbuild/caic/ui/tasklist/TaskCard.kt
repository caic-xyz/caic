// Rich task card matching TaskItemSummary.tsx: state badge, plan mode, error, branch, tokens.
package com.fghbuild.caic.ui.tasklist

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Card
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.caic.sdk.Harnesses
import com.caic.sdk.TaskJSON
import com.fghbuild.caic.ui.theme.stateColor
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import com.fghbuild.caic.util.formatTokens
import kotlinx.coroutines.delay

private val PlanBadgeBg = Color(0xFFEDE9FE)
private val PlanBadgeFg = Color(0xFF7C3AED)
private val TerminalStates = setOf("terminated", "failed")

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun TaskCard(task: TaskJSON, modifier: Modifier = Modifier, onClick: () -> Unit = {}) {
    val firstLine = task.task.lineSequence().firstOrNull().orEmpty()

    Card(modifier = modifier.fillMaxWidth(), onClick = onClick) {
        Column(modifier = Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = firstLine,
                    style = MaterialTheme.typography.bodyMedium,
                    fontWeight = FontWeight.SemiBold,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                    if (task.inPlanMode == true) {
                        Surface(shape = RoundedCornerShape(4.dp), color = PlanBadgeBg) {
                            Text(
                                "P",
                                style = MaterialTheme.typography.labelSmall,
                                color = PlanBadgeFg,
                                fontWeight = FontWeight.Bold,
                                modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
                            )
                        }
                    }
                    Surface(shape = RoundedCornerShape(4.dp), color = stateColor(task.state)) {
                        Text(
                            text = task.state,
                            style = MaterialTheme.typography.labelSmall,
                            modifier = Modifier.padding(horizontal = 6.dp, vertical = 1.dp),
                        )
                    }
                }
            }

            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
            ) {
                Text(
                    text = "${task.repo} \u00b7 ${task.branch}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                if (task.state !in TerminalStates && task.stateUpdatedAt > 0) {
                    TickingElapsed(stateUpdatedAt = task.stateUpdatedAt)
                }
            }

            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                if (task.harness != Harnesses.Claude) {
                    MetaText(task.harness)
                }
                task.model?.let { MetaText(it) }
                val tokenCount = task.activeInputTokens + task.activeCacheReadTokens
                if (tokenCount > 0) {
                    MetaText(formatTokens(tokenCount))
                }
                if (task.costUSD > 0) {
                    MetaText(formatCost(task.costUSD))
                }
                MetaText(formatElapsed(task.durationMs))
            }

            task.error?.let { error ->
                Text(
                    text = error,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.error,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
        }
    }
}

@Composable
private fun MetaText(text: String) {
    Text(
        text = text,
        style = MaterialTheme.typography.labelSmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
}

@Composable
private fun TickingElapsed(stateUpdatedAt: Double) {
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(Unit) {
        while (true) {
            delay(1000)
            now = System.currentTimeMillis()
        }
    }
    val elapsedMs = (now - (stateUpdatedAt * 1000).toLong()).coerceAtLeast(0)
    Text(
        text = formatElapsed(elapsedMs),
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
}
