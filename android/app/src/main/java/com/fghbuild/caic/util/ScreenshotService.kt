// One-shot screenshot capture using MediaProjection with a transient foreground service.
package com.fghbuild.caic.util

import android.app.Activity
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.Bitmap
import android.graphics.PixelFormat
import androidx.core.graphics.createBitmap
import android.hardware.display.DisplayManager
import android.hardware.display.VirtualDisplay
import android.media.ImageReader
import android.media.projection.MediaProjection
import android.media.projection.MediaProjectionManager
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.view.WindowManager
import com.fghbuild.caic.MainActivity
import com.fghbuild.caic.R

/**
 * Minimal foreground service required by Android 14+ for MediaProjection.
 * Starts, counts down 3 seconds (so the user can switch apps), captures one frame,
 * delivers the bitmap via [onScreenshotReady], and stops.
 *
 * On Android 14+, [getMediaProjection] must be called AFTER [startForeground].
 * The activity result code and data are passed as Intent extras so the service
 * can call [MediaProjectionManager.getMediaProjection] itself once the foreground
 * service type is established.
 */
class ScreenshotService : Service() {

    private val handler = Handler(Looper.getMainLooper())

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        ensureChannel()
        startForeground(
            NOTIFICATION_ID,
            buildNotification("Screenshot in $COUNTDOWN_SECONDS..."),
            ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PROJECTION,
        )
        // getMediaProjection must be called AFTER startForeground on Android 14+.
        val resultCode = intent?.getIntExtra(EXTRA_RESULT_CODE, Activity.RESULT_CANCELED)
            ?: Activity.RESULT_CANCELED
        @Suppress("DEPRECATION") // getParcelableExtra(String, Class) requires API 33
        val data = intent?.getParcelableExtra<Intent>(EXTRA_RESULT_DATA)
        if (resultCode != Activity.RESULT_OK || data == null) {
            stopSelf()
            return START_NOT_STICKY
        }
        val mpm = getSystemService(MediaProjectionManager::class.java)
        val projection = mpm.getMediaProjection(resultCode, data)
        if (projection == null) {
            stopSelf()
            return START_NOT_STICKY
        }
        startCountdown(COUNTDOWN_SECONDS) { captureFrame(projection) }
        return START_NOT_STICKY
    }

    private fun startCountdown(remaining: Int, onDone: () -> Unit) {
        if (remaining <= 0) {
            onDone()
            return
        }
        val nm = getSystemService(NotificationManager::class.java)
        nm.notify(NOTIFICATION_ID, buildNotification("Screenshot in $remaining..."))
        handler.postDelayed({ startCountdown(remaining - 1, onDone) }, DELAY_MS)
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun captureFrame(projection: MediaProjection) {
        val nm = getSystemService(NotificationManager::class.java)
        nm.notify(NOTIFICATION_ID, buildNotification())
        val wm = getSystemService(WindowManager::class.java)
        val bounds = wm.currentWindowMetrics.bounds
        val width = bounds.width()
        val height = bounds.height()
        @Suppress("DEPRECATION") // densityDpi not available on WindowMetrics pre-API 34
        val density = resources.displayMetrics.densityDpi

        val reader = ImageReader.newInstance(width, height, PixelFormat.RGBA_8888, 2)
        var virtualDisplay: VirtualDisplay? = null

        // Android 14+ requires registerCallback before createVirtualDisplay.
        projection.registerCallback(object : MediaProjection.Callback() {
            override fun onStop() {
                virtualDisplay?.release()
                reader.close()
                stopSelf()
            }
        }, handler)

        reader.setOnImageAvailableListener({ ir ->
            // acquireLatestImage can transiently return null; wait for the next frame.
            val image = ir.acquireLatestImage() ?: return@setOnImageAvailableListener
            val planes = image.planes
            val buffer = planes[0].buffer
            val pixelStride = planes[0].pixelStride
            val rowStride = planes[0].rowStride
            val rowPadding = rowStride - pixelStride * width
            val bmp = createBitmap(width + rowPadding / pixelStride, height)
            bmp.copyPixelsFromBuffer(buffer)
            // Crop away row padding if present.
            val cropped = if (rowPadding > 0) {
                Bitmap.createBitmap(bmp, 0, 0, width, height).also { bmp.recycle() }
            } else {
                bmp
            }
            image.close()
            onScreenshotReady?.invoke(cropped)
            onScreenshotReady = null
            virtualDisplay?.release()
            projection.stop()
            // Bring the app back to foreground after capture.
            // Allowed from a foreground service with a visible notification (Android BAL exemption).
            startActivity(
                Intent(this, MainActivity::class.java).apply {
                    addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_REORDER_TO_FRONT)
                }
            )
        }, handler)

        virtualDisplay = projection.createVirtualDisplay(
            "screenshot",
            width,
            height,
            density,
            DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR,
            reader.surface,
            null,
            handler,
        )
    }

    private fun ensureChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) != null) return
        val channel = NotificationChannel(
            CHANNEL_ID,
            "Screenshot countdown",
            // HIGH so the countdown banner pops up on screen during the 3-second delay.
            NotificationManager.IMPORTANCE_HIGH,
        )
        channel.setShowBadge(false)
        channel.enableVibration(false)
        channel.setSound(null, null)
        nm.createNotificationChannel(channel)
    }

    private fun buildNotification(title: String = "Capturing screenshot"): Notification =
        Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_screenshot)
            .setContentTitle(title)
            .build()

    companion object {
        private const val CHANNEL_ID = "screenshot_v2"
        private const val NOTIFICATION_ID = 2
        private const val COUNTDOWN_SECONDS = 3
        private const val DELAY_MS = 1000L
        private const val EXTRA_RESULT_CODE = "result_code"
        private const val EXTRA_RESULT_DATA = "result_data"

        /** Callback invoked on the main thread with the captured bitmap. */
        var onScreenshotReady: ((Bitmap) -> Unit)? = null

        fun start(context: Context, resultCode: Int, data: Intent, callback: (Bitmap) -> Unit) {
            onScreenshotReady = callback
            val intent = Intent(context, ScreenshotService::class.java).apply {
                putExtra(EXTRA_RESULT_CODE, resultCode)
                putExtra(EXTRA_RESULT_DATA, data)
            }
            context.startForegroundService(intent)
        }
    }
}
