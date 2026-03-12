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
import androidx.compose.material3.TooltipBox
import androidx.compose.material3.TooltipDefaults
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.rememberTooltipState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
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
import com.fghbuild.caic.ui.theme.appColors

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
                        task?.let {
                            Text(
                                text = it.repos?.firstOrNull()?.branch ?: "",
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                            )
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
                            content = fd.content,
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
    content: String,
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
                            color = MaterialTheme.appColors.diffAddedStat,
                        )
                    }
                    if (stat.deleted > 0) {
                        Text(
                            text = "\u2212${stat.deleted}",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.appColors.diffDeletedStat,
                        )
                    }
                }
            }
        }
        if (!collapsed) {
            DiffContentBlock(content)
        }
    }
}

@Composable
private fun DiffContentBlock(diff: String) {
    val appColors = MaterialTheme.appColors
    val addedColor = appColors.diffAddedLine
    val deletedColor = appColors.diffDeletedLine
    val hunkColor = appColors.diffHunk
    val headerColor = appColors.diffHeader
    val fgColor = appColors.diffCodeFg
    val bgColor = appColors.diffCodeBg
    val annotated = remember(diff, addedColor, deletedColor, hunkColor, headerColor, fgColor) {
        buildAnnotatedString {
            diff.lineSequence().forEachIndexed { i, line ->
                if (i > 0) append("\n")
                val color = when {
                    line.startsWith("+") -> addedColor
                    line.startsWith("-") -> deletedColor
                    line.startsWith("@@") -> hunkColor
                    line.startsWith("diff ") -> headerColor
                    else -> fgColor
                }
                withStyle(SpanStyle(color = color)) {
                    append(line)
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
            .background(bgColor)
            .padding(4.dp),
        color = fgColor,
        style = MaterialTheme.typography.bodySmall,
    )
}
