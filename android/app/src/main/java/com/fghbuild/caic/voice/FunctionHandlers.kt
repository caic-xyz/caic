// Dispatches Gemini function calls to the caic API.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.CreateTaskReq
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.SyncReq
import com.caic.sdk.v1.Task
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonPrimitive

class FunctionHandlers(
    private val apiClient: ApiClient,
    private val taskNumberMap: TaskNumberMap,
    private val excludedTaskIds: () -> Set<String>,
) {

    suspend fun handle(name: String, args: JsonObject): JsonElement {
        return try {
            when (name) {
                "list_tasks" -> handleListTasks()
                "create_task" -> handleCreateTask(args)
                "get_task_detail" -> handleGetTaskDetail(args)
                "send_message" -> handleSendMessage(args)
                "answer_question" -> handleAnswerQuestion(args)
                "sync_task", "push_task" -> handleSyncTask(args)
                "terminate_task" -> handleTerminateTask(args)
                "get_usage" -> handleGetUsage()
                "list_repos" -> handleListRepos()
                else -> errorResult("Unknown function: $name")
            }
        } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
            errorResult("Error: ${e.message}")
        }
    }

    private suspend fun handleListTasks(): JsonElement {
        val excluded = excludedTaskIds()
        val tasks = apiClient.listTasks().filter { it.id !in excluded }
        if (tasks.isEmpty()) return textResult("No tasks running.")
        val lines = tasks.joinToString("\n") { t ->
            val num = taskNumberMap.toNumber(t.id) ?: 0
            taskSummaryLine(num, t)
        }
        return textResult("## Tasks\n\n$lines")
    }

    private suspend fun handleCreateTask(args: JsonObject): JsonElement {
        val prompt = args.requireString("prompt")
        val repo = resolveRepo(args.requireString("repo"))
            ?: return errorResult("Unknown repo: ${args.requireString("repo")}")
        val model = args.optString("model")
        val harness = args.optString("harness") ?: "claude"
        val resp = apiClient.createTask(
            CreateTaskReq(
                initialPrompt = Prompt(text = prompt),
                repo = repo,
                model = model,
                harness = harness,
            )
        )
        // Refresh the map so the new task gets a number.
        val excluded = excludedTaskIds()
        val tasks = apiClient.listTasks().filter { it.id !in excluded }
        taskNumberMap.update(tasks)
        val num = taskNumberMap.toNumber(resp.id)
        return if (num != null) {
            textResult("Created task #$num: ${prompt.lines().first().take(SHORT_NAME_MAX)}")
        } else {
            textResult("Created task: ${prompt.lines().first().take(SHORT_NAME_MAX)}")
        }
    }

    private suspend fun handleGetTaskDetail(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        val tasks = apiClient.listTasks()
        val t = tasks.find { it.id == taskId }
            ?: return errorResult("Task #$num not found")
        val shortName = t.initialPrompt.lines().firstOrNull()?.take(SHORT_NAME_MAX) ?: t.id
        val detail = buildString {
            appendLine("## Task #$num: $shortName")
            appendLine()
            append("**State:** ${t.state}  ")
            append("**Elapsed:** ${formatElapsed(t.duration)}  ")
            appendLine("**Cost:** ${formatCost(t.costUSD)}")
            when {
                t.state == "asking" -> appendLine("Waiting for user input before it can continue.")
                t.state == "terminated" && !t.result.isNullOrBlank() ->
                    appendLine("**Result:** ${t.result}")
                t.state == "failed" ->
                    appendLine("**Error:** ${t.error ?: "unknown"}")
            }
            t.diffStat?.takeIf { it.isNotEmpty() }?.let { diff ->
                append("**Changed:** ${diff.joinToString(", ") { it.path }}")
            }
        }.trim()
        return textResult(detail)
    }

    private suspend fun handleSendMessage(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        val message = args.requireString("message")
        apiClient.sendInput(taskId, InputReq(prompt = Prompt(text = message)))
        return textResult("Sent message to task #$num.")
    }

    private suspend fun handleAnswerQuestion(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        val answer = args.requireString("answer")
        apiClient.sendInput(taskId, InputReq(prompt = Prompt(text = answer)))
        return textResult("Answered task #$num.")
    }

    private suspend fun handleSyncTask(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        val force = args["force"]?.jsonPrimitive?.booleanOrNull ?: false
        val target = args.optString("target").let { if (it == "main" || it == "master") "default" else it }
        val resp = apiClient.syncTask(taskId, SyncReq(force = force, target = target))
        val issues = resp.safetyIssues
        val verb = if (target == "default") "Pushed task #$num to main" else "Synced task #$num"
        return if (issues.isNullOrEmpty()) {
            textResult("$verb.")
        } else {
            val issueLines = issues.joinToString("\n") { "- **${it.kind}** ${it.file}: ${it.detail}" }
            textResult("$verb with safety issues:\n$issueLines")
        }
    }

    private suspend fun handleTerminateTask(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        apiClient.terminateTask(taskId)
        return textResult("Terminated task #$num.")
    }

    private suspend fun handleGetUsage(): JsonElement {
        val usage = apiClient.getUsage()
        fun pct(v: Double) = "${v.toInt()}%"
        val summary = buildString {
            appendLine("5-hour window: ${pct(usage.fiveHour.utilization)} used, resets ${usage.fiveHour.resetsAt}")
            append("7-day window: ${pct(usage.sevenDay.utilization)} used, resets ${usage.sevenDay.resetsAt}")
            if (usage.extraUsage.isEnabled) {
                appendLine()
                val usedDollars = usage.extraUsage.usedCredits / 100
                val limitDollars = usage.extraUsage.monthlyLimit / 100
                append(
                    "Extra usage: \$${usedDollars.toInt()} of " +
                        "\$${limitDollars.toInt()} monthly limit used",
                )
            }
        }
        return textResult(summary)
    }

    private suspend fun handleListRepos(): JsonElement {
        val repos = apiClient.listRepos()
        if (repos.isEmpty()) return textResult("No repositories available.")
        val lines = repos.joinToString("\n") { r ->
            "- **${r.path}** (base: ${r.baseBranch})"
        }
        return textResult("## Repositories\n\n$lines")
    }

    /** Resolve a repo name to its canonical path using case-insensitive matching. */
    private suspend fun resolveRepo(name: String): String? {
        val repos = apiClient.listRepos()
        return repos.find { it.path.equals(name, ignoreCase = true) }?.path
    }

    /** Resolve task_number from args to a real task ID via the map. */
    private fun resolveTaskNumber(args: JsonObject): String? {
        val num = args.requireInt("task_number")
        return taskNumberMap.toId(num)
    }
}

private fun taskSummaryLine(num: Int, t: Task): String {
    val name = t.initialPrompt.lines().firstOrNull()?.take(SHORT_NAME_MAX) ?: t.id
    val base = "$num. **$name** — ${t.state}, ${formatElapsed(t.duration)}, " +
        "${formatCost(t.costUSD)}, ${t.harness}"
    return when {
        t.state == "asking" -> "$base — NEEDS INPUT"
        t.state == "terminated" && !t.result.isNullOrBlank() ->
            "$base — Result: ${t.result!!.take(RESULT_SNIPPET_MAX)}"
        t.state == "failed" -> "$base — Error: ${t.error ?: "unknown"}"
        else -> base
    }
}

private const val SHORT_NAME_MAX = 40
private const val RESULT_SNIPPET_MAX = 120

private fun JsonObject.requireString(key: String): String =
    this[key]?.jsonPrimitive?.content
        ?: throw IllegalArgumentException("Missing required parameter: $key")

private fun JsonObject.requireInt(key: String): Int =
    this[key]?.jsonPrimitive?.intOrNull
        ?: throw IllegalArgumentException("Missing required integer parameter: $key")

private fun JsonObject.optString(key: String): String? =
    this[key]?.jsonPrimitive?.content

private fun textResult(message: String): JsonElement =
    JsonObject(mapOf("result" to JsonPrimitive(message)))

private fun errorResult(message: String): JsonElement =
    JsonObject(mapOf("error" to JsonPrimitive(message)))
