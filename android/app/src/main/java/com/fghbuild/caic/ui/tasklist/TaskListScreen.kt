// Task list screen with creation form, usage badges, and task navigation.
package com.fghbuild.caic.ui.tasklist

import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.material.icons.filled.CameraAlt
import androidx.compose.material.icons.filled.PhotoLibrary
import androidx.compose.material3.DropdownMenu
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.HorizontalDivider
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.AttachFile
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExposedDropdownMenuBox
import androidx.compose.material3.ExposedDropdownMenuDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.MenuAnchorType
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.PlainTooltip
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ImageData
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.caic.util.createCameraPhotoUri
import com.fghbuild.caic.util.imageDataToBitmap
import com.fghbuild.caic.util.uriToImageData

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TaskListScreen(
    viewModel: TaskListViewModel = hiltViewModel(),
    onNavigateToSettings: () -> Unit = {},
    onNavigateToTask: (String) -> Unit = {},
    modifier: Modifier = Modifier,
) {
    val state by viewModel.state.collectAsStateWithLifecycle()

    Scaffold(
        modifier = modifier,
        topBar = {
            TopAppBar(
                title = { Text("caic") },
                actions = {
                    state.usage?.let { UsageBadges(it) }
                    if (state.serverConfigured) {
                        ConnectionDot(connected = state.connected)
                    }
                    TooltipBox(
                        positionProvider = TooltipDefaults.rememberPlainTooltipPositionProvider(),
                        tooltip = { PlainTooltip { Text("Settings") } },
                        state = rememberTooltipState(),
                    ) {
                        IconButton(onClick = onNavigateToSettings) {
                            Icon(Icons.Default.Settings, contentDescription = "Settings")
                        }
                    }
                },
            )
        },
    ) { padding ->
        when {
            !state.serverConfigured -> NotConfiguredContent(padding, onNavigateToSettings)
            else -> MainContent(state, padding, onNavigateToTask, viewModel)
        }
    }
}

@Composable
private fun ConnectionDot(connected: Boolean) {
    val color = if (connected) Color(0xFF4CAF50) else Color(0xFFF44336)
    Box(
        modifier = Modifier
            .padding(horizontal = 8.dp)
            .size(10.dp)
            .clip(CircleShape)
            .background(color)
    )
}

@Composable
private fun NotConfiguredContent(padding: PaddingValues, onNavigateToSettings: () -> Unit) {
    Column(
        modifier = Modifier
            .fillMaxSize()
            .padding(padding),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.Center,
    ) {
        Text("Configure server URL in Settings", style = MaterialTheme.typography.bodyLarge)
        Button(
            onClick = onNavigateToSettings,
            modifier = Modifier.padding(top = 16.dp),
        ) {
            Text("Open Settings")
        }
    }
}

@Composable
private fun MainContent(
    state: TaskListState,
    padding: PaddingValues,
    onNavigateToTask: (String) -> Unit,
    viewModel: TaskListViewModel,
) {
    Box(modifier = Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
    LazyColumn(
        modifier = Modifier
            .widthIn(max = 840.dp)
            .fillMaxWidth(),
        contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        item(key = "__creation_form__") {
            TaskCreationForm(state = state, viewModel = viewModel)
        }
        if (state.error != null) {
            item(key = "__error__") {
                Text(
                    text = state.error,
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.padding(vertical = 4.dp),
                )
            }
        }
        if (state.tasks.isEmpty()) {
            item(key = "__empty__") {
                Box(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(vertical = 32.dp),
                    contentAlignment = Alignment.Center,
                ) {
                    Text("No active tasks", style = MaterialTheme.typography.bodyLarge)
                }
            }
        }
        items(items = state.tasks, key = { it.id }) { task ->
            TaskCard(task = task, onClick = { onNavigateToTask(task.id) })
        }
    }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TaskCreationForm(state: TaskListState, viewModel: TaskListViewModel) {
    val context = LocalContext.current
    val contentResolver = context.contentResolver
    val photoPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.PickMultipleVisualMedia(),
    ) { uris: List<Uri> ->
        val images = uris.mapNotNull { uriToImageData(contentResolver, it) }
        if (images.isNotEmpty()) viewModel.addImages(images)
    }
    var cameraUri by remember { mutableStateOf<Uri?>(null) }
    val cameraLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.TakePicture(),
    ) { success: Boolean ->
        val uri = cameraUri
        if (success && uri != null) {
            val img = uriToImageData(contentResolver, uri)
            if (img != null) viewModel.addImages(listOf(img))
        }
        cameraUri = null
    }
    val hasContent = state.prompt.isNotBlank() || state.pendingImages.isNotEmpty()

    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        if (state.repos.isNotEmpty()) {
            DropdownField(
                label = "Repository",
                selected = state.selectedRepo,
                options = state.repos.map { it.path },
                onSelect = viewModel::selectRepo,
                dividerAfter = state.recentRepoCount,
            )
        }

        val defaultBranch = state.repos.find { it.path == state.selectedRepo }?.baseBranch ?: ""
        OutlinedTextField(
            value = state.baseBranch,
            onValueChange = viewModel::updateBaseBranch,
            label = { Text("Base branch") },
            placeholder = { Text(defaultBranch.ifBlank { "main" }) },
            modifier = Modifier.fillMaxWidth(),
            singleLine = true,
            enabled = !state.submitting,
        )

        if (state.harnesses.size > 1) {
            DropdownField(
                label = "Harness",
                selected = state.selectedHarness,
                options = state.harnesses.map { it.name },
                onSelect = viewModel::selectHarness,
            )
        }

        val models = state.harnesses.firstOrNull { it.name == state.selectedHarness }?.models.orEmpty()
        if (models.isNotEmpty()) {
            DropdownField(
                label = "Model",
                selected = state.selectedModel.ifBlank { models.first() },
                options = models,
                onSelect = viewModel::selectModel,
            )
        }

        if (state.pendingImages.isNotEmpty()) {
            LazyRow(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                itemsIndexed(state.pendingImages) { index, img ->
                    FormImageThumbnail(img = img, onRemove = { viewModel.removeImage(index) })
                }
            }
        }

        OutlinedTextField(
            value = state.prompt,
            onValueChange = viewModel::updatePrompt,
            label = { Text("Prompt") },
            modifier = Modifier
                .fillMaxWidth()
                .onKeyEvent {
                    if (it.key == Key.Enter && it.type == KeyEventType.KeyUp &&
                        hasContent && state.selectedRepo.isNotBlank() && !state.submitting
                    ) {
                        viewModel.createTask(); true
                    } else false
                },
            maxLines = 6,
            enabled = !state.submitting,
            trailingIcon = {
                if (state.submitting) {
                    CircularProgressIndicator(modifier = Modifier.size(24.dp))
                } else {
                    IconButton(
                        onClick = viewModel::createTask,
                        enabled = hasContent && state.selectedRepo.isNotBlank(),
                    ) {
                        Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Create task")
                    }
                }
            },
        )
        if (state.supportsImages) {
            Row {
                var attachExpanded by remember { mutableStateOf(false) }
                Box {
                    IconButton(
                        onClick = { attachExpanded = true },
                        enabled = !state.submitting,
                    ) {
                        Icon(Icons.Default.AttachFile, contentDescription = "Attach image")
                    }
                    DropdownMenu(
                        expanded = attachExpanded,
                        onDismissRequest = { attachExpanded = false },
                    ) {
                        DropdownMenuItem(
                            text = { Text("Take photo") },
                            onClick = {
                                attachExpanded = false
                                val uri = createCameraPhotoUri(context)
                                cameraUri = uri
                                cameraLauncher.launch(uri)
                            },
                            leadingIcon = { Icon(Icons.Default.CameraAlt, contentDescription = null) },
                        )
                        DropdownMenuItem(
                            text = { Text("Choose from gallery") },
                            onClick = {
                                attachExpanded = false
                                photoPicker.launch(
                                    PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)
                                )
                            },
                            leadingIcon = { Icon(Icons.Default.PhotoLibrary, contentDescription = null) },
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun FormImageThumbnail(img: ImageData, onRemove: () -> Unit) {
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

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DropdownField(
    label: String,
    selected: String,
    options: List<String>,
    onSelect: (String) -> Unit,
    dividerAfter: Int = 0,
) {
    var expanded by remember { mutableStateOf(false) }
    ExposedDropdownMenuBox(expanded = expanded, onExpandedChange = { expanded = it }) {
        OutlinedTextField(
            value = selected,
            onValueChange = {},
            readOnly = true,
            label = { Text(label) },
            trailingIcon = { ExposedDropdownMenuDefaults.TrailingIcon(expanded = expanded) },
            modifier = Modifier
                .fillMaxWidth()
                .menuAnchor(MenuAnchorType.PrimaryNotEditable),
        )
        ExposedDropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            options.forEachIndexed { index, option ->
                DropdownMenuItem(
                    text = { Text(option) },
                    onClick = {
                        onSelect(option)
                        expanded = false
                    },
                )
                if (index == dividerAfter - 1 && dividerAfter in 1..<options.size) {
                    HorizontalDivider()
                }
            }
        }
    }
}
