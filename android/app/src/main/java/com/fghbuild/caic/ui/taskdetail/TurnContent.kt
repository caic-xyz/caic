// Renders all message groups within a single turn.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.unit.dp
import com.caic.sdk.EventKinds
import com.caic.sdk.ImageData
import com.fghbuild.caic.util.GroupKind
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.imageDataToBitmap

@Composable
fun TurnContent(turn: Turn, onAnswer: ((String) -> Unit)?) {
    Column(
        modifier = Modifier.fillMaxWidth(),
        verticalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        turn.groups.forEach { group ->
            when (group.kind) {
                GroupKind.TEXT -> TextMessageGroup(events = group.events)
                GroupKind.TOOL -> ToolMessageGroup(toolCalls = group.toolCalls)
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
                    val result = group.events.firstOrNull { it.kind == EventKinds.Result }?.result
                    if (result != null) {
                        ResultCard(result = result)
                    }
                }
            }
        }
    }
}

@Composable
private fun UserInputContent(text: String, images: List<ImageData>) {
    Column(modifier = Modifier.padding(vertical = 4.dp)) {
        if (text.isNotBlank()) {
            Text(
                text = "You: $text",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.primary,
            )
        }
        if (images.isNotEmpty()) {
            Row(
                horizontalArrangement = Arrangement.spacedBy(4.dp),
                modifier = Modifier.padding(top = if (text.isNotBlank()) 4.dp else 0.dp),
            ) {
                images.forEach { img ->
                    val bitmap = remember(img) { imageDataToBitmap(img)?.asImageBitmap() }
                    if (bitmap != null) {
                        Image(
                            bitmap = bitmap,
                            contentDescription = "User image",
                            modifier = Modifier
                                .size(64.dp)
                                .clip(RoundedCornerShape(4.dp)),
                            contentScale = ContentScale.Crop,
                        )
                    }
                }
            }
        }
    }
}
