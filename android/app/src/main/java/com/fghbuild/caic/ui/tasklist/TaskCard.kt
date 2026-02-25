// Rich task card matching TaskItemSummary.tsx: state badge, plan mode, error, branch, tokens.
package com.fghbuild.caic.ui.tasklist

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Card
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.Task
import com.fghbuild.caic.ui.theme.stateColor
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import com.fghbuild.caic.util.formatTokens
import kotlinx.coroutines.delay

private val DiffAddedColor = Color(0xFF22863A)
private val DiffDeletedColor = Color(0xFFCB2431)
private val PlanBadgeBg = Color(0xFFEDE9FE)
private val PlanBadgeFg = Color(0xFF7C3AED)
private val TerminalStates = setOf("terminated", "failed")

@OptIn(ExperimentalLayoutApi::class, ExperimentalFoundationApi::class)
@Composable
fun TaskCard(task: Task, modifier: Modifier = Modifier, onClick: () -> Unit = {}) {
    var showMenu by remember { mutableStateOf(false) }
    val clipboard = LocalClipboardManager.current

    Card(
        modifier = modifier
            .fillMaxWidth()
            .combinedClickable(
                onClick = onClick,
                onLongClick = { showMenu = true },
            ),
    ) {
        Column(modifier = Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = task.title,
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
                val branchLabel = if (!task.baseBranch.isNullOrBlank())
                    "${task.baseBranch}\u2192${task.branch}"
                else task.branch
                Text(
                    text = "${task.repo} \u00b7 $branchLabel",
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
                MetaText(task.harness)
                task.model?.let { MetaText(it) }
                val tokenCount = task.activeInputTokens + task.activeCacheReadTokens
                if (tokenCount > 0) {
                    Text(
                        text = "${formatTokens(tokenCount)}/${formatTokens(task.contextWindowLimit)}",
                        style = MaterialTheme.typography.labelSmall,
                        color = tokenColor(tokenCount, task.contextWindowLimit),
                    )
                }
                if (task.costUSD > 0) {
                    MetaText(formatCost(task.costUSD))
                }
                MetaText(formatElapsed(task.duration))
            }

            task.diffStat?.takeIf { it.isNotEmpty() }?.let { stats ->
                val files = stats.size
                val added = stats.sumOf { it.added }
                val deleted = stats.sumOf { it.deleted }
                Text(
                    text = buildAnnotatedString {
                        append("$files file${if (files != 1) "s" else ""} ")
                        withStyle(SpanStyle(color = DiffAddedColor)) { append("+$added") }
                        append(" ")
                        withStyle(SpanStyle(color = DiffDeletedColor)) { append("-$deleted") }
                    },
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
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

            DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                DropdownMenuItem(
                    text = { Text("Copy branch name") },
                    onClick = {
                        clipboard.setText(AnnotatedString(task.branch))
                        showMenu = false
                    },
                )
                DropdownMenuItem(
                    text = { Text("Copy task ID") },
                    onClick = {
                        clipboard.setText(AnnotatedString(task.id))
                        showMenu = false
                    },
                )
            }
        }
    }
}

private fun tokenColor(current: Int, limit: Int): Color {
    if (limit <= 0) return Color.Unspecified
    val ratio = current.toFloat() / limit
    return when {
        ratio >= 0.9f -> Color(0xFFDC3545)
        ratio >= 0.75f -> Color(0xFFD4A017)
        else -> Color.Unspecified
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
    val elapsedSec = (now - (stateUpdatedAt * 1000).toLong()).coerceAtLeast(0) / 1000.0
    Text(
        text = formatElapsed(elapsedSec),
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
}
