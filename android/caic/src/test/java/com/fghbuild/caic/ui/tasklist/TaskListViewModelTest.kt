// Integration tests for the task list ViewModel: repo/harness loading and selector state.
package com.fghbuild.caic.ui.tasklist

import android.app.Application
import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import androidx.test.core.app.ApplicationProvider
import com.fghbuild.caic.data.SettingsRepository
import com.fghbuild.caic.data.TaskNotifier
import com.fghbuild.caic.data.TaskRepository
import com.fghbuild.caic.voice.VoiceSession
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.cancelAndJoin
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.job
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlinx.coroutines.withTimeout
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import java.io.File

@OptIn(ExperimentalCoroutinesApi::class)
@RunWith(RobolectricTestRunner::class)
class TaskListViewModelTest {

    private val t = object {
        fun run(name: String, block: () -> Unit) {
            try {
                block()
            } catch (e: AssertionError) {
                throw AssertionError("Subtest '$name' failed: ${e.message}", e)
            }
        }
    }

    private val testDispatcher = UnconfinedTestDispatcher()
    private lateinit var server: MockWebServer
    private lateinit var viewModel: TaskListViewModel

    @Before
    fun setUp() {
        Dispatchers.setMain(testDispatcher)
    }

    @After
    fun tearDown() {
        // Cancel the ViewModel's long-lived coroutines (SSE reconnect loops, flow
        // collectors) and wait for them to finish before resetting Main. Otherwise
        // their continuations dispatch through Dispatchers.Main concurrently with
        // resetMain(), causing a flaky IllegalStateException ("Main is used
        // concurrently with setting it").
        if (::viewModel.isInitialized) {
            runBlocking { viewModel.viewModelScope.coroutineContext.job.cancelAndJoin() }
        }
        Dispatchers.resetMain()
        if (::server.isInitialized) server.shutdown()
    }

    @Test
    fun testLoadFormDataPopulatesReposAndHarnesses() = runBlocking {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val path = request.requestUrl?.encodedPath ?: return MockResponse()
                    .setResponseCode(404)
                return when (path) {
                    "/api/caic/v1/server/config" -> jsonResponse(
                        """{"displayName":"test-host","tailscaleAvailable":false,""" +
                            """"usbAvailable":false,"displayAvailable":false,""" +
                            """"sudoAvailable":true,"gitHubTokenAvailable":false,""" +
                            """"voiceGateway":{"mode":"disabled"}}"""
                    )
                    "/api/caic/v1/server/repos" -> jsonResponse(
                        """[
                            {"path":"acme/web-app","branch":"main","baseBranch":{"name":"main"}},
                            {"path":"acme/api-server","branch":"develop","baseBranch":{"name":"develop"}}
                        ]"""
                    )
                    "/api/caic/v1/server/harnesses" -> jsonResponse(
                        """[
                            {"name":"claude-code","models":["sonnet","opus"],"supportsImages":true,"supportsCompact":true},
                            {"name":"codex","models":["o3"],"supportsImages":false,"supportsCompact":false}
                        ]"""
                    )
                    "/api/caic/v1/server/preferences" -> jsonResponse(
                        """{"repositories":[],"settings":{"sudo":false,"autoFixOnCIFailure":false,""" +
                            """"autoFixOnPROpen":false}}"""
                    )
                    else -> MockResponse().setResponseCode(404)
                }
            }
        }
        server.start()

        val baseURL = server.url("/").toString().trimEnd('/')
        val dataStore = PreferenceDataStoreFactory.create {
            File.createTempFile("test_prefs", ".preferences_pb")
        }
        val settingsRepository = SettingsRepository(dataStore)
        settingsRepository.addServer("test")
        settingsRepository.updateServerURL(baseURL)

        // Wait for settings to propagate from DataStore.
        withTimeout(5000) {
            settingsRepository.settings.first { it.serverURL.isNotBlank() }
        }

        val taskRepository = TaskRepository(settingsRepository)
        val context = ApplicationProvider.getApplicationContext<Application>()
        val voiceSession = VoiceSession(context, settingsRepository)
        val taskNotifier = TaskNotifier(context, taskRepository, voiceSession)
        viewModel = TaskListViewModel(taskRepository, settingsRepository, taskNotifier)

        // Wait for loadFormData to complete (real HTTP to MockWebServer).
        val state = withTimeout(5000) {
            viewModel.state.first { it.repos.isNotEmpty() }
        }

        t.run("repos are loaded") {
            assertEquals(2, state.repos.size)
            assertTrue(state.repos.any { it.path == "acme/web-app" })
            assertTrue(state.repos.any { it.path == "acme/api-server" })
        }

        t.run("harnesses are loaded") {
            assertEquals(2, state.harnesses.size)
            assertTrue(state.harnesses.any { it.name == "claude-code" })
            assertTrue(state.harnesses.any { it.name == "codex" })
        }

        t.run("first repo is auto-selected") {
            assertEquals(1, state.selectedRepos.size)
            assertEquals("acme/web-app", state.selectedRepos[0].path)
        }

        t.run("first harness is auto-selected") {
            assertEquals("claude-code", state.selectedHarness)
        }

        t.run("harness models are available for UI") {
            val models = state.harnesses.first { it.name == state.selectedHarness }.models
            assertEquals(listOf("sonnet", "opus"), models)
        }

        t.run("available repos excludes selected") {
            val allAvailable = state.availableRecent + state.availableRest
            assertTrue(allAvailable.none { it.path == "acme/web-app" })
            assertTrue(allAvailable.any { it.path == "acme/api-server" })
        }
    }

    @Test
    fun testLoadFormDataWithPreferencesRestoresHarness() = runBlocking {
        server = MockWebServer()
        server.dispatcher = object : Dispatcher() {
            override fun dispatch(request: RecordedRequest): MockResponse {
                val path = request.requestUrl?.encodedPath ?: return MockResponse()
                    .setResponseCode(404)
                return when (path) {
                    "/api/caic/v1/server/config" -> jsonResponse(
                        """{"displayName":"test-host","tailscaleAvailable":false,""" +
                            """"usbAvailable":false,"displayAvailable":false,""" +
                            """"sudoAvailable":true,"gitHubTokenAvailable":false,""" +
                            """"voiceGateway":{"mode":"disabled"}}"""
                    )
                    "/api/caic/v1/server/repos" -> jsonResponse(
                        """[{"path":"my-org/repo","branch":"main","baseBranch":{"name":"main"}}]"""
                    )
                    "/api/caic/v1/server/harnesses" -> jsonResponse(
                        """[
                            {"name":"claude-code","models":["sonnet","opus"],"supportsImages":true,"supportsCompact":true},
                            {"name":"codex","models":["o3"],"supportsImages":false,"supportsCompact":false}
                        ]"""
                    )
                    "/api/caic/v1/server/preferences" -> jsonResponse(
                        """{"repositories":[{"path":"my-org/repo"}],""" +
                            """"harness":"codex","models":{"codex":"o3"},""" +
                            """"settings":{"sudo":false,"autoFixOnCIFailure":false,""" +
                            """"autoFixOnPROpen":false}}"""
                    )
                    else -> MockResponse().setResponseCode(404)
                }
            }
        }
        server.start()

        val baseURL = server.url("/").toString().trimEnd('/')
        val dataStore = PreferenceDataStoreFactory.create {
            File.createTempFile("test_prefs2", ".preferences_pb")
        }
        val settingsRepository = SettingsRepository(dataStore)
        settingsRepository.addServer("test")
        settingsRepository.updateServerURL(baseURL)
        withTimeout(5000) {
            settingsRepository.settings.first { it.serverURL.isNotBlank() }
        }

        val taskRepository = TaskRepository(settingsRepository)
        val context = ApplicationProvider.getApplicationContext<Application>()
        val voiceSession = VoiceSession(context, settingsRepository)
        val taskNotifier = TaskNotifier(context, taskRepository, voiceSession)
        viewModel = TaskListViewModel(taskRepository, settingsRepository, taskNotifier)

        val state = withTimeout(5000) {
            viewModel.state.first { it.repos.isNotEmpty() }
        }

        t.run("preferred harness is restored") {
            assertEquals("codex", state.selectedHarness)
        }

        t.run("preferred model is restored") {
            assertEquals("o3", state.selectedModel)
        }

        t.run("repo from preferences is selected") {
            assertEquals(1, state.selectedRepos.size)
            assertEquals("my-org/repo", state.selectedRepos[0].path)
        }
    }

    private fun jsonResponse(body: String): MockResponse = MockResponse()
        .addHeader("Content-Type", "application/json")
        .setBody(body)
}
