// Unit tests for message grouping and turn splitting logic.
package com.fghbuild.caic.util

import com.caic.sdk.v1.EventMessage
import com.caic.sdk.v1.EventText
import com.caic.sdk.v1.EventTextDelta
import com.caic.sdk.v1.EventToolResult
import com.caic.sdk.v1.EventToolUse
import com.caic.sdk.v1.EventAsk
import com.caic.sdk.v1.AskQuestion
import com.caic.sdk.v1.EventInit
import com.caic.sdk.v1.EventResult
import com.caic.sdk.v1.EventUsage
import com.caic.sdk.v1.EventUserInput
import com.caic.sdk.v1.EventKind
import com.caic.sdk.v1.EventThinking
import com.caic.sdk.v1.EventThinkingDelta
import com.caic.sdk.v1.EventWidget
import com.caic.sdk.v1.EventRateLimit
import com.caic.sdk.v1.EventWidgetDelta
import java.time.Instant
import kotlinx.serialization.json.JsonObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

class GroupingTest {
    private fun textDeltaEvent(text: String, ts: Long = 0) = EventMessage(
        kind = EventKind.TextDelta, ts = ts,
        textDelta = EventTextDelta(text = text),
    )

    private fun textEvent(text: String, ts: Long = 0) = EventMessage(
        kind = EventKind.Text, ts = ts,
        text = EventText(text = text),
    )

    private fun toolUseEvent(id: String, name: String, ts: Long = 0) = EventMessage(
        kind = EventKind.ToolUse, ts = ts,
        toolUse = EventToolUse(toolUseID = id, name = name, input = JsonObject(emptyMap())),
    )

    private fun toolResultEvent(id: String, duration: Double = 0.1, ts: Long = 0) = EventMessage(
        kind = EventKind.ToolResult, ts = ts,
        toolResult = EventToolResult(toolUseID = id, duration = duration),
    )

    @Suppress("LongMethod")
    private fun resultEvent(ts: Long = 0) = EventMessage(
        kind = EventKind.Result, ts = ts,
        result = EventResult(
            subtype = "success", isError = false, result = "done",
            totalCostUSD = 0.01, duration = 1.0, durationAPI = 0.9,
            numTurns = 1, usage = EventUsage(
                inputTokens = 100, outputTokens = 50,
                cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
            ),
        ),
    )

    private fun askEvent(id: String, question: String, ts: Long = 0) = EventMessage(
        kind = EventKind.Ask, ts = ts,
        ask = EventAsk(
            toolUseID = id,
            questions = listOf(AskQuestion(question = question, options = emptyList())),
        ),
    )

    private fun userInputEvent(text: String, ts: Long = 0) = EventMessage(
        kind = EventKind.UserInput, ts = ts,
        userInput = EventUserInput(text = text),
    )

    @Test
    fun testGroupMessages() {
        t.run("consecutive textDelta events merge into one text group") {
            val groups = groupMessages(listOf(textDeltaEvent("hello "), textDeltaEvent("world")))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.TEXT, groups[0].kind)
            assertEquals(2, groups[0].events.size)
        }

        t.run("text event after textDelta merges into same group") {
            val groups = groupMessages(listOf(textDeltaEvent("draft"), textEvent("final")))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.TEXT, groups[0].kind)
            assertEquals(2, groups[0].events.size)
        }

        t.run("consecutive tool uses form one tool group") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                toolUseEvent("t2", "Bash"),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertEquals(2, groups[0].toolCalls.size)
        }

        t.run("toolResult matches backwards across groups") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                textDeltaEvent("text"),
                toolResultEvent("t1"),
            ))
            assertEquals(2, groups.size)
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertTrue(groups[0].toolCalls[0].done)
            assertEquals("t1", groups[0].toolCalls[0].result?.toolUseID)
        }

        t.run("ask followed by userInput merges answerText") {
            val groups = groupMessages(listOf(
                askEvent("a1", "Continue?"),
                userInputEvent("yes"),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.ASK, groups[0].kind)
            assertEquals("yes", groups[0].answerText)
        }

        t.run("ask followed by result then userInput merges answerText") {
            val groups = groupMessages(listOf(
                askEvent("a1", "Which?"),
                resultEvent(),
                userInputEvent("A"),
            ))
            val askGroup = groups.find { it.kind == GroupKind.ASK }
            assertNotNull(askGroup)
            assertEquals("A", askGroup?.answerText)
        }

        t.run("userInput without preceding ask creates standalone group") {
            val groups = groupMessages(listOf(userInputEvent("hello")))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.USER_INPUT, groups[0].kind)
        }

        t.run("tool calls in same assistant message coalesce across text") {
            // Without a usage event between them, tool calls in the same
            // AssistantMessage are coalesced into one tool group.
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                textDeltaEvent("text"),
                toolUseEvent("t2", "Bash"),
            ))
            assertEquals(2, groups.size) // [TOOL(t1+t2), TEXT]
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertEquals(2, groups[0].toolCalls.size)
        }

        t.run("usage event separates tool groups across assistant messages") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                toolUseEvent("t2", "Bash"),
            ))
            // After merge pass, tool groups separated only by text merge, but
            // a usage boundary creates a new tool group. The merge pass then
            // re-merges them because only text/tool groups sit between.
            assertEquals(1, groups.size)
            assertEquals(2, groups[0].toolCalls.size)
        }

        t.run("non-last tool calls in last group are marked done") {
            // First call is done (agent moved to second); last call is still pending.
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Bash"),
                toolUseEvent("t2", "Read"),
            ))
            assertEquals(1, groups.size)
            assertTrue(groups[0].toolCalls[0].done)
            assertTrue(!groups[0].toolCalls[1].done)
        }

        t.run("non-last tool groups are implicitly marked done") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                textDeltaEvent("text"),
                toolUseEvent("t2", "Bash"),
            ))
            // After merge pass these merge into 1 tool group + 1 text group.
            assertEquals(2, groups.size)
            assertTrue(groups[0].toolCalls[0].done)
            assertTrue(groups[0].toolCalls[1].done)
        }

        t.run("rateLimit warning creates OTHER group") {
            val groups = groupMessages(listOf(EventMessage(
                kind = EventKind.RateLimit, ts = 1,
                rateLimit = EventRateLimit(
                    status = "allowed_warning",
                    rateLimitType = "five_hour", utilization = 0.8,
                ),
            )))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.OTHER, groups[0].kind)
        }

        t.run("rateLimit allowed is filtered out") {
            val groups = groupMessages(listOf(EventMessage(
                kind = EventKind.RateLimit, ts = 1,
                rateLimit = EventRateLimit(
                    status = "allowed",
                    rateLimitType = "five_hour", utilization = 0.3,
                ),
            )))
            assertEquals(0, groups.size)
        }

        t.run("rateLimit rejected creates OTHER group") {
            val groups = groupMessages(listOf(EventMessage(
                kind = EventKind.RateLimit, ts = 1,
                rateLimit = EventRateLimit(
                    status = "rejected", resetsAt = Instant.ofEpochSecond(1711000000),
                    rateLimitType = "seven_day", utilization = 1.0,
                ),
            )))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.OTHER, groups[0].kind)
        }
    }

    @Test
    fun testGroupTurns() {
        t.run("result event splits turns") {
            val events = listOf(
                textDeltaEvent("first turn"),
                toolUseEvent("t1", "Read"),
                resultEvent(),
                textDeltaEvent("second turn"),
            )
            val groups = groupMessages(events)
            val turns = groupTurns(groups)
            assertEquals(2, turns.size)
            assertEquals(1, turns[0].toolCount)
            assertEquals(1, turns[0].textCount)
            assertEquals(0, turns[1].toolCount)
            assertEquals(1, turns[1].textCount)
        }

        t.run("durationMs comes from result event duration") {
            val events = listOf(textDeltaEvent("text"), resultEvent())
            val groups = groupMessages(events)
            val turns = groupTurns(groups)
            assertEquals(1, turns.size)
            assertEquals(1000L, turns[0].durationMs) // resultEvent() has duration: 1.0s
        }

        t.run("durationMs uses result.duration directly (per-invocation, not cumulative)") {
            // ResultMessage.DurationMs is per-invocation wall-clock time for that turn.
            fun makeResult(duration: Double) = EventMessage(
                kind = EventKind.Result, ts = 0,
                result = EventResult(
                    subtype = "success", isError = false, result = "done",
                    totalCostUSD = 0.01, duration = duration, durationAPI = duration * 0.9,
                    numTurns = 1, usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
            )
            val events = listOf(
                textDeltaEvent("turn 1"),
                makeResult(1.0),  // turn 1 took 1s
                textDeltaEvent("turn 2"),
                makeResult(3.0),  // turn 2 took 3s
            )
            val groups = groupMessages(events)
            val turns = groupTurns(groups)
            assertEquals(2, turns.size)
            assertEquals(1000L, turns[0].durationMs) // 1.0s → 1000ms
            assertEquals(3000L, turns[1].durationMs) // 3.0s → 3000ms
        }

        t.run("turnSummary formats correctly") {
            val turn = Turn(groups = emptyList(), toolCount = 3, textCount = 2, durationMs = 5000)
            assertEquals("2 messages, 3 tool calls · 5s", turnSummary(turn))
        }

        t.run("turnSummary singular forms") {
            val turn = Turn(groups = emptyList(), toolCount = 1, textCount = 1, durationMs = 65000)
            assertEquals("1 message, 1 tool call · 1m 5s", turnSummary(turn))
        }
    }

    @Test
    fun testMergePass() {
        t.run("tool groups separated by text merge into one") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                textDeltaEvent("commentary"),
                toolUseEvent("t2", "Bash"),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 200, outputTokens = 100,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                textDeltaEvent("more commentary"),
                toolUseEvent("t3", "Edit"),
            ))
            // Three tool groups separated by text → merge pass consolidates into one.
            assertEquals(3, groups.size) // [TOOL(t1+t2+t3), TEXT, TEXT]
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertEquals(3, groups[0].toolCalls.size)
        }

        t.run("ask group prevents tool group merging across turns") {
            // In practice, an ask is always followed by a usage event from the
            // next assistant turn. The ask + usage together form a hard boundary.
            val usage = EventMessage(
                kind = EventKind.Usage, ts = 0,
                usage = EventUsage(
                    inputTokens = 100, outputTokens = 50,
                    cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                ),
            )
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                usage,
                askEvent("a1", "Continue?"),
                userInputEvent("yes"),
                toolUseEvent("t2", "Bash"),
            ))
            // Ask is a hard boundary — merge pass won't merge tool groups across it.
            assertEquals(3, groups.size)
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertEquals(GroupKind.ASK, groups[1].kind)
            assertEquals(GroupKind.ACTION, groups[2].kind)
        }

        t.run("todo events are skipped and don't split tool groups") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                EventMessage(kind = EventKind.Todo, ts = 0),
                toolUseEvent("t2", "Bash"),
            ))
            assertEquals(1, groups.size)
            assertEquals(2, groups[0].toolCalls.size)
        }

        t.run("thinking followed by usage does not create a barrier before tool use") {
            // usage after a thinking-only group must not create an OTHER barrier that
            // prevents the merge pass from absorbing thinking into the tool group.
            val groups = groupMessages(listOf(
                EventMessage(
                    kind = EventKind.ThinkingDelta, ts = 0,
                    thinkingDelta = EventThinkingDelta(text = "thinking..."),
                ),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                toolUseEvent("t1", "Read"),
            ))
            assertTrue(groups.none { it.kind == GroupKind.OTHER })
            assertEquals(1, groups.size)
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertEquals(1, groups[0].toolCalls.size)
            assertTrue(groups[0].events.any { it.kind == EventKind.ThinkingDelta })
        }

        t.run("thinking events are absorbed into an adjacent tool group") {
            // Realistic pattern: usage ends the first assistant message, then
            // thinking precedes the next tool call in a new assistant message.
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                EventMessage(kind = EventKind.Thinking, ts = 0, thinking = EventThinking("hmm")),
                EventMessage(kind = EventKind.SubagentStart, ts = 0),
                toolUseEvent("t2", "Bash"),
                EventMessage(kind = EventKind.SubagentEnd, ts = 0),
            ))
            // Thinking is absorbed into the merged action group; no standalone thinking group.
            val toolGroup = groups.first { it.kind == GroupKind.ACTION }
            assertEquals(2, toolGroup.toolCalls.size)
            assertTrue(toolGroup.events.any { it.kind == EventKind.Thinking })
            // Subagent events don't create groups.
            assertTrue(groups.none { it.kind == GroupKind.OTHER })
        }

        t.run("thinking immediately after a tool group is absorbed into it") {
            // The agent may start a new thinking block right after tool calls complete,
            // before any text commentary. It should merge into the preceding tool group.
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                EventMessage(
                    kind = EventKind.Usage, ts = 0,
                    usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                EventMessage(
                    kind = EventKind.ThinkingDelta, ts = 0,
                    thinkingDelta = EventThinkingDelta(text = "analyzing..."),
                ),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.ACTION, groups[0].kind)
            assertEquals(1, groups[0].toolCalls.size)
            assertTrue(groups[0].events.any { it.kind == EventKind.ThinkingDelta })
        }

        t.run("thinking before text after tool group is moved into the tool group") {
            // Regression: thinking-2 between a tool group and text was absorbed into
            // the text group by the initial pass, then rendered as a standalone
            // ThinkingCard outside the tool group. The merge pass should extract
            // thinking events from the text group into the preceding tool group.
            val usage = EventMessage(
                kind = EventKind.Usage, ts = 0,
                usage = EventUsage(
                    inputTokens = 100, outputTokens = 50,
                    cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                ),
            )
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                usage,
                EventMessage(
                    kind = EventKind.Thinking, ts = 0,
                    thinking = EventThinking(text = "reflecting"),
                ),
                textDeltaEvent("The result is..."),
            ))
            assertEquals(2, groups.size)
            val toolGroup = groups.first { it.kind == GroupKind.ACTION }
            val textGroup = groups.first { it.kind == GroupKind.TEXT }
            assertTrue(toolGroup.events.any { it.kind == EventKind.Thinking })
            assertTrue(textGroup.events.none { it.kind == EventKind.Thinking })
        }

        t.run("thinking followed by text is absorbed into the text group") {
            // Standalone thinking before text commentary must not produce a separate
            // Thinking block; it should be embedded inside the text group instead.
            val groups = groupMessages(listOf(
                EventMessage(
                    kind = EventKind.ThinkingDelta, ts = 0,
                    thinkingDelta = EventThinkingDelta(text = "thinking..."),
                ),
                textDeltaEvent("hello"),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.TEXT, groups[0].kind)
            assertTrue(groups[0].events.any { it.kind == EventKind.ThinkingDelta })
            assertTrue(groups[0].events.any { it.kind == EventKind.TextDelta })
        }
    }

    @Test
    fun testWidgetGrouping() {
        t.run("widgetDelta events create a widget group") {
            val groups = groupMessages(listOf(
                EventMessage(
                    kind = EventKind.WidgetDelta, ts = 0,
                    widgetDelta = EventWidgetDelta(toolUseID = "w1", delta = "<h1>"),
                ),
                EventMessage(
                    kind = EventKind.WidgetDelta, ts = 0,
                    widgetDelta = EventWidgetDelta(toolUseID = "w1", delta = "Hi</h1>"),
                ),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.WIDGET, groups[0].kind)
            assertEquals("w1", groups[0].widgetToolUseID)
            assertEquals("<h1>Hi</h1>", groups[0].widgetHTML)
            assertEquals(false, groups[0].widgetDone)
        }

        t.run("widget event finalises widget group from deltas") {
            val groups = groupMessages(listOf(
                EventMessage(
                    kind = EventKind.WidgetDelta, ts = 0,
                    widgetDelta = EventWidgetDelta(toolUseID = "w1", delta = "<h1>"),
                ),
                EventMessage(
                    kind = EventKind.Widget, ts = 0,
                    widget = EventWidget(toolUseID = "w1", title = "Chart", html = "<h1>Done</h1>"),
                ),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.WIDGET, groups[0].kind)
            assertEquals("<h1>Done</h1>", groups[0].widgetHTML)
            assertEquals("Chart", groups[0].widgetTitle)
        }

        t.run("widget event alone creates a widget group (replay)") {
            val groups = groupMessages(listOf(
                EventMessage(
                    kind = EventKind.Widget, ts = 0,
                    widget = EventWidget(toolUseID = "w1", title = "Test", html = "<p>hi</p>"),
                ),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.WIDGET, groups[0].kind)
            assertEquals("<p>hi</p>", groups[0].widgetHTML)
            assertEquals("Test", groups[0].widgetTitle)
        }

        t.run("toolResult for widget marks widgetDone") {
            val groups = groupMessages(listOf(
                EventMessage(
                    kind = EventKind.WidgetDelta, ts = 0,
                    widgetDelta = EventWidgetDelta(toolUseID = "w1", delta = "<p>x</p>"),
                ),
                toolResultEvent("w1"),
            ))
            assertEquals(1, groups.size)
            assertEquals(GroupKind.WIDGET, groups[0].kind)
            assertEquals(true, groups[0].widgetDone)
        }
    }

    @Test
    fun testNextGrouped() {
        t.run("currentSessionCompletedTurns reference is stable across incremental live-turn updates") {
            // One completed turn then a live turn message arrives.
            val turn1Msgs = listOf(textDeltaEvent("first"), resultEvent(ts = 1))
            val state1 = nextGrouped(IncrementalGrouped(), turn1Msgs)
            assertEquals(1, state1.currentSessionCompletedTurns.size)
            assertEquals(null, state1.currentTurn)

            // Add a live message — currentSessionCompletedTurns must be the same list reference.
            val state2 = nextGrouped(state1, turn1Msgs + textDeltaEvent("live", ts = 2))
            assertSame(state1.currentSessionCompletedTurns, state2.currentSessionCompletedTurns)
            assertEquals(1, state2.currentSessionCompletedTurns.size)
        }

        t.run("currentSessionCompletedTurns grows on result event") {
            val turn1 = listOf(textDeltaEvent("first"), resultEvent(ts = 1))
            val state1 = nextGrouped(IncrementalGrouped(), turn1)
            val live = turn1 + listOf(textDeltaEvent("second"), resultEvent(ts = 2))
            val state2 = nextGrouped(state1, live)
            assertEquals(2, state2.currentSessionCompletedTurns.size)
            assertEquals(null, state2.currentTurn)
        }

        t.run("pre-init userInput does not appear as Compacted session in completedSessions") {
            // When the message stream starts with a userInput before the first init,
            // the userInput must not be placed in a null-boundary completedSession
            // and rendered as a phantom "Compacted session".
            val msgs = listOf(
                userInputEvent("initial prompt", ts = 0),
                EventMessage(
                    kind = EventKind.Init, ts = 1L,
                    init = EventInit(sessionID = "s1", model = "m", agentVersion = "1", tools = emptyList(), cwd = "/", harness = "claude"),
                ),
                textDeltaEvent("response", ts = 2),
                resultEvent(ts = 3),
            )
            val state = nextGrouped(IncrementalGrouped(), msgs)
            // completedSessions must contain no null-boundary sessions
            assertTrue("null-boundary session must not appear in completedSessions",
                state.completedSessions.none { it.boundaryEvent == null })
        }

        t.run("per-turn duration is correct across incremental updates") {
            // Simulate turn 1 completing, then turn 2 completing incrementally.
            // Both result events have per-invocation DurationMs (1s and 3s).
            fun makeResult(duration: Double, ts: Long) = EventMessage(
                kind = EventKind.Result, ts = ts,
                result = EventResult(
                    subtype = "success", isError = false, result = "done",
                    totalCostUSD = 0.01, duration = duration, durationAPI = duration * 0.9,
                    numTurns = 1, usage = EventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
            )
            // Turn 1 arrives.
            val turn1Msgs = listOf(textDeltaEvent("first", ts = 1), makeResult(1.0, ts = 2))
            val state1 = nextGrouped(IncrementalGrouped(), turn1Msgs)
            assertEquals(1, state1.currentSessionCompletedTurns.size)
            assertEquals(1000L, state1.currentSessionCompletedTurns[0].durationMs) // 1.0s → 1000ms

            // Turn 2 arrives incrementally.
            val allMsgs = turn1Msgs + listOf(textDeltaEvent("second", ts = 3), makeResult(3.0, ts = 4))
            val state2 = nextGrouped(state1, allMsgs)
            assertEquals(2, state2.currentSessionCompletedTurns.size)
            assertEquals(1000L, state2.currentSessionCompletedTurns[0].durationMs) // unchanged
            assertEquals(3000L, state2.currentSessionCompletedTurns[1].durationMs) // 3.0s → 3000ms
        }

        t.run("reset on shrinking message list clears completed turns") {
            val turn1 = listOf(textDeltaEvent("first"), resultEvent(ts = 1))
            val state1 = nextGrouped(IncrementalGrouped(), turn1)
            assertEquals(1, state1.currentSessionCompletedTurns.size)
            // Reconnect: message list shrinks to empty.
            val state2 = nextGrouped(state1, emptyList())
            assertEquals(0, state2.currentSessionCompletedTurns.size)
            assertEquals(0, state2.completedUpToIdx)
        }

        t.run("currentTurn is null immediately after result - last turn must be shown expanded") {
            // Regression: when the agent completes a turn the UI used to elide the last
            // completed turn because buildLiveItems was only called with the live turn.
            // The fix shows the last completed turn expanded when currentTurn is null.
            val msgs = listOf(
                textDeltaEvent("agent output", ts = 1),
                toolUseEvent("t1", "Read", ts = 2),
                toolResultEvent("t1", ts = 3),
                textDeltaEvent("done", ts = 4),
                resultEvent(ts = 5),
            )
            val state = nextGrouped(IncrementalGrouped(), msgs)
            assertEquals(null, state.currentTurn)
            assertEquals(1, state.currentSessionCompletedTurns.size)
            val turn = state.currentSessionCompletedTurns[0]
            // Turn has both text and tool groups.
            assertTrue(turn.toolCount > 0)
            assertTrue(turn.textCount > 0)
        }

        t.run("currentTurn becomes non-null when user reply arrives after result") {
            // After the agent completes a turn (result event), the user sends a reply
            // (userInput event). A new turn begins: currentTurn must be non-null.
            val turn1 = listOf(textDeltaEvent("agent output", ts = 1), resultEvent(ts = 2))
            val state1 = nextGrouped(IncrementalGrouped(), turn1)
            assertEquals(null, state1.currentTurn)

            val withReply = turn1 + listOf(
                userInputEvent("user reply", ts = 3),
                textDeltaEvent("second agent output", ts = 4),
            )
            val state2 = nextGrouped(state1, withReply)
            // The first turn is still complete; a new live turn has started.
            assertEquals(1, state2.currentSessionCompletedTurns.size)
            assertNotNull(state2.currentTurn)
            val liveTurn = state2.currentTurn!!
            assertTrue(liveTurn.groups.any { g -> g.events.any { it.kind == EventKind.UserInput } })
        }

        t.run("completed turn and live turn coexist after user reply") {
            // When the agent completes turn 1 and the user sends a reply that starts
            // turn 2, both currentSessionCompletedTurns and currentTurn are populated.
            // The UI must use different key prefixes for these to avoid LazyColumn key
            // collisions (regression: crash "Key g:0 was already used").
            val turn1 = listOf(textDeltaEvent("agent output", ts = 1), resultEvent(ts = 2))
            val state1 = nextGrouped(IncrementalGrouped(), turn1)

            val withReply = turn1 + listOf(
                userInputEvent("user reply", ts = 3),
                textDeltaEvent("second agent output", ts = 4),
                toolUseEvent("t1", "Read", ts = 5),
            )
            val state2 = nextGrouped(state1, withReply)
            // Both must be non-empty — this is the precondition for the key collision.
            assertTrue("must have completed turns", state2.currentSessionCompletedTurns.isNotEmpty())
            assertNotNull("must have a live turn", state2.currentTurn)
        }

        t.run("last completed turn has correct content after multi-turn conversation") {
            // Two full turns. After the second result the last completed turn must have
            // the second turn's content (not the first) and currentTurn must be null.
            val allMsgs = listOf(
                textDeltaEvent("turn 1", ts = 1),
                resultEvent(ts = 2),
                userInputEvent("reply", ts = 3),
                textDeltaEvent("turn 2", ts = 4),
                resultEvent(ts = 5),
            )
            val state = nextGrouped(IncrementalGrouped(), allMsgs)
            assertEquals(null, state.currentTurn)
            assertEquals(2, state.currentSessionCompletedTurns.size)
            // The last completed turn contains the user reply and the second agent response.
            val lastTurn = state.currentSessionCompletedTurns.last()
            val allEvents = lastTurn.groups.flatMap { it.events }
            assertTrue(allEvents.any { it.kind == EventKind.UserInput })
            assertTrue(allEvents.any { it.kind == EventKind.TextDelta })
        }

        t.run("currentSessionCompletedTurns has stable reference when no turn completes") {
            // Regression: TaskDetailScreen uses remember(currentSessionCompletedTurns) to cache
            // elidableCompletedTurns. If nextGrouped returns a new list reference on every call,
            // remember invalidates and re-allocates dropLast(1) lists on every SSE batch.
            // The frontend equivalent is createMemo wrapping buildTurnItems.
            val msgs = listOf(
                textDeltaEvent("msg", ts = 1),
            )
            val state1 = nextGrouped(IncrementalGrouped(), msgs)
            assertNotNull(state1.currentTurn) // live turn, no result yet

            // Add a thinking_delta — no turn completion, so completed turns must not change reference.
            val moreMsgs = msgs + listOf(
                EventMessage(kind = EventKind.ThinkingDelta, ts = 2, thinkingDelta = com.caic.sdk.v1.EventThinkingDelta(text = "hmm")),
            )
            val state2 = nextGrouped(state1, moreMsgs)
            assertEquals(0, state2.currentSessionCompletedTurns.size)
            // Reference identity: same empty list reference from IncrementalGrouped default.
            // The important guarantee is that it doesn't create a new object unnecessarily.
            assertTrue(state2.currentSessionCompletedTurns === state1.currentSessionCompletedTurns)
        }

        t.run("currentSessionCompletedTurns reference changes only when a turn completes") {
            val state1 = nextGrouped(
                IncrementalGrouped(),
                listOf(textDeltaEvent("turn 1", ts = 1), resultEvent(ts = 2)),
            )
            assertEquals(1, state1.currentSessionCompletedTurns.size)
            val ref1 = state1.currentSessionCompletedTurns

            // Add events for turn 2 (no result yet) — completed turns reference must NOT change.
            val moreMsgs = listOf(
                textDeltaEvent("turn 1", ts = 1), resultEvent(ts = 2),
                textDeltaEvent("turn 2 start", ts = 3),
            )
            val state2 = nextGrouped(state1, moreMsgs)
            assertEquals(1, state2.currentSessionCompletedTurns.size)
            assertTrue(state2.currentSessionCompletedTurns === ref1)

            // Complete turn 2 — reference must change (new element added).
            val allMsgs = moreMsgs + resultEvent(ts = 4)
            val state3 = nextGrouped(state2, allMsgs)
            assertEquals(2, state3.currentSessionCompletedTurns.size)
            assertTrue(state3.currentSessionCompletedTurns !== ref1)
        }
    }

    // Helper to allow t.run("name") { ... } syntax for subtests within a single @Test method.
    private val t = object {
        fun run(name: String, block: () -> Unit) {
            try {
                block()
            } catch (e: AssertionError) {
                throw AssertionError("Subtest '$name' failed: ${e.message}", e)
            }
        }
    }

    /**
     * Performance benchmark: measures how long [nextGrouped] takes to process message histories
     * of increasing size.  The pi harness emits thousands of thinking_delta events per turn;
     * this test catches regressions in the O(n²) incremental path early.
     *
     * Thresholds (JVM, approximate):
     * - 1k  events: < 10 ms   (typical Claude Code turn)
     * - 10k events: < 200 ms  (large pi turn)
     * - 65k events: < 1500 ms (extreme case from caic-43 trace)
     */
    @Test
    fun benchmarkNextGrouped() {
        val sizes = listOf(1000, 10_000, 65_000)
        for (size in sizes) {
            val msgs = generateThinkingDeltas(size)
            val elapsed = kotlin.system.measureTimeMillis {
                nextGrouped(IncrementalGrouped(), msgs)
            }
            println("nextGrouped(${size} events, single call): $elapsed ms")
            val maxMs = when (size) {
                1000 -> 50L
                10_000 -> 500L
                65_000 -> 3000L
                else -> Long.MAX_VALUE
            }
            assertTrue(
                "nextGrouped(${size}) took ${elapsed}ms, expected < ${maxMs}ms",
                elapsed < maxMs,
            )
        }
    }

    /**
     * Simulates the actual SSE batching pattern: N batches of ~50 events each,
     * calling nextGrouped incrementally. This triggers the O(n²) behaviour in
     * the incremental path where groupMessages re-processes the growing current-turn
     * list on every batch.
     */
    @Test
    fun benchmarkNextGroupedIncremental() {
        val totalEvents = 10_000
        val batchSize = 50
        val baseMsgs = generateThinkingDeltas(0) // just the init + thinking_start
        var state = nextGrouped(IncrementalGrouped(), baseMsgs)
        var cumulativeMsgs = baseMsgs
        var totalTime = 0L
        for (i in 0 until totalEvents / batchSize) {
            val newDeltas = (0 until batchSize).map { j ->
                EventMessage(
                    kind = EventKind.ThinkingDelta,
                    ts = 1777988039509 + (i * batchSize + j).toLong(),
                    thinkingDelta = EventThinkingDelta(text = "word${i * batchSize + j} "),
                )
            }
            cumulativeMsgs = cumulativeMsgs + newDeltas
            val batchTime = kotlin.system.measureTimeMillis {
                state = nextGrouped(state, cumulativeMsgs)
            }
            totalTime += batchTime
        }
        println(
            "nextGrouped incremental ($totalEvents events, $batchSize/batch, " +
                "${totalEvents / batchSize} batches): total=${totalTime}ms " +
                "avg=${totalTime / (totalEvents / batchSize)}ms",
        )
        // The O(n²) behaviour means the last few batches dominate. This must not explode.
        assertTrue(
            "Incremental total ${totalTime}ms is too high for $totalEvents events",
            totalTime < 5000,
        )
    }

    /** Generates [count] synthetic thinking_delta EventMessages mimicking a pi session. */
    private fun generateThinkingDeltas(count: Int): List<EventMessage> {
        val msgs = mutableListOf<EventMessage>()
        // Init event at ~realistic offset from the trace.
        msgs.add(EventMessage(
            kind = EventKind.Init, ts = 1777988039000,
            init = EventInit(
                model = "deepseek-v4-pro", agentVersion = "1.0",
                sessionID = "bench-session", tools = emptyList(), cwd = "/home/user",
                harness = "pi",
            ),
        ))
        // One thinking_start, then many thinking_delta.
        msgs.add(EventMessage(
            kind = EventKind.Thinking, ts = 1777988039500,
            thinking = EventThinking(text = "Let me think about this..."),
        ))
        for (i in 0 until count) {
            msgs.add(EventMessage(
                kind = EventKind.ThinkingDelta,
                ts = 1777988039509 + i.toLong(),
                thinkingDelta = EventThinkingDelta(text = "word$i "),
            ))
        }
        return msgs
    }
}
