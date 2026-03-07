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
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Timer
import androidx.compose.material3.Card
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
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
import androidx.compose.foundation.clickable
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.Task
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.ui.theme.stateColor
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import com.fghbuild.caic.util.formatTokens
import kotlinx.coroutines.delay

private val TerminalStates = setOf("terminated", "failed")

@OptIn(ExperimentalLayoutApi::class, ExperimentalFoundationApi::class)
@Composable
fun TaskCard(task: Task, modifier: Modifier = Modifier, onClick: () -> Unit = {}) {
    var showMenu by remember { mutableStateOf(false) }
    val clipboard = LocalClipboardManager.current
    val appColors = MaterialTheme.appColors

    Card(
        modifier = modifier
            .fillMaxWidth()
            .combinedClickable(
                onClick = onClick,
                onLongClick = { showMenu = true },
            ),
    ) {
        Column(modifier = Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            // Line 1: title + plan badge + feature badges (no state badge)
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
                        Surface(shape = RoundedCornerShape(4.dp), color = MaterialTheme.colorScheme.tertiaryContainer) {
                            Text(
                                "P",
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.tertiary,
                                fontWeight = FontWeight.Bold,
                                modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
                            )
                        }
                    }
                    if (task.tailscale != null) FeatureBadge("TS", url = task.tailscale)
                    if (task.usb == true) FeatureBadge("USB")
                    if (task.display == true) FeatureBadge("D")
                }
            }

            // Line 2: base→branch | [timer times] [state badge]
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = buildAnnotatedString {
                        if (!task.baseBranch.isNullOrBlank()) {
                            append("${task.baseBranch}\u2192")
                        }
                        withStyle(SpanStyle(fontWeight = FontWeight.Bold)) {
                            append(task.branch)
                        }
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f),
                )
                Row(
                    horizontalArrangement = Arrangement.spacedBy(4.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    val showTimes = (task.state !in TerminalStates && task.stateUpdatedAt > 0) || task.duration > 0
                    if (showTimes) {
                        Icon(
                            Icons.Outlined.Timer,
                            contentDescription = null,
                            modifier = Modifier.size(11.dp),
                            tint = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        if (task.state !in TerminalStates && task.stateUpdatedAt > 0) {
                            TickingElapsed(stateUpdatedAt = task.stateUpdatedAt)
                            if (task.duration > 0 || task.state == "running") {
                                Text(
                                    "/",
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.5f),
                                )
                            }
                        }
                        if (task.duration > 0 || task.state == "running") {
                            TickingThinkTime(
                                duration = task.duration,
                                state = task.state,
                                stateUpdatedAt = task.stateUpdatedAt,
                                turnStartedAt = task.turnStartedAt ?: 0.0,
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

            // Line 3: model · tokens · cost
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                task.model?.let { MetaText(it) }
                if (task.harness != "claude") MetaText(task.harness)
                val tokenCount = task.activeInputTokens + task.activeCacheReadTokens
                if (tokenCount > 0) {
                    val cachedIn = task.cumulativeCacheReadInputTokens
                    val inputIn = task.cumulativeInputTokens + task.cumulativeCacheCreationInputTokens
                    val output = task.cumulativeOutputTokens
                    Text(
                        text = "${formatTokens(tokenCount)}/${formatTokens(task.contextWindowLimit)}" +
                            " (${formatTokens(cachedIn)} cached" +
                            " + ${formatTokens(inputIn)} in" +
                            " + ${formatTokens(output)} out)",
                        style = MaterialTheme.typography.labelSmall,
                        color = tokenColor(tokenCount, task.contextWindowLimit),
                    )
                }
                if (task.costUSD > 0) {
                    MetaText(formatCost(task.costUSD))
                }
            }

            task.diffStat?.takeIf { it.isNotEmpty() }?.let { stats ->
                val files = stats.size
                val added = stats.sumOf { it.added }
                val deleted = stats.sumOf { it.deleted }
                Text(
                    text = buildAnnotatedString {
                        append("$files file${if (files != 1) "s" else ""} ")
                        withStyle(SpanStyle(color = appColors.diffAddedStat)) { append("+$added") }
                        append(" ")
                        withStyle(SpanStyle(color = appColors.diffDeletedStat)) { append("-$deleted") }
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
private fun FeatureBadge(label: String, url: String? = null) {
    val uriHandler = LocalUriHandler.current
    val clickMod = if (url?.startsWith("https://") == true) {
        Modifier.clickable(onClick = { uriHandler.openUri(url) })
    } else {
        Modifier
    }
    Surface(modifier = clickMod, shape = RoundedCornerShape(4.dp), color = MaterialTheme.appColors.featureBadgeBg) {
        Text(
            text = label,
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.appColors.featureBadgeFg,
            fontWeight = FontWeight.Bold,
            modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
        )
    }
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

@Composable
private fun TickingThinkTime(duration: Double, state: String, stateUpdatedAt: Double, turnStartedAt: Double) {
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(state) {
        if (state == "running") {
            while (true) {
                delay(1000)
                now = System.currentTimeMillis()
            }
        }
    }
    val totalSec = if (state == "running") {
        val turnStart = if (turnStartedAt > 0) turnStartedAt else stateUpdatedAt
        duration + (now - (turnStart * 1000).toLong()).coerceAtLeast(0) / 1000.0
    } else {
        duration
    }
    Text(
        text = formatElapsed(totalSec),
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
    )
}
