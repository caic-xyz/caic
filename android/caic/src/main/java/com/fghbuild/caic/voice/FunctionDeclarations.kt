// Provider-neutral service tool declarations for voice mode, sync with frontend/src/FunctionDeclarations.ts

@file:Suppress("MatchingDeclarationName")

package com.fghbuild.caic.voice

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

@Serializable
data class FunctionDeclaration(
    val name: String,
    val description: String,
    val parameters: JsonElement,
)

// Schema builder helpers.

private val emptyObjectSchema: JsonElement = JsonObject(
    mapOf(
        "type" to JsonPrimitive("object"),
        "properties" to JsonObject(emptyMap()),
    )
)

private fun stringProp(description: String): JsonElement = JsonObject(
    mapOf(
        "type" to JsonPrimitive("string"),
        "description" to JsonPrimitive(description),
    )
)

private fun enumProp(description: String, values: List<String>): JsonElement = JsonObject(
    mapOf(
        "type" to JsonPrimitive("string"),
        "description" to JsonPrimitive(description),
        "enum" to JsonArray(values.map { JsonPrimitive(it) }),
    )
)

private fun intProp(description: String): JsonElement = JsonObject(
    mapOf(
        "type" to JsonPrimitive("integer"),
        "description" to JsonPrimitive(description),
    )
)

private fun boolProp(description: String): JsonElement = JsonObject(
    mapOf(
        "type" to JsonPrimitive("boolean"),
        "description" to JsonPrimitive(description),
    )
)

private fun arrayProp(description: String, items: JsonElement): JsonElement = JsonObject(
    mapOf(
        "type" to JsonPrimitive("array"),
        "description" to JsonPrimitive(description),
        "items" to items,
    )
)

private fun objectSchema(
    vararg properties: Pair<String, JsonElement>,
    required: List<String> = emptyList(),
): JsonElement = JsonObject(
    buildMap {
        put("type", JsonPrimitive("object"))
        put("properties", JsonObject(properties.toMap()))
        if (required.isNotEmpty()) {
            put("required", JsonArray(required.map { JsonPrimitive(it) }))
        }
    }
)

data class ServerCaps(
    val tailscaleAvailable: Boolean = false,
    val usbAvailable: Boolean = false,
    val displayAvailable: Boolean = false,
    val sudoAvailable: Boolean = false,
    val gitHubTokenAvailable: Boolean = false,
)

fun buildFunctionDeclarations(
    harnesses: List<String>,
    repos: List<String>,
    defaultHarness: String?,
    caps: ServerCaps,
): List<FunctionDeclaration> {
    val effectiveDefault = defaultHarness ?: harnesses.firstOrNull()
    val harnessDesc = if (effectiveDefault != null) {
        "Agent harness (default: $effectiveDefault)"
    } else {
        "Agent harness to use (optional)"
    }
    return listOf(
    FunctionDeclaration(
        name = "tasks_list",
        description = "List all current coding tasks with their status, cost, and duration.",
        parameters = emptyObjectSchema,
    ),
    FunctionDeclaration(
        name = "task_create",
        description = "Create a new coding task. Confirm repo and prompt with the user before calling.",
        parameters = objectSchema(
            "prompt" to stringProp("The task description/prompt for the coding agent"),
            "repos" to arrayProp(
                "Repositories to work in (one or more)",
                if (repos.isNotEmpty()) {
                    JsonObject(mapOf("type" to JsonPrimitive("string"), "enum" to JsonArray(repos.map { JsonPrimitive(it) })))
                } else {
                    JsonObject(mapOf("type" to JsonPrimitive("string")))
                },
            ),
            "model" to stringProp("Model to use (optional)"),
            "harness" to if (harnesses.isNotEmpty()) {
                enumProp(harnessDesc, harnesses)
            } else {
                stringProp(harnessDesc)
            },
            "display" to boolProp(
                if (caps.displayAvailable)
                    "Enable virtual display (VNC) for this task"
                else
                    "Enable virtual display (VNC) for this task (not available on this server)"
            ),
            "tailscale" to boolProp(
                if (caps.tailscaleAvailable)
                    "Enable Tailscale networking for this task"
                else
                    "Enable Tailscale networking for this task (not available on this server)"
            ),
            "usb" to boolProp(
                if (caps.usbAvailable)
                    "Enable USB passthrough for this task"
                else
                    "Enable USB passthrough for this task (not available on this server)"
            ),
            "sudo" to boolProp(
                if (caps.sudoAvailable)
                    "Enable root access via sudo with a random password"
                else
                    "Enable root access via sudo with a random password (not available on this server)"
            ),
            "gitHubToken" to boolProp(
                if (caps.gitHubTokenAvailable)
                    "Enable GitHub token injection for this task"
                else
                    "Enable GitHub token injection for this task (not available on this server)"
            ),
            required = listOf("prompt", "repos"),
        ),
    ),
    FunctionDeclaration(
        name = "task_get_detail",
        description = "Get recent activity and status details for a task by its number.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "task_send_message",
        description = "Send a text message to a waiting or asking agent by task number.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            "message" to stringProp("The message to send to the agent"),
            required = listOf("task_number", "message"),
        ),
    ),
    FunctionDeclaration(
        name = "task_answer_question",
        description = "Answer an agent's question by task number. The agent is in 'asking' state.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            "answer" to stringProp("The answer to the agent's question"),
            required = listOf("task_number", "answer"),
        ),
    ),
    FunctionDeclaration(
        name = "task_push_branch_to_remote",
        description = "Sync or push a task's changes to GitHub. " +
            "Push to task branch (default) or squash-push to main.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            "force" to boolProp("Force sync even with safety issues"),
            "target" to enumProp(
                "Where to push: branch (default) or main",
                listOf("branch", "default", "main", "master"),
            ),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "task_stop",
        description = "Stop a running or waiting task. The container is preserved and can be revived later.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "task_purge",
        description = "Permanently delete a stopped task's container. Cannot be undone.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "task_revive",
        description = "Revive a stopped task, restarting its container and agent session.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "task_fork",
        description = "Fork a running or waiting task, snapshotting its container on a new branch." +
            " The prompt describes what the forked task should do." +
            " Optionally override the harness and model.",
        parameters = objectSchema(
            "task_number" to intProp("The task number to fork, e.g. 1 for task #1"),
            "prompt" to stringProp("The initial prompt for the forked task"),
            "harness" to if (harnesses.isNotEmpty()) {
                enumProp("Override harness (optional, inherits from source if omitted)", harnesses)
            } else {
                stringProp("Override harness (optional)")
            },
            "model" to stringProp("Override model (optional, inherits from source if omitted)"),
            required = listOf("task_number", "prompt"),
        ),
    ),
    FunctionDeclaration(
        name = "get_usage",
        description = "Check current task cost and token usage for rolling 5-hour and 7-day windows.",
        parameters = emptyObjectSchema,
    ),
    FunctionDeclaration(
        name = "clone_repo",
        description = "Clone a git repository by URL. Optionally specify a local path.",
        parameters = objectSchema(
            "url" to stringProp("The git repository URL to clone"),
            "path" to stringProp("Local directory name (optional, derived from URL if omitted)"),
            required = listOf("url"),
        ),
    ),
    FunctionDeclaration(
        name = "agent_last_message",
        description = "Get latest agent message, question, or result. Call to check what the agent needs or relay to user.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "web_search",
        description = "Search the web for a query and display the results in an embedded browser.",
        parameters = objectSchema(
            "query" to stringProp("The search query"),
            required = listOf("query"),
        ),
    ),
    FunctionDeclaration(
        name = "web_fetch",
        description = "Open a URL in the embedded browser.",
        parameters = objectSchema(
            "url" to stringProp("The URL to open"),
            required = listOf("url"),
        ),
    ),
    FunctionDeclaration(
        name = "task_fix_pr",
        description = "Inject a fix-PR command into an existing task to fix its failing PR CI in auto mode.",
        parameters = objectSchema(
            "task_number" to intProp("The task number whose PR CI should be fixed"),
            required = listOf("task_number"),
        ),
    ),
    FunctionDeclaration(
        name = "bot_fix_ci",
        description = "Create a task to investigate and fix a failing CI on a repository's default branch.",
        parameters = objectSchema(
            "repo" to if (repos.isNotEmpty()) {
                enumProp("Repository to fix CI for", repos)
            } else {
                stringProp("Repository path to fix CI for")
            },
            required = listOf("repo"),
        ),
    ),
    )
}
