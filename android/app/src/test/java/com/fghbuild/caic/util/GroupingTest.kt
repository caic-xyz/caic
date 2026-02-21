// Unit tests for message grouping and turn splitting logic.
package com.fghbuild.caic.util

import com.caic.sdk.v1.ClaudeEventMessage
import com.caic.sdk.v1.ClaudeEventText
import com.caic.sdk.v1.ClaudeEventTextDelta
import com.caic.sdk.v1.ClaudeEventToolResult
import com.caic.sdk.v1.ClaudeEventToolUse
import com.caic.sdk.v1.ClaudeEventAsk
import com.caic.sdk.v1.ClaudeAskQuestion
import com.caic.sdk.v1.ClaudeEventResult
import com.caic.sdk.v1.ClaudeEventUsage
import com.caic.sdk.v1.ClaudeEventUserInput
import com.caic.sdk.v1.EventKinds
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class GroupingTest {
    private fun textDeltaEvent(text: String, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.TextDelta, ts = ts,
        textDelta = ClaudeEventTextDelta(text = text),
    )

    private fun textEvent(text: String, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.Text, ts = ts,
        text = ClaudeEventText(text = text),
    )

    private fun toolUseEvent(id: String, name: String, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.ToolUse, ts = ts,
        toolUse = ClaudeEventToolUse(toolUseID = id, name = name, input = JsonObject(emptyMap())),
    )

    private fun toolResultEvent(id: String, duration: Double = 0.1, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.ToolResult, ts = ts,
        toolResult = ClaudeEventToolResult(toolUseID = id, duration = duration),
    )

    @Suppress("LongMethod")
    private fun resultEvent(ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.Result, ts = ts,
        result = ClaudeEventResult(
            subtype = "success", isError = false, result = "done",
            totalCostUSD = 0.01, duration = 1.0, durationAPI = 0.9,
            numTurns = 1, usage = ClaudeEventUsage(
                inputTokens = 100, outputTokens = 50,
                cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
            ),
        ),
    )

    private fun askEvent(id: String, question: String, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.Ask, ts = ts,
        ask = ClaudeEventAsk(
            toolUseID = id,
            questions = listOf(ClaudeAskQuestion(question = question, options = emptyList())),
        ),
    )

    private fun userInputEvent(text: String, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.UserInput, ts = ts,
        userInput = ClaudeEventUserInput(text = text),
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
            assertEquals(GroupKind.TOOL, groups[0].kind)
            assertEquals(2, groups[0].toolCalls.size)
        }

        t.run("toolResult matches backwards across groups") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                textDeltaEvent("text"),
                toolResultEvent("t1"),
            ))
            assertEquals(2, groups.size)
            assertEquals(GroupKind.TOOL, groups[0].kind)
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
            assertEquals(GroupKind.TOOL, groups[0].kind)
            assertEquals(2, groups[0].toolCalls.size)
        }

        t.run("usage event separates tool groups across assistant messages") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                ClaudeEventMessage(
                    kind = EventKinds.Usage, ts = 0,
                    usage = ClaudeEventUsage(
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

        t.run("synchronous tools in last group are marked done immediately") {
            // Bash is async (emits toolResult); Read is synchronous (no toolResult).
            // Even before Bash's result arrives, Read should show as done.
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Bash"),
                toolUseEvent("t2", "Read"),
            ))
            assertEquals(1, groups.size)
            assertTrue(!groups[0].toolCalls[0].done) // Bash: async, pending
            assertTrue(groups[0].toolCalls[1].done)  // Read: sync, already done
        }

        t.run("non-last tool groups are implicitly marked done") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                ClaudeEventMessage(
                    kind = EventKinds.Usage, ts = 0,
                    usage = ClaudeEventUsage(
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

        t.run("turnSummary formats correctly") {
            val turn = Turn(groups = emptyList(), toolCount = 3, textCount = 2)
            assertEquals("2 messages, 3 tool calls", turnSummary(turn))
        }

        t.run("turnSummary singular forms") {
            val turn = Turn(groups = emptyList(), toolCount = 1, textCount = 1)
            assertEquals("1 message, 1 tool call", turnSummary(turn))
        }
    }

    @Test
    fun testMergePass() {
        t.run("tool groups separated by text merge into one") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                ClaudeEventMessage(
                    kind = EventKinds.Usage, ts = 0,
                    usage = ClaudeEventUsage(
                        inputTokens = 100, outputTokens = 50,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                textDeltaEvent("commentary"),
                toolUseEvent("t2", "Bash"),
                ClaudeEventMessage(
                    kind = EventKinds.Usage, ts = 0,
                    usage = ClaudeEventUsage(
                        inputTokens = 200, outputTokens = 100,
                        cacheCreationInputTokens = 0, cacheReadInputTokens = 0, model = "test",
                    ),
                ),
                textDeltaEvent("more commentary"),
                toolUseEvent("t3", "Edit"),
            ))
            // Three tool groups separated by text → merge pass consolidates into one.
            assertEquals(3, groups.size) // [TOOL(t1+t2+t3), TEXT, TEXT]
            assertEquals(GroupKind.TOOL, groups[0].kind)
            assertEquals(3, groups[0].toolCalls.size)
        }

        t.run("ask group prevents tool group merging across turns") {
            // In practice, an ask is always followed by a usage event from the
            // next assistant turn. The ask + usage together form a hard boundary.
            val usage = ClaudeEventMessage(
                kind = EventKinds.Usage, ts = 0,
                usage = ClaudeEventUsage(
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
            assertEquals(GroupKind.TOOL, groups[0].kind)
            assertEquals(GroupKind.ASK, groups[1].kind)
            assertEquals(GroupKind.TOOL, groups[2].kind)
        }

        t.run("todo events are skipped and don't split tool groups") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                ClaudeEventMessage(kind = EventKinds.Todo, ts = 0),
                toolUseEvent("t2", "Bash"),
            ))
            assertEquals(1, groups.size)
            assertEquals(2, groups[0].toolCalls.size)
        }
    }

    @Test
    fun testPlanHelpers() {
        t.run("turnHasExitPlanMode detects ExitPlanMode") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "ExitPlanMode"),
                resultEvent(),
            ))
            val turns = groupTurns(groups)
            assertTrue(turnHasExitPlanMode(turns[0]))
        }

        t.run("turnHasExitPlanMode false when absent") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                resultEvent(),
            ))
            val turns = groupTurns(groups)
            assertTrue(!turnHasExitPlanMode(turns[0]))
        }

        t.run("turnPlanContent extracts plan from Write tool call") {
            val writeInput = JsonObject(mapOf(
                "file_path" to JsonPrimitive("/home/user/.claude/plans/my-plan.md"),
                "content" to JsonPrimitive("# My Plan\n\nDo things."),
            ))
            val groups = groupMessages(listOf(
                ClaudeEventMessage(
                    kind = EventKinds.ToolUse, ts = 0,
                    toolUse = ClaudeEventToolUse(toolUseID = "w1", name = "Write", input = writeInput),
                ),
                toolUseEvent("e1", "ExitPlanMode"),
                resultEvent(),
            ))
            val turns = groupTurns(groups)
            assertEquals("# My Plan\n\nDo things.", turnPlanContent(turns[0]))
        }

        t.run("turnPlanContent returns null when no plan file") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                resultEvent(),
            ))
            val turns = groupTurns(groups)
            assertNull(turnPlanContent(turns[0]))
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
}
