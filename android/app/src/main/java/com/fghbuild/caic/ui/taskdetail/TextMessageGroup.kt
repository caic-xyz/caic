// Renders a text group: combines textDelta fragments, renders markdown.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.caic.sdk.ClaudeEventMessage
import com.caic.sdk.EventKinds
import com.mikepenz.markdown.m3.Markdown

@Composable
fun TextMessageGroup(events: List<ClaudeEventMessage>) {
    val text = remember(events) {
        val finalEv = events.lastOrNull { it.kind == EventKinds.Text }
        if (finalEv?.text != null) {
            finalEv.text!!.text
        } else {
            events
                .filter { it.kind == EventKinds.TextDelta && it.textDelta != null }
                .joinToString("") { it.textDelta!!.text }
        }
    }
    if (text.isBlank()) return
    Markdown(
        content = text,
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 4.dp),
        colors = com.mikepenz.markdown.m3.markdownColor(
            text = MaterialTheme.colorScheme.onSurface,
            codeBackground = MaterialTheme.colorScheme.surfaceVariant,
        ),
    )
}
