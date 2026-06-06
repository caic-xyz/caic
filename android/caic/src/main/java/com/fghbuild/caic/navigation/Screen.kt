// Navigation routes for the app.
package com.fghbuild.caic.navigation

import androidx.navigation3.runtime.NavKey
import kotlinx.serialization.Serializable

@Serializable
sealed class Screen : NavKey {
    @Serializable
    data object TaskList : Screen()

    @Serializable
    data object Settings : Screen()

    @Serializable
    data class TaskDetail(val taskId: String) : Screen() {
        companion object {
            const val ARG_TASK_ID = "taskId"
        }
    }

    @Serializable
    data class TaskDiff(val taskId: String) : Screen() {
        companion object {
            const val ARG_TASK_ID = "taskId"
        }
    }
}
