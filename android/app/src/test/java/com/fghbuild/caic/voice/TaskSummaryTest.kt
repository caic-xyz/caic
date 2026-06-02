// Unit tests for task summary formatting helpers used by FunctionHandlers.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.CIStatus
import com.caic.sdk.v1.RuntimeInstance
import com.caic.sdk.v1.DiffFileStat
import com.caic.sdk.v1.ForgePRState
import com.caic.sdk.v1.Harness
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskState
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.Instant

class TaskSummaryTest {

    private fun task(
        id: String = "task-1",
        title: String = "Fix the bug",
        state: TaskState = TaskState.Running,
        harness: Harness = Harness.Claude,
        duration: Double = 120.0,
        costUSD: Double = 0.05,
        diffStat: List<DiffFileStat>? = null,
        result: String? = null,
        error: String? = null,
        forgePR: Int? = null,
        forgePRState: ForgePRState? = null,
        ciStatus: CIStatus? = null,
    ): Task = Task(
        id = id,
        initialPrompt = "test",
        title = title,
        state = state,
        stateUpdatedAt = Instant.EPOCH,
        costUSD = costUSD,
        duration = duration,
        numTurns = 1,
        cumulativeInputTokens = 100,
        cumulativeOutputTokens = 50,
        cumulativeCacheCreationInputTokens = 0,
        cumulativeCacheReadInputTokens = 0,
        activeInputTokens = 0,
        activeCacheReadTokens = 0,
        contextWindowLimit = 200_000,
        harness = harness,
        runtime = RuntimeInstance(id = "ctr"),
        diffStat = diffStat,
        result = result,
        error = error,
        forgePR = forgePR,
        forgePRState = forgePRState,
        ciStatus = ciStatus,
    )

    // ---- diffStatSummary ----

    @Test
    fun `diffStatSummary returns empty for null diffstat`() {
        assertEquals("", diffStatSummary(task()))
    }

    @Test
    fun `diffStatSummary returns empty for empty diffstat`() {
        assertEquals("", diffStatSummary(task(diffStat = emptyList())))
    }

    @Test
    fun `diffStatSummary formats single file`() {
        val ds = listOf(DiffFileStat("main.kt", added = 5, deleted = 2))
        assertEquals(", +5 -2 in 1 file", diffStatSummary(task(diffStat = ds)))
    }

    @Test
    fun `diffStatSummary formats multiple files`() {
        val ds = listOf(
            DiffFileStat("a.kt", added = 3, deleted = 1),
            DiffFileStat("b.kt", added = 7, deleted = 0),
        )
        assertEquals(", +10 -1 in 2 files", diffStatSummary(task(diffStat = ds)))
    }

    @Test
    fun `diffStatSummary sums across files`() {
        val ds = listOf(
            DiffFileStat("a.kt", added = 0, deleted = 0),
            DiffFileStat("b.kt", added = 100, deleted = 50),
        )
        assertEquals(", +100 -50 in 2 files", diffStatSummary(task(diffStat = ds)))
    }

    // ---- taskSummaryLine ----

    @Test
    fun `taskSummaryLine formats running task`() {
        val line = taskSummaryLine(1, task(state = TaskState.Running))
        assertTrue(line.startsWith("1. **Fix the bug**"))
        assertTrue(line.contains("running"))
    }

    @Test
    fun `taskSummaryLine uses title when available`() {
        val line = taskSummaryLine(3, task(title = "Add tests"))
        assertTrue(line.startsWith("3. **Add tests**"))
    }

    @Test
    fun `taskSummaryLine falls back to ID when title is blank`() {
        val line = taskSummaryLine(2, task(title = "", id = "abc123"))
        assertTrue(line.startsWith("2. **abc123**"))
    }

    @Test
    fun `taskSummaryLine shows state value`() {
        val line = taskSummaryLine(1, task(state = TaskState.Waiting))
        assertTrue(line.contains("waiting"))
    }

    @Test
    fun `taskSummaryLine shows harness`() {
        val line = taskSummaryLine(1, task(harness = Harness.Codex))
        assertTrue(line.contains("codex"))
    }

    @Test
    fun `taskSummaryLine includes PR number when open`() {
        val line = taskSummaryLine(1, task(forgePR = 42, forgePRState = ForgePRState.Open))
        assertTrue(line.contains("PR #42"))
    }

    @Test
    fun `taskSummaryLine excludes PR when closed`() {
        val line = taskSummaryLine(1, task(forgePR = 42, forgePRState = ForgePRState.Closed))
        assertTrue(!line.contains("PR #"))
    }

    @Test
    fun `taskSummaryLine excludes PR when merged`() {
        val line = taskSummaryLine(1, task(forgePR = 42, forgePRState = ForgePRState.Merged))
        assertTrue(!line.contains("PR #"))
    }

    @Test
    fun `taskSummaryLine includes CI status when present`() {
        val line = taskSummaryLine(1, task(ciStatus = CIStatus.Failure))
        assertTrue(line.contains("CI: failure"))
    }

    @Test
    fun `taskSummaryLine shows result for purged tasks`() {
        val line = taskSummaryLine(1, task(
            state = TaskState.Purged,
            result = "Fixed the login redirect",
        ))
        assertTrue(line.contains("Fixed the login redirect"))
    }

    @Test
    fun `taskSummaryLine truncates long results`() {
        val longResult = "x".repeat(200)
        val line = taskSummaryLine(1, task(state = TaskState.Purged, result = longResult))
        val snippet = line.substringAfterLast("— ")
        assertTrue(snippet.length <= 120)
    }

    @Test
    fun `taskSummaryLine shows container died for stopped`() {
        val line = taskSummaryLine(1, task(state = TaskState.Stopped))
        assertTrue(line.contains("container died"))
    }

    @Test
    fun `taskSummaryLine shows error for failed`() {
        val line = taskSummaryLine(1, task(
            state = TaskState.Failed,
            error = "Out of memory",
        ))
        assertTrue(line.contains("Out of memory"))
    }

    @Test
    fun `taskSummaryLine no extra suffix for running state`() {
        val line = taskSummaryLine(1, task(state = TaskState.Running))
        assertTrue(!line.contains("— container died"))
        assertTrue(!line.contains("— Out of"))
    }

    @Test
    fun `taskSummaryLine includes diffStat when present`() {
        val ds = listOf(DiffFileStat("lib.rs", added = 10, deleted = 3))
        val line = taskSummaryLine(1, task(diffStat = ds))
        assertTrue(line.contains("+10 -3 in 1 file"))
    }

    @Test
    fun `taskSummaryLine formats elapsed time`() {
        val line = taskSummaryLine(1, task(duration = 65.0))
        assertTrue(line.contains("1m 5s"))
    }

    @Test
    fun `taskSummaryLine formats cost`() {
        val line = taskSummaryLine(1, task(costUSD = 0.42))
        assertTrue(line.contains("$0.42"))
    }

    @Test
    fun `taskSummaryLine combines PR and CI`() {
        val line = taskSummaryLine(1, task(
            forgePR = 10, forgePRState = ForgePRState.Open,
            ciStatus = CIStatus.Pending,
        ))
        assertTrue(line.contains("PR #10"))
        assertTrue(line.contains("CI: pending"))
    }

    @Test
    fun `taskSummaryLine handles PR=0 as no PR`() {
        // PR 0 should not be shown (only positive PR numbers are displayed).
        val line = taskSummaryLine(1, task(forgePR = 0, forgePRState = ForgePRState.Open))
        assertTrue(!line.contains("PR #"))
    }
}
