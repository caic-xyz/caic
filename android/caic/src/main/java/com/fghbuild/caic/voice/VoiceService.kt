// Foreground service that keeps the app alive during a voice session.
package com.fghbuild.caic.voice

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder
import com.fghbuild.caic.MainActivity
import com.fghbuild.caic.R

class VoiceService : Service() {

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        ensureChannel()
        val notification = buildNotification()
        startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE)
        return START_STICKY
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun ensureChannel() {
        val nm = getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL_ID) != null) return
        val channel = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.voice_channel_name),
            NotificationManager.IMPORTANCE_LOW,
        )
        channel.setShowBadge(false)
        nm.createNotificationChannel(channel)
    }

    private fun buildNotification(): Notification {
        val tapIntent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        val pendingIntent = PendingIntent.getActivity(
            this, 0, tapIntent, PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_mic)
            .setContentTitle(getString(R.string.voice_notification_title))
            .setContentText(getString(R.string.voice_notification_text))
            .setContentIntent(pendingIntent)
            .setOngoing(true)
            .build()
    }

    companion object {
        private const val CHANNEL_ID = "voice_session"
        private const val NOTIFICATION_ID = 1

        fun start(context: Context) {
            context.startForegroundService(Intent(context, VoiceService::class.java))
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, VoiceService::class.java))
        }
    }
}
