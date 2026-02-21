// Collapsed past turn: shows summary; tap to expand via the parent LazyColumn.
// Expansion state is lifted to MessageList so the expanded groups are true lazy items.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.turnSummary

@Composable
fun ElidedTurn(turn: Turn, onExpand: () -> Unit) {
    val summary = remember(turn) { turnSummary(turn) }
    Text(
        text = summary,
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onExpand() }
            .padding(vertical = 4.dp),
    )
}
