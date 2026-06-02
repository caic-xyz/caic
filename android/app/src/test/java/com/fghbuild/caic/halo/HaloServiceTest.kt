// Unit tests for HaloService: pure functions (primaryTask, stateLabel, buildStatusString, diffTasks).
package com.fghbuild.caic.halo

import com.caic.sdk.v1.RuntimeInstance
import com.caic.sdk.v1.Harness
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskState
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class HaloServiceTest {

    // ---- primaryTask ----

    @Test
    fun `primaryTask returns null for empty list`() {
        assertNull(HaloService.primaryTask(emptyList()))
    }

    @Test
    fun `primaryTask returns attention task over active`() {
        val waiting = task("t1", "Fix bug", TaskState.Waiting, 100)
        val running = task("t2", "Add feature", TaskState.Running, 200)
        assertEquals("t1", HaloService.primaryTask(listOf(running, waiting))?.id)
    }

    @Test
    fun `primaryTask returns asking over waiting`() {
        val asking = task("t1", "Q", TaskState.Asking, 100)
        val waiting = task("t2", "W", TaskState.Waiting, 200)
        // Both are attention states; the first in list order wins (asking is first).
        assertEquals("t1", HaloService.primaryTask(listOf(asking, waiting))?.id)
    }

    @Test
    fun `primaryTask returns hasPlan over waiting`() {
        val plan = task("t1", "Plan", TaskState.HasPlan, 100)
        val waiting = task("t2", "W", TaskState.Waiting, 200)
        assertEquals("t1", HaloService.primaryTask(listOf(plan, waiting))?.id)
    }

    @Test
    fun `primaryTask falls back to most recently updated when no attention tasks`() {
        val older = task("t1", "Old", TaskState.Running, 100)
        val newer = task("t2", "New", TaskState.Running, 200)
        assertEquals("t2", HaloService.primaryTask(listOf(older, newer))?.id)
    }

    // ---- stateLabel ----

    @Test
    fun `stateLabel returns compact labels`() {
        assertEquals("Running", HaloService.stateLabel(TaskState.Running))
        assertEquals("Waiting", HaloService.stateLabel(TaskState.Waiting))
        assertEquals("Asking", HaloService.stateLabel(TaskState.Asking))
        assertEquals("Plan", HaloService.stateLabel(TaskState.HasPlan))
        assertEquals("Done", HaloService.stateLabel(TaskState.Purged))
        assertEquals("Failed", HaloService.stateLabel(TaskState.Failed))
        assertEquals("Stopping", HaloService.stateLabel(TaskState.Stopping))
        assertEquals("Stopped", HaloService.stateLabel(TaskState.Stopped))
    }

    @Test
    fun `stateLabel truncates Other values`() {
        val other = TaskState.Other("very_long_custom_state")
        assertEquals("very_l", HaloService.stateLabel(other))
    }

    // ---- buildStatusString ----

    @Test
    fun `buildStatusString formats single task`() {
        val t = task("t1", "Fix authentication bug", TaskState.Running, 100)
        val result = HaloService.buildStatusString(listOf(t), t)
        assertEquals("1 task  •  Fix authentication b  •  Running", result)
    }

    @Test
    fun `buildStatusString formats multiple tasks`() {
        val t1 = task("t1", "Fix auth", TaskState.Running, 100)
        val t2 = task("t2", "Add tests", TaskState.Waiting, 200)
        val result = HaloService.buildStatusString(listOf(t1, t2), t2)
        assertEquals("2 tasks  •  Add tests  •  Waiting", result)
    }

    @Test
    fun `buildStatusString handles null primary`() {
        val result = HaloService.buildStatusString(emptyList(), null)
        assertEquals("0 tasks  •  none  •  ", result)
    }

    @Test
    fun `buildStatusString truncates long titles to 20 chars`() {
        // .take(20) may land on a space, so trim() yields ≤20.
        val t = task("t1", "abcdefghijklmnopqrstuvwxyz", TaskState.Running, 100)
        val result = HaloService.buildStatusString(listOf(t), t)
        val titlePart = result.split("\u2022")[1].trim()
        assertEquals(20, titlePart.length)
    }

    // ---- diffTasks ----

    @Test
    fun `diffTasks detects state changes`() {
        val prev = listOf(
            task("t1", "A", TaskState.Running, 100),
            task("t2", "B", TaskState.Waiting, 100),
        )
        val curr = listOf(
            task("t1", "A", TaskState.Running, 100),  // unchanged
            task("t2", "B", TaskState.Purged, 200),    // changed
        )
        val changes = HaloService.diffTasks(prev, curr)
        assertEquals(1, changes.size)
        assertEquals("t2", changes[0].id)
    }

    @Test
    fun `diffTasks detects new tasks`() {
        val prev = listOf(task("t1", "A", TaskState.Running, 100))
        val curr = listOf(
            task("t1", "A", TaskState.Running, 100),
            task("t2", "B", TaskState.Waiting, 100),
        )
        val changes = HaloService.diffTasks(prev, curr)
        assertEquals(1, changes.size)
        assertEquals("t2", changes[0].id)
    }

    @Test
    fun `diffTasks returns empty when nothing changed`() {
        val prev = listOf(task("t1", "A", TaskState.Running, 100))
        val curr = listOf(task("t1", "A", TaskState.Running, 200))
        assertEquals(0, HaloService.diffTasks(prev, curr).size)
    }

    @Test
    fun `diffTasks detects removed tasks as no change`() {
        val prev = listOf(
            task("t1", "A", TaskState.Running, 100),
            task("t2", "B", TaskState.Waiting, 100),
        )
        val curr = listOf(task("t1", "A", TaskState.Running, 100))
        assertEquals(0, HaloService.diffTasks(prev, curr).size)
    }

    // ---- Helpers ----

    private fun task(id: String, title: String, state: TaskState, epochSeconds: Long) = Task(
        id = id,
        initialPrompt = "prompt",
        title = title,
        state = state,
        stateUpdatedAt = java.time.Instant.ofEpochSecond(epochSeconds),
        costUSD = 0.0,
        duration = 0.0,
        numTurns = 0,
        cumulativeInputTokens = 0,
        cumulativeOutputTokens = 0,
        cumulativeCacheCreationInputTokens = 0,
        cumulativeCacheReadInputTokens = 0,
        activeInputTokens = 0,
        activeCacheReadTokens = 0,
        contextWindowLimit = 0,
        harness = Harness.Claude,
        runtime = RuntimeInstance(name = "test"),
    )
}
