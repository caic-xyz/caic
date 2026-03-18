// Manages Android notifications for tasks that need user attention, with auto-dismiss on state change.
package com.fghbuild.caic.data

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat
import com.caic.sdk.v1.Task
import com.fghbuild.caic.MainActivity
import com.fghbuild.caic.R
import com.fghbuild.caic.voice.VoiceSession
import dagger.hilt.android.qualifiers.ApplicationContext
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
import javax.inject.Inject
import javax.inject.Singleton

private val ATTENTION_STATES = setOf("waiting", "asking", "has_plan")

@Singleton
class TaskNotifier @Inject constructor(
    @param:ApplicationContext private val context: Context,
    private val taskRepository: TaskRepository,
    private val voiceSession: VoiceSession,
) {
    private val nm = context.getSystemService(NotificationManager::class.java)
    private var job: Job? = null

    fun start(scope: CoroutineScope) {
        job?.cancel()
        ensureChannel()
        job = scope.launch {
            var prevStates = emptyMap<String, String>()
            var initialized = false
            taskRepository.tasks.collect { tasks ->
                val currentIds = tasks.map { it.id }.toSet()
                if (initialized) {
                    for (task in tasks) {
                        val needsInput = task.state in ATTENTION_STATES
                        val prevNeedsInput = prevStates[task.id] in ATTENTION_STATES
                        when {
                            needsInput && prevStates[task.id] == "running" && !voiceSession.state.value.connected -> postNotification(task)
                            !needsInput && prevNeedsInput -> nm.cancel(notificationId(task.id))
                        }
                    }
                }
                // Cancel notifications for tasks that were removed from the list.
                for ((id, state) in prevStates) {
                    if (id !in currentIds && state in ATTENTION_STATES) {
                        nm.cancel(notificationId(id))
                    }
                }
                prevStates = tasks.associate { it.id to it.state }
                initialized = true
            }
        }
    }

    private fun postNotification(task: Task) {
        val tapIntent = PendingIntent.getActivity(
            context,
            notificationId(task.id),
            Intent(context, MainActivity::class.java).apply {
                flags = Intent.FLAG_ACTIVITY_SINGLE_TOP
            },
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val title = task.title.ifBlank { task.id }
        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_launcher_monochrome)
            .setContentTitle(title)
            .setContentText(context.getString(R.string.task_notification_text))
            .setContentIntent(tapIntent)
            .setAutoCancel(true)
            .build()
        nm.notify(notificationId(task.id), notification)
    }

    private fun ensureChannel() {
        if (nm.getNotificationChannel(CHANNEL_ID) != null) return
        val channel = NotificationChannel(
            CHANNEL_ID,
            context.getString(R.string.task_channel_name),
            NotificationManager.IMPORTANCE_DEFAULT,
        )
        nm.createNotificationChannel(channel)
    }

    private fun notificationId(taskId: String): Int = taskId.hashCode()

    companion object {
        private const val CHANNEL_ID = "task_status"
    }
}
