// Reusable repo chip strip with branch editing and add-repo dropdown.
@file:Suppress("MatchingDeclarationName") // Multiple public declarations: RepoEntry + RepoChipStrip.

package com.fghbuild.caic.ui.common

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp
import com.caic.sdk.v1.BranchInfo
import com.caic.sdk.v1.Repo

data class RepoEntry(val path: String, val branch: String)

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun RepoChipStrip(
    selectedRepos: List<RepoEntry>,
    repos: List<Repo>,
    availableRecent: List<Repo>,
    availableRest: List<Repo>,
    editingBranches: List<BranchInfo>,
    enabled: Boolean,
    onAdd: (String) -> Unit,
    onRemove: (String) -> Unit,
    onSetBranch: (path: String, branch: String) -> Unit,
    onLoadBranches: (String) -> Unit,
    modifier: Modifier = Modifier,
    extraContent: @Composable (() -> Unit)? = null,
) {
    FlowRow(
        horizontalArrangement = Arrangement.spacedBy(4.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp),
        modifier = modifier.fillMaxWidth(),
    ) {
        selectedRepos.forEach { entry ->
            RepoChip(
                entry = entry,
                baseBranch = repos.find { it.path == entry.path }?.baseBranch ?: BranchInfo("main"),
                editingBranches = editingBranches,
                enabled = enabled,
                onRemove = { onRemove(entry.path) },
                onSetBranch = { branch -> onSetBranch(entry.path, branch) },
                onLoadBranches = { onLoadBranches(entry.path) },
            )
        }
        if (availableRecent.isNotEmpty() || availableRest.isNotEmpty()) {
            AddRepoChip(
                availableRecent = availableRecent,
                availableRest = availableRest,
                onAdd = onAdd,
            )
        }
        extraContent?.invoke()
    }
}

@Composable
private fun RepoChip(
    entry: RepoEntry,
    baseBranch: BranchInfo,
    editingBranches: List<BranchInfo>,
    enabled: Boolean,
    onRemove: () -> Unit,
    onSetBranch: (String) -> Unit,
    onLoadBranches: () -> Unit,
) {
    var branchOpen by remember { mutableStateOf(false) }
    var branchFilter by remember { mutableStateOf("") }
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
                        if (branchOpen) {
                            branchFilter = entry.branch
                            onLoadBranches()
                        }
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
            OutlinedTextField(
                value = branchFilter,
                onValueChange = { branchFilter = it },
                placeholder = { Text("Branch name…", style = MaterialTheme.typography.bodySmall) },
                singleLine = true,
                textStyle = MaterialTheme.typography.bodySmall,
                modifier = Modifier.padding(horizontal = 12.dp, vertical = 4.dp).widthIn(min = 180.dp),
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
                keyboardActions = KeyboardActions(onDone = {
                    onSetBranch(branchFilter)
                    branchOpen = false
                }),
            )
            if (branchFilter.isEmpty()) {
                DropdownMenuItem(
                    text = {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text("Default ", color = MaterialTheme.colorScheme.onSurfaceVariant)
                            val prefix = if (!baseBranch.remote.isNullOrEmpty()) "${baseBranch.remote}/" else ""
                            Text("($prefix${baseBranch.name})")
                        }
                    },
                    onClick = { onSetBranch(""); branchOpen = false },
                )
            }
            val filterLower = branchFilter.lowercase()
            editingBranches.filter { filterLower.isEmpty() || it.name.lowercase().contains(filterLower) }
                .forEach { branch ->
                    DropdownMenuItem(
                        text = {
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                Text(
                                    branch.name,
                                    fontWeight = if (branch.name == entry.branch) FontWeight.Bold else FontWeight.Normal,
                                )
                                if (!branch.remote.isNullOrEmpty()) {
                                    Text(
                                        " (${branch.remote})",
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        style = MaterialTheme.typography.bodySmall,
                                    )
                                }
                            }
                        },
                        onClick = { onSetBranch(branch.name); branchOpen = false },
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
    var repoFilter by remember { mutableStateOf("") }
    LaunchedEffect(expanded) { if (!expanded) repoFilter = "" }
    val filterLower = repoFilter.lowercase()
    val filteredRecent = availableRecent.sortedBy { it.path }
        .filter { filterLower.isEmpty() || it.path.lowercase().contains(filterLower) }
    val filteredRest = availableRest
        .filter { filterLower.isEmpty() || it.path.lowercase().contains(filterLower) }
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
            OutlinedTextField(
                value = repoFilter,
                onValueChange = { repoFilter = it },
                placeholder = { Text("Filter repositories…", style = MaterialTheme.typography.bodySmall) },
                singleLine = true,
                textStyle = MaterialTheme.typography.bodySmall,
                modifier = Modifier
                    .padding(horizontal = 12.dp, vertical = 4.dp)
                    .widthIn(min = 180.dp)
                    .onKeyEvent { keyEvent ->
                        if (keyEvent.key == Key.Escape && keyEvent.type == KeyEventType.KeyDown) {
                            expanded = false
                            true
                        } else {
                            false
                        }
                    },
                keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                keyboardActions = KeyboardActions(onSearch = {}),
            )
            if (filteredRecent.isNotEmpty()) {
                Text(
                    text = "Recent",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                )
                filteredRecent.forEach { repo ->
                    DropdownMenuItem(
                        text = { Text(repo.path, style = MaterialTheme.typography.bodyMedium) },
                        onClick = { onAdd(repo.path); expanded = false },
                    )
                }
            }
            if (filteredRest.isNotEmpty()) {
                if (filteredRecent.isNotEmpty()) {
                    Text(
                        text = "All repositories",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                    )
                }
                filteredRest.forEach { repo ->
                    DropdownMenuItem(
                        text = { Text(repo.path, style = MaterialTheme.typography.bodyMedium) },
                        onClick = { onAdd(repo.path); expanded = false },
                    )
                }
            }
            if (filteredRecent.isEmpty() && filteredRest.isEmpty()) {
                Text(
                    text = "No matches",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                )
            }
        }
    }
}
