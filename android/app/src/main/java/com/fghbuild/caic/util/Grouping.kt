// Message grouping and turn splitting, ported from frontend/src/grouping.ts.
package com.fghbuild.caic.util

import androidx.compose.runtime.Immutable
import com.caic.sdk.v1.EventAsk
import com.caic.sdk.v1.EventMessage
import com.caic.sdk.v1.EventResult
import com.caic.sdk.v1.EventToolResult
import com.caic.sdk.v1.EventToolUse
import com.caic.sdk.v1.TodoItem
import com.caic.sdk.v1.EventKinds
import kotlin.math.max

enum class GroupKind { TEXT, ACTION, ASK, USER_INPUT, WIDGET, OTHER }

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
    val widgetToolUseID: String? = null,
    val widgetTitle: String? = null,
    val widgetHTML: String? = null,
    val widgetDone: Boolean = false,
)

@Immutable
data class Turn(
    val groups: List<MessageGroup>,
    val toolCount: Int,
    val textCount: Int,
    // Duration of the turn in milliseconds (last event ts minus first event ts).
    val durationMs: Long,
    // The result event payload for this turn, if the turn has completed.
    val result: EventResult? = null,
)

// A session is a segment of the event stream opened by an init or compact_boundary event.
// Session boundaries are init events (new Claude Code session) and compact_boundary system
// events (context compaction). The current (last) session is never elided.
@Immutable
data class Session(
    // The event that opened this session (init or compact_boundary system event).
    // null for an implicit initial segment before the first session event.
    val boundaryEvent: EventMessage? = null,
    val turns: List<Turn>,
    val toolCount: Int,
    val textCount: Int,
)

// Tool names (lowercase) that are async and emit explicit toolResult events.
// All other Claude Code tools complete synchronously and are done as soon as
// their toolUse event is emitted.
private val ASYNC_TOOLS = setOf("bash", "task", "show_widget")

/** Returns true if this event starts a new session. */
fun EventMessage.isSessionBoundary() =
    kind == EventKinds.Init ||
        (kind == EventKinds.System && system?.subtype == "compact_boundary")

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
    var widgetToolUseID: String? = null,
    var widgetTitle: String? = null,
    var widgetHTML: String? = null,
    var widgetDone: Boolean = false,
) {
    fun freeze() = MessageGroup(
        kind = kind,
        events = events.toList(),
        toolCalls = toolCalls.map { it.freeze() },
        ask = ask,
        answerText = answerText,
        widgetToolUseID = widgetToolUseID,
        widgetTitle = widgetTitle,
        widgetHTML = widgetHTML,
        widgetDone = widgetDone,
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
                // Check widget groups first for matching toolResult.
                for (i in groups.indices.reversed()) {
                    val g = groups[i]
                    if (g.kind == GroupKind.WIDGET && g.widgetToolUseID == tr.toolUseID) {
                        g.widgetDone = true
                        g.events.add(ev)
                        matched = true
                        break
                    }
                }
                if (!matched) {
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
                // Look backwards past result/other groups to find the most recent
                // unanswered ask group. The agent emits a result event after
                // AskUserQuestion, so the ask group is typically not the last group.
                var askGroup: MutableGroup? = null
                for (i in groups.indices.reversed()) {
                    val g = groups[i]
                    if (g.kind == GroupKind.ASK && g.answerText == null) { askGroup = g; break }
                    if (g.kind != GroupKind.OTHER) break // stop at non-other boundaries
                }
                if (askGroup != null) {
                    askGroup.answerText = ev.userInput?.text
                    askGroup.events.add(ev)
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
            EventKinds.ToolOutputDelta -> {
                // Append to the most recent action group that owns this tool call so
                // the accumulated output can be displayed inside its ToolCallCard.
                val id = ev.toolOutputDelta?.toolUseID
                if (id != null) {
                    for (i in groups.indices.reversed()) {
                        val g = groups[i]
                        if (g.kind != GroupKind.ACTION) continue
                        if (g.toolCalls.any { it.use.toolUseID == id }) {
                            g.events.add(ev)
                            break
                        }
                    }
                }
            }
            EventKinds.System -> {
                // compact_boundary is consumed by groupSessions() before reaching here.
                // Thread status changes (active, idle, etc.) duplicate information already in the
                // task state — skip them to avoid noisy OTHER groups.
                // model_rerouted and other informational subtypes are rendered via OTHER.
                val sub = ev.system?.subtype
                if (sub != "active" && sub != "idle" && sub != "notLoaded" && sub != "system_error") {
                    groups.add(MutableGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
                }
            }
            EventKinds.WidgetDelta -> {
                val id = ev.widgetDelta?.toolUseID
                if (id != null) {
                    val existing = groups.lastOrNull { it.kind == GroupKind.WIDGET && it.widgetToolUseID == id }
                    if (existing != null) {
                        existing.events.add(ev)
                        existing.widgetHTML = (existing.widgetHTML ?: "") + (ev.widgetDelta?.delta ?: "")
                    } else {
                        groups.add(MutableGroup(
                            kind = GroupKind.WIDGET,
                            events = mutableListOf(ev),
                            widgetToolUseID = id,
                            widgetHTML = ev.widgetDelta?.delta ?: "",
                        ))
                    }
                }
            }
            EventKinds.Widget -> {
                val widget = ev.widget ?: continue
                val id = widget.toolUseID
                val existing = groups.lastOrNull { it.kind == GroupKind.WIDGET && it.widgetToolUseID == id }
                if (existing != null) {
                    existing.events.add(ev)
                    existing.widgetTitle = widget.title
                    existing.widgetHTML = widget.html
                } else {
                    groups.add(MutableGroup(
                        kind = GroupKind.WIDGET,
                        events = mutableListOf(ev),
                        widgetToolUseID = id,
                        widgetTitle = widget.title,
                        widgetHTML = widget.html,
                    ))
                }
            }
            EventKinds.RateLimit -> {
                // Only surface warning/rejected statuses; "allowed" is not interesting.
                val status = ev.rateLimit?.status
                if (status != null && status != "allowed") {
                    groups.add(MutableGroup(kind = GroupKind.OTHER, events = mutableListOf(ev)))
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
        } else if (g.kind == GroupKind.TEXT) {
            // Text group with embedded thinking (from initial-pass absorption) following
            // a tool-call group: move thinking events into the tool group so they render
            // inside the collapsed tool block, not as a standalone ThinkingCard.
            val last = merged.lastOrNull()
            if (last != null && last.kind == GroupKind.ACTION && last.toolCalls.isNotEmpty()) {
                val thinkingEvs = g.events.filter {
                    it.kind == EventKinds.Thinking || it.kind == EventKinds.ThinkingDelta
                }
                if (thinkingEvs.isNotEmpty()) {
                    last.events.addAll(thinkingEvs)
                    g.events.removeAll { it.kind == EventKinds.Thinking || it.kind == EventKinds.ThinkingDelta }
                }
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

// Splits message groups into turns separated by "result" events.
// ResultMessage.DurationMs is per-invocation; use it directly as the turn's duration.
fun groupTurns(groups: List<MessageGroup>): List<Turn> {
    val turns = mutableListOf<Turn>()
    var current = mutableListOf<MessageGroup>()
    var toolCount = 0
    var textCount = 0
    var firstTs = 0L
    var lastTs = 0L
    var hasTs = false
    // Authoritative duration from the result event (seconds → ms).
    // Null for live incomplete turns, which fall back to ts-based computation.
    var resultDurationMs: Long? = null
    // True when a result event has been seen for this turn (even if duration == 0).
    // Completed turns don't fall back to ts-based, which would inflate with idle time.
    var hasResultEvent = false
    var resultPayload: EventResult? = null

    fun flush() {
        if (current.isNotEmpty()) {
            val durationMs = if (hasResultEvent) (resultDurationMs ?: 0L) else max(0L, lastTs - firstTs)
            turns.add(Turn(
                groups = current.toList(),
                toolCount = toolCount,
                textCount = textCount,
                durationMs = durationMs,
                result = resultPayload,
            ))
            current = mutableListOf()
            toolCount = 0
            textCount = 0
            firstTs = 0L
            lastTs = 0L
            hasTs = false
            resultDurationMs = null
            hasResultEvent = false
            resultPayload = null
        }
    }

    for (g in groups) {
        current.add(g)
        when (g.kind) {
            GroupKind.ACTION -> toolCount += g.toolCalls.size
            GroupKind.TEXT -> textCount++
            else -> {}
        }
        for (ev in g.events) {
            if (!hasTs) { firstTs = ev.ts; hasTs = true }
            lastTs = ev.ts
            if (ev.kind == EventKinds.Result) {
                hasResultEvent = true
                resultPayload = ev.result
                val durationMs = ((ev.result?.duration ?: 0.0) * 1000).toLong()
                if (durationMs > 0L) {
                    resultDurationMs = durationMs
                }
            }
        }
        if (g.kind == GroupKind.OTHER && g.events.any { it.kind == EventKinds.Result }) {
            flush()
        }
    }
    flush()
    return turns
}

// Splits messages into sessions at init (only when sessionID changes) and compact_boundary events.
// The boundary event starts the new session and is NOT passed to groupMessages.
// Re-invocations of Claude Code that share the same sessionID are NOT new sessions.
fun groupSessions(msgs: List<EventMessage>): List<Session> {
    val sessions = mutableListOf<Session>()
    var segment = mutableListOf<EventMessage>()
    var boundaryEvent: EventMessage? = null
    var currentSessionID: String? = null

    fun flushSession() {
        val groups = groupMessages(segment)
        val turns = groupTurns(groups)
        var toolCount = 0
        var textCount = 0
        for (t in turns) {
            toolCount += t.toolCount
            textCount += t.textCount
        }
        sessions.add(Session(boundaryEvent = boundaryEvent, turns = turns, toolCount = toolCount, textCount = textCount))
        boundaryEvent = null
        segment = mutableListOf()
    }

    fun flushAndCarry() {
        val lastResultIdx = segment.indexOfLast { it.kind == EventKinds.Result }
        val carry: List<EventMessage> = if (lastResultIdx in 0 until segment.size - 1) {
            segment.subList(lastResultIdx + 1, segment.size).toList().also {
                while (segment.size > lastResultIdx + 1) segment.removeAt(segment.lastIndex)
            }
        } else emptyList()
        flushSession()
        segment.addAll(carry)
    }

    for (ev in msgs) {
        when {
            ev.kind == EventKinds.Init -> {
                val newID = ev.init?.sessionID
                if (newID != currentSessionID) {
                    if (boundaryEvent != null) flushAndCarry()
                    boundaryEvent = ev
                    currentSessionID = newID
                }
                // Same sessionID: re-invocation within the same session; skip.
            }
            ev.kind == EventKinds.System && ev.system?.subtype == "compact_boundary" -> {
                if (boundaryEvent != null) flushAndCarry()
                boundaryEvent = ev
                currentSessionID = null
            }
            else -> segment.add(ev)
        }
    }
    if (segment.isNotEmpty() || boundaryEvent != null) {
        flushSession()
    }
    return sessions
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
    val summary = if (parts.isNotEmpty()) parts.joinToString(", ") else "empty turn"
    return if (turn.durationMs > 0) "$summary \u00b7 ${formatElapsed(turn.durationMs / 1000.0)}" else summary
}

/** Summarize a session for elided display. */
fun sessionSummary(session: Session): String {
    val parts = mutableListOf<String>()
    val boundary = session.boundaryEvent
    if (boundary?.kind == EventKinds.Init && boundary.init != null) {
        parts.add("Session ${boundary.init!!.sessionID.take(8)}")
    } else {
        parts.add("Compacted session")
    }
    if (session.textCount > 0) {
        parts.add(if (session.textCount == 1) "1 message" else "${session.textCount} messages")
    }
    if (session.toolCount > 0) {
        parts.add(if (session.toolCount == 1) "1 tool call" else "${session.toolCount} tool calls")
    }
    return parts.joinToString(" \u00b7 ")
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
 * Incrementally grouped state tracking sessions, turns, and the current live turn.
 *
 * Sessions are separated by init or compact_boundary events. Within the current session,
 * completed turns are cached and only new messages are reprocessed on each SSE flush.
 */
@Immutable
data class IncrementalGrouped(
    /** All sessions before the current (last) session. */
    val completedSessions: List<Session> = emptyList(),
    /** The boundary event (init/compact_boundary) that opened the current session; null if none. */
    val currentSessionBoundaryEvent: EventMessage? = null,
    /** Completed turns within the current session (delimited by result events). */
    val currentSessionCompletedTurns: List<Turn> = emptyList(),
    /** The in-progress turn at the end of the current session; null if current session is idle. */
    val currentTurn: Turn? = null,
    /** Global message index past the last result event in the current session. */
    val completedUpToIdx: Int = 0,
    /** Global message index of the first content message in the current session (after boundary). */
    val currentSessionStart: Int = 0,
    val todos: List<TodoItem> = emptyList(),
    val activeAgents: Map<String, String> = emptyMap(),
) {
    /** All sessions including the current one (for display). */
    val sessions: List<Session>
        get() {
            val curTurns = currentSessionCompletedTurns + listOfNotNull(currentTurn)
            val currentSession = Session(
                boundaryEvent = currentSessionBoundaryEvent,
                turns = curTurns,
                toolCount = curTurns.sumOf { it.toolCount },
                textCount = curTurns.sumOf { it.textCount },
            )
            return if (currentSession.turns.isEmpty() && currentSession.boundaryEvent == null) {
                completedSessions
            } else {
                completedSessions + currentSession
            }
        }
}

/**
 * Computes the next [IncrementalGrouped] from [prev] state and an updated [msgs] snapshot.
 *
 * Session boundaries (init/compact_boundary) cause past sessions to be recomputed from scratch
 * (rare event). Within the current session, only messages since the last result event are
 * regrouped, keeping completed turns cached.
 */
@Suppress("CyclomaticComplexMethod", "LongMethod")
fun nextGrouped(prev: IncrementalGrouped, msgs: List<EventMessage>): IncrementalGrouped {
    // Reset detection: if message list shrunk (reconnect), start from scratch.
    val upTo = if (msgs.size >= prev.completedUpToIdx) prev.completedUpToIdx else 0
    val isReset = upTo < prev.completedUpToIdx

    // Compute todos and active agents from messages since last processed boundary.
    val newMsgs = msgs.subList(upTo, msgs.size)
    val newTodo = newMsgs.lastOrNull { it.kind == EventKinds.Todo }?.todo?.todos
    val todos = newTodo ?: if (isReset) emptyList() else prev.todos
    val activeAgents = run {
        val map = if (isReset) mutableMapOf() else prev.activeAgents.toMutableMap()
        for (msg in newMsgs) {
            when (msg.kind) {
                EventKinds.SubagentStart -> msg.subagentStart?.let { map[it.taskID] = it.description }
                EventKinds.SubagentEnd -> msg.subagentEnd?.let { map.remove(it.taskID) }
            }
        }
        map.toMap()
    }

    if (msgs.isEmpty()) {
        return IncrementalGrouped(todos = todos, activeAgents = activeAgents)
    }

    // Check if a genuinely new session boundary appeared in the newly arrived messages.
    // An init is only a new boundary when its sessionID differs from the current one.
    var lastSessionBoundaryInNew = -1
    var lastBoundaryGlobalIdx = if (isReset) -1 else (prev.currentSessionStart - 1)
    var scanSessionID: String? = prev.currentSessionBoundaryEvent?.init?.sessionID
    for (i in newMsgs.indices) {
        val msg = newMsgs[i]
        val isNewBoundary = when {
            msg.kind == EventKinds.System && msg.system?.subtype == "compact_boundary" -> true
            msg.kind == EventKinds.Init && msg.init?.sessionID != scanSessionID -> true
            else -> false
        }
        if (isNewBoundary) {
            lastSessionBoundaryInNew = i
            lastBoundaryGlobalIdx = upTo + i
            scanSessionID = if (msg.kind == EventKinds.Init) msg.init?.sessionID else null
        }
    }
    val sessionChanged = isReset || lastSessionBoundaryInNew >= 0

    if (sessionChanged) {

        // Past sessions: all messages before the last boundary.
        // Filter out null-boundary sessions: these are pre-init messages (e.g. the initial
        // userInput) that arrived before any session event. They should not be shown as
        // a phantom "Compacted session" — they belong conceptually to the first real session.
        val pastMsgs = if (lastBoundaryGlobalIdx > 0) msgs.subList(0, lastBoundaryGlobalIdx) else emptyList()
        val completedSessions = if (lastBoundaryGlobalIdx >= 0) {
            groupSessions(pastMsgs).filter { it.boundaryEvent != null }
        } else emptyList()

        // Current session starts after the boundary event.
        val newCurrentSessionStart = lastBoundaryGlobalIdx + 1
        val newCurrentSessionBoundary = if (lastBoundaryGlobalIdx >= 0) msgs[lastBoundaryGlobalIdx] else null

        // Process current session messages (excluding any further boundary events).
        val currentSessionMsgs = msgs.subList(newCurrentSessionStart, msgs.size)
            .filter { !it.isSessionBoundary() }
        val groups = groupMessages(currentSessionMsgs)
        val currentTurns = groupTurns(groups)

        val lastTurnComplete = currentTurns.lastOrNull()?.groups?.any { g ->
            g.kind == GroupKind.OTHER && g.events.any { it.kind == EventKinds.Result }
        } ?: false
        val newlyCompleted = if (lastTurnComplete) currentTurns
            else if (currentTurns.size > 1) currentTurns.dropLast(1)
            else emptyList()
        val currentTurn = if (lastTurnComplete) null else currentTurns.lastOrNull()

        val newBoundary = if (newlyCompleted.isEmpty()) {
            newCurrentSessionStart
        } else {
            var count = 0
            var boundary = msgs.size
            for (i in newCurrentSessionStart until msgs.size) {
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

        return IncrementalGrouped(
            completedSessions = completedSessions,
            currentSessionBoundaryEvent = newCurrentSessionBoundary,
            currentSessionCompletedTurns = newlyCompleted,
            currentTurn = currentTurn,
            completedUpToIdx = newBoundary,
            currentSessionStart = newCurrentSessionStart,
            todos = todos,
            activeAgents = activeAgents,
        )
    }

    // No session boundary change. Incremental turn update within current session.
    val priorCompleted = prev.currentSessionCompletedTurns

    val currentSessionNewMsgs = newMsgs.filter { !it.isSessionBoundary() }
    if (currentSessionNewMsgs.isEmpty()) {
        return prev.copy(todos = todos, activeAgents = activeAgents)
    }

    val groups = groupMessages(currentSessionNewMsgs)
    val currentTurns = groupTurns(groups)
    val lastTurnComplete = currentTurns.lastOrNull()?.groups?.any { g ->
        g.kind == GroupKind.OTHER && g.events.any { it.kind == EventKinds.Result }
    } ?: false
    val newlyCompleted = if (lastTurnComplete) currentTurns
        else if (currentTurns.size > 1) currentTurns.dropLast(1)
        else emptyList()
    val currentTurn = if (lastTurnComplete) null else currentTurns.lastOrNull()
    // Reuse the same list reference when nothing changed so callers can use referential equality
    // (e.g. Compose remember keys) to skip recomputation.
    val allCompleted = if (newlyCompleted.isEmpty()) priorCompleted else priorCompleted + newlyCompleted

    val newBoundary = if (newlyCompleted.isEmpty()) {
        upTo
    } else {
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

    return prev.copy(
        currentSessionCompletedTurns = allCompleted,
        currentTurn = currentTurn,
        completedUpToIdx = newBoundary,
        todos = todos,
        activeAgents = activeAgents,
    )
}
