// Reusable attach dropdown menu with Take photo, Screenshot, and Choose from gallery options.
package com.fghbuild.caic.ui.common

import androidx.compose.foundation.layout.Box
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AttachFile
import androidx.compose.material.icons.filled.CameraAlt
import androidx.compose.material.icons.filled.PhotoLibrary
import androidx.compose.material.icons.filled.Screenshot
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.PlainTooltip
import androidx.compose.material3.Text
import androidx.compose.material3.TooltipAnchorPosition
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AttachMenu(
    enabled: Boolean,
    onCamera: () -> Unit,
    onScreenshot: () -> Unit,
    onGallery: () -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    Box {
        TooltipBox(
            positionProvider = TooltipDefaults.rememberTooltipPositionProvider(TooltipAnchorPosition.Above),
            tooltip = { PlainTooltip { Text("Attach image") } },
            state = rememberTooltipState(),
        ) {
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
                text = { Text("Screenshot") },
                onClick = { expanded = false; onScreenshot() },
                leadingIcon = { Icon(Icons.Default.Screenshot, contentDescription = null) },
            )
            DropdownMenuItem(
                text = { Text("Choose from gallery") },
                onClick = { expanded = false; onGallery() },
                leadingIcon = { Icon(Icons.Default.PhotoLibrary, contentDescription = null) },
            )
        }
    }
}
