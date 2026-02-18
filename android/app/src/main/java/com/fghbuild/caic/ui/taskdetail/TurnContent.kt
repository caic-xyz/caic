// Renders all message groups within a single turn.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.caic.sdk.EventKinds
import com.fghbuild.caic.util.GroupKind
import com.fghbuild.caic.util.Turn

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
                    val text = group.events.firstOrNull()?.userInput?.text
                    if (text != null) {
                        Text(
                            text = "You: $text",
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.primary,
                            modifier = Modifier.padding(vertical = 4.dp),
                        )
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
