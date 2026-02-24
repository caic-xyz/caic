// Renders a text group: combines textDelta fragments, renders markdown.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ClaudeEventMessage
import com.caic.sdk.v1.EventKinds
import com.fghbuild.caic.ui.theme.markdownTypography
import com.mikepenz.markdown.m3.Markdown

private data class TextState(val text: String, val isStreaming: Boolean)

@Composable
fun TextMessageGroup(events: List<ClaudeEventMessage>) {
    val state = remember(events) {
        val finalEv = events.lastOrNull { it.kind == EventKinds.Text }
        if (finalEv?.text != null) {
            TextState(finalEv.text!!.text, isStreaming = false)
        } else {
            TextState(
                text = events
                    .filter { it.kind == EventKinds.TextDelta && it.textDelta != null }
                    .joinToString("") { it.textDelta!!.text },
                isStreaming = true,
            )
        }
    }
    if (state.text.isBlank()) return
    val modifier = Modifier
        .fillMaxWidth()
        .padding(vertical = 4.dp)
    if (state.isStreaming) {
        Text(
            text = state.text,
            style = MaterialTheme.typography.bodyMedium,
            modifier = modifier,
        )
    } else {
        Markdown(
            content = state.text,
            modifier = modifier,
            typography = markdownTypography(),
            colors = com.mikepenz.markdown.m3.markdownColor(
                text = MaterialTheme.colorScheme.onSurface,
                codeBackground = MaterialTheme.colorScheme.surfaceVariant,
            ),
        )
    }
}
