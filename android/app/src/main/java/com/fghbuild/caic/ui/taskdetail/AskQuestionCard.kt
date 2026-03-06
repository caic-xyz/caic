// Card for an ask question with options and answer display.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.snapshots.SnapshotStateMap
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp

import com.caic.sdk.v1.AskQuestion
import com.caic.sdk.v1.EventAsk

private val AskActiveBg = Color(0xFFF0F6FF)
private val AskActiveBorder = Color(0xFF4A90D9)
private val AskInactiveBg = Color(0xFFF8F9FA)
private val AskInactiveBorder = Color(0xFFDDDDDD)
private val AskAnsweredColor = Color(0xFF28A745)

private fun toggleOption(
    selections: SnapshotStateMap<Int, Set<String>>,
    qIdx: Int,
    label: String,
    multiSelect: Boolean,
) {
    val current = selections[qIdx] ?: emptySet()
    selections[qIdx] = if (current.contains(label)) {
        current - label
    } else if (multiSelect) {
        current + label
    } else {
        setOf(label)
    }
}

private fun formatAnswer(
    questions: List<AskQuestion>,
    selections: Map<Int, Set<String>>,
    otherTexts: Map<Int, String>,
): String {
    val parts = questions.mapIndexed { i, q ->
        val sel = selections[i] ?: emptySet()
        val labels = sel.map { label ->
            if (label == "__other__") otherTexts[i] ?: "" else label
        }.filter { it.isNotEmpty() }
        val answer = labels.joinToString(", ")
        if (questions.size == 1) answer else "${q.header ?: "Q${i + 1}"}: $answer"
    }
    return parts.joinToString("\n")
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun AskQuestionCard(
    ask: EventAsk,
    answerText: String?,
    onAnswer: ((String) -> Unit)?,
) {
    val answered = answerText != null
    val interactive = onAnswer != null && !answered

    val selections = remember(ask.toolUseID) { mutableStateMapOf<Int, Set<String>>() }
    val otherTexts = remember(ask.toolUseID) { mutableStateMapOf<Int, String>() }

    val bg = if (interactive) AskActiveBg else AskInactiveBg
    val borderColor = if (interactive) AskActiveBorder else AskInactiveBorder
    Surface(
        modifier = Modifier
            .fillMaxWidth()
            .border(1.dp, borderColor, MaterialTheme.shapes.medium),
        shape = MaterialTheme.shapes.medium,
        color = bg,
    ) {
        Column(
            modifier = Modifier.padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            ask.questions.forEachIndexed { qIdx, q ->
                q.header?.let { header ->
                    Text(
                        text = header,
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        fontWeight = androidx.compose.ui.text.font.FontWeight.Bold,
                    )
                }
                Text(
                    text = q.question,
                    style = MaterialTheme.typography.bodyMedium,
                )
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    q.options.forEach { option ->
                        val selected = selections[qIdx]?.contains(option.label) == true
                        FilterChip(
                            selected = selected,
                            onClick = {
                                if (interactive) {
                                    toggleOption(selections, qIdx, option.label, q.multiSelect == true)
                                }
                            },
                            label = {
                                Column {
                                    Text(option.label)
                                    option.description?.let { desc ->
                                        Text(
                                            text = desc,
                                            style = MaterialTheme.typography.labelSmall,
                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        )
                                    }
                                }
                            },
                            enabled = interactive,
                        )
                    }
                    val otherSelected = selections[qIdx]?.contains("__other__") == true
                    FilterChip(
                        selected = otherSelected,
                        onClick = {
                            if (interactive) {
                                toggleOption(selections, qIdx, "__other__", q.multiSelect == true)
                            }
                        },
                        label = { Text("Other") },
                        enabled = interactive,
                    )
                }
                if (selections[qIdx]?.contains("__other__") == true) {
                    OutlinedTextField(
                        value = otherTexts[qIdx] ?: "",
                        onValueChange = { otherTexts[qIdx] = it },
                        placeholder = { Text("Type your answer...") },
                        enabled = interactive,
                        modifier = Modifier.fillMaxWidth(),
                        singleLine = false,
                        maxLines = 4,
                    )
                }
            }
            if (interactive) {
                Button(
                    onClick = {
                        val answer = formatAnswer(ask.questions, selections, otherTexts)
                        if (answer.isNotBlank()) onAnswer(answer)
                    },
                ) {
                    Text("Submit")
                }
            }
            if (answered) {
                Text(
                    text = "Answered: $answerText",
                    style = MaterialTheme.typography.bodySmall,
                    color = AskAnsweredColor,
                )
            }
        }
    }
}
