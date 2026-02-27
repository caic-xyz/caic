// Navigation routes for the app.
package com.fghbuild.caic.navigation

sealed class Screen(val route: String) {
    data object TaskList : Screen("tasks")
    data object Settings : Screen("settings")
    data class TaskDetail(val taskId: String) : Screen("tasks/$taskId") {
        companion object {
            const val ROUTE = "tasks/{taskId}"
            const val ARG_TASK_ID = "taskId"
        }
    }
    data class TaskDiff(val taskId: String) : Screen("tasks/$taskId/diff") {
        companion object {
            const val ROUTE = "tasks/{taskId}/diff"
            const val ARG_TASK_ID = "taskId"
        }
    }
}
