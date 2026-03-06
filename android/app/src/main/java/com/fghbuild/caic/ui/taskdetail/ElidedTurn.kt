// Collapsed past turn: shows summary; tap to expand via the parent LazyColumn.
// Expansion state is lifted to MessageList so the expanded groups are true lazy items.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.turnSummary

private val ElidedBgColor = Color(0xFFE4EAF1)
private val ElidedTextColor = Color(0xFF4A6785)

@Composable
fun ElidedTurn(turn: Turn, onExpand: () -> Unit) {
    val summary = remember(turn) { turnSummary(turn) }
    Surface(
        modifier = Modifier.fillMaxWidth().clickable { onExpand() },
        shape = RoundedCornerShape(4.dp),
        color = ElidedBgColor,
    ) {
        Text(
            text = summary,
            style = MaterialTheme.typography.bodySmall,
            color = ElidedTextColor,
            modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
        )
    }
}
