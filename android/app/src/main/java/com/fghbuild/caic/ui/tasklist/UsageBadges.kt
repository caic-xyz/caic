// Usage badges showing API utilization with color-coded thresholds.
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
import com.caic.sdk.UsageResp
import com.caic.sdk.UsageWindow
import kotlin.math.roundToInt

private val BadgeGreen = Color(0xFF22C55E)
private val BadgeYellow = Color(0xFFEAB308)
private val BadgeRed = Color(0xFFEF4444)

@Composable
fun UsageBadges(usage: UsageResp) {
    Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
        Badge(label = "5h", window = usage.fiveHour, yellowAt = 80, redAt = 90)
        Badge(label = "7d", window = usage.sevenDay, yellowAt = 90, redAt = 95)
    }
}

@Composable
private fun Badge(label: String, window: UsageWindow, yellowAt: Int, redAt: Int) {
    val pct = window.utilization.roundToInt()
    val color = when {
        pct >= redAt -> BadgeRed
        pct >= yellowAt -> BadgeYellow
        else -> BadgeGreen
    }
    Text(
        text = "$label: $pct%",
        style = MaterialTheme.typography.labelSmall,
        color = Color.White,
        modifier = Modifier
            .background(color, RoundedCornerShape(4.dp))
            .padding(horizontal = 4.dp, vertical = 2.dp),
    )
}
