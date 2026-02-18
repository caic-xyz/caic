// Alert dialog listing safety issues from a sync attempt.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import com.caic.sdk.SafetyIssue

@Composable
fun SafetyDialog(
    issues: List<SafetyIssue>,
    onDismiss: () -> Unit,
    onForceSync: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Safety Issues") },
        text = {
            Text(
                text = issues.joinToString("\n") { "${it.file}: ${it.kind} \u2014 ${it.detail}" },
                style = MaterialTheme.typography.bodySmall,
            )
        },
        confirmButton = {
            TextButton(onClick = onForceSync) {
                Text("Force Sync")
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) {
                Text("Cancel")
            }
        },
    )
}
