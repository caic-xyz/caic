// Bottom input bar with send, sync, terminate, and optional image attach actions.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.Image
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
import androidx.compose.material.icons.filled.AttachFile
import androidx.compose.material.icons.filled.CameraAlt
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.PhotoLibrary
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Sync
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
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
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
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
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ImageData
import com.fghbuild.caic.util.imageDataToBitmap

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun InputBar(
    draft: String,
    onDraftChange: (String) -> Unit,
    onSend: () -> Unit,
    onSync: () -> Unit,
    onTerminate: () -> Unit,
    sending: Boolean,
    pendingAction: String?,
    repoURL: String? = null,
    pendingImages: List<ImageData> = emptyList(),
    supportsImages: Boolean = false,
    onAttachGallery: () -> Unit = {},
    onAttachCamera: () -> Unit = {},
    onRemoveImage: (Int) -> Unit = {},
) {
    val busy = sending || pendingAction != null
    val hasContent = draft.isNotBlank() || pendingImages.isNotEmpty()
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 8.dp, vertical = 4.dp),
    ) {
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
                )
            }
            if (pendingAction == "sync") {
                CircularProgressIndicator(modifier = Modifier.size(24.dp).padding(8.dp))
            } else {
                Tip("Sync") {
                    IconButton(onClick = onSync, enabled = !busy) {
                        if (repoURL?.contains("github.com") == true) {
                            Icon(painterResource(R.drawable.ic_github), contentDescription = "Sync")
                        } else {
                            Icon(Icons.Default.Sync, contentDescription = "Sync")
                        }
                    }
                }
            }
            if (pendingAction == "terminate") {
                CircularProgressIndicator(modifier = Modifier.size(24.dp).padding(8.dp))
            } else {
                var showTerminateConfirm by remember { mutableStateOf(false) }
                Tip("Terminate") {
                    IconButton(onClick = { showTerminateConfirm = true }, enabled = !busy) {
                        Icon(
                            Icons.Default.Delete,
                            contentDescription = "Terminate",
                            tint = MaterialTheme.colorScheme.error,
                        )
                    }
                }
                if (showTerminateConfirm) {
                    AlertDialog(
                        onDismissRequest = { showTerminateConfirm = false },
                        title = { Text("Terminate container?") },
                        text = { Text("This will stop the running task and destroy its container.") },
                        confirmButton = {
                            TextButton(onClick = { showTerminateConfirm = false; onTerminate() }) {
                                Text("Terminate")
                            }
                        },
                        dismissButton = {
                            TextButton(onClick = { showTerminateConfirm = false }) {
                                Text("Cancel")
                            }
                        },
                    )
                }
            }
        }
    }
}

@Composable
private fun AttachMenu(
    enabled: Boolean,
    onGallery: () -> Unit,
    onCamera: () -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    Box {
        Tip("Attach image") {
            IconButton(onClick = { expanded = true }, enabled = enabled) {
                Icon(Icons.Default.AttachFile, contentDescription = "Attach image")
            }
        }
        DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            DropdownMenuItem(
                text = { Text("Take photo") },
                onClick = { expanded = false; onCamera() },
                leadingIcon = { Icon(Icons.Default.CameraAlt, contentDescription = null) },
            )
            DropdownMenuItem(
                text = { Text("Choose from gallery") },
                onClick = { expanded = false; onGallery() },
                leadingIcon = { Icon(Icons.Default.PhotoLibrary, contentDescription = null) },
            )
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
