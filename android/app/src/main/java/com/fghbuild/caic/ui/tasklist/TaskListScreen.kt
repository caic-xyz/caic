// Task list screen with creation form, usage badges, and task navigation.
package com.fghbuild.caic.ui.tasklist

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Send
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
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun TaskListScreen(
    viewModel: TaskListViewModel = hiltViewModel(),
    onNavigateToSettings: () -> Unit = {},
    onNavigateToTask: (String) -> Unit = {},
) {
    val state by viewModel.state.collectAsStateWithLifecycle()

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("caic") },
                actions = {
                    state.usage?.let { UsageBadges(it) }
                    if (state.serverConfigured) {
                        ConnectionDot(connected = state.connected)
                    }
                    IconButton(onClick = onNavigateToSettings) {
                        Icon(Icons.Default.Settings, contentDescription = "Settings")
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
    LazyColumn(
        modifier = Modifier
            .fillMaxSize()
            .padding(padding),
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

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun TaskCreationForm(state: TaskListState, viewModel: TaskListViewModel) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        if (state.repos.isNotEmpty()) {
            DropdownField(
                label = "Repository",
                selected = state.selectedRepo,
                options = state.repos.map { it.path },
                onSelect = viewModel::selectRepo,
            )
        }

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

        Row(
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            OutlinedTextField(
                value = state.prompt,
                onValueChange = viewModel::updatePrompt,
                label = { Text("Prompt") },
                modifier = Modifier.weight(1f),
                singleLine = true,
                enabled = !state.submitting,
            )
            if (state.submitting) {
                CircularProgressIndicator(modifier = Modifier.size(24.dp))
            } else {
                IconButton(
                    onClick = viewModel::createTask,
                    enabled = state.prompt.isNotBlank() && state.selectedRepo.isNotBlank(),
                ) {
                    Icon(Icons.Default.Send, contentDescription = "Create task")
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DropdownField(
    label: String,
    selected: String,
    options: List<String>,
    onSelect: (String) -> Unit,
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
            options.forEach { option ->
                DropdownMenuItem(
                    text = { Text(option) },
                    onClick = {
                        onSelect(option)
                        expanded = false
                    },
                )
            }
        }
    }
}
