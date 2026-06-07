// ViewModel for the Halo screen: scan results, connection state, and persisted device preferences.
@file:Suppress("MissingPermission") // UI requests Bluetooth permissions before scanning or connecting.

package com.fghbuild.caic.halo

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.caic.halo.ble.HaloScannedDevice
import com.fghbuild.caic.data.SettingsRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.catch
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import javax.inject.Inject

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

@HiltViewModel
class HaloViewModel @Inject constructor(
    private val haloService: HaloService,
    private val settingsRepository: SettingsRepository,
) : ViewModel() {
    private val _state = MutableStateFlow(HaloScreenState())
    val state: StateFlow<HaloScreenState> = _state.asStateFlow()

    private val scannedDevices = mutableMapOf<String, HaloScannedDevice>()
    private var scanJob: Job? = null

    init {
        viewModelScope.launch {
            haloService.state.collect { connectionState ->
                _state.update { it.copy(connectionState = connectionState) }
            }
        }
        viewModelScope.launch {
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
        _state.update { it.copy(devices = emptyList(), error = null, selectedDeviceId = null) }
        scanJob = viewModelScope.launch {
            haloService.scan()
                .catch { error ->
                    haloService.stopScan()
                    _state.update { it.copy(error = error.message ?: "BLE scan failed") }
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
        haloService.stopScan()
    }

    fun connect(deviceId: String) {
        val scanned = scannedDevices[deviceId] ?: return
        stopScan()
        _state.update { it.copy(selectedDeviceId = deviceId, error = null) }
        viewModelScope.launch {
            val result = haloService.connect(scanned)
            result.fold(
                onSuccess = {
                    settingsRepository.updateHaloAddress(scanned.toItem().address)
                    _state.update { it.copy(error = null) }
                },
                onFailure = { error ->
                    _state.update { it.copy(error = error.message ?: "Halo connection failed") }
                },
            )
        }
    }

    fun disconnect() {
        viewModelScope.launch {
            haloService.disconnect()
        }
    }

    fun forgetDevice() {
        viewModelScope.launch {
            settingsRepository.updateHaloAddress(null)
        }
    }

    fun updateAutoConnect(enabled: Boolean) {
        viewModelScope.launch {
            settingsRepository.updateHaloAutoConnect(enabled)
        }
    }

    override fun onCleared() {
        stopScan()
        super.onCleared()
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
