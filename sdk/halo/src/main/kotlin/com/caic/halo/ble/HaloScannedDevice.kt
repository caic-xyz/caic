// Scan result produced by HaloScanner, carrying the platform BluetoothDevice and RSSI.
package com.caic.halo.ble

import android.bluetooth.BluetoothDevice

data class HaloScannedDevice(
    val device: BluetoothDevice,
    val rssi: Int,
)
