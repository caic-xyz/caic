// Compose Settings screen for configuring servers and voice.
package com.fghbuild.caic.ui.settings

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Checkbox
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.hilt.lifecycle.viewmodel.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.caic.data.ServerConfig

private val VoiceNames = listOf("Orus", "Puck", "Charon", "Kore", "Fenrir", "Aoede")
private val GitHubTokenOptions = listOf("none" to "None (default)", "read-write" to "Read-write")

@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun SettingsScreen(
    viewModel: SettingsViewModel = hiltViewModel(),
    onNavigateBack: () -> Unit,
) {
    val screenState by viewModel.state.collectAsStateWithLifecycle()
    val settings = screenState.settings

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Settings") },
                navigationIcon = {
                    IconButton(onClick = onNavigateBack) {
                        Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
            )
        },
    ) { innerPadding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(innerPadding)
                .padding(horizontal = 16.dp)
                .verticalScroll(rememberScrollState()),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            // Server section
            Text("Server", style = MaterialTheme.typography.titleMedium)

            ServerList(
                servers = settings.servers,
                activeServerId = settings.activeServerId,
                onSelect = { viewModel.switchServer(it) },
                onRemove = { viewModel.removeServer(it) },
            )

            TextButton(onClick = { viewModel.addServer() }) {
                Icon(Icons.Filled.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(modifier = Modifier.width(4.dp))
                Text("Add Server")
            }

            if (settings.servers.isNotEmpty()) {
                OutlinedTextField(
                    value = screenState.serverLabel,
                    onValueChange = { viewModel.updateServerLabel(it) },
                    label = { Text("Name") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )

                OutlinedTextField(
                    value = settings.serverURL,
                    onValueChange = { viewModel.updateServerURL(it) },
                    label = { Text("URL") },
                    placeholder = { Text("http://192.168.1.x:8080") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )

                Row(verticalAlignment = Alignment.CenterVertically) {
                    Button(onClick = { viewModel.testConnection() }) {
                        Text("Test Connection")
                    }
                    Spacer(modifier = Modifier.width(12.dp))
                    ConnectionStatusIndicator(screenState.connectionStatus)
                }
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))

            // Voice section
            Text("Voice", style = MaterialTheme.typography.titleMedium)

            ListItem(
                headlineContent = { Text("Voice Enabled") },
                trailingContent = {
                    Switch(
                        checked = settings.voiceEnabled,
                        onCheckedChange = { viewModel.updateVoiceEnabled(it) },
                    )
                },
            )

            if (settings.voiceEnabled) {
                FlowRow(
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    VoiceNames.forEach { name ->
                        FilterChip(
                            selected = settings.voiceName == name,
                            onClick = { viewModel.updateVoiceName(name) },
                            label = { Text(name) },
                        )
                    }
                }
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("GitHub", style = MaterialTheme.typography.titleMedium)
            ListItem(
                headlineContent = { Text("Auto-fix CI failures") },
                supportingContent = { Text("Automatically start a new task when PR CI fails") },
                trailingContent = {
                    Switch(
                        checked = screenState.autoFixCI,
                        onCheckedChange = { viewModel.updateAutoFixCI(it) },
                    )
                },
            )
            ListItem(
                headlineContent = { Text("Auto-fix PRs") },
                supportingContent = { Text("Automatically review and fix opened pull requests") },
                trailingContent = {
                    Switch(
                        checked = screenState.autoFixPR,
                        onCheckedChange = { viewModel.updateAutoFixPR(it) },
                    )
                },
            )

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("Container", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(
                value = screenState.baseImage,
                onValueChange = { viewModel.updateBaseImage(it) },
                label = { Text("Docker image") },
                placeholder = { Text("ghcr.io/caic-xyz/md:latest") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            Text("GitHub token access", style = MaterialTheme.typography.bodyMedium, modifier = Modifier.padding(top = 8.dp))
            GitHubTokenOptions.forEach { (value, label) ->
                val selected = (screenState.gitHubTokenAccess.ifEmpty { "none" }) == value
                ListItem(
                    headlineContent = { Text(label) },
                    leadingContent = { RadioButton(selected = selected, onClick = { viewModel.updateGitHubTokenAccess(value) }) },
                    modifier = Modifier.clickable { viewModel.updateGitHubTokenAccess(value) },
                    colors = ListItemDefaults.colors(containerColor = Color.Transparent),
                )
            }
            Text(
                "Controls whether the GitHub token is injected into containers.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("Well-known caches", style = MaterialTheme.typography.titleMedium)
            screenState.wellKnownCachesList.forEach { cache ->
                val currentlyOn = screenState.wellKnownCaches[cache.name] != false
                ListItem(
                    headlineContent = { Text(cache.name) },
                    supportingContent = if (cache.description.isNotBlank()) {
                        { Text(cache.description) }
                    } else {
                        null
                    },
                    leadingContent = {
                        Checkbox(
                            checked = currentlyOn,
                            onCheckedChange = { viewModel.updateWellKnownCache(cache.name, it) },
                        )
                    },
                    modifier = Modifier.clickable { viewModel.updateWellKnownCache(cache.name, !currentlyOn) },
                )
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("Custom cache mappings", style = MaterialTheme.typography.titleMedium)
            screenState.cacheMappings.forEachIndexed { index, mapping ->
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    OutlinedTextField(
                        value = mapping.hostPath,
                        onValueChange = { viewModel.updateCacheMapping(index, it, mapping.containerPath) },
                        placeholder = { Text("Host path") },
                        singleLine = true,
                        modifier = Modifier.weight(1f),
                    )
                    Text(" → ", style = MaterialTheme.typography.bodyMedium)
                    OutlinedTextField(
                        value = mapping.containerPath,
                        onValueChange = { viewModel.updateCacheMapping(index, mapping.hostPath, it) },
                        placeholder = { Text("Container path") },
                        singleLine = true,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = { viewModel.removeCacheMapping(index) }) {
                        Icon(Icons.Filled.Delete, contentDescription = "Remove")
                    }
                }
            }
            TextButton(onClick = { viewModel.addCacheMapping() }) {
                Icon(Icons.Filled.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(modifier = Modifier.width(4.dp))
                Text("Add mapping")
            }

            if (screenState.serverVersion.isNotEmpty()) {
                Text(
                    "caic v${screenState.serverVersion}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(vertical = 16.dp),
                    textAlign = TextAlign.Center,
                )
            }
        }
    }
}

@Composable
private fun ServerList(
    servers: List<ServerConfig>,
    activeServerId: String,
    onSelect: (String) -> Unit,
    onRemove: (String) -> Unit,
) {
    servers.forEach { server ->
        val isActive = server.id == activeServerId
        val displayName = server.label.ifBlank { server.url.ifBlank { "Untitled" } }
        ListItem(
            headlineContent = { Text(displayName, maxLines = 1) },
            supportingContent = if (server.label.isNotBlank() && server.url.isNotBlank()) {
                { Text(server.url, maxLines = 1, style = MaterialTheme.typography.bodySmall) }
            } else {
                null
            },
            leadingContent = {
                RadioButton(selected = isActive, onClick = { onSelect(server.id) })
            },
            trailingContent = if (servers.size > 1) {
                {
                    IconButton(onClick = { onRemove(server.id) }) {
                        Icon(Icons.Filled.Delete, contentDescription = "Remove server")
                    }
                }
            } else {
                null
            },
            colors = ListItemDefaults.colors(
                containerColor = if (isActive) {
                    MaterialTheme.colorScheme.surfaceVariant
                } else {
                    MaterialTheme.colorScheme.surface
                },
            ),
            modifier = Modifier.clickable { onSelect(server.id) },
        )
    }
}

@Composable
private fun ConnectionStatusIndicator(status: ConnectionStatus) {
    when (status) {
        ConnectionStatus.Idle -> {}
        ConnectionStatus.Testing -> CircularProgressIndicator(modifier = Modifier.size(24.dp))
        ConnectionStatus.Success -> Icon(
            Icons.Filled.Check,
            contentDescription = "Connection successful",
            tint = Color(0xFF4CAF50),
            modifier = Modifier.size(24.dp),
        )
        ConnectionStatus.Failed -> Icon(
            Icons.Filled.Close,
            contentDescription = "Connection failed",
            tint = Color(0xFFF44336),
            modifier = Modifier.size(24.dp),
        )
    }
}
