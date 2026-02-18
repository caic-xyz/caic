// Collapsible TODO list panel showing task progress.
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
import androidx.compose.material.icons.filled.Circle
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material3.Card
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.caic.sdk.ClaudeTodoItem

@Composable
fun TodoPanel(todos: List<ClaudeTodoItem>, modifier: Modifier = Modifier) {
    if (todos.isEmpty()) return
    var expanded by rememberSaveable { mutableStateOf(true) }
    val completed = todos.count { it.status == "completed" }

    Card(modifier = modifier.fillMaxWidth()) {
        Column {
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .clickable { expanded = !expanded }
                    .padding(12.dp),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = "TODOS $completed/${todos.size}",
                    style = MaterialTheme.typography.labelMedium,
                    fontWeight = FontWeight.Bold,
                )
            }
            AnimatedVisibility(visible = expanded) {
                Column(modifier = Modifier.padding(start = 12.dp, end = 12.dp, bottom = 12.dp)) {
                    todos.forEach { item ->
                        TodoItemRow(item)
                    }
                }
            }
        }
    }
}

@Composable
private fun TodoItemRow(item: ClaudeTodoItem) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        when (item.status) {
            "completed" -> Icon(
                Icons.Default.Check, contentDescription = "Done",
                tint = Color(0xFF4CAF50), modifier = Modifier.size(16.dp),
            )
            "in_progress" -> Icon(
                Icons.Default.PlayArrow, contentDescription = "In progress",
                tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(16.dp),
            )
            else -> Icon(
                Icons.Default.Circle, contentDescription = "Pending",
                tint = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.size(16.dp),
            )
        }
        val displayText = if (item.status == "in_progress") {
            item.activeForm ?: item.content
        } else {
            item.content
        }
        Text(
            text = displayText,
            style = MaterialTheme.typography.bodySmall.let {
                when (item.status) {
                    "completed" -> it.copy(
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        textDecoration = TextDecoration.LineThrough,
                    )
                    "in_progress" -> it.copy(fontWeight = FontWeight.Bold)
                    else -> it
                }
            },
            maxLines = 2,
            overflow = TextOverflow.Ellipsis,
        )
    }
}
