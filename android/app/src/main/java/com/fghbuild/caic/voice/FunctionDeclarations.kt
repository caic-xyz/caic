// Gemini Live function declaration structures for voice mode tool calling.
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
    val behavior: String? = null,
    val scheduling: String? = null,
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

val functionDeclarations: List<FunctionDeclaration> = listOf(
    FunctionDeclaration(
        name = "list_tasks",
        description = "List all current coding tasks with their status, cost, and duration.",
        parameters = emptyObjectSchema,
        behavior = "NON_BLOCKING",
        scheduling = "WHEN_IDLE",
    ),
    FunctionDeclaration(
        name = "create_task",
        description = "Create a new coding task. Confirm repo and prompt with the user before calling.",
        parameters = objectSchema(
            "prompt" to stringProp("The task description/prompt for the coding agent"),
            "repo" to stringProp("Repository path to work in"),
            "model" to stringProp("Model to use (optional)"),
            "harness" to stringProp("Agent harness: 'claude' or 'gemini' (default: claude)"),
            required = listOf("prompt", "repo"),
        ),
        behavior = "NON_BLOCKING",
        scheduling = "INTERRUPT",
    ),
    FunctionDeclaration(
        name = "get_task_detail",
        description = "Get recent activity and status details for a task by its number.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
        behavior = "NON_BLOCKING",
        scheduling = "WHEN_IDLE",
    ),
    FunctionDeclaration(
        name = "send_message",
        description = "Send a text message to a waiting or asking agent by task number.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            "message" to stringProp("The message to send to the agent"),
            required = listOf("task_number", "message"),
        ),
        behavior = "NON_BLOCKING",
        scheduling = "INTERRUPT",
    ),
    FunctionDeclaration(
        name = "answer_question",
        description = "Answer an agent's question by task number. The agent is in 'asking' state.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            "answer" to stringProp("The answer to the agent's question"),
            required = listOf("task_number", "answer"),
        ),
        behavior = "NON_BLOCKING",
        scheduling = "INTERRUPT",
    ),
    FunctionDeclaration(
        name = "sync_task",
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
        behavior = "NON_BLOCKING",
        scheduling = "INTERRUPT",
    ),
    FunctionDeclaration(
        name = "terminate_task",
        description = "Stop a running coding task by its number.",
        parameters = objectSchema(
            "task_number" to intProp("The task number, e.g. 1 for task #1"),
            required = listOf("task_number"),
        ),
        behavior = "NON_BLOCKING",
        scheduling = "INTERRUPT",
    ),
    FunctionDeclaration(
        name = "get_usage",
        description = "Check current API quota utilization and limits.",
        parameters = emptyObjectSchema,
        behavior = "NON_BLOCKING",
        scheduling = "WHEN_IDLE",
    ),
    FunctionDeclaration(
        name = "list_repos",
        description = "List available git repositories.",
        parameters = emptyObjectSchema,
        behavior = "NON_BLOCKING",
        scheduling = "WHEN_IDLE",
    ),
)
