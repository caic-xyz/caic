// Usage badges showing rolling window cost derived from container output.
package com.fghbuild.caic.ui.tasklist

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.UsageResp
import com.caic.sdk.v1.UsageWindow
import java.util.Locale

private val BadgeGreen = Color(0xFF22C55E)
private val BadgeYellow = Color(0xFFEAB308)
private val BadgeRed = Color(0xFFEF4444)

private fun fmtCost(cost: Double): String = if (cost >= 10) {
    "$${cost.toInt()}"
} else {
    "$${String.format(Locale.US, "%.2f", cost)}"
}

@Composable
fun UsageBadges(usage: UsageResp) {
    Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
        Badge(label = "5h", window = usage.fiveHour)
        Badge(label = "7d", window = usage.sevenDay)
    }
}

@Composable
private fun Badge(label: String, window: UsageWindow) {
    val cost = window.costUSD
    val color = when {
        cost >= 5.0 -> BadgeRed
        cost >= 1.0 -> BadgeYellow
        else -> BadgeGreen
    }
    Text(
        text = "$label: ${fmtCost(cost)}",
        style = MaterialTheme.typography.labelSmall,
        color = Color.White,
        modifier = Modifier
            .background(color, RoundedCornerShape(4.dp))
            .padding(horizontal = 4.dp, vertical = 2.dp),
    )
}
