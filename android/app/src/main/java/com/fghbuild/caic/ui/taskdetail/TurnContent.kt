// Renders all message groups within a single turn.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.Image
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.EventKinds
import com.caic.sdk.v1.ImageData
import com.fghbuild.caic.util.GroupKind
import com.fghbuild.caic.util.MessageGroup
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.formatTokens
import com.fghbuild.caic.util.imageDataToBitmap
import kotlinx.serialization.json.JsonElement
import com.fghbuild.caic.ui.theme.markdownTypography
import com.mikepenz.markdown.m3.Markdown

private val PlanBorderColor = Color(0xFFDDD6FE)
private val PlanBgColor = Color(0xFFF5F3FF)
private val UserMsgBgColor = Color(0xFFDBE9F9)
private val ImageBorderColor = Color(0xFFB8D0EA)

/** Renders a single [MessageGroup]. Used both in [TurnContent] and the flat list. */
@Composable
fun MessageGroupContent(
    group: MessageGroup,
    onAnswer: ((String) -> Unit)?,
    onNavigateToDiff: (() -> Unit)? = null,
    onLoadToolInput: (suspend (String) -> JsonElement?)? = null,
) {
    when (group.kind) {
        GroupKind.ACTION -> {
            if (group.toolCalls.isEmpty()) {
                ThinkingCard(events = group.events)
            } else {
                val thinkingEvents = group.events.filter {
                    it.kind == EventKinds.Thinking || it.kind == EventKinds.ThinkingDelta
                }
                if (thinkingEvents.isNotEmpty()) {
                    ThinkingCard(events = thinkingEvents)
                }
                ToolMessageGroup(toolCalls = group.toolCalls, onLoadInput = onLoadToolInput)
            }
        }
        GroupKind.TEXT -> TextMessageGroup(events = group.events)
        GroupKind.ASK -> {
            group.ask?.let { ask ->
                AskQuestionCard(ask = ask, answerText = group.answerText, onAnswer = onAnswer)
            }
        }
        GroupKind.USER_INPUT -> {
            val userInput = group.events.firstOrNull()?.userInput
            if (userInput != null) {
                UserInputContent(text = userInput.text, images = userInput.images.orEmpty())
            }
        }
        GroupKind.OTHER -> {
            val event = group.events.firstOrNull()
            when {
                event?.kind == EventKinds.Init -> {
                    val init = event.init
                    if (init != null) {
                        Text(
                            text = "Session started \u00b7 ${init.model}" +
                                " \u00b7 ${init.agentVersion} \u00b7 ${init.sessionID}",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                event?.kind == EventKinds.Usage -> {
                    val usage = event.usage
                    if (usage != null) {
                        val totalIn = usage.inputTokens + usage.cacheCreationInputTokens + usage.cacheReadInputTokens
                        val cached = usage.cacheReadInputTokens
                        val text = buildString {
                            append("${usage.model} \u00b7 ${formatTokens(totalIn)}")
                            append(" in + ${formatTokens(usage.outputTokens)} out")
                            if (cached > 0) append(" \u00b7 ${formatTokens(cached)} cached")
                        }
                        Text(
                            text = text,
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                event?.kind == EventKinds.System && event.system?.subtype == "context_cleared" -> {
                    Text(
                        text = "Context cleared",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp),
                    )
                }
                event?.kind == EventKinds.System -> {
                    val subtype = event.system?.subtype
                    if (!subtype.isNullOrBlank()) {
                        Text(
                            text = "[$subtype]",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                event?.kind == EventKinds.Log -> {
                    val line = event.log?.line
                    if (!line.isNullOrBlank()) {
                        Text(
                            text = line,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            fontFamily = androidx.compose.ui.text.font.FontFamily.Monospace,
                        )
                    }
                }
                event?.kind == EventKinds.Result -> {
                    val result = event.result
                    if (result != null) {
                        ResultCard(result = result, onNavigateToDiff = onNavigateToDiff)
                    }
                }
                event?.error != null -> {
                    Text(
                        text = "Parse error: ${event.error!!.err}",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
            }
        }
    }
}

/** Renders all groups in [turn] as a non-lazy Column. Used for expanded elided turns. */
@Composable
fun TurnContent(
    turn: Turn,
    onAnswer: ((String) -> Unit)?,
    onLoadToolInput: (suspend (String) -> JsonElement?)? = null,
) {
    Column(
        modifier = Modifier.fillMaxWidth(),
        verticalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        turn.groups.forEach { group ->
            MessageGroupContent(group, onAnswer, onLoadToolInput = onLoadToolInput)
        }
    }
}

@Composable
fun PlanApprovalSection(planContent: String, onExecute: () -> Unit) {
    Column(
        modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Surface(
            modifier = Modifier
                .fillMaxWidth()
                .border(1.dp, PlanBorderColor, RoundedCornerShape(6.dp)),
            shape = RoundedCornerShape(6.dp),
            color = PlanBgColor,
        ) {
            Markdown(
                content = planContent,
                modifier = Modifier.padding(12.dp).fillMaxWidth(),
                typography = markdownTypography(),
                colors = com.mikepenz.markdown.m3.markdownColor(
                    codeBackground = MaterialTheme.colorScheme.surfaceVariant,
                ),
            )
        }
        Button(
            onClick = onExecute,
            colors = ButtonDefaults.buttonColors(
                containerColor = MaterialTheme.colorScheme.surfaceVariant,
                contentColor = MaterialTheme.colorScheme.onSurfaceVariant,
            ),
        ) {
            Text("Clear and execute plan")
        }
    }
}

@Composable
private fun UserInputContent(text: String, images: List<ImageData>) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(6.dp),
        color = UserMsgBgColor,
    ) {
        Column(modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp)) {
            if (text.isNotBlank()) {
                Markdown(
                    content = text,
                    typography = markdownTypography(),
                    colors = com.mikepenz.markdown.m3.markdownColor(
                        text = MaterialTheme.colorScheme.onSurface,
                        codeBackground = MaterialTheme.colorScheme.surfaceVariant,
                    ),
                )
            }
            if (images.isNotEmpty()) {
                Column(
                    verticalArrangement = Arrangement.spacedBy(4.dp),
                    modifier = Modifier.padding(top = if (text.isNotBlank()) 4.dp else 0.dp),
                ) {
                    images.forEach { img ->
                        val bitmap = remember(img) { imageDataToBitmap(img)?.asImageBitmap() }
                        if (bitmap != null) {
                            Image(
                                bitmap = bitmap,
                                contentDescription = "User image",
                                modifier = Modifier
                                    .fillMaxWidth()
                                    .clip(RoundedCornerShape(4.dp))
                                    .border(1.dp, ImageBorderColor, RoundedCornerShape(4.dp)),
                                contentScale = ContentScale.FillWidth,
                            )
                        }
                    }
                }
            }
        }
    }
}
