// Bottom input bar with send, sync, fork, stop, purge, revive, clear context, compact, and optional image attach actions.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
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
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Block
import androidx.compose.material.icons.filled.Compress
import androidx.compose.material.icons.filled.RestartAlt
import androidx.compose.material.icons.filled.StopCircle
import androidx.compose.material.icons.filled.Sync
import androidx.compose.material.icons.outlined.ForkRight
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
import androidx.compose.material3.TooltipAnchorPosition
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.res.painterResource
import com.fghbuild.caic.R
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.HarnessInfo
import com.caic.sdk.v1.ImageData
import com.caic.sdk.v1.Repo
import com.caic.sdk.v1.RepoSpec
import com.caic.sdk.v1.SafetyIssue
import com.fghbuild.caic.ui.common.AttachMenu
import com.fghbuild.caic.ui.common.RepoChipStrip
import com.fghbuild.caic.ui.common.RepoEntry
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.util.imageDataToBitmap

@OptIn(ExperimentalMaterial3Api::class)
/** Returns valid effort levels for [harness], empty if unsupported. */
private fun effortOptions(harness: String): List<String> = when (harness) {
    "claude" -> listOf("low", "medium", "high", "max")
    "codex" -> listOf("none", "minimal", "low", "medium", "high", "xhigh")
    "pi" -> listOf("off", "minimal", "low", "medium", "high", "xhigh")
    else -> emptyList()
}

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
    onFork: (
        prompt: String,
        harness: String?,
        model: String?,
        effort: String?,
        extraRepos: List<RepoSpec>?,
        tailscale: Boolean,
        usb: Boolean,
        display: Boolean,
        sudo: Boolean,
        gitHubToken: Boolean,
    ) -> Unit = { _, _, _, _, _, _, _, _, _, _ -> },
    taskState: String = "",
    taskTitle: String = "",
    taskRepo: String = "",
    taskBranch: String = "",
    taskBaseBranch: String = "",
    taskHarness: String = "",
    taskModel: String = "",
    taskEffort: String = "",
    taskTailscale: Boolean = false,
    taskUsb: Boolean = false,
    taskDisplay: Boolean = false,
    taskSudo: Boolean = false,
    taskGitHubToken: Boolean = false,
    harnesses: List<HarnessInfo> = emptyList(),
    allRepos: List<Repo> = emptyList(),
    forkAvailableRecent: List<Repo> = emptyList(),
    forkAvailableRest: List<Repo> = emptyList(),
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
    tailscaleAvailable: Boolean = true,
    usbAvailable: Boolean = true,
    displayAvailable: Boolean = true,
    sudoAvailable: Boolean = true,
    gitHubTokenAvailable: Boolean = false,
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
        val syncLabel = when {
            (forge == "github" || forge == "gitlab") && (forgePR == null || forgePR == 0) -> "Create PR"
            else -> "Push"
        }
        val waitingStates = setOf("waiting", "asking", "has_plan")
        val activeStates = setOf("waiting", "running", "asking", "has_plan")
        val isStopped = taskState == "stopped"
        val isActive = taskState in activeStates
        val isWaiting = taskState in waitingStates
        var contextMenuExpanded by remember { mutableStateOf(false) }
        var showForkDialog by remember { mutableStateOf(false) }
        var forkPrompt by remember { mutableStateOf("") }
        var showStopConfirm by remember { mutableStateOf(false) }
        var showPurgeConfirm by remember { mutableStateOf(false) }
        Row(
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            OutlinedTextField(
                value = draft,
                onValueChange = onDraftChange,
                modifier = Modifier
                    .weight(1f)
                    .onKeyEvent {
                        if (it.key == Key.Enter && it.type == KeyEventType.KeyUp && hasContent && !busy) {
                            onSend(); true
                        } else false
                    },
                placeholder = { Text("Message...") },
                maxLines = 6,
                enabled = !busy,
                trailingIcon = {
                    Column(verticalArrangement = Arrangement.Top) {
                        Row {
                            if (supportsImages) {
                                AttachMenu(
                                    enabled = !busy,
                                    onGallery = onAttachGallery,
                                    onCamera = onAttachCamera,
                                    onScreenshot = onScreenshot,
                                )
                            }
                            if (sending) {
                                CircularProgressIndicator(modifier = Modifier.size(24.dp))
                            } else {
                                IconButton(onClick = onSend, enabled = hasContent && !busy, modifier = Modifier.testTag("send-input")) {
                                    Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Send")
                                }
                            }
                        }
                    }
                },
            )
            if (pendingAction == "clear-context" || pendingAction == "compact" || pendingAction == "sync" ||
                pendingAction == "stop" || pendingAction == "purge" || pendingAction == "revive" || pendingAction == "fork"
            ) {
                CircularProgressIndicator(modifier = Modifier.size(24.dp).padding(4.dp))
            } else {
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
                            text = { Text(syncLabel) },
                            leadingIcon = {
                                when (forge) {
                                    "github" -> Icon(painterResource(R.drawable.ic_github), contentDescription = null)
                                    "gitlab" -> Icon(painterResource(R.drawable.ic_gitlab), contentDescription = null)
                                    else -> Icon(Icons.Default.Sync, contentDescription = null)
                                }
                            },
                            enabled = taskState != "purging",
                            onClick = { contextMenuExpanded = false; onSync() },
                        )
                        if (taskBaseBranch.isNotBlank()) {
                            DropdownMenuItem(
                                text = { Text("Push to $taskBaseBranch") },
                                leadingIcon = { Icon(Icons.Default.Sync, contentDescription = null) },
                                enabled = taskState != "purging",
                                onClick = { contextMenuExpanded = false; onSyncToBaseBranch() },
                            )
                        }
                        if (isActive) {
                            DropdownMenuItem(
                                text = { Text("Stop", color = MaterialTheme.colorScheme.error) },
                                leadingIcon = {
                                    Icon(Icons.Default.StopCircle, null, tint = MaterialTheme.colorScheme.error)
                                },
                                onClick = {
                                    contextMenuExpanded = false
                                    if (taskState == "running") showStopConfirm = true else onStop()
                                },
                                modifier = Modifier.testTag("stop-task"),
                            )
                        }
                        if (isStopped) {
                            DropdownMenuItem(
                                text = { Text("Revive") },
                                leadingIcon = { Icon(Icons.Default.RestartAlt, contentDescription = null) },
                                onClick = { contextMenuExpanded = false; onRevive() },
                                modifier = Modifier.testTag("revive-task"),
                            )
                        }
                        if (isActive || isStopped) {
                            DropdownMenuItem(
                                text = { Text("Purge", color = MaterialTheme.colorScheme.error) },
                                leadingIcon = {
                                    Icon(Icons.Default.Delete, null, tint = MaterialTheme.colorScheme.error)
                                },
                                onClick = { contextMenuExpanded = false; showPurgeConfirm = true },
                                modifier = Modifier.testTag("purge-task"),
                            )
                        }
                        DropdownMenuItem(
                            text = { Text("Clear context") },
                            leadingIcon = { Icon(Icons.Default.Block, contentDescription = null) },
                            enabled = false,
                            onClick = { contextMenuExpanded = false; onClearContext() },
                        )
                        if (supportsCompact) {
                            DropdownMenuItem(
                                text = { Text("Compact context") },
                                leadingIcon = { Icon(Icons.Default.Compress, contentDescription = null) },
                                enabled = isWaiting,
                                onClick = { contextMenuExpanded = false; onCompact() },
                            )
                        }
                        if (taskRepo.isNotEmpty()) {
                            DropdownMenuItem(
                                text = { Text("Fork") },
                                leadingIcon = { Icon(Icons.Outlined.ForkRight, contentDescription = null) },
                                onClick = { contextMenuExpanded = false; showForkDialog = true },
                            )
                        }
                    }
                }
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
        if (showForkDialog) {
            val forkFocus = remember { FocusRequester() }
            var forkSelectedHarness by remember { mutableStateOf(taskHarness) }
            var forkSelectedModel by remember { mutableStateOf(taskModel) }
            var forkSelectedEffort by remember { mutableStateOf(taskEffort) }
            var forkExtraRepos by remember { mutableStateOf(emptyList<RepoEntry>()) }
            var forkTailscale by remember { mutableStateOf(taskTailscale) }
            var forkUsb by remember { mutableStateOf(taskUsb) }
            var forkDisplay by remember { mutableStateOf(taskDisplay) }
            var forkSudo by remember { mutableStateOf(taskSudo) }
            var forkGitHubToken by remember { mutableStateOf(taskGitHubToken) }
            val forkExtraPaths = forkExtraRepos.map { it.path }.toSet()
            AlertDialog(
                onDismissRequest = { showForkDialog = false },
                title = { Text("Fork task") },
                text = {
                    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedTextField(
                            value = forkPrompt,
                            onValueChange = { forkPrompt = it },
                            label = { Text("Prompt for forked task") },
                            modifier = Modifier.fillMaxWidth().focusRequester(forkFocus),
                        )
                        if (forkAvailableRecent.isNotEmpty() || forkAvailableRest.isNotEmpty() ||
                            forkExtraRepos.isNotEmpty()
                        ) {
                            RepoChipStrip(
                                selectedRepos = forkExtraRepos,
                                repos = allRepos,
                                availableRecent = forkAvailableRecent.filter { it.path !in forkExtraPaths },
                                availableRest = forkAvailableRest.filter { it.path !in forkExtraPaths },
                                editingBranches = emptyList(),
                                enabled = true,
                                onAdd = { path ->
                                    forkExtraRepos = forkExtraRepos + RepoEntry(path, "")
                                },
                                onRemove = { path ->
                                    forkExtraRepos = forkExtraRepos.filter { it.path != path }
                                },
                                onSetBranch = { path, branch ->
                                    forkExtraRepos = forkExtraRepos.map {
                                        if (it.path == path) it.copy(branch = branch) else it
                                    }
                                },
                                onLoadBranches = {},
                            )
                        }
                        if (harnesses.size > 1) {
                            ForkDropdown(
                                label = "Harness",
                                selected = forkSelectedHarness,
                                options = harnesses.map { it.name },
                                onSelect = { h ->
                                    forkSelectedHarness = h
                                    val models = harnesses.firstOrNull { it.name == h }?.models.orEmpty()
                                    if (forkSelectedModel !in models) forkSelectedModel = ""
                                },
                            )
                        }
                        val models =
                            harnesses.firstOrNull { it.name == forkSelectedHarness }?.models.orEmpty()
                        if (models.isNotEmpty()) {
                            ForkDropdown(
                                label = "Model",
                                selected = forkSelectedModel.ifBlank { "Default" },
                                options = listOf("Default") + models,
                                onSelect = { m ->
                                    forkSelectedModel = if (m == "Default") "" else m
                                },
                            )
                        }
                        val effortOpts = effortOptions(forkSelectedHarness)
                        if (effortOpts.isNotEmpty()) {
                            ForkDropdown(
                                label = "Effort",
                                selected = forkSelectedEffort.ifBlank { "Default" },
                                options = listOf("Default") + effortOpts,
                                onSelect = { e ->
                                    forkSelectedEffort = if (e == "Default") "" else e
                                },
                            )
                        }
                        Row(
                            horizontalArrangement = Arrangement.spacedBy(4.dp),
                        ) {
                            FeatureToggle(
                                checked = forkTailscale,
                                onCheckedChange = { forkTailscale = it },
                                iconRes = com.fghbuild.caic.R.drawable.ic_tailscale,
                                contentDescription = "Enable Tailscale networking",
                                enabled = tailscaleAvailable,
                                unavailableDescription = "Tailscale is not available on this server",
                            )
                            FeatureToggle(
                                checked = forkUsb,
                                onCheckedChange = { forkUsb = it },
                                iconRes = com.fghbuild.caic.R.drawable.ic_usb,
                                contentDescription = "Enable USB passthrough",
                                enabled = usbAvailable,
                                unavailableDescription = "USB passthrough is not available on this server",
                            )
                            FeatureToggle(
                                checked = forkDisplay,
                                onCheckedChange = { forkDisplay = it },
                                iconRes = com.fghbuild.caic.R.drawable.ic_display,
                                contentDescription = "Enable virtual display",
                                enabled = displayAvailable,
                                unavailableDescription = "Virtual display is not available on this server",
                            )
                            FeatureToggle(
                                checked = forkSudo,
                                onCheckedChange = { forkSudo = it },
                                iconRes = com.fghbuild.caic.R.drawable.ic_sudo,
                                contentDescription = "Enable root access via sudo",
                                enabled = sudoAvailable,
                                unavailableDescription = "Root access (sudo) is not available on this server",
                            )
                            FeatureToggle(
                                checked = forkGitHubToken,
                                onCheckedChange = { forkGitHubToken = it },
                                iconRes = com.fghbuild.caic.R.drawable.ic_github,
                                contentDescription = "Enable GitHub token",
                                enabled = gitHubTokenAvailable,
                                unavailableDescription = "GitHub token is not available on this server",
                            )
                        }
                        LaunchedEffect(Unit) { forkFocus.requestFocus() }
                    }
                },
                confirmButton = {
                    TextButton(
                        onClick = {
                            showForkDialog = false
                            val h = forkSelectedHarness.takeIf { it != taskHarness }
                            val m = forkSelectedModel.takeIf { it != taskModel }
                            val extras = forkExtraRepos.takeIf { it.isNotEmpty() }?.map {
                                RepoSpec(name = it.path, baseBranch = it.branch.ifBlank { null })
                            }
                            val e = forkSelectedEffort.takeIf { it != taskEffort }
                            onFork(
                                forkPrompt.trim(), h, m, e, extras,
                                forkTailscale, forkUsb, forkDisplay, forkSudo, forkGitHubToken,
                            )
                        },
                        enabled = forkPrompt.isNotBlank(),
                    ) { Text("Fork") }
                },
                dismissButton = {
                    TextButton(onClick = { showForkDialog = false }) { Text("Cancel") }
                },
            )
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun Tip(text: String, content: @Composable () -> Unit) {
    TooltipBox(
        positionProvider = TooltipDefaults.rememberTooltipPositionProvider(TooltipAnchorPosition.Above),
        tooltip = { PlainTooltip { Text(text) } },
        state = rememberTooltipState(),
        content = content,
    )
}

@Composable
private fun ForkDropdown(label: String, selected: String, options: List<String>, onSelect: (String) -> Unit) {
    var expanded by remember { mutableStateOf(false) }
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(label, style = MaterialTheme.typography.bodyMedium)
        Box {
            Row(
                modifier = Modifier
                    .clip(MaterialTheme.shapes.small)
                    .background(MaterialTheme.colorScheme.surfaceVariant)
                    .clickable { expanded = true }
                    .padding(start = 10.dp, end = 6.dp, top = 6.dp, bottom = 6.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(2.dp),
            ) {
                Text(selected, style = MaterialTheme.typography.bodyMedium)
                Icon(Icons.Default.ArrowDropDown, contentDescription = null, modifier = Modifier.size(16.dp))
            }
            DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
                options.forEach { option ->
                    DropdownMenuItem(
                        text = {
                            Text(
                                option,
                                fontWeight = if (option == selected) FontWeight.Bold else FontWeight.Normal,
                            )
                        },
                        onClick = { onSelect(option); expanded = false },
                    )
                }
            }
        }
    }
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
                .clip(RoundedCornerShape(4.dp))
                .testTag("attached-image"),
            contentScale = ContentScale.Crop,
        )
        Icon(
            Icons.Default.Close,
            contentDescription = "Remove",
            modifier = Modifier
                .size(16.dp)
                .clickable(onClick = onRemove)
                .testTag("remove-image"),
        )
    }
}

@Composable
private fun FeatureToggle(
    checked: Boolean,
    onCheckedChange: (Boolean) -> Unit,
    iconRes: Int,
    contentDescription: String,
    enabled: Boolean = true,
    unavailableDescription: String? = null,
) {
    val desc = if (!enabled && unavailableDescription != null) unavailableDescription else contentDescription
    Row(
        modifier = Modifier
            .clip(MaterialTheme.shapes.small)
            .background(
                if (!enabled) MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f)
                else if (checked) MaterialTheme.colorScheme.primary
                else MaterialTheme.colorScheme.surfaceVariant
            )
            .then(if (enabled) Modifier.clickable { onCheckedChange(!checked) } else Modifier)
            .padding(start = 8.dp, end = 8.dp, top = 6.dp, bottom = 6.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(
            painter = painterResource(id = iconRes),
            contentDescription = desc,
            modifier = Modifier.size(18.dp),
            tint = if (!enabled) MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.3f)
            else if (checked) MaterialTheme.colorScheme.onPrimary
            else MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}
