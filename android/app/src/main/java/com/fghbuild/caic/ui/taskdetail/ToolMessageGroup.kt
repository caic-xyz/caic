// Renders a tool group: single card or collapsible group with summary.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.fghbuild.caic.util.ToolCall
import com.fghbuild.caic.util.toolCountSummary

@Composable
fun ToolMessageGroup(toolCalls: List<ToolCall>) {
    if (toolCalls.isEmpty()) return
    if (toolCalls.size == 1) {
        ToolCallCard(call = toolCalls[0])
        return
    }
    val groupKey = toolCalls.firstOrNull()?.use?.toolUseID ?: ""
    var expanded by rememberSaveable(groupKey) { mutableStateOf(false) }
    val doneCount = toolCalls.count { it.done }
    val summary = "$doneCount/${toolCalls.size} tools: ${toolCountSummary(toolCalls)}"

    Column(modifier = Modifier.fillMaxWidth()) {
        Text(
            text = summary,
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier
                .fillMaxWidth()
                .clickable { expanded = !expanded }
                .padding(vertical = 4.dp),
        )
        AnimatedVisibility(visible = expanded) {
            Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                toolCalls.forEach { call ->
                    ToolCallCard(call = call)
                }
            }
        }
    }
}
