// Renders a text group: combines textDelta fragments, renders markdown or isolated HTML.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.ContentCopy
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.caic.sdk.v1.EventKinds
import com.caic.sdk.v1.EventMessage
import com.fghbuild.caic.ui.theme.markdownTypography
import com.fghbuild.caic.util.GroupKind
import com.fghbuild.caic.util.MessageGroup
import com.mikepenz.markdown.m3.Markdown

/** Returns true when text is a raw HTML fragment (weaker model dumped HTML as text). */
private fun looksLikeHTML(text: String): Boolean {
    val trimmed = text.trimStart()
    return trimmed.startsWith("<style") ||
        trimmed.startsWith("<div") ||
        trimmed.startsWith("<!--")
}

/** Markdown content with a toggle to show the raw source. */
@Composable
fun MarkdownWithRawToggle(text: String, modifier: Modifier = Modifier) {
    var showRaw by remember { mutableStateOf(false) }
    Column(modifier = modifier) {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.End,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                text = if (showRaw) "rendered" else "raw",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.primary,
                modifier = Modifier
                    .clip(MaterialTheme.shapes.small)
                    .clickable { showRaw = !showRaw }
                    .padding(horizontal = 8.dp, vertical = 4.dp),
            )
            if (showRaw) {
                val clipboard = LocalClipboardManager.current
                Icon(
                    imageVector = Icons.Outlined.ContentCopy,
                    contentDescription = "Copy",
                    tint = MaterialTheme.colorScheme.primary,
                    modifier = Modifier
                        .size(32.dp)
                        .clip(MaterialTheme.shapes.small)
                        .clickable { clipboard.setText(AnnotatedString(text)) }
                        .padding(6.dp),
                )
            }
        }
        if (showRaw) {
            Surface(
                shape = MaterialTheme.shapes.small,
                color = MaterialTheme.colorScheme.surfaceVariant,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text(
                    text = text,
                    style = MaterialTheme.typography.bodySmall.copy(
                        fontFamily = FontFamily.Monospace,
                        fontSize = 12.sp,
                        lineHeight = 18.sp,
                    ),
                    modifier = Modifier
                        .padding(8.dp)
                        .horizontalScroll(rememberScrollState()),
                )
            }
        } else {
            Markdown(
                content = text,
                modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp),
                typography = markdownTypography(),
                colors = com.mikepenz.markdown.m3.markdownColor(
                    text = MaterialTheme.colorScheme.onSurface,
                    codeBackground = MaterialTheme.colorScheme.surfaceVariant,
                ),
            )
        }
    }
}

@Composable
fun TextMessageGroup(events: List<EventMessage>) {
    val thinkingEvents = remember(events) {
        events.filter { it.kind == EventKinds.Thinking || it.kind == EventKinds.ThinkingDelta }
    }
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
    if (thinkingEvents.isEmpty() && text.isBlank()) return
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        if (thinkingEvents.isNotEmpty()) {
            ThinkingCard(events = thinkingEvents)
        }
        if (text.isNotBlank()) {
            if (looksLikeHTML(text)) {
                val widgetGroup = remember(text, events) {
                    MessageGroup(
                        kind = GroupKind.WIDGET,
                        events = events,
                        widgetHTML = text,
                        widgetDone = events.any { it.kind == EventKinds.Text },
                    )
                }
                WidgetCard(group = widgetGroup)
            } else {
                MarkdownWithRawToggle(text = text)
            }
        }
    }
}
