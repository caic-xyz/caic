// Expandable card for a single tool call: name, detail, duration, error.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.fghbuild.caic.util.ToolCall
import com.fghbuild.caic.util.formatDuration
import com.fghbuild.caic.util.toolCallDetail
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonPrimitive

@Composable
fun ToolCallCard(call: ToolCall, modifier: Modifier = Modifier) {
    var expanded by rememberSaveable(call.use.toolUseID) { mutableStateOf(false) }
    val detail = toolCallDetail(call.use.name, call.use.input)
    val hasError = call.result?.error != null

    Surface(
        modifier = modifier.fillMaxWidth(),
        tonalElevation = 1.dp,
        shape = MaterialTheme.shapes.small,
    ) {
        Column {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .clickable { expanded = !expanded }
                    .padding(8.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                ToolStatusIcon(done = call.done, hasError = hasError)
                Text(
                    text = call.use.name,
                    style = MaterialTheme.typography.labelMedium,
                )
                if (detail != null) {
                    Text(
                        text = detail,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.weight(1f),
                    )
                }
                call.result?.let { result ->
                    Text(
                        text = formatDuration(result.durationMs),
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            AnimatedVisibility(visible = expanded) {
                Column(modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp)) {
                    ToolInputDisplay(input = call.use.input)
                    call.result?.error?.let { error ->
                        Text(
                            text = error,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.error,
                            modifier = Modifier.padding(top = 4.dp),
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun ToolStatusIcon(done: Boolean, hasError: Boolean) {
    when {
        hasError -> Icon(
            Icons.Default.Close,
            contentDescription = "Error",
            tint = MaterialTheme.colorScheme.error,
            modifier = Modifier.size(16.dp),
        )
        done -> Icon(
            Icons.Default.Check,
            contentDescription = "Done",
            tint = Color(0xFF4CAF50),
            modifier = Modifier.size(16.dp),
        )
        else -> CircularProgressIndicator(modifier = Modifier.size(16.dp), strokeWidth = 2.dp)
    }
}

@Composable
private fun ToolInputDisplay(input: JsonElement) {
    val obj = input as? JsonObject ?: return
    Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
        obj.entries.forEach { (key, value) ->
            val display = formatJsonValue(value)
            if (display.length <= 200) {
                Text(
                    text = "$key: $display",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 3,
                    overflow = TextOverflow.Ellipsis,
                )
            }
        }
    }
}

private fun formatJsonValue(value: JsonElement): String = when (value) {
    is JsonPrimitive -> if (value.isString) value.jsonPrimitive.content else value.toString()
    else -> value.toString()
}
