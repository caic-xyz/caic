// E2E tests for plan mode and ask question state transitions. Mirrors e2e/tests/plan-and-ask.spec.ts.
package com.fghbuild.caic.e2e

import androidx.test.ext.junit.runners.AndroidJUnit4
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.RestartReq
import dagger.hilt.android.testing.HiltAndroidTest
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Test
import org.junit.runner.RunWith

@HiltAndroidTest
@RunWith(AndroidJUnit4::class)
class PlanAndAskTest : E2eTestBase() {

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
    fun testPlanAndAsk() = runBlocking {
        t.run("FAKE_PLAN reaches has_plan state and restart resolves it") {
            runBlocking {
                val id = createTaskAPI("FAKE_PLAN e2e test")
                val task = waitForTaskState(id, "has_plan", 20_000)
                assertEquals("has_plan", task.state)

                // Restart with a new prompt to clear the plan (mirrors "Clear and execute plan").
                api.restartTask(id, RestartReq(prompt = Prompt(text = "execute now")))
                waitForTaskState(id, "waiting", 20_000)
            }
        }

        t.run("FAKE_ASK reaches asking state and input resolves it") {
            runBlocking {
                val id = createTaskAPI("FAKE_ASK e2e test")
                val task = waitForTaskState(id, "asking", 20_000)
                assertEquals("asking", task.state)

                // Answer the question — the agent processes it and returns to waiting.
                api.sendInput(id, InputReq(prompt = Prompt(text = "In-memory (sync.Map)")))
                waitForTaskState(id, "waiting", 20_000)
            }
        }
    }
}
