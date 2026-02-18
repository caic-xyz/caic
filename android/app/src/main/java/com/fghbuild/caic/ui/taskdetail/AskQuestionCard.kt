// Card for an ask question with options and answer display.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import com.caic.sdk.ClaudeEventAsk

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun AskQuestionCard(
    ask: ClaudeEventAsk,
    answerText: String?,
    onAnswer: ((String) -> Unit)?,
) {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        tonalElevation = 2.dp,
        shape = MaterialTheme.shapes.medium,
        color = MaterialTheme.colorScheme.secondaryContainer,
    ) {
        Column(
            modifier = Modifier.padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            ask.questions.forEach { q ->
                Text(
                    text = q.question,
                    style = MaterialTheme.typography.bodyMedium,
                )
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    q.options.forEach { option ->
                        FilterChip(
                            selected = answerText == option.label,
                            onClick = {
                                if (answerText == null) onAnswer?.invoke(option.label)
                            },
                            label = { Text(option.label) },
                            enabled = answerText == null,
                        )
                    }
                }
            }
            if (answerText != null) {
                Text(
                    text = "Answered: $answerText",
                    style = MaterialTheme.typography.bodySmall,
                    color = Color(0xFF22C55E),
                )
            }
        }
    }
}
