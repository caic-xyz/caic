// Message grouping and turn splitting, ported from frontend/src/TaskView.tsx.
package com.fghbuild.caic.util

import com.caic.sdk.v1.ClaudeEventAsk
import com.caic.sdk.v1.ClaudeEventMessage
import com.caic.sdk.v1.ClaudeEventToolResult
import com.caic.sdk.v1.ClaudeEventToolUse
import com.caic.sdk.v1.EventKinds
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive

enum class GroupKind { TEXT, TOOL, ASK, USER_INPUT, OTHER }

data class ToolCall(
    val use: ClaudeEventToolUse,
    var result: ClaudeEventToolResult? = null,
    var done: Boolean = false,
)

data class MessageGroup(
    val kind: GroupKind,
    val events: MutableList<ClaudeEventMessage>,
    val toolCalls: MutableList<ToolCall> = mutableListOf(),
    var ask: ClaudeEventAsk? = null,
    var answerText: String? = null,
)

data class Turn(
    val groups: List<MessageGroup>,
    val toolCount: Int,
    val textCount: Int,
)

// Tool names (lowercase) that are async and emit explicit toolResult events.
// All other Claude Code tools complete synchronously and are done as soon as
// their toolUse event is emitted.
private val ASYNC_TOOLS = setOf("bash", "task")

/** Groups consecutive events for cohesive rendering. */
@Suppress("CyclomaticComplexMethod", "LoopWithTooManyJumpStatements")
fun groupMessages(msgs: List<ClaudeEventMessage>): List<MessageGroup> {
    val groups = mutableListOf<MessageGroup>()

    fun lastGroup(): MessageGroup? = groups.lastOrNull()

    // Tracks whether a usage event appeared since the last tool group,
    // which signals a new AssistantMessage boundary.
    var usageSinceLastTool = false

    for (ev in msgs) {
        when (ev.kind) {
            EventKinds.Text -> {
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.TEXT &&
                    last.events.any { it.kind == EventKinds.TextDelta }
                ) {
                    last.events.add(ev)
                } else {
                    groups.add(MessageGroup(kind = GroupKind.TEXT, events = mutableListOf(ev)))
                }
            }
            EventKinds.TextDelta -> {
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.TEXT) {
                    last.events.add(ev)
                } else {
                    groups.add(MessageGroup(kind = GroupKind.TEXT, events = mutableListOf(ev)))
                }
            }
            EventKinds.ToolUse -> {
                val toolUse = ev.toolUse ?: continue
                val call = ToolCall(use = toolUse, done = toolUse.name.lowercase() !in ASYNC_TOOLS)
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.TOOL && !usageSinceLastTool) {
                    // Consecutive toolUse in the same AssistantMessage — merge.
                    last.events.add(ev)
                    last.toolCalls.add(call)
                } else if (!usageSinceLastTool) {
                    // Same AssistantMessage but intervening text; find the most
                    // recent tool group to coalesce into.
                    val anchor = groups.lastOrNull { it.kind == GroupKind.TOOL }
                    if (anchor != null) {
                        anchor.events.add(ev)
                        anchor.toolCalls.add(call)
                    } else {
                        groups.add(newToolGroup(ev, call))
                    }
                } else {
                    // New AssistantMessage — start a new tool group.
                    groups.add(newToolGroup(ev, call))
                    usageSinceLastTool = false
                }
            }
            EventKinds.ToolResult -> {
                val tr = ev.toolResult ?: continue
                var matched = false
                for (i in groups.indices.reversed()) {
                    val g = groups[i]
                    if (g.kind != GroupKind.TOOL) continue
                    val tc = g.toolCalls.firstOrNull { it.use.toolUseID == tr.toolUseID && it.result == null }
                    if (tc != null) {
                        tc.result = tr
                        tc.done = true
                        g.events.add(ev)
                        matched = true
                        break
                    }
                }
                if (!matched) {
                    groups.add(MessageGroup(kind = GroupKind.TOOL, events = mutableListOf(ev)))
                }
            }
            EventKinds.Ask -> {
                val ask = ev.ask ?: continue
                groups.add(MessageGroup(kind = GroupKind.ASK, events = mutableListOf(ev), ask = ask))
            }
            EventKinds.UserInput -> {
                val prev = lastGroup()
                if (prev != null && prev.kind == GroupKind.ASK && prev.answerText == null) {
                    prev.answerText = ev.userInput?.text
                    prev.events.add(ev)
                } else {
                    groups.add(MessageGroup(kind = GroupKind.USER_INPUT, events = mutableListOf(ev)))
                }
            }
            EventKinds.Usage -> {
                usageSinceLastTool = true
                val last = lastGroup()
                if (last != null && (last.kind == GroupKind.TEXT || last.kind == GroupKind.TOOL)) {
                    last.events.add(ev)
                } else {
                    groups.add(MessageGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
                }
            }
            EventKinds.Todo -> { /* Rendered by TodoPanel directly; skip to avoid splitting tool groups. */ }
            EventKinds.DiffStat -> { /* Metadata-only; skip. */ }
            else -> {
                groups.add(MessageGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
            }
        }
    }

    // Merge tool groups separated only by text groups. The agent often emits
    // short commentary between tool calls ("Let me read...", "Now let me edit...").
    // Without merging, each appears as a separate 1-tool block. ask, userInput,
    // and other groups act as hard boundaries that prevent merging.
    val merged = mutableListOf<MessageGroup>()
    for (g in groups) {
        if (g.kind == GroupKind.TOOL) {
            val anchor = merged.lastOrNull { it.kind != GroupKind.TEXT }
            if (anchor != null && anchor.kind == GroupKind.TOOL) {
                anchor.events.addAll(g.events)
                anchor.toolCalls.addAll(g.toolCalls)
                continue
            }
        }
        merged.add(g)
    }

    // Mark tool calls as implicitly done when later events exist.
    val lastToolIdx = merged.indexOfLast { it.kind == GroupKind.TOOL }
    for (i in merged.indices) {
        val g = merged[i]
        if (g.kind != GroupKind.TOOL) continue
        if (i < lastToolIdx || i < merged.size - 1) {
            for (tc in g.toolCalls) tc.done = true
        }
    }
    return merged
}

private fun newToolGroup(ev: ClaudeEventMessage, call: ToolCall) = MessageGroup(
    kind = GroupKind.TOOL,
    events = mutableListOf(ev),
    toolCalls = mutableListOf(call),
)

/** Splits message groups into turns separated by "result" events. */
fun groupTurns(groups: List<MessageGroup>): List<Turn> {
    val turns = mutableListOf<Turn>()
    var current = mutableListOf<MessageGroup>()
    var toolCount = 0
    var textCount = 0

    fun flush() {
        if (current.isNotEmpty()) {
            turns.add(Turn(groups = current.toList(), toolCount = toolCount, textCount = textCount))
            current = mutableListOf()
            toolCount = 0
            textCount = 0
        }
    }

    for (g in groups) {
        current.add(g)
        when (g.kind) {
            GroupKind.TOOL -> toolCount += g.toolCalls.size
            GroupKind.TEXT -> textCount++
            else -> {}
        }
        if (g.kind == GroupKind.OTHER && g.events.any { it.kind == EventKinds.Result }) {
            flush()
        }
    }
    flush()
    return turns
}

/** Summarize a turn for elided display. */
fun turnSummary(turn: Turn): String {
    val parts = mutableListOf<String>()
    if (turn.textCount > 0) {
        parts.add(if (turn.textCount == 1) "1 message" else "${turn.textCount} messages")
    }
    if (turn.toolCount > 0) {
        parts.add(if (turn.toolCount == 1) "1 tool call" else "${turn.toolCount} tool calls")
    }
    return if (parts.isNotEmpty()) parts.joinToString(", ") else "empty turn"
}

/** Summarize tool call counts for group headers: "Read x2, Bash". */
fun toolCountSummary(calls: List<ToolCall>): String {
    val counts = linkedMapOf<String, Int>()
    for (tc in calls) {
        counts[tc.use.name] = (counts[tc.use.name] ?: 0) + 1
    }
    return counts.entries.joinToString(", ") { (name, c) ->
        if (c > 1) "$name \u00d7$c" else name
    }
}

/** True if any tool call in the turn is ExitPlanMode. */
fun turnHasExitPlanMode(turn: Turn): Boolean =
    turn.groups.any { g ->
        g.kind == GroupKind.TOOL && g.toolCalls.any { it.use.name == "ExitPlanMode" }
    }

/** Extracts the plan markdown content from the Write tool call that wrote to .claude/plans/. */
fun turnPlanContent(turn: Turn): String? {
    val writes = turn.groups
        .filter { it.kind == GroupKind.TOOL }
        .flatMap { it.toolCalls }
        .filter { it.use.name == "Write" }
    for (tc in writes) {
        val obj = runCatching { tc.use.input.jsonObject }.getOrNull() ?: continue
        val fp = (obj["file_path"] ?: obj["filePath"])?.jsonPrimitive?.content ?: ""
        if (".claude/plans/" in fp) return obj["content"]?.jsonPrimitive?.content
    }
    return null
}
