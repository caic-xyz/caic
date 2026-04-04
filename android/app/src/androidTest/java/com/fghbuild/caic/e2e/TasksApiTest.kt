// API-only e2e tests for the task lifecycle (no UI). Mirrors e2e/tests/tasks-api.spec.ts.
package com.fghbuild.caic.e2e

import androidx.test.ext.junit.runners.AndroidJUnit4
import com.caic.sdk.v1.ApiException
import com.caic.sdk.v1.InputReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.RestartReq
import dagger.hilt.android.testing.HiltAndroidTest
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test
import org.junit.runner.RunWith

@HiltAndroidTest
@RunWith(AndroidJUnit4::class)
class TasksApiTest : E2eTestBase() {

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
    fun testTasksApi() = runBlocking {
        t.run("create task and reach waiting state") {
            runBlocking {
                val id = createTaskAPI("api lifecycle test")
                val task = waitForTaskState(id, "waiting")
                assertEquals("fake", task.harness)
                assertTrue("numTurns should be >= 1", task.numTurns >= 1)
            }
        }

        t.run("purge a waiting task") {
            runBlocking {
                val id = createTaskAPI("api purge test")
                waitForTaskState(id, "waiting")
                api.purgeTask(id)
                waitForTaskState(id, "purged")
            }
        }

        t.run("send input triggers another turn") {
            runBlocking {
                val id = createTaskAPI("api input test")
                waitForTaskState(id, "waiting")
                api.sendInput(id, InputReq(prompt = Prompt(text = "continue")))
                val task = waitForTaskState(id, "waiting", 20_000)
                assertTrue("numTurns should be >= 2, got ${task.numTurns}", task.numTurns >= 2)
            }
        }

        t.run("restart a waiting task starts a new session") {
            runBlocking {
                val id = createTaskAPI("api restart test")
                waitForTaskState(id, "waiting")
                api.restartTask(id, RestartReq(prompt = Prompt(text = "try again")))
                val task = waitForTaskState(id, "waiting", 20_000)
                assertTrue("numTurns should be >= 1", task.numTurns >= 1)
            }
        }

        t.run("fake backend sets PR and CI status that cycles to success") {
            runBlocking {
                val id = createTaskAPI("api ci dot test")
                waitForTaskState(id, "waiting")

                // Wait for fake CI to set PR and checks.
                waitForCondition(5_000) {
                    val tasks = api.listTasks()
                    val task = tasks.firstOrNull { it.id == id }
                    task?.forgePR == 1 && task.ciChecks?.size == 3
                }

                // CI transitions to success within ~10s.
                waitForCondition(10_000) {
                    val tasks = api.listTasks()
                    val task = tasks.firstOrNull { it.id == id }
                    task?.ciStatus == "success"
                }
            }
        }

        t.run("list tasks includes created task") {
            runBlocking {
                val id = createTaskAPI("api list test")
                waitForTaskState(id, "waiting")
                val tasks = api.listTasks()
                val found = tasks.firstOrNull { it.id == id }
                assertNotNull("task should be in list", found)
                assertEquals("api list test", found!!.initialPrompt)
                assertTrue("repos should not be empty", found.repos?.isNotEmpty() == true)
                assertTrue("branch should not be blank", found.repos!![0].branch.isNotBlank())
            }
        }
    }

    @Test
    fun testErrorHandling() = runBlocking {
        t.run("missing prompt returns 400") {
            runBlocking {
                try {
                    // Send a request missing the required initialPrompt field.
                    api.createTask(
                        com.caic.sdk.v1.CreateTaskReq(
                            initialPrompt = Prompt(text = ""),
                            harness = "fake",
                        ),
                    )
                    fail("Expected ApiException")
                } catch (e: ApiException) {
                    assertEquals(400, e.statusCode)
                }
            }
        }

        t.run("unknown repo returns 400") {
            runBlocking {
                try {
                    api.createTask(
                        com.caic.sdk.v1.CreateTaskReq(
                            initialPrompt = Prompt(text = "hello"),
                            repos = listOf(com.caic.sdk.v1.RepoSpec(name = "nonexistent")),
                            harness = "fake",
                        ),
                    )
                    fail("Expected ApiException")
                } catch (e: ApiException) {
                    assertEquals(400, e.statusCode)
                }
            }
        }

        t.run("unknown harness returns 400") {
            runBlocking {
                try {
                    api.createTask(
                        com.caic.sdk.v1.CreateTaskReq(
                            initialPrompt = Prompt(text = "hello"),
                            harness = "does-not-exist",
                        ),
                    )
                    fail("Expected ApiException")
                } catch (e: ApiException) {
                    assertEquals(400, e.statusCode)
                }
            }
        }

        t.run("purge nonexistent task returns 404") {
            runBlocking {
                try {
                    api.purgeTask("nonexistent-id")
                    fail("Expected ApiException")
                } catch (e: ApiException) {
                    assertEquals(404, e.statusCode)
                }
            }
        }

        t.run("send input to nonexistent task returns 404") {
            runBlocking {
                try {
                    api.sendInput("nonexistent-id", InputReq(prompt = Prompt(text = "hello")))
                    fail("Expected ApiException")
                } catch (e: ApiException) {
                    assertEquals(404, e.statusCode)
                }
            }
        }

        t.run("special characters in prompt are preserved") {
            runBlocking {
                val prompt = """<script>alert("xss")</script> & "quotes" & émojis"""
                val id = createTaskAPI(prompt)
                waitForTaskState(id, "waiting")
                val tasks = api.listTasks()
                val found = tasks.firstOrNull { it.id == id }
                assertNotNull(found)
                assertEquals(prompt, found!!.initialPrompt)
            }
        }
    }
}
