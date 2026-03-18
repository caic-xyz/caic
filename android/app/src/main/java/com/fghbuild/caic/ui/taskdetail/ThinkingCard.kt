// Collapsed card for an agent thinking block, analogous to ToolCallCard.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.IntrinsicSize
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.EventKinds
import com.caic.sdk.v1.EventMessage
import com.fghbuild.caic.ui.theme.appColors

@Composable
fun ThinkingCard(events: List<EventMessage>, modifier: Modifier = Modifier) {
    val text = run {
        // Process events sequentially so each thinking block (final or still-streaming
        // deltas) is collected independently. Multiple thinking blocks can land in the
        // same group when consecutive tool-call turns are merged by groupMessages.
        val parts = mutableListOf<String>()
        val deltaBuffer = StringBuilder()
        for (ev in events) {
            if (ev.kind == EventKinds.ThinkingDelta && ev.thinkingDelta != null) {
                deltaBuffer.append(ev.thinkingDelta!!.text)
            } else if (ev.kind == EventKinds.Thinking && ev.thinking != null) {
                deltaBuffer.clear()
                parts.add(ev.thinking!!.text)
            }
        }
        if (deltaBuffer.isNotEmpty()) parts.add(deltaBuffer.toString())
        parts.joinToString("\n\n")
    }
    if (text.isBlank()) return

    var expanded by rememberSaveable(events.firstOrNull()?.ts) { mutableStateOf(false) }

    Row(
        modifier = modifier
            .fillMaxWidth()
            .height(IntrinsicSize.Min)
            .clip(MaterialTheme.shapes.small)
            .background(MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Box(
            modifier = Modifier
                .width(2.dp)
                .fillMaxHeight()
                .background(MaterialTheme.appColors.thinkingBorder),
        )
        Column(modifier = Modifier.weight(1f)) {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .clickable { expanded = !expanded }
                    .padding(8.dp),
            ) {
                Text(
                    text = "Thinking",
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    fontStyle = FontStyle.Italic,
                )
            }
            AnimatedVisibility(visible = expanded) {
                Text(
                    text = text,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    fontFamily = FontFamily.Monospace,
                    modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
                )
            }
        }
    }
}
