// HalosideApp: manages the full haloside application lifecycle on the device.
//
// Standard startup sequence (matching brilliant_msg simple_brilliant_app):
//   1. Send break + reset + break to ensure Lua REPL mode
//   2. Upload standard Lua libraries (data, sprite, plain_text, camera, audio, imu)
//   3. Upload the main application Lua file
//   4. Start the haloside event loop
//   5. Exchange messages asynchronously
//
// Lua library assets are bundled in this SDK under assets/halo/.
package com.caic.halo.msg

import android.content.Context
import com.caic.halo.ble.HaloDevice
import com.caic.halo.ble.HaloDeviceType
import java.io.IOException

class HalosideApp(
    private val context: Context,
    private val device: HaloDevice,
) {

    companion object {
        // Lua library filenames (as stored in assets/halo/).
        val DEFAULT_LIBS = listOf(
            "data.min.lua",
            "sprite.min.lua",
            "plain_text.min.lua",
            "camera.min.lua",
            "audio.min.lua",
            "imu.min.lua",
            "battery.min.lua",
            "code.min.lua",
            "image_sprite_block.min.lua",
            "text_sprite_block.min.lua",
        )
    }

    /**
     * Execute the full startup sequence: break/reset/break, upload all libraries,
     * upload [appCode], start the event loop.
     *
     * [appCode] is the Lua source for the main application loop.
     * [libs] are the Lua library filenames to upload from assets/halo/.
     */
    suspend fun start(appCode: String, libs: List<String> = DEFAULT_LIBS) {
        // 1. Break any running script, reset, break again.
        device.sendBreakSignal()
        device.sendResetSignal()
        device.sendBreakSignal()

        // 2. Upload standard libraries.
        for (lib in libs) {
            val content = readAsset("halo/$lib")
            device.uploadFile(lib, content)
        }

        // 3. Clear display (device-aware).
        device.clearDisplay()

        // 4. Upload the main app loop.
        device.uploadFile("main.lua", appCode)

        // 5. Restart Lua runtime → runs main.lua automatically.
        device.sendResetSignal()
    }

    /**
     * Send a typed message to the device via [HaloDevice.sendMessage].
     * The device-side data.lua will route it to the msgCode handler.
     */
    suspend fun send(msgCode: Int, message: TxMessage) {
        device.sendMessage(msgCode, message.pack())
    }

    private fun readAsset(path: String): String {
        return try {
            context.assets.open(path).bufferedReader().use { it.readText() }
        } catch (e: IOException) {
            throw IOException("Failed to read asset '$path': ${e.message}", e)
        }
    }
}
