// Message grouping and turn splitting, ported from frontend/src/TaskView.tsx.
package com.fghbuild.caic.util

import com.caic.sdk.ClaudeEventAsk
import com.caic.sdk.ClaudeEventMessage
import com.caic.sdk.ClaudeEventToolResult
import com.caic.sdk.ClaudeEventToolUse
import com.caic.sdk.EventKinds

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

/** Groups consecutive events for cohesive rendering. */
@Suppress("CyclomaticComplexMethod", "LoopWithTooManyJumpStatements")
fun groupMessages(msgs: List<ClaudeEventMessage>): List<MessageGroup> {
    val groups = mutableListOf<MessageGroup>()

    fun lastGroup(): MessageGroup? = groups.lastOrNull()

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
                val call = ToolCall(use = toolUse)
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.TOOL) {
                    last.events.add(ev)
                    last.toolCalls.add(call)
                } else {
                    groups.add(
                        MessageGroup(
                            kind = GroupKind.TOOL,
                            events = mutableListOf(ev),
                            toolCalls = mutableListOf(call),
                        )
                    )
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
                val last = lastGroup()
                if (last != null && (last.kind == GroupKind.TEXT || last.kind == GroupKind.TOOL)) {
                    last.events.add(ev)
                } else {
                    groups.add(MessageGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
                }
            }
            EventKinds.DiffStat -> { /* Metadata-only; skip. */ }
            else -> {
                groups.add(MessageGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
            }
        }
    }

    // Mark tool calls as implicitly done when later events exist.
    val lastToolIdx = groups.indexOfLast { it.kind == GroupKind.TOOL }
    for (i in groups.indices) {
        val g = groups[i]
        if (g.kind != GroupKind.TOOL) continue
        if (i < lastToolIdx || i < groups.size - 1) {
            for (tc in g.toolCalls) tc.done = true
        }
    }
    return groups
}

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
