// Task list screen with creation form, usage badges, and task navigation.
package com.fghbuild.caic.ui.tasklist

import android.app.Activity
import android.media.projection.MediaProjectionManager
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
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
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.DropdownMenu
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
import androidx.compose.material3.TextButton
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.rememberTooltipState
import android.graphics.BitmapFactory
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
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
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.ImageData
import com.caic.sdk.v1.Repo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import com.fghbuild.caic.ui.common.AttachMenu
import com.fghbuild.caic.ui.login.LoginScreen
import com.fghbuild.caic.util.ScreenshotService
import com.fghbuild.caic.util.bitmapToImageData
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.caic.util.createCameraPhotoUri
import com.fghbuild.caic.util.imageDataToBitmap
import com.fghbuild.caic.ui.common.rememberNotificationPermissionRequester
import com.fghbuild.caic.ui.theme.activeStates
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
                title = { Text("caic", style = MaterialTheme.typography.titleMedium) },
                actions = {
                    state.usage?.let { UsageBadges(it) }
                    if (state.serverConfigured) {
                        ConnectionDot(connected = state.connected)
                    }
                    val user = state.user
                    if (user != null) {
                        UserAvatar(
                            username = user.username,
                            avatarURL = user.avatarURL,
                            onSettings = onNavigateToSettings,
                            onLogout = { viewModel.logout() },
                        )
                    } else {
                        TooltipBox(
                            positionProvider = TooltipDefaults.rememberPlainTooltipPositionProvider(),
                            tooltip = { PlainTooltip { Text("Settings") } },
                            state = rememberTooltipState(),
                        ) {
                            IconButton(onClick = onNavigateToSettings) {
                                Icon(Icons.Default.Settings, contentDescription = "Settings")
                            }
                        }
                    }
                },
            )
        },
    ) { padding ->
        when {
            !state.serverConfigured -> NotConfiguredContent(padding, onNavigateToSettings)
            state.authRequired -> LoginScreen(serverURL = state.serverURL, providers = state.authProviders)
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
private fun UserAvatar(
    username: String,
    avatarURL: String?,
    onSettings: () -> Unit,
    onLogout: () -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    var avatarBitmap by remember { mutableStateOf<android.graphics.Bitmap?>(null) }
    if (avatarURL != null) {
        LaunchedEffect(avatarURL) {
            avatarBitmap = withContext(Dispatchers.IO) {
                try {
                    val url = java.net.URL(avatarURL)
                    val stream = url.openStream()
                    BitmapFactory.decodeStream(stream).also { stream.close() }
                } catch (_: Exception) {
                    null
                }
            }
        }
    }
    Box {
        IconButton(onClick = { expanded = true }) {
            val bmp = avatarBitmap
            if (bmp != null) {
                Image(
                    bitmap = bmp.asImageBitmap(),
                    contentDescription = username,
                    modifier = Modifier
                        .size(32.dp)
                        .clip(CircleShape),
                    contentScale = ContentScale.Crop,
                )
            } else {
                Box(
                    modifier = Modifier
                        .size(32.dp)
                        .clip(CircleShape)
                        .background(MaterialTheme.colorScheme.primary),
                    contentAlignment = Alignment.Center,
                ) {
                    Text(
                        text = username.take(2).uppercase(),
                        color = MaterialTheme.colorScheme.onPrimary,
                        style = MaterialTheme.typography.labelMedium,
                        fontWeight = FontWeight.SemiBold,
                    )
                }
            }
        }
        DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            DropdownMenuItem(
                text = { Text(username, fontWeight = FontWeight.SemiBold) },
                onClick = {},
                enabled = false,
            )
            DropdownMenuItem(
                text = { Text("Settings") },
                onClick = { expanded = false; onSettings() },
                leadingIcon = { Icon(Icons.Default.Settings, contentDescription = null) },
            )
            DropdownMenuItem(
                text = { Text("Sign out") },
                onClick = { expanded = false; onLogout() },
            )
        }
    }
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
        val activeTasks = state.tasks.filter { it.state in activeStates }
        val terminalTasks = state.tasks.filter { it.state !in activeStates }
        val repoGroups = activeTasks.groupBy { it.repos?.firstOrNull()?.name ?: "" }
        for ((repo, tasksInRepo) in repoGroups) {
            item(key = "repo_header_$repo") {
                val uriHandler = LocalUriHandler.current
                val repoMeta = state.repos.find { it.path == repo }
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(6.dp),
                    modifier = Modifier.padding(top = 4.dp),
                ) {
                    Text(
                        text = repo,
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        fontWeight = FontWeight.SemiBold,
                    )
                    val ciStatus = repoMeta?.defaultBranchCIStatus
                    if (ciStatus != null) {
                        val dotColor = when (ciStatus) {
                            "success" -> Color(0xFF28A745)
                            "failure" -> Color(0xFFDC3545)
                            else -> Color(0xFFFFC107)
                        }
                        val ciUrl = ciDotUrl(repoMeta)
                        Box(
                            modifier = Modifier
                                .size(8.dp)
                                .clip(CircleShape)
                                .background(dotColor)
                                .then(
                                    if (ciUrl != null) Modifier.clickable { uriHandler.openUri(ciUrl) } else Modifier
                                ),
                        )
                    }
                }
            }
            items(items = tasksInRepo, key = { it.id }) { task ->
                TaskCard(task = task, onClick = { onNavigateToTask(task.id) })
            }
        }
        items(items = terminalTasks, key = { it.id }) { task ->
            TaskCard(task = task, onClick = { onNavigateToTask(task.id) })
        }
    }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TaskCreationForm(state: TaskListState, viewModel: TaskListViewModel) {
    val requestNotificationPermission = rememberNotificationPermissionRequester()
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
    val mpm = remember { context.getSystemService(MediaProjectionManager::class.java) }
    val screenshotLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        if (result.resultCode == Activity.RESULT_OK && result.data != null) {
            ScreenshotService.start(context, result.resultCode, result.data!!) { bitmap ->
                val img = bitmapToImageData(bitmap)
                bitmap.recycle()
                viewModel.addImages(listOf(img))
            }
        }
    }
    val hasContent = state.prompt.isNotBlank() || state.pendingImages.isNotEmpty()
    var showCloneDialog by remember { mutableStateOf(false) }

    if (showCloneDialog) {
        CloneRepoDialog(
            cloning = state.cloning,
            onDismiss = { showCloneDialog = false },
            onClone = { url, path ->
                viewModel.cloneRepo(url, path)
                showCloneDialog = false
            },
        )
    }

    val models = state.harnesses.firstOrNull { it.name == state.selectedHarness }?.models.orEmpty()

    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        RepoChipStrip(
            state = state,
            viewModel = viewModel,
            onShowCloneDialog = { showCloneDialog = true },
        )

        // Row 2: Harness + Model (side by side when both present)
        if (state.harnesses.size > 1 || models.isNotEmpty()) {
            Row(
                horizontalArrangement = Arrangement.spacedBy(4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                if (state.harnesses.size > 1) {
                    Box(modifier = Modifier.weight(1f)) {
                        DropdownField(
                            label = "Harness",
                            selected = state.selectedHarness,
                            options = state.harnesses.map { it.name },
                            onSelect = viewModel::selectHarness,
                        )
                    }
                }
                if (models.isNotEmpty()) {
                    Box(modifier = Modifier.weight(1f)) {
                        DropdownField(
                            label = "Model",
                            selected = state.selectedModel.ifBlank { models.first() },
                            options = models,
                            onSelect = viewModel::selectModel,
                        )
                    }
                }
            }
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
                        hasContent && !state.submitting
                    ) {
                        requestNotificationPermission(); viewModel.createTask(); true
                    } else false
                },
            maxLines = 6,
            enabled = !state.submitting,
            trailingIcon = {
                if (state.submitting) {
                    CircularProgressIndicator(modifier = Modifier.size(24.dp))
                } else {
                    IconButton(
                        onClick = { requestNotificationPermission(); viewModel.createTask() },
                        enabled = hasContent,
                    ) {
                        Icon(Icons.AutoMirrored.Filled.Send, contentDescription = "Create task")
                    }
                }
            },
        )
        if (state.supportsImages) {
            AttachMenu(
                enabled = !state.submitting,
                onCamera = {
                    val uri = createCameraPhotoUri(context)
                    cameraUri = uri
                    cameraLauncher.launch(uri)
                },
                onScreenshot = { screenshotLauncher.launch(mpm.createScreenCaptureIntent()) },
                onGallery = {
                    photoPicker.launch(
                        PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)
                    )
                },
            )
        }
    }
}

@OptIn(ExperimentalLayoutApi::class, ExperimentalMaterial3Api::class)
@Composable
private fun RepoChipStrip(
    state: TaskListState,
    viewModel: TaskListViewModel,
    onShowCloneDialog: () -> Unit,
) {
    FlowRow(
        horizontalArrangement = Arrangement.spacedBy(4.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        state.selectedRepos.forEach { entry ->
            RepoChip(
                entry = entry,
                baseBranch = state.repos.find { it.path == entry.path }?.baseBranch ?: "",
                editingBranches = state.editingBranches,
                enabled = !state.submitting,
                onRemove = { viewModel.removeRepo(entry.path) },
                onSetBranch = { branch -> viewModel.setBranch(entry.path, branch) },
                onLoadBranches = { viewModel.loadBranchesForPath(entry.path) },
            )
        }
        if (state.availableRecent.isNotEmpty() || state.availableRest.isNotEmpty()) {
            AddRepoChip(
                availableRecent = state.availableRecent,
                availableRest = state.availableRest,
                onAdd = viewModel::addRepo,
            )
        }
        TooltipBox(
            positionProvider = TooltipDefaults.rememberPlainTooltipPositionProvider(),
            tooltip = { PlainTooltip { Text("Clone repository") } },
            state = rememberTooltipState(),
        ) {
            IconButton(
                onClick = onShowCloneDialog,
                enabled = !state.cloning && !state.submitting,
                modifier = Modifier.size(32.dp),
            ) {
                if (state.cloning) {
                    CircularProgressIndicator(modifier = Modifier.size(18.dp))
                } else {
                    Icon(Icons.Default.ContentCopy, contentDescription = "Clone repository", modifier = Modifier.size(18.dp))
                }
            }
        }
    }
}

@Composable
private fun RepoChip(
    entry: RepoEntry,
    baseBranch: String,
    editingBranches: List<String>,
    enabled: Boolean,
    onRemove: () -> Unit,
    onSetBranch: (String) -> Unit,
    onLoadBranches: () -> Unit,
) {
    var branchOpen by remember { mutableStateOf(false) }
    val shortName = entry.path.substringAfterLast("/")
    Box {
        Row(
            modifier = Modifier
                .clip(MaterialTheme.shapes.small)
                .background(MaterialTheme.colorScheme.surfaceVariant),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box(
                modifier = Modifier
                    .clickable(enabled = enabled) {
                        branchOpen = !branchOpen
                        if (branchOpen) onLoadBranches()
                    }
                    .padding(start = 10.dp, end = 6.dp, top = 6.dp, bottom = 6.dp),
            ) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(shortName, style = MaterialTheme.typography.bodyMedium)
                    if (entry.branch.isNotEmpty()) {
                        Text(
                            " · ${entry.branch}",
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
            Box(
                modifier = Modifier
                    .clickable(enabled = enabled, onClick = onRemove)
                    .padding(start = 4.dp, end = 8.dp, top = 6.dp, bottom = 6.dp),
            ) {
                Text("×", style = MaterialTheme.typography.bodyMedium)
            }
        }
        DropdownMenu(expanded = branchOpen, onDismissRequest = { branchOpen = false }) {
            DropdownMenuItem(
                text = {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text("Default ", color = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text("($baseBranch)")
                    }
                },
                onClick = { onSetBranch(""); branchOpen = false },
            )
            editingBranches.forEach { branch ->
                DropdownMenuItem(
                    text = {
                        Text(
                            branch,
                            fontWeight = if (branch == entry.branch) FontWeight.Bold else FontWeight.Normal,
                        )
                    },
                    onClick = { onSetBranch(branch); branchOpen = false },
                )
            }
        }
    }
}

@Composable
private fun AddRepoChip(
    availableRecent: List<Repo>,
    availableRest: List<Repo>,
    onAdd: (String) -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    Box {
        Box(
            modifier = Modifier
                .clip(MaterialTheme.shapes.small)
                .background(MaterialTheme.colorScheme.surfaceVariant)
                .clickable { expanded = true }
                .padding(horizontal = 10.dp, vertical = 6.dp),
        ) {
            Text("+", style = MaterialTheme.typography.bodyMedium, fontWeight = FontWeight.Bold)
        }
        DropdownMenu(expanded = expanded, onDismissRequest = { expanded = false }) {
            if (availableRecent.isNotEmpty()) {
                Text(
                    text = "Recent",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                )
                availableRecent.sortedBy { it.path }.forEach { repo ->
                    DropdownMenuItem(
                        text = { Text(repo.path, style = MaterialTheme.typography.bodyMedium) },
                        onClick = { onAdd(repo.path); expanded = false },
                    )
                }
            }
            if (availableRecent.isNotEmpty() && availableRest.isNotEmpty()) {
                HorizontalDivider()
            }
            if (availableRest.isNotEmpty()) {
                if (availableRecent.isNotEmpty()) {
                    Text(
                        text = "All repositories",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                    )
                }
                availableRest.forEach { repo ->
                    DropdownMenuItem(
                        text = { Text(repo.path, style = MaterialTheme.typography.bodyMedium) },
                        onClick = { onAdd(repo.path); expanded = false },
                    )
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
    itemLabel: (String) -> String = { it },
) {
    var expanded by remember { mutableStateOf(false) }
    ExposedDropdownMenuBox(expanded = expanded, onExpandedChange = { expanded = it }) {
        OutlinedTextField(
            value = itemLabel(selected),
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
                    text = { Text(itemLabel(option)) },
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

@Composable
private fun CloneRepoDialog(
    cloning: Boolean,
    onDismiss: () -> Unit,
    onClone: (url: String, path: String?) -> Unit,
) {
    var url by remember { mutableStateOf("") }
    var path by remember { mutableStateOf("") }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Clone Repository") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(
                    value = url,
                    onValueChange = { url = it },
                    label = { Text("Repository URL") },
                    placeholder = { Text("https://github.com/org/repo") },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                    enabled = !cloning,
                )
                OutlinedTextField(
                    value = path,
                    onValueChange = { path = it },
                    label = { Text("Local path (optional)") },
                    modifier = Modifier.fillMaxWidth(),
                    singleLine = true,
                    enabled = !cloning,
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = { onClone(url.trim(), path.trim().ifBlank { null }) },
                enabled = url.isNotBlank() && !cloning,
            ) {
                Text("Clone")
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss, enabled = !cloning) {
                Text("Cancel")
            }
        },
    )
}

private val nonPassingConclusions = setOf("failure", "cancelled", "timed_out", "action_required", "stale")

private fun ciDotUrl(repo: Repo): String? {
    val isGitLab = repo.remoteURL?.contains("gitlab.com") == true
    if (repo.defaultBranchCIStatus == "failure") {
        val failed = repo.defaultBranchChecks?.find { it.conclusion in nonPassingConclusions }
        if (failed != null) {
            return if (isGitLab) {
                "https://gitlab.com/${failed.owner}/${failed.repo}/-/jobs/${failed.jobID}"
            } else {
                "https://github.com/${failed.owner}/${failed.repo}/actions/runs/${failed.runID}/job/${failed.jobID}"
            }
        }
    }
    return repo.remoteURL?.let { url -> if (isGitLab) "$url/-/pipelines" else "$url/actions" }
}
