// Native settings fallback for configuring the active Go Mode service instance.
package com.fghbuild.gomode.ui.settings

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Bluetooth
import androidx.compose.material3.Button
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.unit.dp
import com.fghbuild.gomode.data.SettingsRepository
import com.fghbuild.gomode.data.SettingsState
import kotlinx.coroutines.launch

@Composable
fun SettingsScreen(
    settings: SettingsState,
    settingsRepository: SettingsRepository,
    onDone: () -> Unit,
    onOpenHalo: () -> Unit,
) {
    val scope = rememberCoroutineScope()
    val activeService = settings.services.firstOrNull { it.id == settings.activeServiceId }
    val initialLabel = activeService?.label ?: SettingsRepository.DEFAULT_SERVICE_LABEL
    var label by remember { mutableStateOf(initialLabel) }
    var url by remember { mutableStateOf(settings.activeServiceURL) }
    var error by remember { mutableStateOf<String?>(null) }

    LaunchedEffect(settings.activeServiceId, settings.activeServiceURL) {
        val active = settings.services.firstOrNull { it.id == settings.activeServiceId }
        label = active?.label ?: SettingsRepository.DEFAULT_SERVICE_LABEL
        url = settings.activeServiceURL
    }

    BackHandler(enabled = settings.activeServiceURL.isNotBlank(), onBack = onDone)

    Scaffold { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .padding(20.dp)
                .testTag("gomode-settings"),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            Text("Go Mode", style = MaterialTheme.typography.headlineMedium)
            Text(
                "Configure a backend-hosted frontend.",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            OutlinedTextField(
                value = label,
                onValueChange = { label = it },
                modifier = Modifier.fillMaxWidth().testTag("gomode-service-label"),
                singleLine = true,
                label = { Text("Service label") },
            )
            OutlinedTextField(
                value = url,
                onValueChange = {
                    url = it
                    error = null
                },
                modifier = Modifier.fillMaxWidth().testTag("gomode-service-url"),
                singleLine = true,
                label = { Text("Server URL") },
                supportingText = error?.let { { Text(it) } },
                isError = error != null,
            )
            Row {
                Button(
                    onClick = {
                        val normalized = SettingsRepository.normalizeURL(url)
                        if (!hasSupportedScheme(normalized)) {
                            error = "Use an http:// or https:// URL."
                            return@Button
                        }
                        scope.launch {
                            settingsRepository.saveActiveService(label = label, url = normalized)
                            onDone()
                        }
                    },
                    modifier = Modifier.testTag("gomode-save-service"),
                ) {
                    Text("Load")
                }
                Spacer(Modifier.width(12.dp))
                if (settings.activeServiceURL.isNotBlank()) {
                    Button(
                        onClick = onDone,
                        modifier = Modifier.testTag("gomode-cancel-settings"),
                    ) {
                        Text("Cancel")
                    }
                }
            }
            Spacer(Modifier.height(8.dp))
            if (settings.services.isNotEmpty()) {
                Text("Configured services", style = MaterialTheme.typography.titleMedium)
                settings.services.forEach { service ->
                    Button(
                        onClick = {
                            scope.launch {
                                settingsRepository.switchService(service.id)
                                onDone()
                            }
                        },
                        modifier = Modifier
                            .fillMaxWidth()
                            .testTag("gomode-service-${service.id}"),
                    ) {
                        Text(service.label.ifBlank { service.url })
                    }
                }
            }
            HorizontalDivider()
            Text("Halo", style = MaterialTheme.typography.titleMedium)
            ListItem(
                headlineContent = { Text("Device") },
                supportingContent = { Text(settings.haloAddress ?: "No device selected") },
                leadingContent = { Icon(Icons.Filled.Bluetooth, contentDescription = null) },
                trailingContent = {
                    Button(
                        onClick = onOpenHalo,
                        modifier = Modifier.testTag("gomode-manage-halo"),
                    ) {
                        Text("Manage")
                    }
                },
            )
        }
    }
}

private fun hasSupportedScheme(url: String): Boolean =
    url.startsWith("http://") || url.startsWith("https://")
