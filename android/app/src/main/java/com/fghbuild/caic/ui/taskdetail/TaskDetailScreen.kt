// Full-screen task detail view with live SSE message stream, grouping, and actions.
package com.fghbuild.caic.ui.taskdetail

import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.PlainTooltip
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.foundation.clickable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.caic.ui.theme.stateColor
import com.fghbuild.caic.util.createCameraPhotoUri
import com.fghbuild.caic.util.GroupKind
import com.fghbuild.caic.util.MessageGroup
import com.fghbuild.caic.util.ToolCall
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.turnSummary
import com.fghbuild.caic.util.uriToImageData

private val PlanBadgeBg = Color(0xFFEDE9FE)
private val PlanBadgeFg = Color(0xFF7C3AED)
private val TerminalStates = setOf("terminated", "failed")

// Flat list items for the LazyColumn. Expansion state is owned here so collapsed items are
// never composed — true laziness without AnimatedVisibility wrappers.
//
// Key namespaces (all position-based; timestamps are not unique within a millisecond):
//   Long  turnIndex              — Elided past turn
//   "th:N"                       — header row for an expanded past turn
//   "g:j"  / "e{N}g{j}"         — group item (live / expanded-past)
//   "gh:j" / "e{N}gh{j}"        — tool-group header item
//   "g:j:k" / "e{N}g{j}:{k}"   — individual ToolCallItem within expanded tool group
//
// Long vs String keys in the same list never collide even if numeric values match.
private sealed interface MsgItem {
    val key: Any
    /** Collapsed past turn — one summary row per turn. */
    data class Elided(val turn: Turn, override val key: Long) : MsgItem
    /** Header shown at the top of an expanded past turn; tap to collapse. */
    data class ExpandedTurnHeader(val turn: Turn, val turnKey: Long, override val key: String) : MsgItem
    /** Non-tool group (text, ask, result, user-input, other). */
    data class Group(
        val group: MessageGroup,
        val isLiveTurn: Boolean,
        override val key: String,
    ) : MsgItem
    /** Summary header for a multi-call tool group; tap to expand/collapse its call items. */
    data class ToolGroupHeader(
        val toolCalls: List<ToolCall>,
        val groupKey: String,
        override val key: String,
    ) : MsgItem
    /** One tool call within an expanded multi-call tool group. */
    data class ToolCallItem(val call: ToolCall, override val key: String) : MsgItem
}

/**
 * Builds the flat item list.
 *
 * @param expandedTurnKeys  Turn indices (Long) whose groups should be shown as individual items.
 * @param expandedToolGroups  Tool-group keys whose call items should be shown.
 */
private fun buildItems(
    turns: List<Turn>,
    expandedTurnKeys: Set<Long>,
    expandedToolGroups: Set<String>,
): List<MsgItem> {
    if (turns.isEmpty()) return emptyList()
    val result = mutableListOf<MsgItem>()
    for ((i, turn) in turns.withIndex()) {
        val isLive = i == turns.size - 1
        val turnKey = i.toLong()
        if (!isLive && turnKey !in expandedTurnKeys) {
            result.add(MsgItem.Elided(turn, turnKey))
            continue
        }
        // Expanded past turn or live turn: emit groups as individual items.
        if (!isLive) {
            result.add(MsgItem.ExpandedTurnHeader(turn, turnKey, "th:$turnKey"))
        }
        turn.groups.forEachIndexed { j, group ->
            val base = if (isLive) "g:$j" else "e${turnKey}g$j"
            if (group.kind == GroupKind.TOOL && group.toolCalls.size > 1) {
                // Multi-call tool group: header + optional per-call items.
                val toolGroupKey = group.toolCalls.first().use.toolUseID
                result.add(MsgItem.ToolGroupHeader(group.toolCalls, toolGroupKey, "${base}h"))
                if (toolGroupKey in expandedToolGroups) {
                    group.toolCalls.forEachIndexed { k, call ->
                        result.add(MsgItem.ToolCallItem(call, "$base:$k"))
                    }
                }
            } else {
                result.add(MsgItem.Group(group, isLive, base))
            }
        }
    }
    return result
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TaskDetailScreen(
    taskId: String,
    onNavigateBack: () -> Unit,
    onNavigateToDiff: () -> Unit = {},
    viewModel: TaskDetailViewModel = hiltViewModel(),
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val task = state.task
    val context = LocalContext.current
    val contentResolver = context.contentResolver
    val photoPicker = rememberLauncherForActivityResult(
        ActivityResultContracts.PickMultipleVisualMedia(),
    ) { uris: List<Uri> ->
        val images = uris.mapNotNull { uriToImageData(contentResolver, it) }
        if (images.isNotEmpty()) viewModel.addImages(images)
    }
    var cameraUri by rememberSaveable { mutableStateOf<Uri?>(null) }
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

    // Safety dialog
    if (state.safetyIssues.isNotEmpty()) {
        SafetyDialog(
            issues = state.safetyIssues,
            onDismiss = viewModel::dismissSafetyIssues,
            onForceSync = {
                viewModel.dismissSafetyIssues()
                viewModel.syncTask(force = true)
            },
        )
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Row(
                            horizontalArrangement = Arrangement.spacedBy(4.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Text(
                                text = task?.repo ?: taskId,
                                style = MaterialTheme.typography.titleMedium,
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                            )
                            if (task?.inPlanMode == true) {
                                Surface(shape = RoundedCornerShape(4.dp), color = PlanBadgeBg) {
                                    Text(
                                        "P",
                                        style = MaterialTheme.typography.labelSmall,
                                        color = PlanBadgeFg,
                                        fontWeight = FontWeight.Bold,
                                        modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
                                    )
                                }
                            }
                        }
                        task?.let {
                            Row(
                                horizontalArrangement = Arrangement.spacedBy(4.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Text(
                                    text = it.branch,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                                Surface(
                                    shape = RoundedCornerShape(4.dp),
                                    color = stateColor(it.state),
                                ) {
                                    Text(
                                        text = it.state,
                                        style = MaterialTheme.typography.labelSmall,
                                        modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
                                    )
                                }
                            }
                        }
                    }
                },
                navigationIcon = {
                    TooltipBox(
                        positionProvider = TooltipDefaults.rememberPlainTooltipPositionProvider(),
                        tooltip = { PlainTooltip { Text("Back") } },
                        state = rememberTooltipState(),
                    ) {
                        IconButton(onClick = onNavigateBack) {
                            Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                        }
                    }
                },
            )
        },
        bottomBar = {
            if (task?.state !in TerminalStates) {
                Box(modifier = Modifier.fillMaxWidth(), contentAlignment = Alignment.BottomCenter) {
                Column(modifier = Modifier.widthIn(max = 840.dp)) {
                    state.actionError?.let { error ->
                        Text(
                            text = error,
                            color = MaterialTheme.colorScheme.error,
                            style = MaterialTheme.typography.bodySmall,
                            modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                        )
                    }
                    InputBar(
                        draft = state.inputDraft,
                        onDraftChange = viewModel::updateInputDraft,
                        onSend = viewModel::sendInput,
                        onSync = { viewModel.syncTask() },
                        onTerminate = viewModel::terminateTask,
                        sending = state.sending,
                        pendingAction = state.pendingAction,
                        repoURL = task?.repoURL,
                        pendingImages = state.pendingImages,
                        supportsImages = state.supportsImages,
                        onAttachGallery = {
                            photoPicker.launch(
                                PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)
                            )
                        },
                        onAttachCamera = {
                            val uri = createCameraPhotoUri(context)
                            cameraUri = uri
                            cameraLauncher.launch(uri)
                        },
                        onRemoveImage = viewModel::removeImage,
                    )
                }
                }
            }
        },
    ) { padding ->
        if (!state.isReady && !state.hasMessages) {
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(padding),
                contentAlignment = Alignment.Center,
            ) {
                CircularProgressIndicator()
            }
        } else {
            MessageList(
        state = state,
        padding = padding,
        onAnswer = { viewModel.sendInput() },
        onClearAndExecutePlan = {
            viewModel.restartTask(state.inputDraft.trim())
            viewModel.updateInputDraft("")
        },
        onNavigateToDiff = onNavigateToDiff,
    )

        }
    }
}

@Composable
private fun MessageList(
    state: TaskDetailState,
    padding: PaddingValues,
    onAnswer: (String) -> Unit,
    onClearAndExecutePlan: () -> Unit,
    onNavigateToDiff: () -> Unit,
) {
    val listState = rememberLazyListState()
    var userScrolledUp by remember { mutableStateOf(false) }
    val turns = state.turns
    val isWaiting = state.task?.state == "waiting" || state.task?.state == "asking"
    var expandedTurnKeys by remember { mutableStateOf(setOf<Long>()) }
    var expandedToolGroups by remember { mutableStateOf(setOf<String>()) }
    val items = remember(turns, expandedTurnKeys, expandedToolGroups) {
        buildItems(turns, expandedTurnKeys, expandedToolGroups)
    }

    // Auto-scroll to bottom when new messages arrive, unless user scrolled up.
    LaunchedEffect(turns.size, state.messageCount) {
        if (!userScrolledUp && turns.isNotEmpty()) {
            val total = listState.layoutInfo.totalItemsCount
            val lastVisible = listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: -1
            if (total > 0 && lastVisible < total - 1) {
                listState.animateScrollToItem(total - 1)
            }
        }
    }

    // Detect user scroll direction.
    LaunchedEffect(listState.isScrollInProgress) {
        if (listState.isScrollInProgress) {
            val info = listState.layoutInfo
            val lastVisible = info.visibleItemsInfo.lastOrNull()?.index ?: 0
            userScrolledUp = lastVisible < info.totalItemsCount - 2
        }
    }

    Box(modifier = Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
    Column(modifier = Modifier.widthIn(max = 840.dp).fillMaxWidth()) {
        // Todo panel
        if (state.todos.isNotEmpty()) {
            TodoPanel(
                todos = state.todos,
                modifier = Modifier.padding(horizontal = 12.dp, vertical = 4.dp),
            )
        }

        // Unified lazy list: past turns are Elided (one row each, groups not composed);
        // the live turn's groups and tool-call items are individual lazy items. Expand state
        // is owned here so collapsed content is never in the composition tree at all.
        SelectionContainer(modifier = Modifier.weight(1f)) {
            LazyColumn(
                state = listState,
                modifier = Modifier.fillMaxWidth(),
                contentPadding = PaddingValues(horizontal = 12.dp, vertical = 8.dp),
                verticalArrangement = Arrangement.spacedBy(4.dp),
            ) {
                items(
                    items = items,
                    key = { item -> item.key },
                    contentType = { item -> item::class },
                ) { item ->
                    when (item) {
                        is MsgItem.Elided -> ElidedTurn(
                            turn = item.turn,
                            onExpand = { expandedTurnKeys = expandedTurnKeys + item.key },
                        )
                        is MsgItem.ExpandedTurnHeader -> ExpandedTurnHeader(
                            turn = item.turn,
                            onCollapse = { expandedTurnKeys = expandedTurnKeys - item.turnKey },
                        )
                        is MsgItem.Group -> MessageGroupContent(
                            group = item.group,
                            onAnswer = if (item.isLiveTurn) onAnswer else null,
                            onNavigateToDiff = onNavigateToDiff,
                        )
                        is MsgItem.ToolGroupHeader -> ToolGroupHeaderItem(
                            toolCalls = item.toolCalls,
                            isExpanded = item.groupKey in expandedToolGroups,
                            onToggle = {
                                expandedToolGroups = if (item.groupKey in expandedToolGroups)
                                    expandedToolGroups - item.groupKey
                                else
                                    expandedToolGroups + item.groupKey
                            },
                        )
                        is MsgItem.ToolCallItem -> ToolCallCard(call = item.call)
                    }
                }

                // Plan panel: shown inline below messages when task is waiting with a plan.
                val planContent = state.task?.planContent
                if (isWaiting && !planContent.isNullOrEmpty()) {
                    item(key = "plan") {
                        PlanApprovalSection(
                            planContent = planContent,
                            onExecute = onClearAndExecutePlan,
                        )
                    }
                }
            }
        }
    }
    }
}

/** Header row shown at the top of an expanded past turn; tap to collapse back. */
@Composable
private fun ExpandedTurnHeader(turn: Turn, onCollapse: () -> Unit) {
    val summary = remember(turn) { turnSummary(turn) }
    Text(
        text = summary,
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onCollapse() }
            .padding(vertical = 4.dp),
    )
}
