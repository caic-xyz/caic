// ServiceNotificationPublisher posts monitored service events through Go Mode's native alert channel.
package com.fghbuild.gomode.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.content.ContextCompat
import com.fghbuild.gomode.MainActivity
import com.fghbuild.gomode.R

class ServiceNotificationPublisher(private val context: Context) {
    fun publish(notification: ServiceNotification) {
        if (!hasNotificationPermission()) return
        val manager = context.getSystemService(NotificationManager::class.java)
        ensureChannel(manager)
        manager.notify(
            notification.id.hashCode(),
            Notification.Builder(context, ChannelID)
                .setSmallIcon(android.R.drawable.ic_dialog_info)
                .setContentTitle(notification.title)
                .setContentText(notification.text)
                .setAutoCancel(true)
                .setContentIntent(openAppIntent())
                .build(),
        )
    }

    private fun hasNotificationPermission(): Boolean =
        ContextCompat.checkSelfPermission(context, android.Manifest.permission.POST_NOTIFICATIONS) ==
            android.content.pm.PackageManager.PERMISSION_GRANTED

    private fun ensureChannel(manager: NotificationManager) {
        if (manager.getNotificationChannel(ChannelID) != null) return
        manager.createNotificationChannel(
            NotificationChannel(
                ChannelID,
                context.getString(R.string.service_alerts_notification_channel),
                NotificationManager.IMPORTANCE_DEFAULT,
            ),
        )
    }

    private fun openAppIntent(): PendingIntent = PendingIntent.getActivity(
        context,
        0,
        Intent(context, MainActivity::class.java),
        PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
    )

    private companion object {
        const val ChannelID = "gomode_service_alerts"
    }
}
