// Base class for Android e2e tests against the fake backend.
//
// Mirrors e2e/helpers.ts: provides ApiClient, createTaskAPI(), waitForTaskState(),
// and configures the app's SettingsRepository to point at the fake backend.
//
// Usage:
//   @HiltAndroidTest
//   @RunWith(AndroidJUnit4::class)
//   class MyTest : E2eTestBase() { ... }
package com.fghbuild.caic.e2e

import androidx.test.platform.app.InstrumentationRegistry
import com.caic.sdk.v1.ApiClient
import com.caic.sdk.v1.CreateTaskReq
import com.caic.sdk.v1.Prompt
import com.caic.sdk.v1.RepoSpec
import com.caic.sdk.v1.Task
import com.fghbuild.caic.data.SettingsRepository
import dagger.hilt.android.testing.HiltAndroidRule
import kotlinx.coroutines.delay
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Before
import org.junit.Rule
import javax.inject.Inject

@Suppress("UnnecessaryAbstractClass")
abstract class E2eTestBase {
    @get:Rule(order = 0)
    val hiltRule = HiltAndroidRule(this)

    @Inject
    lateinit var settingsRepository: SettingsRepository

    /** Direct API client pointed at the fake backend (no DI). */
    lateinit var api: ApiClient
        private set

    /** Base URL of the fake backend. */
    val baseUrl: String by lazy {
        InstrumentationRegistry.getArguments().getString("baseUrl", DEFAULT_BASE_URL)
    }

    @Before
    fun setUpE2e() {
        hiltRule.inject()
        api = ApiClient(baseUrl)
        runBlocking {
            // Add a server entry pointing at the fake backend so the app connects.
            val id = settingsRepository.addServer("E2E")
            settingsRepository.switchServer(id)
            settingsRepository.updateServerURL(baseUrl)
        }
    }

    /** Create a task via API using the first available repo and harness. Returns the task ID. */
    suspend fun createTaskAPI(prompt: String): String {
        val repos = api.listRepos()
        require(repos.isNotEmpty()) { "No repos available from fake backend" }
        val harnesses = api.listHarnesses()
        require(harnesses.isNotEmpty()) { "No harnesses available from fake backend" }
        val resp = api.createTask(
            CreateTaskReq(
                initialPrompt = Prompt(text = prompt),
                repos = listOf(RepoSpec(name = repos[0].path)),
                harness = harnesses[0].name,
            ),
        )
        require(resp.id.isNotBlank()) { "createTask returned empty ID" }
        return resp.id
    }

    /** Poll until a task reaches the expected state. Throws on timeout. */
    suspend fun waitForTaskState(
        taskId: String,
        state: String,
        timeoutMs: Long = DEFAULT_TIMEOUT_MS,
    ): Task {
        return withTimeout(timeoutMs) {
            while (true) {
                val tasks = api.listTasks()
                val task = tasks.firstOrNull { it.id == taskId }
                if (task != null && task.state == state) return@withTimeout task
                delay(POLL_INTERVAL_MS)
            }
            @Suppress("UNREACHABLE_CODE")
            error("unreachable")
        }
    }

    /** Poll until a condition is true. Throws on timeout. */
    suspend fun waitForCondition(timeoutMs: Long = DEFAULT_TIMEOUT_MS, condition: suspend () -> Boolean) {
        withTimeout(timeoutMs) {
            while (!condition()) {
                delay(POLL_INTERVAL_MS)
            }
        }
    }

    companion object {
        const val DEFAULT_BASE_URL = "http://localhost:8090"
        const val DEFAULT_TIMEOUT_MS = 15_000L
        private const val POLL_INTERVAL_MS = 500L
    }
}
