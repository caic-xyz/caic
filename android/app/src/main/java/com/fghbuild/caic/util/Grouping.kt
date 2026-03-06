// Message grouping and turn splitting, ported from frontend/src/grouping.ts.
package com.fghbuild.caic.util

import androidx.compose.runtime.Immutable
import com.caic.sdk.v1.EventAsk
import com.caic.sdk.v1.EventMessage
import com.caic.sdk.v1.EventToolResult
import com.caic.sdk.v1.EventToolUse
import com.caic.sdk.v1.TodoItem
import com.caic.sdk.v1.EventKinds

enum class GroupKind { TEXT, ACTION, ASK, USER_INPUT, OTHER }

@Immutable
data class ToolCall(
    val use: EventToolUse,
    val result: EventToolResult? = null,
    val done: Boolean = false,
)

@Immutable
data class MessageGroup(
    val kind: GroupKind,
    val events: List<EventMessage>,
    val toolCalls: List<ToolCall> = emptyList(),
    val ask: EventAsk? = null,
    val answerText: String? = null,
)

@Immutable
data class Turn(
    val groups: List<MessageGroup>,
    val toolCount: Int,
    val textCount: Int,
)

// Tool names (lowercase) that are async and emit explicit toolResult events.
// All other Claude Code tools complete synchronously and are done as soon as
// their toolUse event is emitted.
private val ASYNC_TOOLS = setOf("bash", "task")

/** Mutable builder for ToolCall, used only inside groupMessages(). */
private class MutableToolCall(
    val use: EventToolUse,
    var result: EventToolResult? = null,
    var done: Boolean = false,
) {
    fun freeze() = ToolCall(use, result, done)
}

/** Mutable builder for MessageGroup, used only inside groupMessages(). */
private class MutableGroup(
    val kind: GroupKind,
    val events: MutableList<EventMessage> = mutableListOf(),
    val toolCalls: MutableList<MutableToolCall> = mutableListOf(),
    var ask: EventAsk? = null,
    var answerText: String? = null,
) {
    fun freeze() = MessageGroup(
        kind = kind,
        events = events.toList(),
        toolCalls = toolCalls.map { it.freeze() },
        ask = ask,
        answerText = answerText,
    )
}

/** Groups consecutive events for cohesive rendering. */
@Suppress("CyclomaticComplexMethod", "LoopWithTooManyJumpStatements")
fun groupMessages(msgs: List<EventMessage>): List<MessageGroup> {
    val groups = mutableListOf<MutableGroup>()

    fun lastGroup(): MutableGroup? = groups.lastOrNull()

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
                } else if (last != null && last.kind == GroupKind.ACTION && last.toolCalls.isEmpty()) {
                    // Text immediately after a thinking-only group: absorb thinking into this text group
                    // so it renders as a collapsed block inside the text rather than a separate top-level item.
                    val thinkingGroup = groups.removeAt(groups.lastIndex)
                    groups.add(MutableGroup(
                        kind = GroupKind.TEXT,
                        events = (thinkingGroup.events + ev).toMutableList(),
                    ))
                } else {
                    groups.add(MutableGroup(kind = GroupKind.TEXT, events = mutableListOf(ev)))
                }
            }
            EventKinds.TextDelta -> {
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.TEXT) {
                    last.events.add(ev)
                } else if (last != null && last.kind == GroupKind.ACTION && last.toolCalls.isEmpty()) {
                    // Text immediately after a thinking-only group: absorb thinking into this text group.
                    val thinkingGroup = groups.removeAt(groups.lastIndex)
                    groups.add(MutableGroup(
                        kind = GroupKind.TEXT,
                        events = (thinkingGroup.events + ev).toMutableList(),
                    ))
                } else {
                    groups.add(MutableGroup(kind = GroupKind.TEXT, events = mutableListOf(ev)))
                }
            }
            EventKinds.ToolUse -> {
                val toolUse = ev.toolUse ?: continue
                val call = MutableToolCall(use = toolUse, done = toolUse.name.lowercase() !in ASYNC_TOOLS)
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.ACTION &&
                    last.toolCalls.isNotEmpty() && !usageSinceLastTool
                ) {
                    // Consecutive toolUse in the same AssistantMessage — merge.
                    last.events.add(ev)
                    last.toolCalls.add(call)
                } else if (!usageSinceLastTool) {
                    // Same AssistantMessage but intervening text; find the most
                    // recent action group with tool calls to coalesce into.
                    val anchor = groups.lastOrNull { it.kind == GroupKind.ACTION && it.toolCalls.isNotEmpty() }
                    if (anchor != null) {
                        anchor.events.add(ev)
                        anchor.toolCalls.add(call)
                    } else {
                        groups.add(newActionGroup(ev, call))
                    }
                } else {
                    // New AssistantMessage — start a new action group.
                    groups.add(newActionGroup(ev, call))
                    usageSinceLastTool = false
                }
            }
            EventKinds.ToolResult -> {
                val tr = ev.toolResult ?: continue
                var matched = false
                for (i in groups.indices.reversed()) {
                    val g = groups[i]
                    if (g.kind != GroupKind.ACTION) continue
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
                    groups.add(MutableGroup(kind = GroupKind.ACTION, events = mutableListOf(ev)))
                }
            }
            EventKinds.Ask -> {
                val ask = ev.ask ?: continue
                groups.add(MutableGroup(kind = GroupKind.ASK, events = mutableListOf(ev), ask = ask))
            }
            EventKinds.UserInput -> {
                val prev = lastGroup()
                if (prev != null && prev.kind == GroupKind.ASK && prev.answerText == null) {
                    prev.answerText = ev.userInput?.text
                    prev.events.add(ev)
                } else {
                    groups.add(MutableGroup(kind = GroupKind.USER_INPUT, events = mutableListOf(ev)))
                }
            }
            EventKinds.Usage -> {
                usageSinceLastTool = true
                val last = lastGroup()
                if (last != null && (last.kind == GroupKind.TEXT || last.kind == GroupKind.ACTION)) {
                    last.events.add(ev)
                } else {
                    groups.add(MutableGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
                }
            }
            EventKinds.Todo -> { /* Rendered by ProgressPanel directly; skip to avoid splitting tool groups. */ }
            EventKinds.DiffStat -> { /* Metadata-only; skip. */ }
            EventKinds.Thinking -> {
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.ACTION && last.toolCalls.isEmpty() &&
                    last.events.any { it.kind == EventKinds.ThinkingDelta }
                ) {
                    last.events.add(ev)
                } else {
                    groups.add(MutableGroup(kind = GroupKind.ACTION, events = mutableListOf(ev)))
                }
            }
            EventKinds.ThinkingDelta -> {
                val last = lastGroup()
                if (last != null && last.kind == GroupKind.ACTION && last.toolCalls.isEmpty()) {
                    last.events.add(ev)
                } else {
                    groups.add(MutableGroup(kind = GroupKind.ACTION, events = mutableListOf(ev)))
                }
            }
            // Subagent lifecycle events are not rendered. Explicitly listed to
            // avoid creating OTHER groups that act as hard barriers.
            EventKinds.SubagentStart, EventKinds.SubagentEnd -> {}
            else -> {
                groups.add(MutableGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
            }
        }
    }

    // Merge action groups separated only by text groups. The agent often emits
    // short commentary between tool calls ("Let me read...", "Now let me edit...").
    // Without merging, each appears as a separate 1-tool block. ask, userInput,
    // and other groups act as hard boundaries that prevent merging.
    // Thinking-only action groups adjacent to tool-call action groups are absorbed
    // so their events can be rendered alongside the tool calls.
    val merged = mutableListOf<MutableGroup>()
    for (g in groups) {
        if (g.kind == GroupKind.ACTION && g.toolCalls.isNotEmpty()) {
            // Find the nearest non-text, non-thinking-only anchor action group.
            val anchor = merged.lastOrNull {
                it.kind != GroupKind.TEXT && !(it.kind == GroupKind.ACTION && it.toolCalls.isEmpty())
            }
            // Absorb any trailing thinking-only action groups from the merged list.
            val thinkingEvents = mutableListOf<EventMessage>()
            while (merged.isNotEmpty() && merged.last().kind == GroupKind.ACTION &&
                merged.last().toolCalls.isEmpty()
            ) {
                thinkingEvents.addAll(0, merged.removeAt(merged.lastIndex).events)
            }
            if (anchor != null && anchor.kind == GroupKind.ACTION) {
                anchor.events.addAll(thinkingEvents)
                anchor.events.addAll(g.events)
                anchor.toolCalls.addAll(g.toolCalls)
                continue
            }
            if (thinkingEvents.isNotEmpty()) {
                val combined = MutableGroup(
                    kind = GroupKind.ACTION,
                    events = (thinkingEvents + g.events).toMutableList(),
                    toolCalls = g.toolCalls,
                )
                merged.add(combined)
                continue
            }
        } else if (g.kind == GroupKind.ACTION && g.toolCalls.isEmpty()) {
            // Thinking-only group immediately following a tool-call group: absorb into it.
            val last = merged.lastOrNull()
            if (last != null && last.kind == GroupKind.ACTION && last.toolCalls.isNotEmpty()) {
                last.events.addAll(g.events)
                continue
            }
        }
        merged.add(g)
    }

    // Mark tool calls as implicitly done when later events exist.
    val lastToolIdx = merged.indexOfLast { it.kind == GroupKind.ACTION && it.toolCalls.isNotEmpty() }
    for (i in merged.indices) {
        val g = merged[i]
        if (g.kind != GroupKind.ACTION || g.toolCalls.isEmpty()) continue
        if (i < lastToolIdx || i < merged.size - 1) {
            for (tc in g.toolCalls) tc.done = true
        }
    }

    return merged.map { it.freeze() }
}

private fun newActionGroup(ev: EventMessage, call: MutableToolCall) = MutableGroup(
    kind = GroupKind.ACTION,
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
            GroupKind.ACTION -> toolCount += g.toolCalls.size
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

/**
 * Incremental grouping state that caches completed turns to avoid reprocessing all messages
 * on every SSE flush. Completed turns are those terminated by a Result event and never change.
 */
@Immutable
data class IncrementalGrouped(
    val completedTurns: List<Turn> = emptyList(),
    val currentTurn: Turn? = null,
    val completedUpToIdx: Int = 0,
    val todos: List<TodoItem> = emptyList(),
    val activeAgents: Map<String, String> = emptyMap(), // taskID → description
) {
    val turns: List<Turn> get() = if (currentTurn != null) completedTurns + currentTurn!! else completedTurns
}

/**
 * Computes the next [IncrementalGrouped] from [prev] state and an updated [msgs] snapshot.
 *
 * On append-only growth, only messages from [IncrementalGrouped.completedUpToIdx] onwards are
 * regrouped, keeping completed turns cached. Falls back to a full recompute when the list shrinks
 * (reconnect).
 */
fun nextGrouped(prev: IncrementalGrouped, msgs: List<EventMessage>): IncrementalGrouped {
    val upTo = if (msgs.size >= prev.completedUpToIdx) prev.completedUpToIdx else 0
    val isReset = upTo < prev.completedUpToIdx
    val priorCompleted = if (isReset) emptyList() else prev.completedTurns

    val currentMsgs = msgs.subList(upTo, msgs.size)
    val newTodo = currentMsgs.lastOrNull { it.kind == EventKinds.Todo }?.todo?.todos
    val todos = newTodo ?: if (isReset) emptyList() else prev.todos

    // Compute active subagents incrementally: start from prev (or empty on reset) and
    // apply subagentStart/End events seen in the new message slice.
    val activeAgents = run {
        val map = if (isReset) mutableMapOf() else prev.activeAgents.toMutableMap()
        for (msg in currentMsgs) {
            when (msg.kind) {
                EventKinds.SubagentStart -> msg.subagentStart?.let { map[it.taskID] = it.description }
                EventKinds.SubagentEnd -> msg.subagentEnd?.let { map.remove(it.taskID) }
            }
        }
        map.toMap()
    }

    if (currentMsgs.isEmpty()) {
        return IncrementalGrouped(priorCompleted, null, upTo, todos, activeAgents)
    }

    val groups = groupMessages(currentMsgs)
    val currentTurns = groupTurns(groups)
    // The last turn is complete if it contains a Result event. groupTurns flushes on Result,
    // but also flushes the final partial turn — we must check the content, not position.
    val lastTurnComplete = currentTurns.lastOrNull()?.groups?.any { g ->
        g.kind == GroupKind.OTHER && g.events.any { it.kind == EventKinds.Result }
    } ?: false
    val newlyCompleted = if (lastTurnComplete) currentTurns
        else if (currentTurns.size > 1) currentTurns.dropLast(1)
        else emptyList()
    val currentTurn = if (lastTurnComplete) null else currentTurns.lastOrNull()
    val allCompleted = priorCompleted + newlyCompleted

    val newBoundary = if (newlyCompleted.isEmpty()) {
        upTo
    } else {
        // Advance the boundary past all newly completed turns by counting Result events.
        var count = 0
        var boundary = msgs.size
        for (i in upTo until msgs.size) {
            if (msgs[i].kind == EventKinds.Result) {
                count++
                if (count == newlyCompleted.size) {
                    boundary = i + 1
                    break
                }
            }
        }
        boundary
    }

    return IncrementalGrouped(allCompleted, currentTurn, newBoundary, todos, activeAgents)
}
