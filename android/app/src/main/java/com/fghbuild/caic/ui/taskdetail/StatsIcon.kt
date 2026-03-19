// StatsIcon renders a 2×2 bar-chart icon in the task header that opens a popup
// showing container resource history (CPU, MEM, NET, DISK) and per-turn perf data.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawWithContent
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.EventStats
import com.fghbuild.caic.util.Session
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.formatDuration
import com.fghbuild.caic.util.formatTokens
import java.util.Locale

// Color for CPU/MEM bars: ratio is 0–1 of a hard limit.
private fun barColor(ratio: Float): Color = when {
    ratio >= 0.85f -> Color(0xFFDC3545)
    ratio >= 0.5f -> Color(0xFF856404)
    else -> Color(0xFF28A745)
}

// Color for NET bar: absolute thresholds on cumulative bytes.
private fun netColor(bytes: Long): Color = when {
    bytes >= 1_000_000_000L -> Color(0xFFDC3545) // ≥ 1 GB
    bytes >= 100_000_000L -> Color(0xFF856404)    // ≥ 100 MB
    else -> Color(0xFF28A745)
}

// Color for DISK bar: absolute thresholds on writable layer size.
private fun diskColor(bytes: Long): Color = when {
    bytes >= 10_000_000_000L -> Color(0xFFDC3545) // ≥ 10 GB
    bytes >= 5_000_000_000L -> Color(0xFF856404)   // ≥ 5 GB
    else -> Color(0xFF28A745)
}

private fun formatBytes(bytes: Long): String {
    if (bytes <= 0L) return "0 B"
    val units = listOf("B", "KB", "MB", "GB", "TB")
    var idx = 0
    var value = bytes.toDouble()
    while (value >= 1024.0 && idx < units.size - 1) {
        value /= 1024.0
        idx++
    }
    return if (value < 10.0) {
        String.format(Locale.US, "%.1f %s", value, units[idx])
    } else {
        "${value.toLong()} ${units[idx]}"
    }
}

/** 2×2 bar-chart icon drawn on a Canvas. */
@Composable
private fun BarChartIcon(
    cpuRatio: Float,
    memRatio: Float,
    netRatio: Float,
    diskRatio: Float,
    netBarColor: Color,
    diskBarColor: Color,
    modifier: Modifier = Modifier,
) {
    val inactive = MaterialTheme.colorScheme.outlineVariant
    Box(
        modifier = modifier
            .size(18.dp)
            .drawWithContent {
                drawContent()
                val w = size.width
                val h = size.height
                val barW = w * 0.38f
                val gap = w * 0.24f
                val halfH = h / 2f - 1.dp.toPx()
                val cornerR = CornerRadius(1.dp.toPx())

                // Top-left: CPU
                val cpuH = halfH * cpuRatio.coerceIn(0f, 1f)
                drawRoundRect(
                    color = if (cpuRatio > 0f) barColor(cpuRatio) else inactive,
                    topLeft = Offset(0f, halfH - cpuH),
                    size = Size(barW, cpuH.coerceAtLeast(1.dp.toPx())),
                    cornerRadius = cornerR,
                )
                // Top-right: MEM
                val memH = halfH * memRatio.coerceIn(0f, 1f)
                drawRoundRect(
                    color = if (memRatio > 0f) barColor(memRatio) else inactive,
                    topLeft = Offset(barW + gap, halfH - memH),
                    size = Size(barW, memH.coerceAtLeast(1.dp.toPx())),
                    cornerRadius = cornerR,
                )
                val rowTop = halfH + 2.dp.toPx()
                // Bottom-left: NET
                val netH = halfH * netRatio.coerceIn(0f, 1f)
                drawRoundRect(
                    color = if (netRatio > 0f) netBarColor else inactive,
                    topLeft = Offset(0f, rowTop + halfH - netH),
                    size = Size(barW, netH.coerceAtLeast(1.dp.toPx())),
                    cornerRadius = cornerR,
                )
                // Bottom-right: DISK
                val diskH = halfH * diskRatio.coerceIn(0f, 1f)
                drawRoundRect(
                    color = if (diskRatio > 0f) diskBarColor else inactive,
                    topLeft = Offset(barW + gap, rowTop + halfH - diskH),
                    size = Size(barW, diskH.coerceAtLeast(1.dp.toPx())),
                    cornerRadius = cornerR,
                )
            },
    )
}

private fun collectTurnPerfs(
    completedSessions: List<Session>,
    currentSessionTurns: List<Turn>,
): List<Pair<Int, com.caic.sdk.v1.EventResult>> {
    val perfs = mutableListOf<Pair<Int, com.caic.sdk.v1.EventResult>>()
    var idx = 1
    for (session in completedSessions) {
        for (turn in session.turns) {
            turn.result?.let { perfs.add(idx to it) }
            idx++
        }
    }
    for (turn in currentSessionTurns) {
        turn.result?.let { perfs.add(idx to it) }
        idx++
    }
    return perfs
}

@Composable
fun StatsIcon(
    stats: List<EventStats>,
    completedSessions: List<Session>,
    currentSessionTurns: List<Turn>,
    modifier: Modifier = Modifier,
) {
    val latest = stats.lastOrNull()
    val maxNet = stats.maxOfOrNull { it.netRx + it.netTx }?.takeIf { it > 0 } ?: 1L
    val maxDisk = stats.maxOfOrNull { it.diskUsed.coerceAtLeast(0L) }?.takeIf { it > 0 } ?: 1L

    val cpuRatio = ((latest?.cpuPerc ?: 0.0) / 100.0).toFloat().coerceIn(0f, 1f)
    val memRatio = ((latest?.memPerc ?: 0.0) / 100.0).toFloat().coerceIn(0f, 1f)
    val netRatio = latest?.let { ((it.netRx + it.netTx).toFloat() / maxNet.toFloat()).coerceIn(0f, 1f) } ?: 0f
    val diskRatio = latest?.let { (it.diskUsed.coerceAtLeast(0L).toFloat() / maxDisk.toFloat()).coerceIn(0f, 1f) } ?: 0f

    var expanded by remember { mutableStateOf(false) }

    val perfs = remember(completedSessions, currentSessionTurns) {
        collectTurnPerfs(completedSessions, currentSessionTurns)
    }

    Box(modifier = modifier) {
        IconButton(onClick = { expanded = !expanded }, modifier = Modifier.size(32.dp)) {
            BarChartIcon(
                cpuRatio = cpuRatio,
                memRatio = memRatio,
                netRatio = netRatio,
                diskRatio = diskRatio,
                netBarColor = netColor((latest?.netRx ?: 0L) + (latest?.netTx ?: 0L)),
                diskBarColor = diskColor(latest?.diskUsed?.coerceAtLeast(0L) ?: 0L),
            )
        }
        DropdownMenu(
            expanded = expanded,
            onDismissRequest = { expanded = false },
        ) {
            Column(modifier = Modifier.widthIn(min = 240.dp).padding(horizontal = 12.dp, vertical = 8.dp)) {
                Text(
                    text = "RESOURCES",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(modifier = Modifier.height(4.dp))
                if (latest == null) {
                    Text(
                        text = "No data yet",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    val recentStats = stats.takeLast(5)
                    data class StatRow(
                        val label: String,
                        val ratio: Float,
                        val value: String,
                        val history: List<Float>,
                        val historyColors: List<Color>? = null,
                    )
                    val rows = listOf(
                        StatRow(
                            "CPU",
                            (latest.cpuPerc / 100.0).toFloat().coerceIn(0f, 1f),
                            "${String.format(Locale.US, "%.1f", latest.cpuPerc)}%",
                            recentStats.map { (it.cpuPerc / 100.0).toFloat().coerceIn(0f, 1f) },
                        ),
                        StatRow(
                            "MEM",
                            (latest.memPerc / 100.0).toFloat().coerceIn(0f, 1f),
                            "${formatBytes(latest.memUsed)}/${formatBytes(latest.memLimit)}",
                            recentStats.map { (it.memPerc / 100.0).toFloat().coerceIn(0f, 1f) },
                        ),
                        StatRow(
                            "NET",
                            ((latest.netRx + latest.netTx).toFloat() / maxNet.toFloat()).coerceIn(0f, 1f),
                            "${formatBytes(latest.netRx)}/${formatBytes(latest.netTx)}",
                            recentStats.map {
                                ((it.netRx + it.netTx).toFloat() / maxNet.toFloat()).coerceIn(0f, 1f)
                            },
                            recentStats.map { netColor(it.netRx + it.netTx) },
                        ),
                        StatRow(
                            "DISK",
                            (latest.diskUsed.coerceAtLeast(0L).toFloat() / maxDisk.toFloat()).coerceIn(0f, 1f),
                            if (latest.diskUsed >= 0L) formatBytes(latest.diskUsed) else "—",
                            recentStats.map {
                                (it.diskUsed.coerceAtLeast(0L).toFloat() / maxDisk.toFloat()).coerceIn(0f, 1f)
                            },
                            recentStats.map { diskColor(it.diskUsed.coerceAtLeast(0L)) },
                        ),
                    )
                    rows.forEach { row ->
                        Row(
                            verticalAlignment = Alignment.CenterVertically,
                            horizontalArrangement = Arrangement.spacedBy(6.dp),
                            modifier = Modifier.padding(vertical = 2.dp),
                        ) {
                            Text(
                                text = row.label,
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.width(32.dp),
                            )
                            Text(
                                text = row.value,
                                style = MaterialTheme.typography.bodySmall,
                                modifier = Modifier.weight(1f),
                            )
                            // Mini history bars
                            Row(
                                horizontalArrangement = Arrangement.spacedBy(2.dp),
                                verticalAlignment = Alignment.Bottom,
                                modifier = Modifier.height(12.dp),
                            ) {
                                row.history.forEachIndexed { i, ratio ->
                                    val color = if (ratio > 0f) {
                                        row.historyColors?.getOrNull(i) ?: barColor(ratio)
                                    } else {
                                        Color.LightGray
                                    }
                                    Box(
                                        modifier = Modifier
                                            .width(6.dp)
                                            .height(12.dp)
                                            .drawWithContent {
                                                drawContent()
                                                val barH = (size.height * ratio)
                                                    .coerceAtLeast(1.dp.toPx())
                                                drawRoundRect(
                                                    color = color,
                                                    topLeft = Offset(0f, size.height - barH),
                                                    size = Size(size.width, barH),
                                                    cornerRadius = CornerRadius(1.dp.toPx()),
                                                )
                                            },
                                    )
                                }
                            }
                        }
                    }
                }
                if (perfs.isNotEmpty()) {
                    Spacer(modifier = Modifier.height(8.dp))
                    HorizontalDivider()
                    Spacer(modifier = Modifier.height(8.dp))
                    Text(
                        text = "INVOCATIONS",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Spacer(modifier = Modifier.height(4.dp))
                    Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                        listOf("#", "Wall", "API", "Cost", "Tokens").forEach { h ->
                            Text(
                                text = h,
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.weight(if (h == "Cost") 1.2f else 1f),
                            )
                        }
                    }
                    perfs.forEach { (idx, result) ->
                        val totalTokens = result.usage.inputTokens +
                            result.usage.cacheCreationInputTokens +
                            result.usage.cacheReadInputTokens +
                            result.usage.outputTokens
                        val costText = if (result.totalCostUSD > 0) {
                            "\$${String.format(Locale.US, "%.4f", result.totalCostUSD)}"
                        } else {
                            "—"
                        }
                        Row(
                            horizontalArrangement = Arrangement.spacedBy(6.dp),
                            modifier = Modifier.padding(vertical = 1.dp),
                        ) {
                            listOf(
                                "$idx" to 1f,
                                formatDuration(result.duration) to 1f,
                                formatDuration(result.durationAPI) to 1f,
                                costText to 1.2f,
                                formatTokens(totalTokens) to 1f,
                            ).forEach { (text, weight) ->
                                Text(
                                    text = text,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier.weight(weight),
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}
