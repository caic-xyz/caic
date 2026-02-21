// Renders a tool group: single card, or a header item used when tool calls are lazy list items.
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
import com.fghbuild.caic.util.ToolCall
import com.fghbuild.caic.util.toolCountSummary

// Used by MessageGroupContent for single-call TOOL groups only (size 0 or 1).
// Multi-call groups are split into ToolGroupHeaderItem + individual ToolCallCard lazy items.
@Composable
fun ToolMessageGroup(toolCalls: List<ToolCall>) {
    if (toolCalls.isEmpty()) return
    ToolCallCard(call = toolCalls[0])
}

/**
 * Header row for a multi-call tool group rendered as lazy list items. The expand/collapse
 * state is owned by the parent LazyColumn so the tool call items are never composed when
 * the group is collapsed (true laziness — no AnimatedVisibility wrapper needed).
 */
@Composable
fun ToolGroupHeaderItem(
    toolCalls: List<ToolCall>,
    isExpanded: Boolean,
    onToggle: () -> Unit,
) {
    val baseSummary = remember(toolCalls) {
        val doneCount = toolCalls.count { it.done }
        "$doneCount/${toolCalls.size} tools: ${toolCountSummary(toolCalls)}"
    }
    Text(
        text = "${if (isExpanded) "▼" else "▶"} $baseSummary",
        style = MaterialTheme.typography.labelMedium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onToggle() }
            .padding(vertical = 4.dp),
    )
}
