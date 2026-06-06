// Multi-turn interaction and concurrent task tests. Mirrors e2e/tests/multi-turn.spec.ts.
package com.fghbuild.caic.e2e

import androidx.test.ext.junit.runners.AndroidJUnit4
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import dagger.hilt.android.testing.HiltAndroidTest
import kotlinx.coroutines.async
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@HiltAndroidTest
@RunWith(AndroidJUnit4::class)
class MultiTurnTest : E2eTestBase() {

    private val t = object {
        fun run(name: String, block: () -> Unit) {
            try {
                block()
            } catch (e: AssertionError) {
                throw AssertionError("Subtest '$name' failed: ${e.message}", e)
            }
        }
    }

    @Test
    fun testMultiTurn() = runBlocking {
        t.run("send input cycles to next turn") {
            runBlocking {
                val id = createTaskAPI("multi-turn test")
                waitForTaskState(id, "waiting")

                api.sendInput(id, InputReq(prompt = Prompt(text = "tell me another")))
                val task = waitForTaskState(id, "waiting", 20_000)
                assertTrue("numTurns should be >= 2, got ${task.numTurns}", task.numTurns >= 2)
            }
        }

        t.run("concurrent tasks run independently") {
            runBlocking {
                val id1 = createTaskAPI("concurrent task A")
                val id2 = createTaskAPI("concurrent task B")

                val task1 = async { waitForTaskState(id1, "waiting") }
                val task2 = async { waitForTaskState(id2, "waiting") }

                val t1 = task1.await()
                val t2 = task2.await()

                assertNotEquals("tasks should have different IDs", t1.id, t2.id)
                assertNotEquals(
                    "tasks should have different branches",
                    t1.repos!![0].branch,
                    t2.repos!![0].branch,
                )

                // Purge both.
                api.purgeTask(id1)
                api.purgeTask(id2)
                val purge1 = async { waitForTaskState(id1, "purged") }
                val purge2 = async { waitForTaskState(id2, "purged") }
                purge1.await()
                purge2.await()
            }
        }
    }
}
