// HaloConnection manages BLE scanning, connection, bonding, MTU negotiation, service discovery, and GATT writes for Halo/Frame devices.
@file:Suppress("MissingPermission")  // Library: permissions are the consuming app's responsibility.

package com.caic.halo.ble

import android.bluetooth.BluetoothAdapter
import android.bluetooth.BluetoothDevice
import android.bluetooth.BluetoothGatt
import android.bluetooth.BluetoothGattCallback
import android.bluetooth.BluetoothGattCharacteristic
import android.bluetooth.BluetoothManager
import android.bluetooth.BluetoothProfile
import android.bluetooth.le.ScanCallback
import android.bluetooth.le.ScanFilter
import android.bluetooth.le.ScanResult
import android.bluetooth.le.ScanSettings
import android.content.Context
import android.os.ParcelUuid
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeoutOrNull
import java.util.UUID
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

class HaloConnection(private val context: Context) {

    private val bluetoothManager: BluetoothManager =
        context.getSystemService(Context.BLUETOOTH_SERVICE) as BluetoothManager

    private val adapter: BluetoothAdapter
        get() = bluetoothManager.adapter
            ?: throw HaloException("Bluetooth not available on this device")

    private var activeGatt: BluetoothGatt? = null

    // Track the current GATT callback so we can swap it temporarily for write ACKs.
    @Volatile
    private var currentGattCallback: BluetoothGattCallback? = null

    companion object {
        // Brilliant Labs BLE service UUIDs
        val LUA_SERVICE_UUID: UUID = UUID.fromString("7A230001-5475-A6A4-654C-8431F6AD49C4")
        val LUA_TX_CHAR_UUID: UUID = UUID.fromString("7A230002-5475-A6A4-654C-8431F6AD49C4")
        val LUA_RX_CHAR_UUID: UUID = UUID.fromString("7A230003-5475-A6A4-654C-8431F6AD49C4")
        val AUDIO_TX_CHAR_UUID: UUID = UUID.fromString("7A230005-5475-A6A4-654C-8431F6AD49C4")

        // DFU service (Frame in bootloader mode)
        val DFU_SERVICE_UUID: UUID = UUID.fromString("0000FE59-0000-1000-8000-00805F9B34FB")

        // Standard BLE battery service
        val BATTERY_SERVICE_UUID: UUID = UUID.fromString("0000180F-0000-1000-8000-00805F9B34FB")
        val BATTERY_LEVEL_CHAR_UUID: UUID = UUID.fromString("00002A19-0000-1000-8000-00805F9B34FB")
    }

    // ---- Scanning ----

    /**
     * Start scanning for Halo/Frame devices advertising the Lua service UUID.
     * Emits [HaloScannedDevice] items (best RSSI per scan cycle).
     * Callers must collect in a coroutine and cancel to stop scanning.
     */
    fun scan(): Flow<HaloScannedDevice> = callbackFlow {
        val scanner = adapter.bluetoothLeScanner
            ?: throw HaloException("BLE scanner not available")

        val filter = ScanFilter.Builder()
            .setServiceUuid(ParcelUuid(LUA_SERVICE_UUID))
            .build()

        val settings = ScanSettings.Builder()
            .setScanMode(ScanSettings.SCAN_MODE_LOW_LATENCY)
            .build()

        val callback = object : ScanCallback() {
            override fun onScanResult(callbackType: Int, result: ScanResult) {
                val device = result.device
                val rssi = result.rssi
                val name = device.name ?: return
                // Filter for Halo or Frame devices by advertised name
                if (name.startsWith("Halo ") || name.startsWith("Frame ")) {
                    trySend(HaloScannedDevice(device, rssi))
                }
            }

            override fun onScanFailed(errorCode: Int) {
                close(HaloException("BLE scan failed with code $errorCode"))
            }
        }

        scanner.startScan(listOf(filter), settings, callback)

        awaitClose {
            try {
                scanner.stopScan(callback)
            } catch (_: Exception) { }
        }
    }

    // ---- Connection ----

    /**
     * Connect to a scanned device and return a fully wired [HaloDevice].
     * Handles bond creation, MTU negotiation, service discovery, and GATT wiring.
     */
    @Suppress("TooGenericExceptionCaught")  // GATT cleanup on any failure requires a broad catch
    suspend fun connect(scanned: HaloScannedDevice, timeoutMs: Long = 15_000): HaloDevice {
        val device = scanned.device
        val gatt = connectGatt(device, timeoutMs)

        return try {
            // Bond + negotiate MTU + request high connection priority
            gatt.device.createBond()

            try {
                gatt.requestConnectionPriority(BluetoothGatt.CONNECTION_PRIORITY_HIGH)
            } catch (_: SecurityException) { }

            // Discover services
            withTimeoutOrNull(timeoutMs) {
                suspendCancellableCoroutine<Unit> { cont ->
                    val success = gatt.discoverServices()
                    if (!success) {
                        cont.resumeWithException(HaloException("Service discovery initiation failed"))
                    }
                    // Result delivered via onServicesDiscovered callback
                }
            } ?: throw HaloException("Service discovery timed out")

            enableServices(device, gatt, timeoutMs)
        } catch (e: HaloException) {
            try { gatt.disconnect() } catch (_: Exception) { }
            try { gatt.close() } catch (_: Exception) { }
            throw e
        } catch (e: Exception) {
            try { gatt.disconnect() } catch (_: Exception) { }
            try { gatt.close() } catch (_: Exception) { }
            throw HaloException("Connection failed: ${e.message}", e)
        }
    }

    private suspend fun connectGatt(device: BluetoothDevice, timeoutMs: Long): BluetoothGatt {
        return withTimeoutOrNull(timeoutMs) {
            suspendCancellableCoroutine<BluetoothGatt> { cont ->
                val gatt = device.connectGatt(
                    context,
                    false, // autoConnect = false (direct connection)
                    object : BluetoothGattCallback() {
                        override fun onConnectionStateChange(
                            gatt: BluetoothGatt,
                            status: Int,
                            newState: Int,
                        ) {
                            when (newState) {
                                BluetoothProfile.STATE_CONNECTED -> {
                                    if (cont.isActive) {
                                        activeGatt = gatt
                                        cont.resume(gatt)
                                    }
                                }
                                BluetoothProfile.STATE_DISCONNECTED -> {
                                    if (cont.isActive) {
                                        cont.resumeWithException(
                                            HaloException("Disconnected during connection (status=$status)")
                                        )
                                    }
                                }
                            }
                        }
                    },
                    BluetoothDevice.TRANSPORT_LE,
                )
                if (gatt == null) {
                    cont.resumeWithException(HaloException("connectGatt returned null"))
                }
            }
        } ?: throw HaloException("Connection timed out after ${timeoutMs}ms")
    }

    private suspend fun enableServices(
        device: BluetoothDevice,
        gatt: BluetoothGatt,
        timeoutMs: Long,
    ): HaloDevice {
        val services = withTimeoutOrNull(timeoutMs) {
            suspendCancellableCoroutine<List<android.bluetooth.BluetoothGattService>> { cont ->
                currentGattCallback = object : BluetoothGattCallback() {
                    override fun onServicesDiscovered(gatt: BluetoothGatt, status: Int) {
                        if (status == BluetoothGatt.GATT_SUCCESS) {
                            cont.resume(gatt.services)
                        } else {
                            cont.resumeWithException(
                                HaloException("Service discovery failed with status=$status")
                            )
                        }
                    }

                    override fun onConnectionStateChange(
                        gatt: BluetoothGatt,
                        status: Int,
                        newState: Int,
                    ) {
                        if (newState == BluetoothProfile.STATE_DISCONNECTED) {
                            // Connection dropped — errors surface through Flow
                        }
                    }
                }
                // Trigger discovery if not already done
                if (gatt.services.isEmpty()) {
                    gatt.discoverServices()
                } else {
                    cont.resume(gatt.services)
                }
            }
        } ?: throw HaloException("Service discovery timed out after ${timeoutMs}ms")

        val luaService = services.find { it.uuid == LUA_SERVICE_UUID }
            ?: throw HaloException("Lua service not found on device")

        val txChar = luaService.getCharacteristic(LUA_TX_CHAR_UUID)
            ?: throw HaloException("LUA TX characteristic not found")
        val rxChar = luaService.getCharacteristic(LUA_RX_CHAR_UUID)
            ?: throw HaloException("LUA RX characteristic not found")
        val audioTxChar = luaService.getCharacteristic(AUDIO_TX_CHAR_UUID)

        val deviceType = if (audioTxChar != null) HaloDeviceType.HALO else HaloDeviceType.FRAME

        // Enable notifications on RX characteristic and build a Flow from it.
        val notificationsFlow: Flow<ByteArray> = callbackFlow {
            currentGattCallback = object : BluetoothGattCallback() {
                override fun onCharacteristicChanged(
                    gatt: BluetoothGatt,
                    characteristic: BluetoothGattCharacteristic,
                    value: ByteArray,
                ) {
                    if (characteristic.uuid == LUA_RX_CHAR_UUID) {
                        trySend(value)
                    }
                }

                override fun onConnectionStateChange(
                    gatt: BluetoothGatt,
                    status: Int,
                    newState: Int,
                ) {
                    if (newState == BluetoothProfile.STATE_DISCONNECTED) {
                        close(HaloException("Device disconnected (status=$status)"))
                    }
                }
            }

            val notifySuccess = gatt.setCharacteristicNotification(rxChar, true)
            if (!notifySuccess) {
                close(HaloException("Failed to enable RX notifications"))
            }

            awaitClose {
                try { gatt.setCharacteristicNotification(rxChar, false) } catch (_: Exception) { }
            }
        }

        val mtu = 517 // After requestMtu(517), but GATT may not expose the result cleanly.

        val maxStringLen = mtu - 3
        // Halo's AUDIO TX characteristic reduces effective MTU by 2 more bytes.
        val maxDataLen = if (deviceType == HaloDeviceType.HALO) mtu - 6 else mtu - 4

        val haloDevice = HaloDevice(
            platformDevice = device,
            type = deviceType,
            txChar = txChar,
            rxChar = rxChar,
            audioTxChar = audioTxChar,
            maxStringLen = maxStringLen,
            maxDataLen = maxDataLen,
            rawNotifications = notificationsFlow,
        )

        // Wire the device's writeSink to actual GATT writes through the active GATT connection.
        // Each write temporarily replaces the GATT callback to wait for the write ACK,
        // then restores the original notification-handling callback.
        @Suppress("DEPRECATION")  // setValue/writeCharacteristic deprecated API 35; fine on minSdk 33
        haloDevice.writeSink = { write ->
            suspendCancellableCoroutine<Unit> { cont ->
                val char = write.characteristic
                char.setWriteType(write.writeType)
                char.setValue(write.data)

                val previousCallback = currentGattCallback
                currentGattCallback = object : BluetoothGattCallback() {
                    override fun onCharacteristicWrite(
                        gatt: BluetoothGatt,
                        characteristic: BluetoothGattCharacteristic,
                        status: Int,
                    ) {
                        currentGattCallback = previousCallback
                        if (status == BluetoothGatt.GATT_SUCCESS) {
                            if (cont.isActive) cont.resume(Unit)
                        } else {
                            if (cont.isActive) {
                                cont.resumeWithException(
                                    HaloException("GATT write failed with status=$status")
                                )
                            }
                        }
                    }

                    override fun onCharacteristicChanged(
                        gatt: BluetoothGatt,
                        characteristic: BluetoothGattCharacteristic,
                        value: ByteArray,
                    ) {
                        // Forward to the original notification handler.
                        previousCallback?.onCharacteristicChanged(gatt, characteristic, value)
                    }

                    override fun onConnectionStateChange(
                        gatt: BluetoothGatt,
                        status: Int,
                        newState: Int,
                    ) {
                        previousCallback?.onConnectionStateChange(gatt, status, newState)
                    }
                }

                val success = gatt.writeCharacteristic(char)
                if (!success) {
                    currentGattCallback = previousCallback
                    if (cont.isActive) {
                        cont.resumeWithException(HaloException("writeCharacteristic returned false"))
                    }
                }
            }
        }

        return haloDevice
    }

    // ---- Disconnect ----

    suspend fun disconnect() {
        try {
            activeGatt?.disconnect()
            activeGatt?.close()
        } catch (_: Exception) { }
        activeGatt = null
        currentGattCallback = null
    }

    // ---- System-connected device query ----

    /**
     * Check if the device with the given address is already connected at the OS level.
     * Returns the [BluetoothDevice] if found, null otherwise.
     */
    fun getSystemConnectedDevice(address: String): BluetoothDevice? {
        @Suppress("SwallowedException")  // Intentional: SecurityException → null means device not connected
        return try {
            val connectedDevices = bluetoothManager.getConnectedDevices(BluetoothProfile.GATT)
            connectedDevices.find { it.address == address }
        } catch (e: SecurityException) {
            null
        }
    }
}
