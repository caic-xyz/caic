// Full-screen diff viewer showing per-file diffs for a task.
package com.fghbuild.caic.ui.diff

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
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.PlainTooltip
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TooltipAnchorPosition
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.hilt.lifecycle.viewmodel.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.caic.sdk.v1.DiffFileStat

private object DiffPalette {
    val statAdded = Color(0xFF22863A)
    val statDeleted = Color(0xFFCB2431)
    val addedLine = Color(0xFF4EC94E)
    val deletedLine = Color(0xFFFF4444)
    val movedAddedLine = Color(0xFF7DD3FC)
    val movedAddedAltLine = Color(0xFFA7F3D0)
    val movedDeletedLine = Color(0xFFF0ABFC)
    val movedDeletedAltLine = Color(0xFFFCD34D)
    val movedAddedBg = Color(0xFF102F3D)
    val movedAddedAltBg = Color(0xFF123226)
    val movedDeletedBg = Color(0xFF351836)
    val movedDeletedAltBg = Color(0xFF3B2B0B)
    val hunk = Color(0xFFB48EAD)
    val header = Color(0xFF888888)
    val codeBg = Color(0xFF1E1E1E)
    val codeFg = Color(0xFFD4D4D4)
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DiffScreen(
    taskId: String,
    onNavigateBack: () -> Unit,
    viewModel: DiffViewModel = hiltViewModel(),
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val task = state.task
    val statByPath = remember(state.files) {
        state.files.associateBy { it.path }
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text(
                            text = task?.repos?.firstOrNull()?.name ?: taskId,
                            style = MaterialTheme.typography.titleMedium,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                        )
                        task?.repos?.let { repos ->
                            if (repos.size == 1) {
                                Text(
                                    text = repos[0].branch,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            } else {
                                Text(
                                    text = repos.joinToString("  ·  ") { "${it.name}@${it.branch}" },
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 2,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                    }
                },
                navigationIcon = {
                    TooltipBox(
                        positionProvider = TooltipDefaults.rememberTooltipPositionProvider(TooltipAnchorPosition.Above),
                        tooltip = { PlainTooltip { Text("Back") } },
                        state = rememberTooltipState(),
                    ) {
                        IconButton(onClick = onNavigateBack) {
                            Icon(
                                Icons.AutoMirrored.Filled.ArrowBack,
                                contentDescription = "Back",
                            )
                        }
                    }
                },
            )
        },
    ) { padding ->
        when {
            state.loading -> Box(
                modifier = Modifier.fillMaxSize().padding(padding),
                contentAlignment = Alignment.Center,
            ) {
                CircularProgressIndicator()
            }
            state.error != null -> Box(
                modifier = Modifier.fillMaxSize().padding(padding),
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    text = state.error.orEmpty(),
                    color = MaterialTheme.colorScheme.error,
                )
            }
            else -> SelectionContainer {
                LazyColumn(
                    modifier = Modifier
                        .fillMaxSize()
                        .padding(padding),
                    contentPadding = PaddingValues(
                        horizontal = 12.dp,
                        vertical = 8.dp,
                    ),
                    verticalArrangement = Arrangement.spacedBy(2.dp),
                ) {
                    items(
                        state.fileDiffs,
                        key = { it.path },
                    ) { fd ->
                        val stat = statByPath[fd.path]
                        val collapsed = fd.path in state.collapsedFiles
                        FileSection(
                            path = fd.path,
                            stat = stat,
                            lines = fd.lines,
                            collapsed = collapsed,
                            onToggle = { viewModel.toggleFile(fd.path) },
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun FileSection(
    path: String,
    stat: DiffFileStat?,
    lines: List<DiffLine>,
    collapsed: Boolean,
    onToggle: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .clickable { onToggle() }
                .padding(vertical = 4.dp),
            horizontalArrangement = Arrangement.spacedBy(6.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                text = if (collapsed) "\u25b6" else "\u25bc",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(
                text = path,
                style = MaterialTheme.typography.bodySmall,
                modifier = Modifier.weight(1f),
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
            if (stat?.binary == true) {
                Text(
                    text = "binary",
                    style = MaterialTheme.typography.bodySmall,
                    fontStyle = FontStyle.Italic,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else if (stat != null) {
                Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                    if (stat.added > 0) {
                        Text(
                            text = "+${stat.added}",
                            style = MaterialTheme.typography.bodySmall,
                            color = DiffPalette.statAdded,
                        )
                    }
                    if (stat.deleted > 0) {
                        Text(
                            text = "\u2212${stat.deleted}",
                            style = MaterialTheme.typography.bodySmall,
                            color = DiffPalette.statDeleted,
                        )
                    }
                }
            }
        }
        if (!collapsed) {
            DiffContentBlock(lines)
        }
    }
}

@Composable
private fun DiffContentBlock(lines: List<DiffLine>) {
    val annotated = remember(lines) {
        buildAnnotatedString {
            lines.forEachIndexed { i, line ->
                if (i > 0) append("\n")
                val style = when (line.kind) {
                    DiffLineKind.Added -> SpanStyle(color = DiffPalette.addedLine)
                    DiffLineKind.Deleted -> SpanStyle(color = DiffPalette.deletedLine)
                    DiffLineKind.Hunk -> SpanStyle(color = DiffPalette.hunk)
                    DiffLineKind.Header -> SpanStyle(color = DiffPalette.header)
                    DiffLineKind.MovedAdded -> if (line.movedVariant == 1) {
                        SpanStyle(
                            color = DiffPalette.movedAddedAltLine,
                            background = DiffPalette.movedAddedAltBg,
                        )
                    } else {
                        SpanStyle(
                            color = DiffPalette.movedAddedLine,
                            background = DiffPalette.movedAddedBg,
                        )
                    }
                    DiffLineKind.MovedDeleted -> if (line.movedVariant == 1) {
                        SpanStyle(
                            color = DiffPalette.movedDeletedAltLine,
                            background = DiffPalette.movedDeletedAltBg,
                        )
                    } else {
                        SpanStyle(
                            color = DiffPalette.movedDeletedLine,
                            background = DiffPalette.movedDeletedBg,
                        )
                    }
                    DiffLineKind.Context -> SpanStyle(color = DiffPalette.codeFg)
                }
                withStyle(style) {
                    append(line.text)
                }
            }
        }
    }
    Text(
        text = annotated,
        fontFamily = FontFamily.Monospace,
        fontSize = 11.sp,
        lineHeight = 15.sp,
        modifier = Modifier
            .fillMaxWidth()
            .background(DiffPalette.codeBg)
            .padding(4.dp),
        color = DiffPalette.codeFg,
        style = MaterialTheme.typography.bodySmall,
    )
}
