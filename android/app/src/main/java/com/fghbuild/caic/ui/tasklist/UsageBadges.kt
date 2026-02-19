// Usage badges showing API utilization with color-coded thresholds.
package com.fghbuild.caic.ui.tasklist

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import com.caic.sdk.ExtraUsage
import com.caic.sdk.UsageResp
import com.caic.sdk.UsageWindow
import java.time.Instant
import java.time.temporal.ChronoUnit
import java.util.Locale
import kotlin.math.roundToInt

private val BadgeGreen = Color(0xFF22C55E)
private val BadgeYellow = Color(0xFFEAB308)
private val BadgeRed = Color(0xFFEF4444)
private val BadgeDisabled = Color(0xFF6B7280)

/** Grace period after resetsAt before zeroing utilization, matching frontend. */
private const val RESET_GRACE_MS = 60_000L

private fun effectiveUtilization(w: UsageWindow): Double {
    if (w.resetsAt.isBlank()) return w.utilization
    return try {
        val resetMs = Instant.parse(w.resetsAt).toEpochMilli()
        if (System.currentTimeMillis() > resetMs + RESET_GRACE_MS) 0.0 else w.utilization
    } catch (_: Exception) {
        w.utilization
    }
}

private fun formatReset(iso: String): String {
    if (iso.isBlank()) return ""
    return try {
        val reset = Instant.parse(iso)
        val now = Instant.now()
        val diffMs = reset.toEpochMilli() - now.toEpochMilli()
        if (diffMs <= 0) return "Resets now"
        val hours = ChronoUnit.HOURS.between(now, reset).toInt()
        val mins = ((diffMs % 3_600_000) / 60_000).toInt()
        if (hours >= 24) {
            val days = hours / 24
            "Resets in ${days}d ${hours % 24}h"
        } else if (hours > 0) {
            "Resets in ${hours}h ${mins}m"
        } else {
            "Resets in ${mins}m"
        }
    } catch (_: Exception) {
        ""
    }
}

@Composable
fun UsageBadges(usage: UsageResp) {
    Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
        if (usage.fiveHour.resetsAt.isNotBlank()) {
            Badge(label = "5h", window = usage.fiveHour, yellowAt = 80, redAt = 90)
        }
        if (usage.sevenDay.resetsAt.isNotBlank()) {
            Badge(label = "7d", window = usage.sevenDay, yellowAt = 90, redAt = 95)
        }
        ExtraBadge(extra = usage.extraUsage)
    }
}

@Composable
private fun Badge(label: String, window: UsageWindow, yellowAt: Int, redAt: Int) {
    val pct = effectiveUtilization(window).roundToInt()
    val color = when {
        pct >= redAt -> BadgeRed
        pct >= yellowAt -> BadgeYellow
        else -> BadgeGreen
    }
    val resetText = formatReset(window.resetsAt)
    var expanded by remember { mutableStateOf(false) }
    Box {
        Text(
            text = "$label: $pct%",
            style = MaterialTheme.typography.labelSmall,
            color = Color.White,
            modifier = Modifier
                .clickable { expanded = true }
                .background(color, RoundedCornerShape(4.dp))
                .padding(horizontal = 4.dp, vertical = 2.dp),
        )
        if (resetText.isNotBlank()) {
            DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
                Text(
                    text = resetText,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp),
                )
            }
        }
    }
}

@Composable
private fun ExtraBadge(extra: ExtraUsage) {
    if (extra.usedCredits == 0.0 && extra.monthlyLimit == 0.0) return
    // API values are in cents; convert to dollars.
    val used = extra.usedCredits / 100
    val limit = extra.monthlyLimit / 100
    val pct = extra.utilization.roundToInt()
    val color = when {
        !extra.isEnabled -> BadgeDisabled
        pct >= 80 -> BadgeRed
        pct >= 50 -> BadgeYellow
        else -> BadgeGreen
    }
    val detail = if (extra.isEnabled) {
        "$${String.format(Locale.US, "%.2f", used)} / $${String.format(Locale.US, "%.2f", limit)}"
    } else {
        "Disabled — $${String.format(Locale.US, "%.2f", used)} / $${String.format(Locale.US, "%.2f", limit)}"
    }
    var expanded by remember { mutableStateOf(false) }
    Box {
        Text(
            text = "Extra: $${used.toInt()} / $${limit.toInt()}",
            style = MaterialTheme.typography.labelSmall,
            color = Color.White,
            modifier = Modifier
                .clickable { expanded = true }
                .background(color, RoundedCornerShape(4.dp))
                .padding(horizontal = 4.dp, vertical = 2.dp),
        )
        DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            Text(
                text = detail,
                style = MaterialTheme.typography.bodySmall,
                modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp),
            )
        }
    }
}
