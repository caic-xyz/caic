// Dispatches Gemini function calls to the caic API.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.BotFixCIReq
import com.caic.sdk.v1.BotFixPRReq
import com.caic.sdk.v1.CloneRepoReq
import com.caic.sdk.v1.CreateTaskReq
import com.caic.sdk.v1.ForkTaskReq
import com.caic.sdk.v1.EventKinds
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.RepoSpec
import com.caic.sdk.v1.SyncReq
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.WebFetchReq
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.data.TaskSSEEvent
import com.fghbuild.caic.util.formatCost
import com.fghbuild.caic.util.formatElapsed
import kotlinx.coroutines.flow.takeWhile
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonPrimitive

class FunctionHandlers(
    private val apiClient: ApiClient,
    private val taskRepository: TaskRepository,
    private val baseURL: String,
    private val taskNumberMap: TaskNumberMap,
    private val excludedTaskIds: () -> Set<String>,
    private val defaultHarness: String = "",
    private val defaultModel: String = "",
) {

    suspend fun handle(name: String, args: JsonObject): JsonElement {
        return try {
            when (name) {
                "tasks_list" -> handleListTasks()
                "task_create" -> handleCreateTask(args)
                "task_get_detail" -> handleGetTaskDetail(args)
                "task_send_message" -> handleSendMessage(args)
                "task_answer_question" -> handleAnswerQuestion(args)
                "task_push_branch_to_remote" -> handleSyncTask(args)
                "task_stop" -> handleStopTask(args)
                "task_purge" -> handlePurgeTask(args)
                "task_revive" -> handleReviveTask(args)
                "task_fork" -> handleForkTask(args)
                "get_usage" -> handleGetUsage()
                "clone_repo" -> handleCloneRepo(args)
                "task_get_last_message_from_assistant" -> handleGetLastMessage(args)
                "web_search" -> handleWebSearch(args)
                "web_fetch" -> handleWebFetch(args)
                "task_fix_pr" -> handleTaskFixPR(args)
                "bot_fix_ci" -> handleBotFixCI(args)
                else -> errorResult("Unknown function: $name")
            }
        } catch (@Suppress("TooGenericExceptionCaught") e: Exception) {
            errorResult("Error: ${e.message}")
        }
    }

    private suspend fun handleListTasks(): JsonElement {
        val excluded = excludedTaskIds()
        val tasks = apiClient.listTasks().filter { it.id !in excluded }
            .sortedWith(compareBy<Task> { it.id.length }.thenBy { it.id })
        if (tasks.isEmpty()) return textResult("No tasks running.")
        val lines = tasks.joinToString("\n") { t ->
            val num = taskNumberMap.toNumber(t.id) ?: 0
            taskSummaryLine(num, t)
        }
        return textResult("## Tasks\n\n$lines")
    }

    private suspend fun handleCreateTask(args: JsonObject): JsonElement {
        val prompt = args.requireString("prompt")
        val reposArg = args["repos"]
        val repoNames = when (reposArg) {
            is JsonArray -> reposArg.mapNotNull { (it as? JsonPrimitive)?.content?.ifBlank { null } }
            is JsonPrimitive -> listOf(reposArg.content)
            else -> emptyList()
        }
        val model = args.optString("model") ?: defaultModel.ifBlank { null }
        val harness = args.optString("harness") ?: defaultHarness
        val resp = apiClient.createTask(
            CreateTaskReq(
                initialPrompt = Prompt(text = prompt),
                repos = repoNames.map { RepoSpec(name = it) },
                model = model,
                harness = harness,
            )
        )
        // Refresh the map so the new task gets a number.
        val excluded = excludedTaskIds()
        val tasks = apiClient.listTasks().filter { it.id !in excluded }
        taskNumberMap.update(tasks)
        val num = taskNumberMap.toNumber(resp.id)
        val title = tasks.find { it.id == resp.id }?.title?.ifBlank { null } ?: resp.id
        return if (num != null) {
            textResult("Created task #$num: $title")
        } else {
            textResult("Created task: $title")
        }
    }

    private suspend fun handleGetTaskDetail(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        val tasks = apiClient.listTasks()
        val t = tasks.find { it.id == taskId }
            ?: return errorResult("Task #$num not found")
        val shortName = t.title.ifBlank { t.id }
        val detail = buildString {
            appendLine("## Task #$num: $shortName")
            appendLine()
            append("State: ${t.state}  ")
            append("Elapsed: ${formatElapsed(t.duration)}  ")
            appendLine("Cost: ${formatCost(t.costUSD)}")
            when {
                t.state == "purged" && !t.result.isNullOrBlank() ->
                    appendLine("**Result:** ${t.result}")
                t.state == "stopped" ->
                    appendLine("**Stopped:** container died")
                t.state == "failed" && !t.error.isNullOrBlank() ->
                    appendLine("**Error:** ${t.error}")
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

    private suspend fun handleStopTask(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        apiClient.stopTask(taskId)
        return textResult("Stopping task #$num.")
    }

    private suspend fun handlePurgeTask(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        apiClient.purgeTask(taskId)
        return textResult("Purged task #$num.")
    }

    private suspend fun handleReviveTask(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        apiClient.reviveTask(taskId)
        return textResult("Reviving task #$num.")
    }

    private suspend fun handleForkTask(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        val prompt = args.requireString("prompt")
        val harness = args.optString("harness")
        val model = args.optString("model")
        val resp = apiClient.forkTask(
            taskId,
            ForkTaskReq(prompt = Prompt(text = prompt), harness = harness, model = model),
        )
        return textResult("Forked task #$num. New task ID: ${resp.id}")
    }

    private suspend fun handleGetUsage(): JsonElement {
        val usage = apiClient.getUsage()
        fun fmt(cost: Double) = if (cost >= 10) {
            "\$${cost.toInt()}"
        } else {
            "\$${String.format(java.util.Locale.US, "%.2f", cost)}"
        }
        fun tokens(w: com.caic.sdk.v1.UsageWindow) = w.inputTokens + w.outputTokens
        val summary = buildString {
            appendLine("5-hour cost: ${fmt(usage.fiveHour.costUSD)} (${tokens(usage.fiveHour)} tokens)")
            append("7-day cost: ${fmt(usage.sevenDay.costUSD)} (${tokens(usage.sevenDay)} tokens)")
        }
        return textResult(summary)
    }

    private suspend fun handleGetLastMessage(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")

        val events = mutableListOf<com.caic.sdk.v1.EventMessage>()
        taskRepository.taskRawEventsWithReady(baseURL, taskId)
            .takeWhile { it !is TaskSSEEvent.Ready }
            .collect { event ->
                if (event is TaskSSEEvent.Event) events.add(event.msg)
            }

        val message = events.lastOrNull { it.kind == EventKinds.Result }?.result?.result?.let { r ->
            "Task #$num result: $r"
        } ?: events.lastOrNull { it.kind == EventKinds.Ask }?.ask?.questions?.firstOrNull()?.let { q ->
            val opts = q.options.joinToString(", ") { it.label }
            "Task #$num is asking: ${q.question} Options: $opts"
        } ?: events.lastOrNull { it.kind == EventKinds.Text }?.text?.text?.let { t ->
            "Last message from task #$num: $t"
        } ?: "No messages from task #$num yet."
        return textResult(message)
    }

    private suspend fun handleWebSearch(args: JsonObject): JsonElement {
        val query = args.requireString("query")
        val url = "https://html.duckduckgo.com/html/?q=${java.net.URLEncoder.encode(query, "UTF-8")}"
        val resp = apiClient.webFetch(WebFetchReq(url = url))
        return JsonObject(mapOf("title" to JsonPrimitive(resp.title), "content" to JsonPrimitive(resp.content)))
    }

    private suspend fun handleWebFetch(args: JsonObject): JsonElement {
        val url = args.requireString("url")
        val resp = apiClient.webFetch(WebFetchReq(url = url))
        return JsonObject(mapOf("title" to JsonPrimitive(resp.title), "content" to JsonPrimitive(resp.content)))
    }

    private suspend fun handleTaskFixPR(args: JsonObject): JsonElement {
        val taskId = resolveTaskNumber(args) ?: return errorResult("Unknown task number")
        val num = args.requireInt("task_number")
        apiClient.botFixPR(BotFixPRReq(taskId = taskId))
        return textResult("Injected fix-PR command into task #$num.")
    }

    private suspend fun handleBotFixCI(args: JsonObject): JsonElement {
        val repo = args.requireString("repo")
        val resp = apiClient.botFixCI(BotFixCIReq(repo = repo))
        val excluded = excludedTaskIds()
        val tasks = apiClient.listTasks().filter { it.id !in excluded }
        taskNumberMap.update(tasks)
        val num = taskNumberMap.toNumber(resp.id)
        return if (num != null) {
            textResult("Created fix-CI task #$num for $repo.")
        } else {
            textResult("Created fix-CI task for $repo.")
        }
    }

    private suspend fun handleCloneRepo(args: JsonObject): JsonElement {
        val url = args.requireString("url")
        val path = args.optString("path")
        val repo = apiClient.cloneRepo(CloneRepoReq(url = url, path = path))
        val base = repo.baseBranch
        val prefix = if (!base.remote.isNullOrEmpty()) "${base.remote}/" else ""
        return textResult("Cloned **${repo.path}** (base: $prefix${base.name}).")
    }

    /** Resolve task_number from args to a real task ID via the map. */
    private fun resolveTaskNumber(args: JsonObject): String? {
        val num = args.requireInt("task_number")
        return taskNumberMap.toId(num)
    }
}

private fun diffStatSummary(t: Task): String {
    val ds = t.diffStat ?: return ""
    if (ds.isEmpty()) return ""
    var added = 0
    var deleted = 0
    for (f in ds) {
        added += f.added
        deleted += f.deleted
    }
    val label = if (ds.size == 1) "file" else "files"
    return ", +$added -$deleted in ${ds.size} $label"
}

private fun taskSummaryLine(num: Int, t: Task): String {
    val name = t.title.ifBlank { t.id }
    val extras = buildList {
        val pr = t.forgePR
        if (pr != null && pr > 0) add("PR #$pr")
        if (!t.ciStatus.isNullOrBlank()) add("CI: ${t.ciStatus}")
    }
    val extrasStr = if (extras.isNotEmpty()) ", ${extras.joinToString(", ")}" else ""
    val base = "$num. **$name** — ${t.state}, ${formatElapsed(t.duration)}, " +
        "${formatCost(t.costUSD)}, ${t.harness}${diffStatSummary(t)}$extrasStr"
    return when {
        t.state == "purged" && !t.result.isNullOrBlank() ->
            "$base — ${t.result!!.take(RESULT_SNIPPET_MAX)}"
        t.state == "stopped" ->
            "$base — container died"
        t.state == "failed" && !t.error.isNullOrBlank() ->
            "$base — ${t.error}"
        else -> base
    }
}

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
