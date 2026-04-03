// Bottom input bar with send, sync, stop, purge, revive, clear context, compact, and optional image attach actions.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.Image
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.ui.res.painterResource
import com.fghbuild.caic.R
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.PhotoLibrary
import androidx.compose.material.icons.filled.ArrowDropDown
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Sync
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Surface
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.PlainTooltip
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.rememberTooltipState
import com.fghbuild.caic.ui.theme.appColors
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableLongStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ImageData
import com.caic.sdk.v1.SafetyIssue
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.ui.common.AttachMenu
import com.fghbuild.caic.util.imageDataToBitmap

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun InputBar(
    draft: String,
    onDraftChange: (String) -> Unit,
    onSend: () -> Unit,
    onSync: () -> Unit,
    onSyncToBaseBranch: () -> Unit = {},
    onStop: () -> Unit,
    onPurge: () -> Unit,
    onRevive: () -> Unit,
    taskState: String = "",
    taskTitle: String = "",
    taskRepo: String = "",
    taskBranch: String = "",
    taskBaseBranch: String = "",
    sending: Boolean,
    pendingAction: String?,
    forge: String? = null,
    forgePR: Int? = null,
    pendingImages: List<ImageData> = emptyList(),
    supportsImages: Boolean = false,
    onAttachGallery: () -> Unit = {},
    onAttachCamera: () -> Unit = {},
    onScreenshot: () -> Unit = {},
    onRemoveImage: (Int) -> Unit = {},
    onClearContext: () -> Unit = {},
    onCompact: () -> Unit = {},
    supportsCompact: Boolean = false,
    safetyIssues: List<SafetyIssue> = emptyList(),
    onForceSync: () -> Unit = {},
) {
    val busy = sending || pendingAction != null
    val hasContent = draft.isNotBlank() || pendingImages.isNotEmpty()
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 8.dp, vertical = 4.dp),
    ) {
        if (safetyIssues.isNotEmpty()) {
            Surface(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(bottom = 4.dp)
                    .border(1.dp, MaterialTheme.appColors.safetyBorder, MaterialTheme.shapes.small),
                shape = MaterialTheme.shapes.small,
                color = MaterialTheme.appColors.warningBg,
            ) {
                Column(
                    modifier = Modifier.padding(horizontal = 8.dp, vertical = 6.dp),
                    verticalArrangement = Arrangement.spacedBy(2.dp),
                ) {
                    Text(
                        "Safety issues detected:",
                        style = MaterialTheme.typography.bodySmall,
                        fontWeight = FontWeight.Bold,
                    )
                    safetyIssues.forEach { issue ->
                        Text(
                            "${issue.file}: ${issue.kind} \u2014 ${issue.detail}",
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                    TextButton(onClick = onForceSync) { Text("Force Push") }
                }
            }
        }
        if (pendingImages.isNotEmpty()) {
            LazyRow(
                horizontalArrangement = Arrangement.spacedBy(4.dp),
                modifier = Modifier.padding(bottom = 4.dp),
            ) {
                itemsIndexed(pendingImages) { index, img ->
                    ImageThumbnail(img = img, onRemove = { onRemoveImage(index) })
                }
            }
        }
        OutlinedTextField(
            value = draft,
            onValueChange = onDraftChange,
            modifier = Modifier
                .fillMaxWidth()
                .onKeyEvent {
                    if (it.key == Key.Enter && it.type == KeyEventType.KeyUp && hasContent && !busy) {
                        onSend(); true
                    } else false
                },
            placeholder = { Text("Message...") },
            maxLines = 6,
            enabled = !busy,
            trailingIcon = {
                if (sending) {
                    CircularProgressIndicator(modifier = Modifier.size(24.dp))
                } else {
                    IconButton(onClick = onSend, enabled = hasContent && !busy) {
                        Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Send")
                    }
                }
            },
        )
        Row(
            horizontalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            if (supportsImages) {
                AttachMenu(
                    enabled = !busy,
                    onGallery = onAttachGallery,
                    onCamera = onAttachCamera,
                    onScreenshot = onScreenshot,
                )
            }
            if (pendingAction == "sync") {
                CircularProgressIndicator(modifier = Modifier.size(24.dp).padding(8.dp))
            } else {
                val syncLabel = when {
                    (forge == "github" || forge == "gitlab") && (forgePR == null || forgePR == 0) -> "Create PR"
                    else -> "Push"
                }
                var syncMenuExpanded by remember { mutableStateOf(false) }
                Box {
                    Tip(syncLabel) {
                        IconButton(onClick = onSync, enabled = !busy) {
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                when (forge) {
                                    "github" ->
                                        Icon(painterResource(R.drawable.ic_github), contentDescription = syncLabel)
                                    "gitlab" ->
                                        Icon(painterResource(R.drawable.ic_gitlab), contentDescription = syncLabel)
                                    else ->
                                        Icon(Icons.Default.Sync, contentDescription = syncLabel)
                                }
                                Text(syncLabel, style = MaterialTheme.typography.labelSmall)
                            }
                        }
                    }
                    if (taskBaseBranch.isNotBlank()) {
                        IconButton(
                            onClick = { syncMenuExpanded = true },
                            enabled = !busy,
                            modifier = Modifier.size(16.dp).align(Alignment.BottomEnd),
                        ) {
                            Icon(
                                Icons.Default.ArrowDropDown,
                                contentDescription = "Sync options",
                                modifier = Modifier.size(12.dp),
                            )
                        }
                        DropdownMenu(
                            expanded = syncMenuExpanded,
                            onDismissRequest = { syncMenuExpanded = false },
                        ) {
                            DropdownMenuItem(
                                text = { Text("Push to $taskBaseBranch") },
                                onClick = { syncMenuExpanded = false; onSyncToBaseBranch() },
                            )
                        }
                    }
                }
            }
            val waitingStates = setOf("waiting", "asking", "has_plan")
            val activeStates = setOf("waiting", "running", "asking", "has_plan")
            val isStopped = taskState == "stopped"
            val isActive = taskState in activeStates
            val isWaiting = taskState in waitingStates
            if (pendingAction == "stop" || pendingAction == "purge" || pendingAction == "revive") {
                CircularProgressIndicator(modifier = Modifier.size(24.dp).padding(8.dp))
            } else if (isStopped) {
                Tip("Revive") {
                    IconButton(onClick = onRevive, enabled = !busy, modifier = Modifier.testTag("revive-task")) {
                        Icon(
                            Icons.Default.Refresh,
                            contentDescription = "Revive",
                            tint = MaterialTheme.appColors.success,
                        )
                    }
                }
                var showPurgeConfirm by remember { mutableStateOf(false) }
                Tip("Purge") {
                    IconButton(onClick = { showPurgeConfirm = true }, enabled = !busy, modifier = Modifier.testTag("purge-task")) {
                        Icon(
                            Icons.Default.Delete,
                            contentDescription = "Purge",
                            tint = MaterialTheme.colorScheme.error,
                        )
                    }
                }
                if (showPurgeConfirm) {
                    AlertDialog(
                        onDismissRequest = { showPurgeConfirm = false },
                        title = { Text("Purge container?") },
                        text = { Text("$taskTitle\nrepo: $taskRepo\nbranch: $taskBranch") },
                        confirmButton = {
                            TextButton(onClick = { showPurgeConfirm = false; onPurge() }) {
                                Text("Purge")
                            }
                        },
                        dismissButton = {
                            TextButton(onClick = { showPurgeConfirm = false }) {
                                Text("Cancel")
                            }
                        },
                    )
                }
            } else if (isActive) {
                var showStopConfirm by remember { mutableStateOf(false) }
                var showPurgeFromActive by remember { mutableStateOf(false) }
                var lastStopTap by remember { mutableLongStateOf(0L) }
                Tip("Stop (double-tap to purge)") {
                    IconButton(
                        onClick = {
                            val now = System.currentTimeMillis()
                            if (now - lastStopTap < 400) {
                                // Double-tap: skip stop, go straight to purge.
                                lastStopTap = 0L
                                showPurgeFromActive = true
                            } else {
                                lastStopTap = now
                                if (taskState == "running") showStopConfirm = true else onStop()
                            }
                        },
                        enabled = !busy,
                        modifier = Modifier.testTag("stop-task"),
                    ) {
                        Icon(
                            Icons.Default.Delete,
                            contentDescription = "Stop",
                            tint = MaterialTheme.colorScheme.error,
                        )
                    }
                }
                if (showStopConfirm) {
                    AlertDialog(
                        onDismissRequest = { showStopConfirm = false },
                        title = { Text("Stop task?") },
                        text = { Text("$taskTitle\nrepo: $taskRepo\nbranch: $taskBranch") },
                        confirmButton = {
                            TextButton(onClick = { showStopConfirm = false; onStop() }) {
                                Text("Stop")
                            }
                        },
                        dismissButton = {
                            TextButton(onClick = { showStopConfirm = false }) {
                                Text("Cancel")
                            }
                        },
                    )
                }
                if (showPurgeFromActive) {
                    AlertDialog(
                        onDismissRequest = { showPurgeFromActive = false },
                        title = { Text("Purge container?") },
                        text = { Text("$taskTitle\nrepo: $taskRepo\nbranch: $taskBranch") },
                        confirmButton = {
                            TextButton(onClick = { showPurgeFromActive = false; onPurge() }) {
                                Text("Purge")
                            }
                        },
                        dismissButton = {
                            TextButton(onClick = { showPurgeFromActive = false }) {
                                Text("Cancel")
                            }
                        },
                    )
                }
            }
            if (pendingAction == "clear-context" || pendingAction == "compact") {
                CircularProgressIndicator(modifier = Modifier.size(24.dp).padding(8.dp))
            } else {
                var contextMenuExpanded by remember { mutableStateOf(false) }
                Box {
                    Tip("Context actions") {
                        IconButton(onClick = { contextMenuExpanded = true }, enabled = !busy) {
                            Icon(Icons.Default.MoreVert, contentDescription = "Context actions")
                        }
                    }
                    DropdownMenu(
                        expanded = contextMenuExpanded,
                        onDismissRequest = { contextMenuExpanded = false },
                    ) {
                        DropdownMenuItem(
                            text = { Text("Clear context") },
                            enabled = false,
                            onClick = { contextMenuExpanded = false; onClearContext() },
                        )
                        if (supportsCompact) {
                            DropdownMenuItem(
                                text = { Text("Compact context") },
                                enabled = isWaiting,
                                onClick = { contextMenuExpanded = false; onCompact() },
                            )
                        }
                    }
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun Tip(text: String, content: @Composable () -> Unit) {
    TooltipBox(
        positionProvider = TooltipDefaults.rememberPlainTooltipPositionProvider(),
        tooltip = { PlainTooltip { Text(text) } },
        state = rememberTooltipState(),
        content = content,
    )
}

@Composable
private fun ImageThumbnail(img: ImageData, onRemove: () -> Unit) {
    val bitmap = remember(img) { imageDataToBitmap(img)?.asImageBitmap() } ?: return
    Row(verticalAlignment = Alignment.Top) {
        Image(
            bitmap = bitmap,
            contentDescription = "Attached image",
            modifier = Modifier
                .size(48.dp)
                .clip(RoundedCornerShape(4.dp)),
            contentScale = ContentScale.Crop,
        )
        Icon(
            Icons.Default.Close,
            contentDescription = "Remove",
            modifier = Modifier
                .size(16.dp)
                .clickable(onClick = onRemove),
        )
    }
}
