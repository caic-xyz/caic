// Full-screen task detail view with live SSE message stream, grouping, and actions.
package com.fghbuild.caic.ui.taskdetail

import android.app.Activity
import android.media.projection.MediaProjectionManager
import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.saveable.rememberSaveable
import com.fghbuild.caic.util.ScreenshotService
import com.fghbuild.caic.util.bitmapToImageData
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
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.ExperimentalMaterial3Api
import com.fghbuild.caic.ui.common.rememberNotificationPermissionRequester
import com.fghbuild.caic.ui.theme.markdownTypography
import com.mikepenz.markdown.m3.Markdown
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
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.lifecycle.viewmodel.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.caic.ui.theme.appColors
import com.fghbuild.caic.ui.theme.stateColor
import com.fghbuild.caic.ui.theme.waitingStates
import com.fghbuild.caic.util.createCameraPhotoUri
import com.fghbuild.caic.util.formatElapsed
import com.caic.sdk.v1.EventKinds
import com.caic.sdk.v1.ForgeCheck
import java.time.Instant
import com.fghbuild.caic.util.GroupKind
import kotlinx.serialization.json.JsonElement
import com.fghbuild.caic.util.MessageGroup
import com.fghbuild.caic.util.Session
import com.fghbuild.caic.util.ToolCall
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.isSessionBoundary
import com.fghbuild.caic.util.sessionSummary
import com.fghbuild.caic.util.turnSummary
import com.fghbuild.caic.util.uriToImageData
import com.caic.sdk.v1.EventKinds as SdkEventKinds

/** Visual indent level for items inside expanded containers (session or turn). */
private enum class Indent { Session, Turn }

// Flat list items for the LazyColumn. Expansion state is owned here so collapsed items are
// never composed — true laziness without AnimatedVisibility wrappers.
//
// Key namespaces (all String):
//   "se:<sessionKey>"             — Elided past session
//   "sh:<sessionKey>"             — Header of an expanded past session
//   "sb:<sessionKey>"             — Session boundary (init / compact_boundary) for current session
//   "e:<turnKey>"                 — Elided past turn
//   "th:<turnKey>"                — Header of an expanded past turn
//   "g:j"  / "s{si}t{ti}g{j}"   — Group item (live turn / other)
//   "<base>h"                     — Tool-group header item
//   "<base>:<k>"                  — Individual ToolCallItem within expanded tool group
//   "<base>thinking"              — Thinking block within expanded tool group
private sealed interface MsgItem {
    val key: String
    /** Collapsed past session — one summary row. */
    data class SessionElided(val session: Session, val sessionKey: String, override val key: String) : MsgItem
    /** Header of an expanded past session; tap to collapse. */
    data class SessionHeader(val session: Session, val sessionKey: String, override val key: String) : MsgItem
    /** Session boundary event (init or compact_boundary) shown as an inline separator. */
    data class SessionBoundary(val event: com.caic.sdk.v1.EventMessage, override val key: String) : MsgItem
    /** Collapsed past turn — one summary row per turn. */
    data class Elided(val turn: Turn, val turnKey: String, override val key: String, val indent: Indent? = null) : MsgItem
    /** Header shown at the top of an expanded past turn; tap to collapse. */
    data class ExpandedTurnHeader(val turn: Turn, val turnKey: String, override val key: String, val indent: Indent? = null) : MsgItem
    /** Non-tool group (text, ask, result, user-input, other). */
    data class Group(
        val group: MessageGroup,
        val isLiveTurn: Boolean,
        override val key: String,
        val indent: Indent? = null,
    ) : MsgItem
    /** Summary header for a multi-call tool group; tap to expand/collapse its call items. */
    data class ToolGroupHeader(
        val toolCalls: List<ToolCall>,
        val groupKey: String,
        override val key: String,
        val indent: Indent? = null,
    ) : MsgItem
    /** One tool call within an expanded multi-call tool group. */
    data class ToolCallItem(val call: ToolCall, override val key: String, val indent: Indent? = null) : MsgItem
    /** Thinking block inside an expanded multi-call tool group. */
    data class ThinkingItem(val events: List<com.caic.sdk.v1.EventMessage>, override val key: String, val indent: Indent? = null) : MsgItem
    /** Plan content hoisted out of a collapsed multi-call tool group. */
    data class PlanApproval(val plan: String, override val key: String, val indent: Indent? = null) : MsgItem
}

/**
 * Builds flat items for past sessions, the current session boundary, and completed turns
 * within the current session. Stable during streaming: only changes at session/turn boundaries.
 *
 * @param expandedSessionKeys  Session keys that the user has expanded.
 * @param expandedTurnKeys  Turn keys whose groups should be shown expanded.
 * @param expandedToolGroups  Tool-group keys whose call items should be shown.
 */
private fun buildCompletedItems(
    completedSessions: List<Session>,
    currentSessionBoundaryEvent: com.caic.sdk.v1.EventMessage?,
    currentSessionCompletedTurns: List<Turn>,
    expandedSessionKeys: Set<String>,
    expandedTurnKeys: Set<String>,
    expandedToolGroups: Set<String>,
): List<MsgItem> {
    val result = mutableListOf<MsgItem>()
    for ((si, session) in completedSessions.withIndex()) {
        val sessionKey = "session:$si:${session.boundaryEvent?.ts ?: ""}"
        if (sessionKey !in expandedSessionKeys) {
            result.add(MsgItem.SessionElided(session, sessionKey, "se:$sessionKey"))
        } else {
            result.add(MsgItem.SessionHeader(session, sessionKey, "sh:$sessionKey"))
            emitTurns(
                result, session.turns, sessionKey, expandedTurnKeys, expandedToolGroups,
                liveSessionTurn = false, si, inPastSession = true,
            )
        }
    }
    val currentSi = completedSessions.size
    val currentSessionKey = "session:$currentSi:${currentSessionBoundaryEvent?.ts ?: ""}"
    currentSessionBoundaryEvent?.let {
        result.add(MsgItem.SessionBoundary(it, "sb:$currentSessionKey"))
    }
    emitTurns(
        result, currentSessionCompletedTurns, currentSessionKey, expandedTurnKeys, expandedToolGroups,
        liveSessionTurn = false, currentSi,
    )
    return result
}

/** Emits turn items for all turns in a session into [result].
 *  inPastSession: true when the parent session is an expanded past session; items receive
 *  indent markers so the renderer can draw a visual left-border hierarchy. */
private fun emitTurns(
    result: MutableList<MsgItem>,
    turns: List<Turn>,
    sessionKey: String,
    expandedTurnKeys: Set<String>,
    expandedToolGroups: Set<String>,
    liveSessionTurn: Boolean,
    si: Int,
    inPastSession: Boolean = false,
) {
    for ((ti, turn) in turns.withIndex()) {
        val isLiveTurn = liveSessionTurn && ti == turns.size - 1
        val turnKey = "$sessionKey:turn:$ti:${turn.groups.firstOrNull()?.events?.firstOrNull()?.ts ?: ""}"
        val sessionIndent = if (inPastSession) Indent.Session else null

        if (!isLiveTurn) {
            if (turnKey !in expandedTurnKeys) {
                result.add(MsgItem.Elided(turn, turnKey, "e:$turnKey", indent = sessionIndent))
                continue
            }
            result.add(MsgItem.ExpandedTurnHeader(turn, turnKey, "th:$turnKey", indent = sessionIndent))
        }
        // Groups inside an expanded past turn get Turn indent; live turn groups have no indent.
        val groupIndent = if (!isLiveTurn) Indent.Turn else null
        turn.groups.forEachIndexed { j, group ->
            val base = if (isLiveTurn) "g:$j" else "s${si}t${ti}g${j}"
            if (group.kind == GroupKind.ACTION && group.toolCalls.size > 1) {
                val toolGroupKey = group.toolCalls.first().use.toolUseID
                result.add(MsgItem.ToolGroupHeader(group.toolCalls, toolGroupKey, "${base}h", indent = groupIndent))
                val planCall = group.toolCalls.firstOrNull { it.use.planContent != null }
                if (planCall != null) {
                    result.add(MsgItem.PlanApproval(planCall.use.planContent!!, "${base}plan", indent = groupIndent))
                }
                if (toolGroupKey in expandedToolGroups) {
                    val thinkingEvents = group.events.filter {
                        it.kind == SdkEventKinds.Thinking || it.kind == SdkEventKinds.ThinkingDelta
                    }
                    if (thinkingEvents.isNotEmpty()) {
                        result.add(MsgItem.ThinkingItem(thinkingEvents, "${base}thinking", indent = groupIndent))
                    }
                    group.toolCalls.forEachIndexed { k, call ->
                        result.add(MsgItem.ToolCallItem(call, "$base:$k", indent = groupIndent))
                    }
                }
            } else {
                result.add(MsgItem.Group(group, isLiveTurn, base, indent = groupIndent))
            }
        }
    }
}

// Builds flat items for the live (current) turn. Rebuilds on every message batch.
// When isLiveTurn is false the groups are shown expanded but without interactive
// actions (used for the last completed turn when there is no active live turn).
private fun buildLiveItems(liveTurn: Turn?, expandedToolGroups: Set<String>, isLiveTurn: Boolean = true): List<MsgItem> {
    val turn = liveTurn ?: return emptyList()
    val result = mutableListOf<MsgItem>()
    turn.groups.forEachIndexed { j, group ->
        val base = "g:$j"
        if (group.kind == GroupKind.ACTION && group.toolCalls.size > 1) {
            val toolGroupKey = group.toolCalls.first().use.toolUseID
            result.add(MsgItem.ToolGroupHeader(group.toolCalls, toolGroupKey, "${base}h"))
            val planCall = group.toolCalls.firstOrNull { it.use.planContent != null }
            if (planCall != null) {
                result.add(MsgItem.PlanApproval(planCall.use.planContent!!, "${base}plan"))
            }
            if (toolGroupKey in expandedToolGroups) {
                val thinkingEvents = group.events.filter {
                    it.kind == SdkEventKinds.Thinking || it.kind == SdkEventKinds.ThinkingDelta
                }
                if (thinkingEvents.isNotEmpty()) {
                    result.add(MsgItem.ThinkingItem(thinkingEvents, "${base}thinking"))
                }
                group.toolCalls.forEachIndexed { k, call ->
                    result.add(MsgItem.ToolCallItem(call, "$base:$k"))
                }
            }
        } else {
            result.add(MsgItem.Group(group, isLiveTurn = isLiveTurn, base))
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
    onNavigateToTask: (String) -> Unit = {},
    viewModel: TaskDetailViewModel = hiltViewModel(),
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val task = state.task
    val requestNotificationPermission = rememberNotificationPermissionRequester()
    val uriHandler = LocalUriHandler.current
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
    val mpm = remember { context.getSystemService(MediaProjectionManager::class.java) }
    val screenshotLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        // Pass resultCode + data to the service so it can call getMediaProjection AFTER
        // startForeground — required on Android 14+ (getMediaProjection throws SecurityException
        // if called before the FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION service is running).
        if (result.resultCode == Activity.RESULT_OK && result.data != null) {
            ScreenshotService.start(context, result.resultCode, result.data!!) { bitmap ->
                val img = bitmapToImageData(bitmap)
                bitmap.recycle()
                viewModel.addImages(listOf(img))
            }
        }
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
                            val remoteURL = task?.repos?.firstOrNull()?.remoteURL
                            Text(
                                text = task?.repos?.firstOrNull()?.name ?: taskId,
                                style = MaterialTheme.typography.titleMedium,
                                color = if (remoteURL != null) MaterialTheme.colorScheme.primary else Color.Unspecified,
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                                modifier = if (remoteURL != null) {
                                    Modifier.clickable { uriHandler.openUri(remoteURL) }
                                } else {
                                    Modifier
                                },
                            )
                            if (task?.inPlanMode == true) {
                                Surface(
                                shape = RoundedCornerShape(4.dp),
                                color = MaterialTheme.colorScheme.tertiaryContainer,
                            ) {
                                    Text(
                                        "P",
                                        style = MaterialTheme.typography.labelSmall,
                                        color = MaterialTheme.colorScheme.tertiary,
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
                                val primaryRepo = it.repos?.firstOrNull()
                                val primaryBranch = primaryRepo?.branch ?: ""
                                val branchURL = primaryRepo?.remoteURL?.let { url ->
                                    when (primaryRepo.forge) {
                                        "gitlab" -> "$url/-/compare/${primaryBranch}?expand=1"
                                        "github" -> "$url/compare/${primaryBranch}?expand=1"
                                        else -> null
                                    }
                                }
                                Text(
                                    text = primaryBranch,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = if (branchURL != null) {
                                        MaterialTheme.colorScheme.primary
                                    } else {
                                        MaterialTheme.colorScheme.onSurfaceVariant
                                    },
                                    modifier = if (branchURL != null) {
                                        Modifier.clickable { uriHandler.openUri(branchURL) }
                                    } else {
                                        Modifier
                                    },
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
                                val forgeOwner = it.forgeOwner
                                val forgeRepo = it.forgeRepo
                                val forgePR = it.forgePR
                                if (forgeOwner != null && forgeRepo != null && forgePR != null && forgePR > 0) {
                                    val forge = it.repos?.firstOrNull()?.forge
                                    val prURL = if (forge == "gitlab") {
                                        "https://gitlab.com/$forgeOwner/$forgeRepo/-/merge_requests/$forgePR"
                                    } else {
                                        "https://github.com/$forgeOwner/$forgeRepo/pull/$forgePR"
                                    }
                                    val prLabel = if (forge == "gitlab") "MR #$forgePR" else "PR #$forgePR"
                                    Text(
                                        text = "·",
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                    Text(
                                        text = prLabel,
                                        style = MaterialTheme.typography.bodySmall,
                                        color = MaterialTheme.colorScheme.primary,
                                        modifier = Modifier.clickable { uriHandler.openUri(prURL) },
                                    )
                                }
                                val appColors = MaterialTheme.appColors
                                val checks = it.ciChecks
                                val hasChecks = !checks.isNullOrEmpty()
                                val ciLabel = ciLabel(it.ciStatus, checks)
                                var ciExpanded by rememberSaveable { mutableStateOf(false) }
                                when (it.ciStatus) {
                                    "pending" -> Column {
                                        Surface(
                                            shape = RoundedCornerShape(4.dp),
                                            color = appColors.warningBg,
                                            modifier = if (hasChecks) Modifier.clickable {
                                                ciExpanded = !ciExpanded
                                            } else Modifier,
                                        ) {
                                            Text(
                                                text = ciLabel,
                                                style = MaterialTheme.typography.labelSmall,
                                                color = appColors.warningText,
                                                modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
                                            )
                                        }
                                        if (ciExpanded && hasChecks) {
                                            CICheckList(checks = checks!!)
                                        }
                                    }
                                    "success" -> Column {
                                        Surface(
                                            shape = RoundedCornerShape(4.dp),
                                            color = appColors.successBg,
                                            modifier = if (hasChecks) Modifier.clickable {
                                                ciExpanded = !ciExpanded
                                            } else Modifier,
                                        ) {
                                            Text(
                                                text = ciLabel,
                                                style = MaterialTheme.typography.labelSmall,
                                                color = appColors.successText,
                                                modifier = Modifier.padding(horizontal = 4.dp, vertical = 1.dp),
                                            )
                                        }
                                        if (ciExpanded && hasChecks) {
                                            CICheckList(checks = checks!!)
                                        }
                                    }
                                    "failure" -> Column {
                                        Row(
                                            verticalAlignment = Alignment.CenterVertically,
                                            horizontalArrangement = Arrangement.spacedBy(4.dp),
                                        ) {
                                            Surface(
                                                shape = RoundedCornerShape(4.dp),
                                                color = MaterialTheme.colorScheme.errorContainer,
                                                modifier = if (hasChecks) Modifier.clickable {
                                                    ciExpanded = !ciExpanded
                                                } else Modifier,
                                            ) {
                                                Text(
                                                    text = ciLabel,
                                                    style = MaterialTheme.typography.labelSmall,
                                                    color = MaterialTheme.colorScheme.onErrorContainer,
                                                    modifier = Modifier.padding(
                                                        horizontal = 4.dp,
                                                        vertical = 1.dp,
                                                    ),
                                                )
                                            }
                                            val fixingCI = state.pendingAction == "fixCI"
                                            Surface(
                                                shape = RoundedCornerShape(4.dp),
                                                color = if (fixingCI) MaterialTheme.colorScheme.errorContainer
                                                        else MaterialTheme.colorScheme.error,
                                                modifier = Modifier.clickable(enabled = !fixingCI) {
                                                    viewModel.fixCI { newTaskId ->
                                                        onNavigateToTask(newTaskId)
                                                    }
                                                },
                                            ) {
                                                Text(
                                                    text = if (fixingCI) "Creating…" else "Fix CI",
                                                    style = MaterialTheme.typography.labelSmall,
                                                    color = if (fixingCI) {
                                                        MaterialTheme.colorScheme.onErrorContainer
                                                    } else {
                                                        MaterialTheme.colorScheme.onError
                                                    },
                                                    modifier = Modifier.padding(
                                                        horizontal = 4.dp,
                                                        vertical = 1.dp,
                                                    ),
                                                )
                                            }
                                        }
                                        if (ciExpanded && hasChecks) {
                                            CICheckList(checks = checks!!)
                                        }
                                    }
                                    else -> Unit
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
            val noActionStates = setOf("stopping", "purging", "purged", "failed")
            if (task?.state !in noActionStates) {
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
                        onSend = { requestNotificationPermission(); viewModel.sendInput() },
                        onSync = { viewModel.syncTask() },
                        onSyncToBaseBranch = { viewModel.syncTask(target = "default") },
                        onStop = viewModel::stopTask,
                        onPurge = viewModel::purgeTask,
                        onRevive = viewModel::reviveTask,
                        taskState = task?.state ?: "",
                        taskTitle = task?.title ?: "",
                        taskRepo = task?.repos?.firstOrNull()?.name ?: "",
                        taskBranch = task?.repos?.firstOrNull()?.branch ?: "",
                        taskBaseBranch = task?.repos?.firstOrNull()?.baseBranch ?: "",
                        sending = state.sending,
                        pendingAction = state.pendingAction,
                        forge = task?.repos?.firstOrNull()?.forge,
                        forgePR = task?.forgePR,
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
                        onScreenshot = {
                            screenshotLauncher.launch(mpm.createScreenCaptureIntent())
                        },
                        onRemoveImage = viewModel::removeImage,
                        safetyIssues = state.safetyIssues,
                        onForceSync = {
                            viewModel.dismissSafetyIssues()
                            viewModel.syncTask(force = true)
                        },
                    )
                }
                }
            }
        },
    ) { padding ->
        if (!state.isReady && !state.hasMessages) {
            val prompt = state.task?.initialPrompt
            if (!prompt.isNullOrBlank()) {
                Column(
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(padding)
                        .padding(12.dp),
                ) {
                    Surface(
                        modifier = Modifier.fillMaxWidth(),
                        shape = RoundedCornerShape(6.dp),
                        color = MaterialTheme.appColors.userMsgBg,
                    ) {
                        Markdown(
                            content = prompt,
                            typography = markdownTypography(),
                            colors = com.mikepenz.markdown.m3.markdownColor(
                                text = MaterialTheme.colorScheme.onSurface,
                                codeBackground = MaterialTheme.colorScheme.surfaceVariant,
                            ),
                            modifier = Modifier.padding(horizontal = 8.dp, vertical = 4.dp),
                        )
                    }
                    CircularProgressIndicator(modifier = Modifier.padding(top = 16.dp))
                }
            } else {
                Box(
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(padding),
                    contentAlignment = Alignment.Center,
                ) {
                    CircularProgressIndicator()
                }
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
        onLoadToolInput = { toolUseID -> viewModel.loadToolInput(toolUseID) },
    )

        }
    }
}

/** Draws a vertical left-border line to indicate an item belongs to an expanded container.
 *
 * The line extends 2dp past the top and bottom of the item to bridge the spacedBy(4.dp) gap
 * between consecutive indented items, making the line appear continuous. graphicsLayer with
 * clip = false is required so the out-of-bounds drawing is not clipped by the layer. */
@Composable
private fun IndentBorder(indent: Indent?, content: @Composable () -> Unit) {
    if (indent == null) {
        content()
        return
    }
    val strokeDp = if (indent == Indent.Session) 3.dp else 2.dp
    val borderColor = MaterialTheme.colorScheme.outlineVariant.let {
        if (indent == Indent.Turn) it.copy(alpha = it.alpha * 0.7f) else it
    }
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .graphicsLayer { clip = false }
            .drawBehind {
                val strokePx = strokeDp.toPx()
                val overlapPx = 2.dp.toPx()
                drawLine(
                    color = borderColor,
                    start = Offset(strokePx / 2f, -overlapPx),
                    end = Offset(strokePx / 2f, size.height + overlapPx),
                    strokeWidth = strokePx,
                )
            }
            .padding(start = strokeDp + 6.dp),
    ) {
        content()
    }
}

@Composable
private fun MessageList(
    state: TaskDetailState,
    padding: PaddingValues,
    onAnswer: (String) -> Unit,
    onClearAndExecutePlan: () -> Unit,
    onNavigateToDiff: () -> Unit,
    onLoadToolInput: (suspend (String) -> JsonElement?)? = null,
) {
    val listState = rememberLazyListState()
    var userScrolledUp by remember { mutableStateOf(false) }
    val completedSessions = state.completedSessions
    val currentSessionBoundaryEvent = state.currentSessionBoundaryEvent
    val currentSessionCompletedTurns = state.currentSessionCompletedTurns
    val liveTurn = state.liveTurn
    val isWaiting = state.task?.state in waitingStates
    var expandedSessionKeys by remember { mutableStateOf(setOf<String>()) }
    var expandedTurnKeys by remember { mutableStateOf(setOf<String>()) }
    var expandedToolGroups by remember { mutableStateOf(setOf<String>()) }
    // The last completed turn is always expanded to provide context for the current state,
    // whether the agent is idle (no live turn) or actively streaming.
    val elidableCompletedTurns = currentSessionCompletedTurns.dropLast(1)
    val lastExpandedTurn = currentSessionCompletedTurns.lastOrNull()
    // Completed items are stable during streaming: references are unchanged until a turn boundary,
    // so this remember block only recomputes then or on expansion.
    val completedItems = remember(
        completedSessions, currentSessionBoundaryEvent, elidableCompletedTurns,
        expandedSessionKeys, expandedTurnKeys, expandedToolGroups,
    ) {
        buildCompletedItems(
            completedSessions, currentSessionBoundaryEvent, elidableCompletedTurns,
            expandedSessionKeys, expandedTurnKeys, expandedToolGroups,
        )
    }
    // Last completed turn shown expanded (no interactive actions) when there is no live turn.
    // Uses the same key prefix as liveItems ("g:j") so the LazyColumn can reuse composables
    // as the turn transitions between live and last-expanded states.
    val lastTurnItems = remember(lastExpandedTurn, expandedToolGroups) {
        buildLiveItems(lastExpandedTurn, expandedToolGroups, isLiveTurn = false)
    }
    // Live items rebuild on every message batch, but only cover the current turn.
    val liveItems = remember(liveTurn, expandedToolGroups) {
        buildLiveItems(liveTurn, expandedToolGroups)
    }
    val items = remember(completedItems, lastTurnItems, liveItems) { completedItems + lastTurnItems + liveItems }

    // Auto-scroll to bottom when new messages arrive, unless user scrolled up.
    LaunchedEffect(completedSessions.size, currentSessionCompletedTurns.size, state.messageCount) {
        if (!userScrolledUp && (completedSessions.isNotEmpty() || currentSessionCompletedTurns.isNotEmpty() || liveTurn != null)) {
            val total = listState.layoutInfo.totalItemsCount
            val lastVisible = listState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: -1
            if (total > 0 && lastVisible < total - 1) {
                listState.animateScrollToItem(total - 1)
            }
        }
    }

    // Update userScrolledUp when a scroll gesture ends so the final position is used.
    // Checking at scroll-start is unreliable: the user is still at the bottom when the gesture begins.
    LaunchedEffect(listState.isScrollInProgress) {
        if (!listState.isScrollInProgress) {
            val info = listState.layoutInfo
            if (info.totalItemsCount > 0) {
                val lastVisible = info.visibleItemsInfo.lastOrNull()?.index ?: 0
                userScrolledUp = lastVisible < info.totalItemsCount - 2
            }
        }
    }

    Box(modifier = Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.TopCenter) {
    Column(modifier = Modifier.widthIn(max = 840.dp).fillMaxWidth()) {
        ProgressPanel(
            todos = state.todos,
            activeAgentDescriptions = state.activeAgentDescriptions,
            modifier = Modifier.padding(horizontal = 12.dp, vertical = 4.dp),
        )

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
                        is MsgItem.SessionElided -> ElidedSession(
                            session = item.session,
                            onExpand = { expandedSessionKeys = expandedSessionKeys + item.sessionKey },
                        )
                        is MsgItem.SessionHeader -> SessionHeader(
                            session = item.session,
                            onCollapse = { expandedSessionKeys = expandedSessionKeys - item.sessionKey },
                        )
                        is MsgItem.SessionBoundary -> SessionBoundaryRow(event = item.event)
                        is MsgItem.Elided -> IndentBorder(item.indent) {
                            ElidedTurn(
                                turn = item.turn,
                                onExpand = { expandedTurnKeys = expandedTurnKeys + item.turnKey },
                            )
                        }
                        is MsgItem.ExpandedTurnHeader -> IndentBorder(item.indent) {
                            ExpandedTurnHeader(
                                turn = item.turn,
                                onCollapse = { expandedTurnKeys = expandedTurnKeys - item.turnKey },
                            )
                        }
                        is MsgItem.Group -> IndentBorder(item.indent) {
                            MessageGroupContent(
                                group = item.group,
                                onAnswer = if (item.isLiveTurn) onAnswer else null,
                                onNavigateToDiff = onNavigateToDiff,
                                onLoadToolInput = onLoadToolInput,
                                onClearAndExecutePlan = if (item.isLiveTurn && isWaiting) onClearAndExecutePlan else null,
                            )
                        }
                        is MsgItem.ToolGroupHeader -> IndentBorder(item.indent) {
                            ToolGroupHeaderItem(
                                toolCalls = item.toolCalls,
                                isExpanded = item.groupKey in expandedToolGroups,
                                onToggle = {
                                    expandedToolGroups = if (item.groupKey in expandedToolGroups)
                                        expandedToolGroups - item.groupKey
                                    else
                                        expandedToolGroups + item.groupKey
                                },
                            )
                        }
                        is MsgItem.ThinkingItem -> IndentBorder(item.indent) {
                            ThinkingCard(events = item.events)
                        }
                        is MsgItem.ToolCallItem -> IndentBorder(item.indent) {
                            ToolCallCard(
                                call = item.call,
                                onLoadInput = onLoadToolInput?.takeIf { item.call.use.inputTruncated == true }
                                    ?.let { loader -> { loader(item.call.use.toolUseID) } },
                                onClearAndExecutePlan = if (isWaiting) onClearAndExecutePlan else null,
                                suppressPlanContent = true,
                            )
                        }
                        is MsgItem.PlanApproval -> IndentBorder(item.indent) {
                            PlanContent(
                                planContent = item.plan,
                                onExecute = if (isWaiting) onClearAndExecutePlan else null,
                            )
                        }
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
        text = "▼ $summary",
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onCollapse() }
            .padding(vertical = 4.dp),
    )
}

/** Collapsed past session row — tap to expand. */
@Composable
private fun ElidedSession(session: Session, onExpand: () -> Unit) {
    val summary = remember(session) { sessionSummary(session) }
    Text(
        text = "▶ $summary",
        style = MaterialTheme.typography.bodySmall,
        fontWeight = FontWeight.Medium,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onExpand() }
            .padding(vertical = 6.dp),
    )
}

/** Header of an expanded past session; tap to collapse. */
@Composable
private fun SessionHeader(session: Session, onCollapse: () -> Unit) {
    val summary = remember(session) { sessionSummary(session) }
    Text(
        text = "▼ $summary",
        style = MaterialTheme.typography.bodySmall,
        fontWeight = FontWeight.Medium,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier
            .fillMaxWidth()
            .clickable { onCollapse() }
            .padding(vertical = 6.dp),
    )
}

/** Inline separator for a session boundary (init or compact_boundary) in the current session. */
@Composable
private fun SessionBoundaryRow(event: com.caic.sdk.v1.EventMessage) {
    if (event.kind == SdkEventKinds.Init) {
        val init = event.init
        val label = if (init != null) {
            "Session started \u00b7 ${init.model} \u00b7 ${init.agentVersion} \u00b7 ${init.sessionID}"
        } else {
            "Session started"
        }
        Text(
            text = label,
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline,
            modifier = Modifier
                .fillMaxWidth()
                .padding(vertical = 4.dp),
        )
    } else {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(vertical = 8.dp),
        ) {
            HorizontalDivider()
            Text(
                text = "Conversation compacted",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.outline,
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(top = 4.dp),
            )
        }
    }
}

/** Compute the CI badge label, showing completion progress when checks are available. */
private fun ciLabel(status: String?, checks: List<ForgeCheck>?): String {
    if (checks.isNullOrEmpty()) {
        return when (status) {
            "pending" -> "CI: pending"
            "success" -> "CI: passed"
            "failure" -> "CI: failed"
            else -> "CI"
        }
    }
    if (status == "pending" || status == "failure") {
        val done = checks.count { it.status == "completed" }
        if (done < checks.size) return "CI: $done/${checks.size}"
    }
    return when (status) {
        "pending" -> "CI: pending"
        "success" -> "CI: passed"
        "failure" -> "CI: failed"
        else -> "CI"
    }
}

/** Human-readable status label for a single CI check. */
private fun checkStatusLabel(c: ForgeCheck): String = when {
    c.status == "completed" -> {
        if (c.conclusion == "success" || c.conclusion == "neutral" || c.conclusion == "skipped") "passed"
        else c.conclusion.ifEmpty { "failed" }
    }
    c.status == "in_progress" -> "running"
    else -> "queued"
}

/** Elapsed duration string for a CI check, from startedAt (or queuedAt) to completedAt (or now). */
private fun checkDuration(c: ForgeCheck): String {
    val startStr = c.startedAt ?: c.queuedAt ?: return ""
    val start = try {
        Instant.parse(startStr)
    } catch (_: Exception) {
        return ""
    }
    val end = if (c.completedAt != null) {
        try {
            Instant.parse(c.completedAt)
        } catch (_: Exception) {
            Instant.now()
        }
    } else {
        Instant.now()
    }
    val seconds = java.time.Duration.between(start, end).seconds.toDouble()
    return formatElapsed(seconds)
}

/** Expandable list of per-check detail rows for the CI badge. */
@Composable
private fun CICheckList(checks: List<ForgeCheck>) {
    val appColors = MaterialTheme.appColors
    Column(
        modifier = Modifier.padding(start = 4.dp, top = 4.dp),
        verticalArrangement = Arrangement.spacedBy(2.dp),
    ) {
        for (c in checks) {
            val label = checkStatusLabel(c)
            val color = when {
                c.status == "completed" && (c.conclusion == "success" || c.conclusion == "neutral"
                    || c.conclusion == "skipped") -> appColors.successText
                c.status == "completed" -> MaterialTheme.colorScheme.error
                c.status == "in_progress" -> appColors.warningText
                else -> MaterialTheme.colorScheme.onSurfaceVariant
            }
            Row(
                horizontalArrangement = Arrangement.spacedBy(6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = c.name,
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurface,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = false),
                )
                Text(
                    text = label,
                    style = MaterialTheme.typography.labelSmall,
                    color = color,
                )
                val duration = checkDuration(c)
                if (duration.isNotEmpty()) {
                    Text(
                        text = duration,
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
    }
}
