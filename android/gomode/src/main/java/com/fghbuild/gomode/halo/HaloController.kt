// HaloController manages Go Mode Halo BLE scanning, connection state, and selected-device settings.
@file:Suppress("MissingPermission") // The Compose screen requests Bluetooth permissions before BLE operations.

package com.fghbuild.gomode.halo

import android.content.Context
import com.caic.halo.ble.HaloConnection
import com.caic.halo.ble.HaloScannedDevice
import com.fghbuild.gomode.data.SettingsRepository
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.catch
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

enum class HaloConnectionState {
    Disconnected,
    Scanning,
    Connecting,
    Connected,
    Error,
}

data class HaloDeviceItem(
    val id: String,
    val name: String,
    val address: String,
    val rssi: Int,
)

data class HaloScreenState(
    val connectionState: HaloConnectionState = HaloConnectionState.Disconnected,
    val devices: List<HaloDeviceItem> = emptyList(),
    val haloAddress: String? = null,
    val haloAutoConnect: Boolean = false,
    val selectedDeviceId: String? = null,
    val error: String? = null,
)

class HaloController(
    context: Context,
    private val settingsRepository: SettingsRepository,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val haloConnection = HaloConnection(context.applicationContext)
    private val scannedDevices = mutableMapOf<String, HaloScannedDevice>()
    private var scanJob: Job? = null

    private val _state = MutableStateFlow(HaloScreenState())
    val state: StateFlow<HaloScreenState> = _state.asStateFlow()

    init {
        scope.launch {
            settingsRepository.settings.collect { settings ->
                _state.update {
                    it.copy(
                        haloAddress = settings.haloAddress,
                        haloAutoConnect = settings.haloAutoConnect,
                    )
                }
            }
        }
    }

    fun startScan() {
        if (scanJob?.isActive == true) return
        scannedDevices.clear()
        _state.update {
            it.copy(
                connectionState = HaloConnectionState.Scanning,
                devices = emptyList(),
                error = null,
                selectedDeviceId = null,
            )
        }
        scanJob = scope.launch {
            haloConnection.scan()
                .catch { error ->
                    stopScan()
                    _state.update {
                        it.copy(
                            connectionState = HaloConnectionState.Error,
                            error = error.message ?: "BLE scan failed",
                        )
                    }
                }
                .collect { scanned ->
                    val item = scanned.toItem()
                    scannedDevices[item.id] = scanned
                    _state.update { current ->
                        val devices = current.devices
                            .filterNot { it.id == item.id }
                            .plus(item)
                            .sortedBy { it.name }
                        current.copy(devices = devices)
                    }
                }
        }
    }

    fun stopScan() {
        scanJob?.cancel()
        scanJob = null
        if (_state.value.connectionState == HaloConnectionState.Scanning) {
            _state.update { it.copy(connectionState = HaloConnectionState.Disconnected) }
        }
    }

    fun connect(deviceId: String) {
        val scanned = scannedDevices[deviceId] ?: return
        stopScan()
        _state.update {
            it.copy(
                connectionState = HaloConnectionState.Connecting,
                selectedDeviceId = deviceId,
                error = null,
            )
        }
        scope.launch {
            runCatching { haloConnection.connect(scanned) }
                .onSuccess {
                    settingsRepository.updateHaloAddress(scanned.toItem().address)
                    _state.update {
                        it.copy(
                            connectionState = HaloConnectionState.Connected,
                            error = null,
                        )
                    }
                }
                .onFailure { error ->
                    _state.update {
                        it.copy(
                            connectionState = HaloConnectionState.Error,
                            error = error.message ?: "Halo connection failed",
                        )
                    }
                }
        }
    }

    fun disconnect() {
        scope.launch {
            runCatching { haloConnection.disconnect() }
            _state.update {
                it.copy(
                    connectionState = HaloConnectionState.Disconnected,
                    selectedDeviceId = null,
                )
            }
        }
    }

    fun forgetDevice() {
        scope.launch {
            settingsRepository.updateHaloAddress(null)
        }
    }

    fun updateAutoConnect(enabled: Boolean) {
        scope.launch {
            settingsRepository.updateHaloAutoConnect(enabled)
        }
    }

    fun close() {
        stopScan()
        scope.cancel()
    }
}

private fun HaloScannedDevice.toItem(): HaloDeviceItem {
    val address = runCatching { device.address }.getOrNull().orEmpty()
    val name = runCatching { device.name }.getOrNull()?.ifBlank { null } ?: "Halo"
    val id = address.ifBlank { name }
    return HaloDeviceItem(
        id = id,
        name = name,
        address = address,
        rssi = rssi,
    )
}
