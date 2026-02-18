// Unit tests for message grouping and turn splitting logic.
package com.fghbuild.caic.util

import com.caic.sdk.ClaudeEventMessage
import com.caic.sdk.ClaudeEventText
import com.caic.sdk.ClaudeEventTextDelta
import com.caic.sdk.ClaudeEventToolResult
import com.caic.sdk.ClaudeEventToolUse
import com.caic.sdk.ClaudeEventAsk
import com.caic.sdk.ClaudeAskQuestion
import com.caic.sdk.ClaudeEventResult
import com.caic.sdk.ClaudeEventUsage
import com.caic.sdk.ClaudeEventUserInput
import com.caic.sdk.EventKinds
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

    private fun toolResultEvent(id: String, durationMs: Long = 100, ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.ToolResult, ts = ts,
        toolResult = ClaudeEventToolResult(toolUseID = id, durationMs = durationMs),
    )

    @Suppress("LongMethod")
    private fun resultEvent(ts: Long = 0) = ClaudeEventMessage(
        kind = EventKinds.Result, ts = ts,
        result = ClaudeEventResult(
            subtype = "success", isError = false, result = "done",
            totalCostUSD = 0.01, durationMs = 1000, durationAPIMs = 900,
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

        t.run("non-last tool groups are implicitly marked done") {
            val groups = groupMessages(listOf(
                toolUseEvent("t1", "Read"),
                textDeltaEvent("text"),
                toolUseEvent("t2", "Bash"),
            ))
            assertEquals(3, groups.size)
            assertTrue(groups[0].toolCalls[0].done)
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
