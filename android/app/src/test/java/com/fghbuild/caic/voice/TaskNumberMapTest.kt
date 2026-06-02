// Unit tests for the bidirectional task ID to number map.
package com.fghbuild.caic.voice

import com.caic.sdk.v1.RuntimeInstance
import com.caic.sdk.v1.Harness
import com.caic.sdk.v1.Task
import com.caic.sdk.v1.TaskState
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test
import java.time.Instant

class TaskNumberMapTest {

    private fun task(id: String, title: String = ""): Task = Task(
        id = id,
        initialPrompt = "test",
        title = title,
        state = TaskState.Running,
        stateUpdatedAt = Instant.EPOCH,
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
        runtime = RuntimeInstance(name = "test-ctr"),
    )

    @Test
    fun `reset clears all mappings`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a")))
        assertEquals(1, map.toNumber("a"))
        map.reset()
        assertNull(map.toNumber("a"))
        assertNull(map.toId(1))
    }

    @Test
    fun `reset restarts counter from 1`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a")))
        map.reset()
        map.update(listOf(task("b")))
        assertEquals(1, map.toNumber("b"))
    }

    @Test
    fun `first task gets number 1`() {
        val map = TaskNumberMap()
        map.update(listOf(task("task-a")))
        assertEquals(1, map.toNumber("task-a"))
        assertEquals("task-a", map.toId(1))
    }

    @Test
    fun `tasks are numbered by ID order`() {
        // KSID ordering: shorter IDs first, then lexicographic.
        val map = TaskNumberMap()
        val tasks = listOf(
            task("ccc"),
            task("aaa"),
            task("bb"),
        )
        map.update(tasks)
        // Sorted: "bb" (shortest), then "aaa" < "ccc"
        assertEquals(1, map.toNumber("bb"))
        assertEquals(2, map.toNumber("aaa"))
        assertEquals(3, map.toNumber("ccc"))
    }

    @Test
    fun `update preserves existing task numbers`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a"), task("b")))
        assertEquals(1, map.toNumber("a"))
        assertEquals(2, map.toNumber("b"))
        // Update with same tasks — numbers stay.
        map.update(listOf(task("a"), task("b")))
        assertEquals(1, map.toNumber("a"))
        assertEquals(2, map.toNumber("b"))
    }

    @Test
    fun `update assigns new numbers to new tasks`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a")))
        map.update(listOf(task("a"), task("b")))
        assertEquals(1, map.toNumber("a"))
        assertEquals(2, map.toNumber("b"))
    }

    @Test
    fun `update removes stale task mappings`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a"), task("b"), task("c")))
        map.update(listOf(task("a"), task("c"))) // b is gone
        assertEquals(1, map.toNumber("a"))
        assertEquals(3, map.toNumber("c"))
        assertNull(map.toNumber("b"))
        assertNull(map.toId(2)) // b's old number is freed
    }

    @Test
    fun `toId returns null for unknown number`() {
        val map = TaskNumberMap()
        assertNull(map.toId(42))
    }

    @Test
    fun `toNumber returns null for unknown ID`() {
        val map = TaskNumberMap()
        assertNull(map.toNumber("unknown"))
    }

    @Test
    fun `formatTaskRef uses task number and title`() {
        val map = TaskNumberMap()
        map.update(listOf(task("task-1", "Fix login bug")))
        assertEquals("task #1 (Fix login bug)", map.formatTaskRef(
            task("task-1", "Fix login bug")
        ))
    }

    @Test
    fun `formatTaskRef uses ID when title is blank`() {
        val map = TaskNumberMap()
        map.update(listOf(task("task-1", "")))
        assertEquals("task #1 (task-1)", map.formatTaskRef(task("task-1", "")))
    }

    @Test
    fun `formatTaskRef falls back to ID when no mapping exists`() {
        val map = TaskNumberMap()
        assertEquals("task-unknown", map.formatTaskRef(task("task-unknown", "Title")))
    }

    @Test
    fun `update with empty list clears all`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a"), task("b")))
        map.update(emptyList())
        assertNull(map.toNumber("a"))
        assertNull(map.toNumber("b"))
    }

    @Test
    fun `new tasks after stale removal reuse higher numbers`() {
        val map = TaskNumberMap()
        map.update(listOf(task("a"), task("b")))
        map.update(listOf(task("a"))) // b removed, nextNumber=3
        map.update(listOf(task("a"), task("c"))) // c gets 3, not recycled 2
        assertEquals(1, map.toNumber("a"))
        assertEquals(3, map.toNumber("c"))
    }
}
