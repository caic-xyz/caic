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
import androidx.compose.material.icons.filled.Bluetooth
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
import androidx.compose.ui.unit.dp
import androidx.hilt.lifecycle.viewmodel.compose.hiltViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.caic.data.ServerConfig
import java.util.Locale

private val VoiceNames = listOf("Orus", "Puck", "Charon", "Kore", "Fenrir", "Aoede")
@OptIn(ExperimentalMaterial3Api::class, ExperimentalLayoutApi::class)
@Composable
fun SettingsScreen(
    viewModel: SettingsViewModel = hiltViewModel(),
    onNavigateBack: () -> Unit,
    onNavigateToHalo: () -> Unit = {},
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
                    placeholder = { Text("http://192.168.1.x:2242") },
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
            Text("Halo", style = MaterialTheme.typography.titleMedium)
            ListItem(
                headlineContent = { Text("Device") },
                supportingContent = {
                    Text(settings.haloAddress ?: "No device selected")
                },
                leadingContent = {
                    Icon(Icons.Filled.Bluetooth, contentDescription = null)
                },
                trailingContent = {
                    TextButton(onClick = onNavigateToHalo) {
                        Text("Manage")
                    }
                },
            )

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
                placeholder = { Text("ghcr.io/caic-xyz/md-user:latest") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            Text("CPU architecture", style = MaterialTheme.typography.titleSmall)
            PlatformOption(
                label = "Native",
                selected = screenState.containerPlatform.isBlank(),
                onClick = { viewModel.updateContainerPlatform("") },
            )
            PlatformOption(
                label = "linux/amd64",
                selected = screenState.containerPlatform == "linux/amd64",
                onClick = { viewModel.updateContainerPlatform("linux/amd64") },
            )
            PlatformOption(
                label = "linux/arm64",
                selected = screenState.containerPlatform == "linux/arm64",
                onClick = { viewModel.updateContainerPlatform("linux/arm64") },
            )
            OutlinedTextField(
                value = screenState.maxCPUs,
                onValueChange = { viewModel.updateMaxCPUs(it) },
                label = { Text("CPU cores") },
                placeholder = { Text("Default") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            Text(
                "Maximum CPU cores for each container (0 = use default).",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("Well-known caches", style = MaterialTheme.typography.titleMedium)
            screenState.wellKnownCachesList.forEach { cache ->
                val currentlyOn = screenState.wellKnownCaches[cache.name] == true
                val cacheSize = screenState.wellKnownCacheSizes[cache.name]
                ListItem(
                    headlineContent = { Text(cache.name) },
                    supportingContent = {
                        Text(cacheSupportingText(cache.description, cacheSize?.sizeBytes, cacheSize?.error))
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
            Text("Custom caches", style = MaterialTheme.typography.titleMedium)
            Text(
                "Persistent host directories mounted into each container for tool caches.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
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
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                TextButton(onClick = { viewModel.addCacheMapping() }) {
                    Icon(Icons.Filled.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Add mapping")
                }
                TextButton(onClick = { viewModel.saveCacheMappings() }) {
                    Icon(Icons.Filled.Check, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Save mappings")
                }
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("Custom mounts", style = MaterialTheme.typography.titleMedium)
            Text(
                "Additional host directories mounted into each container.",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            screenState.customMounts.forEachIndexed { index, mount ->
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    OutlinedTextField(
                        value = mount.hostPath,
                        onValueChange = { viewModel.updateCustomMount(index, it, mount.containerPath) },
                        placeholder = { Text("Host path") },
                        singleLine = true,
                        modifier = Modifier.weight(1f),
                    )
                    Text(" → ", style = MaterialTheme.typography.bodyMedium)
                    OutlinedTextField(
                        value = mount.containerPath,
                        onValueChange = { viewModel.updateCustomMount(index, mount.hostPath, it) },
                        placeholder = { Text("Container path") },
                        singleLine = true,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = { viewModel.removeCustomMount(index) }) {
                        Icon(Icons.Filled.Delete, contentDescription = "Remove")
                    }
                }
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                TextButton(onClick = { viewModel.addCustomMount() }) {
                    Icon(Icons.Filled.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Add mount")
                }
                TextButton(onClick = { viewModel.saveCustomMounts() }) {
                    Icon(Icons.Filled.Check, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(modifier = Modifier.width(4.dp))
                    Text("Save mounts")
                }
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))
            Text("Version", style = MaterialTheme.typography.titleMedium)
            val versionInfo = screenState.versionInfo
            if (screenState.checkingUpdate) {
                Text(
                    "Checking for updates…",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else if (versionInfo != null) {
                Text(
                    buildString {
                        append("Current: caic v${versionInfo.current}")
                        if (versionInfo.latest != null) {
                            if (versionInfo.updateAvailable) {
                                append(" — latest: v${versionInfo.latest} (update available)")
                            } else {
                                append(" — latest: v${versionInfo.latest} (up to date)")
                            }
                        }
                    },
                    style = MaterialTheme.typography.bodyMedium,
                )
                if (versionInfo.checkError != null) {
                    Text(
                        "Check failed: ${versionInfo.checkError}",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
                if (versionInfo.autoUpdateEnabled && versionInfo.updateAvailable) {
                    Button(
                        onClick = { viewModel.triggerUpdate() },
                        enabled = !screenState.updating,
                    ) {
                        Text(if (screenState.updating) "Updating…" else "Update now")
                    }
                }
                if (screenState.updateStatus.isNotEmpty()) {
                    Text(
                        screenState.updateStatus,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            } else if (screenState.serverVersion.isNotEmpty()) {
                Text(
                    "caic v${screenState.serverVersion}",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }

            Spacer(modifier = Modifier.padding(bottom = 16.dp))
        }
    }
}

@Composable
private fun PlatformOption(
    label: String,
    selected: Boolean,
    onClick: () -> Unit,
) {
    ListItem(
        headlineContent = { Text(label) },
        leadingContent = {
            RadioButton(selected = selected, onClick = onClick)
        },
        modifier = Modifier.clickable(onClick = onClick),
    )
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

private fun cacheSupportingText(description: String, sizeBytes: Long?, error: String?): String {
    val size = when {
        error != null -> "error"
        sizeBytes != null -> formatCacheBytes(sizeBytes)
        else -> "pending"
    }
    return if (description.isBlank()) size else "$description · $size"
}

private fun formatCacheBytes(bytes: Long): String {
    if (bytes <= 0L) return "0 B"
    val units = listOf("B", "KiB", "MiB", "GiB", "TiB")
    var value = bytes.toDouble()
    var unitIndex = 0
    while (value >= 1024.0 && unitIndex < units.lastIndex) {
        value /= 1024.0
        unitIndex++
    }
    return if (value >= 10.0 || unitIndex == 0) {
        String.format(Locale.US, "%.0f %s", value, units[unitIndex])
    } else {
        String.format(Locale.US, "%.1f %s", value, units[unitIndex])
    }
}
