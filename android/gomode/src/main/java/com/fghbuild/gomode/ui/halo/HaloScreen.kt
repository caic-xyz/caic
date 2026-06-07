// Compose Halo screen for Go Mode native BLE device management.
package com.fghbuild.gomode.ui.halo

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Bluetooth
import androidx.compose.material3.Button
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.ListItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.fghbuild.gomode.halo.HaloConnectionState
import com.fghbuild.gomode.halo.HaloController
import com.fghbuild.gomode.halo.HaloDeviceItem

private val BluetoothPermissions = arrayOf(
    Manifest.permission.BLUETOOTH_SCAN,
    Manifest.permission.BLUETOOTH_CONNECT,
)

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HaloScreen(
    haloController: HaloController,
    onNavigateBack: () -> Unit,
) {
    val state by haloController.state.collectAsStateWithLifecycle()
    val context = LocalContext.current
    val permissionLauncher = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestMultiplePermissions(),
    ) { grants ->
        if (BluetoothPermissions.all { grants[it] == true }) {
            haloController.startScan()
        }
    }
    val hasBluetoothPermissions = BluetoothPermissions.all {
        ContextCompat.checkSelfPermission(context, it) == PackageManager.PERMISSION_GRANTED
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Halo") },
                navigationIcon = {
                    IconButton(onClick = onNavigateBack) {
                        Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .padding(horizontal = 16.dp)
                .verticalScroll(rememberScrollState())
                .testTag("gomode-halo-screen"),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            ListItem(
                headlineContent = { Text("Connection") },
                supportingContent = { Text(state.connectionState.label()) },
                leadingContent = {
                    Icon(
                        Icons.Filled.Bluetooth,
                        contentDescription = null,
                        tint = state.connectionState.tint(),
                    )
                },
                trailingContent = {
                    if (state.connectionState == HaloConnectionState.Connected) {
                        OutlinedButton(onClick = { haloController.disconnect() }) {
                            Text("Disconnect")
                        }
                    }
                },
            )

            ListItem(
                headlineContent = { Text("Auto-connect") },
                supportingContent = { Text("Reconnect to the last selected Halo when support is enabled.") },
                trailingContent = {
                    Switch(
                        checked = state.haloAutoConnect,
                        onCheckedChange = { haloController.updateAutoConnect(it) },
                    )
                },
            )

            val selectedAddress = state.haloAddress
            if (selectedAddress != null) {
                ListItem(
                    headlineContent = { Text("Selected device") },
                    supportingContent = { Text(selectedAddress) },
                    trailingContent = {
                        TextButton(onClick = { haloController.forgetDevice() }) {
                            Text("Forget")
                        }
                    },
                )
            }

            state.error?.let { error ->
                Text(
                    text = error,
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }

            HorizontalDivider(modifier = Modifier.padding(vertical = 8.dp))

            ScanButtonRow(
                scanning = state.connectionState == HaloConnectionState.Scanning,
                hasBluetoothPermissions = hasBluetoothPermissions,
                onScan = { haloController.startScan() },
                onRequestPermissions = { permissionLauncher.launch(BluetoothPermissions) },
                onStopScan = { haloController.stopScan() },
            )

            if (!hasBluetoothPermissions) {
                Text(
                    text = "Bluetooth permissions are required to scan and connect.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }

            Text("Devices", style = MaterialTheme.typography.titleMedium)
            if (state.devices.isEmpty()) {
                Text(
                    text = "No Halo devices found.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else {
                state.devices.forEach { device ->
                    HaloDeviceRow(
                        device = device,
                        selected = device.id == state.selectedDeviceId,
                        connecting = state.connectionState == HaloConnectionState.Connecting,
                        onConnect = { haloController.connect(device.id) },
                    )
                }
            }
        }
    }
}

@Composable
private fun ScanButtonRow(
    scanning: Boolean,
    hasBluetoothPermissions: Boolean,
    onScan: () -> Unit,
    onRequestPermissions: () -> Unit,
    onStopScan: () -> Unit,
) {
    Row(
        modifier = Modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (scanning) {
            OutlinedButton(onClick = onStopScan) {
                Text("Stop scan")
            }
        } else {
            Button(
                onClick = {
                    if (hasBluetoothPermissions) {
                        onScan()
                    } else {
                        onRequestPermissions()
                    }
                },
            ) {
                Text("Scan")
            }
        }
    }
}

@Composable
private fun HaloDeviceRow(
    device: HaloDeviceItem,
    selected: Boolean,
    connecting: Boolean,
    onConnect: () -> Unit,
) {
    ListItem(
        headlineContent = { Text(device.name, fontWeight = FontWeight.Medium) },
        supportingContent = {
            Text("${device.address.ifBlank { "Address unavailable" }} - ${device.rssi} dBm")
        },
        trailingContent = {
            Button(
                onClick = onConnect,
                enabled = !connecting,
            ) {
                if (selected && connecting) {
                    Text("Connecting")
                } else {
                    Text("Connect")
                }
            }
        },
    )
}

@Composable
private fun HaloConnectionState.tint() = when (this) {
    HaloConnectionState.Connected -> MaterialTheme.colorScheme.primary
    HaloConnectionState.Error -> MaterialTheme.colorScheme.error
    else -> MaterialTheme.colorScheme.onSurfaceVariant
}

private fun HaloConnectionState.label(): String = when (this) {
    HaloConnectionState.Disconnected -> "Disconnected"
    HaloConnectionState.Scanning -> "Scanning"
    HaloConnectionState.Connecting -> "Waiting for pairing or connection"
    HaloConnectionState.Connected -> "Connected"
    HaloConnectionState.Error -> "Connection error"
}
