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
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import kotlinx.coroutines.launch
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.util.ToolCall
import com.fghbuild.caic.util.formatDuration
import com.fghbuild.caic.util.toolCallDetail
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.jsonPrimitive

@Composable
fun ToolCallCard(
    call: ToolCall,
    modifier: Modifier = Modifier,
    outputDelta: String? = null,
    onLoadInput: (suspend () -> JsonElement?)? = null,
    onClearAndExecutePlan: (() -> Unit)? = null,
    suppressPlanContent: Boolean = false,
) {
    var expanded by rememberSaveable(call.use.toolUseID) { mutableStateOf(false) }
    var loadedInput by remember(call.use.toolUseID) { mutableStateOf<JsonElement?>(null) }
    var loadingInput by remember(call.use.toolUseID) { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val detail = remember(call.use.toolUseID) { toolCallDetail(call.use.name, call.use.input) }
    val hasError = call.result?.error != null

    Column(modifier = modifier.fillMaxWidth()) {
        Surface(
            modifier = Modifier.fillMaxWidth(),
            shape = MaterialTheme.shapes.small,
            color = MaterialTheme.appColors.toolBlockBg,
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
                    ToolStatusIcon(done = call.done, hasError = hasError, background = call.use.background == true)
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
                            text = formatDuration(result.duration),
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                AnimatedVisibility(visible = expanded) {
                    Column(modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp)) {
                        if (call.use.inputTruncated == true && loadedInput == null && onLoadInput != null) {
                            Button(
                                onClick = {
                                    onLoadInput?.let { loader ->
                                        scope.launch {
                                            loadingInput = true
                                            loadedInput = loader()
                                            loadingInput = false
                                        }
                                    }
                                },
                                enabled = !loadingInput,
                            ) {
                                Text(if (loadingInput) "Loading…" else "Load input")
                            }
                        } else {
                            ToolInputDisplay(input = loadedInput ?: call.use.input)
                        }
                        call.result?.error?.let { error ->
                            Surface(
                                modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
                                shape = MaterialTheme.shapes.small,
                                color = MaterialTheme.appColors.toolErrorBg,
                            ) {
                                Text(
                                    text = error,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.error,
                                    fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace,
                                    modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
                                )
                            }
                        }
                        if (!outputDelta.isNullOrEmpty()) {
                            Text(
                                text = outputDelta,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace,
                                modifier = Modifier.padding(top = 4.dp),
                            )
                        }
                    }
                }
            }
        }
        if (!suppressPlanContent) {
            call.use.planContent?.let { plan ->
                PlanContent(planContent = plan, onExecute = onClearAndExecutePlan)
            }
        }
    }
}

@Composable
private fun ToolStatusIcon(done: Boolean, hasError: Boolean, background: Boolean = false) {
    when {
        hasError -> Icon(
            Icons.Default.Close,
            contentDescription = "Error",
            tint = MaterialTheme.colorScheme.error,
            modifier = Modifier.size(16.dp),
        )
        done && background -> Icon(
            Icons.Default.MoreVert,
            contentDescription = "Running in background",
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.size(16.dp),
        )
        done -> Icon(
            Icons.Default.Check,
            contentDescription = "Done",
            tint = MaterialTheme.appColors.success,
            modifier = Modifier.size(16.dp),
        )
        else -> CircularProgressIndicator(modifier = Modifier.size(16.dp), strokeWidth = 2.dp)
    }
}

private fun isFlat(obj: JsonObject): Boolean =
    obj.values.all { it is JsonPrimitive }

@Composable
private fun ToolInputDisplay(input: JsonElement) {
    val obj = input as? JsonObject ?: return
    if (isFlat(obj)) {
        Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
            obj.entries.forEach { (key, value) ->
                val display = if (value is JsonPrimitive && value.isString) {
                    value.jsonPrimitive.content
                } else {
                    value.toString()
                }
                if (display.contains("\n")) {
                    Text(
                        text = "$key:",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    Text(
                        text = display,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace,
                    )
                } else {
                    Text(
                        text = "$key: $display",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    } else {
        Text(
            text = input.toString(),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace,
        )
    }
}
