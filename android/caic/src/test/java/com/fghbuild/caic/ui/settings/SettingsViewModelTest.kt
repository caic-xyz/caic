// Unit tests for SettingsViewModel: state transitions and preferences.
package com.fghbuild.caic.ui.settings

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import com.caic.sdk.v1.ApiClient
import com.fghbuild.caic.data.SettingsRepository
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlinx.coroutines.withTimeout
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
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
class SettingsViewModelTest {

    private val testDispatcher = UnconfinedTestDispatcher()
    private lateinit var server: MockWebServer
    private lateinit var settingsRepository: SettingsRepository
    private lateinit var apiClient: ApiClient

    @Before
    fun setUp() {
        Dispatchers.setMain(testDispatcher)
        server = MockWebServer()

        val dataStore = PreferenceDataStoreFactory.create {
            File.createTempFile("svm_test", ".preferences_pb")
        }
        settingsRepository = SettingsRepository(dataStore)
        apiClient = ApiClient(baseURL = server.url("/").toString().trimEnd('/'))
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
        server.shutdown()
    }

    private fun createViewModel(): SettingsViewModel =
        SettingsViewModel(settingsRepository, apiClient)

    private suspend fun setupServerURL(url: String) {
        settingsRepository.addServer("test")
        settingsRepository.updateServerURL(url)
        withTimeout(5000) {
            settingsRepository.settings.first { it.serverURL.isNotBlank() }
        }
    }

    @Test
    fun `testConnection with blank URL transitions to Failed`() = runBlocking {
        val vm = createViewModel()
        assertEquals(ConnectionStatus.Idle, vm.state.value.connectionStatus)

        vm.testConnection()
        assertEquals(ConnectionStatus.Failed, vm.state.value.connectionStatus)
    }

    @Test
    fun `testConnection with valid URL starts testing`() = runBlocking {
        setupServerURL(server.url("/").toString().trimEnd('/'))

        val vm = createViewModel()
        vm.updateServerURL(server.url("/").toString().trimEnd('/'))
        assertEquals(ConnectionStatus.Idle, vm.state.value.connectionStatus)

        vm.testConnection()
        assertEquals(ConnectionStatus.Testing, vm.state.value.connectionStatus)
    }

    @Test
    fun `updateAutoFixCI updates state synchronously`() = runBlocking {
        val vm = createViewModel()
        assertEquals(false, vm.state.value.autoFixCI)

        vm.updateAutoFixCI(true)
        assertEquals(true, vm.state.value.autoFixCI)

        vm.updateAutoFixCI(false)
        assertEquals(false, vm.state.value.autoFixCI)
    }

    @Test
    fun `updateAutoFixPR updates state synchronously`() = runBlocking {
        val vm = createViewModel()
        vm.updateAutoFixPR(true)
        assertEquals(true, vm.state.value.autoFixPR)
    }

    @Test
    fun `updateBaseImage updates state`() = runBlocking {
        val vm = createViewModel()
        vm.updateBaseImage("ubuntu:22.04")
        assertEquals("ubuntu:22.04", vm.state.value.baseImage)

        vm.updateBaseImage("")
        assertEquals("", vm.state.value.baseImage)
    }

    @Test
    fun `updateMaxCPUs updates state`() = runBlocking {
        val vm = createViewModel()
        vm.updateMaxCPUs("4")
        assertEquals("4", vm.state.value.maxCPUs)
    }

    @Test
    fun `updateWellKnownCache adds and updates cache entries`() = runBlocking {
        val vm = createViewModel()
        assertTrue(vm.state.value.wellKnownCaches.isEmpty())

        vm.updateWellKnownCache("npm", true)
        assertEquals(mapOf("npm" to true), vm.state.value.wellKnownCaches)

        vm.updateWellKnownCache("pip", true)
        assertEquals(mapOf("npm" to true, "pip" to true), vm.state.value.wellKnownCaches)

        vm.updateWellKnownCache("npm", false)
        assertEquals(mapOf("npm" to false, "pip" to true), vm.state.value.wellKnownCaches)
    }

    @Test
    fun `addCacheMapping appends and modifies entries`() = runBlocking {
        val vm = createViewModel()
        assertTrue(vm.state.value.cacheMappings.isEmpty())

        vm.addCacheMapping()
        assertEquals(1, vm.state.value.cacheMappings.size)

        vm.addCacheMapping()
        vm.updateCacheMapping(0, "/host/a", "/container/a")
        vm.updateCacheMapping(1, "/host/b", "/container/b")
        assertEquals("/host/a", vm.state.value.cacheMappings[0].hostPath)
        assertEquals("/container/b", vm.state.value.cacheMappings[1].containerPath)
    }

    @Test
    fun `updateCacheMapping ignores out-of-bounds index`() = runBlocking {
        val vm = createViewModel()
        vm.addCacheMapping()
        val before = vm.state.value.cacheMappings.toList()
        vm.updateCacheMapping(5, "/x", "/y")
        assertEquals(before, vm.state.value.cacheMappings)
    }

    @Test
    fun `removeCacheMapping deletes entry at index`() = runBlocking {
        val vm = createViewModel()
        vm.addCacheMapping()
        vm.addCacheMapping()
        vm.addCacheMapping()
        vm.addCacheMapping()

        vm.removeCacheMapping(1)
        assertEquals(3, vm.state.value.cacheMappings.size)
    }

    @Test
    fun `removeCacheMapping ignores out-of-bounds index`() = runBlocking {
        val vm = createViewModel()
        vm.addCacheMapping()
        val before = vm.state.value.cacheMappings.toList()
        vm.removeCacheMapping(5)
        assertEquals(before, vm.state.value.cacheMappings)
    }

    @Test
    fun `addCustomMount appends and modifies entries`() = runBlocking {
        val vm = createViewModel()
        assertTrue(vm.state.value.customMounts.isEmpty())

        vm.addCustomMount()
        assertEquals(1, vm.state.value.customMounts.size)

        vm.addCustomMount()
        vm.updateCustomMount(0, "/host/a", "/container/a")
        vm.updateCustomMount(1, "/host/b", "/container/b")
        assertEquals("/host/a", vm.state.value.customMounts[0].hostPath)
        assertEquals("/container/b", vm.state.value.customMounts[1].containerPath)
    }

    @Test
    fun `updateCustomMount ignores out-of-bounds index`() = runBlocking {
        val vm = createViewModel()
        vm.addCustomMount()
        val before = vm.state.value.customMounts.toList()
        vm.updateCustomMount(5, "/x", "/y")
        assertEquals(before, vm.state.value.customMounts)
    }

    @Test
    fun `removeCustomMount deletes entry at index`() = runBlocking {
        val vm = createViewModel()
        vm.addCustomMount()
        vm.addCustomMount()
        vm.addCustomMount()
        vm.addCustomMount()

        vm.removeCustomMount(1)
        assertEquals(3, vm.state.value.customMounts.size)
    }

    @Test
    fun `removeCustomMount ignores out-of-bounds index`() = runBlocking {
        val vm = createViewModel()
        vm.addCustomMount()
        val before = vm.state.value.customMounts.toList()
        vm.removeCustomMount(5)
        assertEquals(before, vm.state.value.customMounts)
    }

    @Test
    fun `updateServerURL updates state immediately`() = runBlocking {
        val vm = createViewModel()
        vm.updateServerURL("http://example.com")
        assertEquals("http://example.com", vm.state.value.settings.serverURL)
    }

    @Test
    fun `addServer calls repository and creates server`() = runBlocking {
        val vm = createViewModel()
        vm.addServer()

        val settings = withTimeout(3000) {
            settingsRepository.settings.first { it.servers.isNotEmpty() }
        }
        assertEquals(1, settings.servers.size)
    }
}
