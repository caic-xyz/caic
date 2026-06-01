// HaloConnection: BLE scanning, connection, bonding, MTU negotiation, service discovery, and GATT writes for Halo/Frame devices.
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
import kotlinx.coroutines.channels.awaitClose
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.callbackFlow
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withTimeoutOrNull
import kotlin.coroutines.resume
import kotlin.coroutines.resumeWithException

open class HaloConnection(private val context: Context) {

    private val bluetoothManager: BluetoothManager =
        context.getSystemService(Context.BLUETOOTH_SERVICE) as BluetoothManager

    private val adapter: BluetoothAdapter
        get() = bluetoothManager.adapter
            ?: throw HaloException("Bluetooth not available on this device")

    private var activeGatt: BluetoothGatt? = null

    @Volatile
    private var currentGattCallback: BluetoothGattCallback? = null

    /** Negotiated MTU — stored after connection, used by payload limit calculations. */
    private var negotiatedMtu: Int = 23

    // ---- Scanning ----

    open fun scan(): Flow<HaloScannedDevice> = callbackFlow {
        val scanner = adapter.bluetoothLeScanner
            ?: throw HaloException("BLE scanner not available")

        val filter = ScanFilter.Builder()
            .setServiceUuid(ParcelUuid(HaloServiceDiscovery.LUA_SERVICE))
            .build()

        val settings = ScanSettings.Builder()
            .setScanMode(ScanSettings.SCAN_MODE_LOW_LATENCY)
            .build()

        val callback = object : ScanCallback() {
            override fun onScanResult(callbackType: Int, result: ScanResult) {
                val name = result.device.name ?: return
                if (name.startsWith("Halo ") || name.startsWith("Frame ")) {
                    trySend(HaloScannedDevice(result.device, result.rssi))
                }
            }

            override fun onScanFailed(errorCode: Int) {
                close(HaloException("BLE scan failed with code $errorCode"))
            }
        }

        scanner.startScan(listOf(filter), settings, callback)
        awaitClose { try { scanner.stopScan(callback) } catch (_: Exception) { } }
    }

    // ---- Connection ----

    /** Overridable for testing: create the GATT connection. */
    protected open suspend fun connectGatt(device: BluetoothDevice, timeoutMs: Long): BluetoothGatt {
        return withTimeoutOrNull(timeoutMs) {
            suspendCancellableCoroutine<BluetoothGatt> { cont ->
                val gatt = device.connectGatt(
                    context, false,
                    object : BluetoothGattCallback() {
                        override fun onConnectionStateChange(g: BluetoothGatt, status: Int, newState: Int) {
                            when (newState) {
                                BluetoothProfile.STATE_CONNECTED -> {
                                    if (cont.isActive) { activeGatt = g; cont.resume(g) }
                                }
                                BluetoothProfile.STATE_DISCONNECTED -> {
                                    if (cont.isActive) cont.resumeWithException(
                                        HaloException("Disconnected during connection (status=$status)")
                                    )
                                }
                            }
                        }
                    },
                    BluetoothDevice.TRANSPORT_LE,
                )
                if (gatt == null) cont.resumeWithException(HaloException("connectGatt returned null"))
            }
        } ?: throw HaloException("Connection timed out after ${timeoutMs}ms")
    }

    /** Overridable for testing: store the negotiated MTU. */
    protected open fun onMtuNegotiated(mtu: Int) {
        negotiatedMtu = mtu
    }

    @Suppress("TooGenericExceptionCaught")
    open suspend fun connect(scanned: HaloScannedDevice, timeoutMs: Long = 15_000): HaloDevice {
        val device = scanned.device
        val gatt = connectGatt(device, timeoutMs)

        return try {
            gatt.device.createBond()
            try { gatt.requestConnectionPriority(BluetoothGatt.CONNECTION_PRIORITY_HIGH) } catch (_: SecurityException) { }

            // Request MTU 517; store the negotiated value when the callback fires.
            currentGattCallback = object : BluetoothGattCallback() {
                override fun onMtuChanged(g: BluetoothGatt, mtu: Int, status: Int) {
                    if (status == BluetoothGatt.GATT_SUCCESS) onMtuNegotiated(mtu)
                }
            }
            gatt.requestMtu(517)

            withTimeoutOrNull(timeoutMs) {
                suspendCancellableCoroutine<Unit> { cont ->
                    val success = gatt.discoverServices()
                    if (!success) cont.resumeWithException(HaloException("Service discovery initiation failed"))
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

    /** Overridable for testing: create a HaloDevice from discovered services. */
    protected open fun createHaloDevice(
        platformDevice: BluetoothDevice,
        type: HaloDeviceType,
        txChar: BluetoothGattCharacteristic,
        rxChar: BluetoothGattCharacteristic,
        audioTxChar: BluetoothGattCharacteristic?,
        limits: HaloServiceDiscovery.PayloadLimits,
        notificationsFlow: Flow<ByteArray>,
    ): HaloDevice {
        return HaloDevice(
            platformDevice = platformDevice,
            type = type,
            txChar = txChar,
            rxChar = rxChar,
            audioTxChar = audioTxChar,
            maxStringLen = limits.maxStringLen,
            maxDataLen = limits.maxDataLen,
            rawNotifications = notificationsFlow,
        )
    }

    /** Overridable for testing: wire the writeSink to GATT writes. */
    protected open fun wireWriteSink(device: HaloDevice, gatt: BluetoothGatt) {
        @Suppress("DEPRECATION")
        device.writeSink = { write ->
            suspendCancellableCoroutine<Unit> { cont ->
                val char = write.characteristic
                char.setWriteType(write.writeType)
                char.setValue(write.data)

                val previousCallback = currentGattCallback
                currentGattCallback = object : BluetoothGattCallback() {
                    override fun onCharacteristicWrite(
                        g: BluetoothGatt, c: BluetoothGattCharacteristic, status: Int,
                    ) {
                        currentGattCallback = previousCallback
                        if (status == BluetoothGatt.GATT_SUCCESS) {
                            if (cont.isActive) cont.resume(Unit)
                        } else {
                            if (cont.isActive) cont.resumeWithException(
                                HaloException("GATT write failed with status=$status")
                            )
                        }
                    }

                    override fun onCharacteristicChanged(
                        g: BluetoothGatt, c: BluetoothGattCharacteristic, value: ByteArray,
                    ) {
                        previousCallback?.onCharacteristicChanged(g, c, value)
                    }

                    override fun onConnectionStateChange(g: BluetoothGatt, status: Int, newState: Int) {
                        previousCallback?.onConnectionStateChange(g, status, newState)
                    }
                }

                val success = gatt.writeCharacteristic(char)
                if (!success) {
                    currentGattCallback = previousCallback
                    if (cont.isActive) cont.resumeWithException(
                        HaloException("writeCharacteristic returned false")
                    )
                }
            }
        }
    }

    private suspend fun enableServices(
        device: BluetoothDevice,
        gatt: BluetoothGatt,
        timeoutMs: Long,
    ): HaloDevice {
        val services = withTimeoutOrNull(timeoutMs) {
            suspendCancellableCoroutine<List<android.bluetooth.BluetoothGattService>> { cont ->
                currentGattCallback = object : BluetoothGattCallback() {
                    override fun onServicesDiscovered(g: BluetoothGatt, status: Int) {
                        if (status == BluetoothGatt.GATT_SUCCESS) cont.resume(g.services)
                        else cont.resumeWithException(
                            HaloException("Service discovery failed with status=$status")
                        )
                    }
                    override fun onConnectionStateChange(g: BluetoothGatt, status: Int, newState: Int) {
                        if (newState == BluetoothProfile.STATE_DISCONNECTED) { /* errors surface through Flow */ }
                    }
                }
                if (gatt.services.isEmpty()) gatt.discoverServices() else cont.resume(gatt.services)
            }
        } ?: throw HaloException("Service discovery timed out after ${timeoutMs}ms")

        val luaService = HaloServiceDiscovery.findLuaService(services)
            ?: throw HaloException("Lua service not found on device")

        val deviceType = HaloServiceDiscovery.deviceType(luaService)
        val (txChar, rxChar, audioTxChar) = HaloServiceDiscovery.requiredCharacteristics(luaService)
        val limits = HaloServiceDiscovery.payloadLimits(negotiatedMtu, deviceType)

        // Enable RX notifications.
        val notificationsFlow: Flow<ByteArray> = callbackFlow {
            currentGattCallback = object : BluetoothGattCallback() {
                override fun onCharacteristicChanged(
                    g: BluetoothGatt, c: BluetoothGattCharacteristic, value: ByteArray,
                ) {
                    if (c.uuid == HaloServiceDiscovery.LUA_RX_CHAR) trySend(value)
                }
                override fun onConnectionStateChange(g: BluetoothGatt, status: Int, newState: Int) {
                    if (newState == BluetoothProfile.STATE_DISCONNECTED) {
                        close(HaloException("Device disconnected (status=$status)"))
                    }
                }
            }

            if (!gatt.setCharacteristicNotification(rxChar, true)) {
                close(HaloException("Failed to enable RX notifications"))
            }
            awaitClose {
                try { gatt.setCharacteristicNotification(rxChar, false) } catch (_: Exception) { }
            }
        }

        val haloDevice = createHaloDevice(device, deviceType, txChar, rxChar, audioTxChar, limits, notificationsFlow)
        wireWriteSink(haloDevice, gatt)
        return haloDevice
    }

    // ---- Disconnect ----

    open suspend fun disconnect() {
        try { activeGatt?.disconnect() } catch (_: Exception) { }
        try { activeGatt?.close() } catch (_: Exception) { }
        activeGatt = null
        currentGattCallback = null
    }

    // ---- System-connected device query ----

    fun getSystemConnectedDevice(address: String): BluetoothDevice? {
        @Suppress("SwallowedException")
        return try {
            bluetoothManager.getConnectedDevices(BluetoothProfile.GATT).find { it.address == address }
        } catch (e: SecurityException) {
            null
        }
    }
}
